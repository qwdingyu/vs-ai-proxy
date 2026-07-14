package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

const maxAggregatedOpenAIStreamBytes int64 = 64 << 20

var errOpenAIStreamTooLarge = provider.ErrOpenAIStreamTooLarge

type rawOpenAIChatProvider interface {
	ChatRaw(ctx context.Context, req *provider.ChatRequest) ([]byte, error)
}

func (s *Server) cacheRawOpenAIChatResponse(body []byte) {
	var resp provider.ChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}
	s.cacheChatResponse(&resp)
}

func openAIStreamBodyToChatResponse(body []byte, model string, allowedTools map[string]struct{}) ([]byte, error) {
	if !looksLikeSSEBody(body) {
		return nil, fmt.Errorf("response is not an SSE body")
	}
	// 真实代理非流式路径会优先 raw 透传 OpenAI 响应，以保留 provider 扩展字段。
	// 但 useai/gpt-5.5 这类上游可能在 stream=false 时返回 SSE；VS/普通 OpenAI 客户端
	// 此时期待 JSON 而不是 data: 行，所以必须在写给下游前聚合成标准 chat.completion。
	// 注意：这是“非流式请求收到 SSE”的兼容兜底，不是标准流式透传路径。
	// 因此这里需要复用正式流式的工具分片合并逻辑，避免上游实际返回了 create_file
	// 但代理在 SSE->JSON 转换时丢失 tool_calls，造成 VS 端误报“无法运行工具”。
	resp, err := aggregateOpenAIStreamReader(bytes.NewReader(body), model, allowedTools)
	if err != nil {
		return nil, err
	}
	return json.Marshal(resp)
}

// looksLikeSSEBody 只识别从正文开头开始的标准 SSE 字段序列，并要求最终出现
// data 字段。这样既兼容 event/id/retry/注释前导，也不会因普通正文稍后出现
// "data:" 文本而误判。
func looksLikeSSEBody(body []byte) bool {
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	for len(body) > 0 {
		line := body
		if newline := bytes.IndexByte(body, '\n'); newline >= 0 {
			line = body[:newline]
			body = body[newline+1:]
		} else {
			body = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		field, _, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			return false
		}
		switch string(field) {
		case "data":
			return true
		case "event", "id", "retry":
			continue
		default:
			return false
		}
	}
	return false
}

func collectOpenAIStreamChatResponse(ctx context.Context, prov provider.Provider, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	streamReq := cloneChatRequest(req)
	streamReq.Stream = true
	stream, err := prov.ChatStream(ctx, streamReq)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return collectOpenAIStreamReader(stream, streamReq.Model, allowedToolNames(req))
}

func collectOpenAIStreamReader(stream io.Reader, model string, allowedTools map[string]struct{}) (*provider.ChatResponse, error) {
	resp, err := aggregateOpenAIStreamReader(stream, model, allowedTools)
	if err != nil {
		return nil, err
	}
	message := resp.Choices[0].Message
	if strings.TrimSpace(message.Content) == "" &&
		strings.TrimSpace(message.Reasoning) == "" &&
		len(message.ToolCalls) == 0 && message.FunctionCall == nil {
		return nil, fmt.Errorf("SSE response has no content or tool calls")
	}
	return resp, nil
}

func aggregateOpenAIStreamReader(stream io.Reader, model string, allowedTools map[string]struct{}) (*provider.ChatResponse, error) {
	return aggregateOpenAIStreamReaderWithLimit(stream, model, allowedTools, maxAggregatedOpenAIStreamBytes)
}

func aggregateOpenAIStreamReaderWithLimit(stream io.Reader, model string, allowedTools map[string]struct{}, maxBytes int64) (*provider.ChatResponse, error) {
	resp, err := provider.CollectOpenAIChatSSE(stream, model, maxBytes)
	if err != nil {
		return nil, err
	}
	normalizeProviderSpecificToolCalls(resp, allowedTools)
	return resp, nil
}

func normalizeProviderSpecificToolCalls(resp *provider.ChatResponse, allowedTools map[string]struct{}) {
	if resp != nil {
		for i := range resp.Choices {
			choice := &resp.Choices[i]
			msg := &choice.Message
			canonicalizeProviderToolCallNames(msg.ToolCalls, allowedTools)
			canonicalizeFunctionCallName(msg.FunctionCall, allowedTools)
			if isOpenAITruncationFinishReason(choice.FinishReason) {
				msg.ToolCalls = nil
				msg.FunctionCall = nil
				continue
			}
			if len(msg.ToolCalls) > 0 && validateProviderToolCalls(msg.ToolCalls) == nil {
				choice.FinishReason = "tool_calls"
			}
			if msg.FunctionCall != nil && validateProviderToolCalls([]provider.ToolCall{{Function: *msg.FunctionCall}}) == nil {
				choice.FinishReason = "function_call"
			}
		}
	}
	normalizeDSMLToolCallsInChatResponse(resp, allowedTools)
}

func normalizeProviderSpecificToolCallsInOpenAIJSON(body []byte, allowedTools map[string]struct{}) []byte {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	choices, ok := root["choices"].([]any)
	if !ok {
		return body
	}
	changed := false
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		message, _ := choice["message"].(map[string]any)
		if message == nil {
			continue
		}
		if normalizeRawToolChoice(choice, message, allowedTools) {
			changed = true
		}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			continue
		}
		if functionCall, ok := message["function_call"].(map[string]any); ok && functionCall != nil {
			name, _ := functionCall["name"].(string)
			if strings.TrimSpace(name) == "" {
				delete(message, "function_call")
				message["content"] = appendToolSanitizationNotice(asString(message["content"]), []string{toolNoticeName(name)})
				choice["finish_reason"] = "stop"
				changed = true
			}
			continue
		}
		content, _ := message["content"].(string)
		toolCalls, cleaned := parseDSMLToolCalls(content, allowedTools)
		if len(toolCalls) == 0 {
			continue
		}
		message["content"] = cleaned
		message["tool_calls"] = toolCalls
		choice["finish_reason"] = "tool_calls"
		changed = true
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func toolNoticeName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "空工具名"
	}
	return name
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}

func writeOpenAIChatResponseAsSSE(w http.ResponseWriter, flusher http.Flusher, resp *provider.ChatResponse) error {
	if resp == nil || len(resp.Choices) == 0 {
		return fmt.Errorf("chat response has no choices")
	}
	choice := resp.Choices[0]
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if roleChunk, err := json.Marshal(map[string]any{
		"id":      resp.ID,
		"object":  "chat.completion.chunk",
		"created": resp.Created,
		"model":   resp.Model,
		"choices": []map[string]any{{"index": choice.Index, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	}); err == nil {
		if _, writeErr := fmt.Fprintf(w, "data: %s\n\n", roleChunk); writeErr != nil {
			return writeErr
		}
	}
	delta := map[string]any{}
	if choice.Message.Content != "" {
		delta["content"] = choice.Message.Content
	}
	if choice.Message.Reasoning != "" {
		delta["reasoning_content"] = choice.Message.Reasoning
	}
	if len(choice.Message.ToolCalls) > 0 {
		delta["tool_calls"] = choice.Message.ToolCalls
	}
	if len(delta) > 0 {
		contentChunk, err := json.Marshal(map[string]any{
			"id":      resp.ID,
			"object":  "chat.completion.chunk",
			"created": resp.Created,
			"model":   resp.Model,
			"choices": []map[string]any{{"index": choice.Index, "delta": delta, "finish_reason": nil}},
		})
		if err != nil {
			return err
		}
		if _, writeErr := fmt.Fprintf(w, "data: %s\n\n", contentChunk); writeErr != nil {
			return writeErr
		}
	}
	finish := visualStudioFinishReason(choice.FinishReason)
	finishChunk, err := json.Marshal(map[string]any{
		"id":      resp.ID,
		"object":  "chat.completion.chunk",
		"created": resp.Created,
		"model":   resp.Model,
		"choices": []map[string]any{{"index": choice.Index, "delta": map[string]any{}, "finish_reason": finish}},
	})
	if err != nil {
		return err
	}
	if _, writeErr := fmt.Fprintf(w, "data: %s\n\n", finishChunk); writeErr != nil {
		return writeErr
	}
	if _, writeErr := io.WriteString(w, "data: [DONE]\n\n"); writeErr != nil {
		return writeErr
	}
	flusher.Flush()
	return nil
}

func mergeOpenAIStreamToolCalls(acc map[int]map[string]any, chunks []any) {
	for _, raw := range chunks {
		chunk, ok := raw.(map[string]any)
		if !ok || chunk == nil {
			continue
		}
		index := 0
		if rawIndex, ok := chunk["index"].(float64); ok {
			index = int(rawIndex)
		}
		current := acc[index]
		if current == nil {
			current = map[string]any{"index": float64(index), "type": "function"}
			acc[index] = current
		}
		mergeToolCallChunk(current, chunk)
	}
}

func mergeToolCallChunk(current map[string]any, chunk map[string]any) {
	for key, value := range chunk {
		switch key {
		case "function":
			fnChunk, _ := value.(map[string]any)
			if fnChunk == nil {
				continue
			}
			fnCurrent, _ := current["function"].(map[string]any)
			if fnCurrent == nil {
				fnCurrent = map[string]any{}
				current["function"] = fnCurrent
			}
			for fnKey, fnValue := range fnChunk {
				if fnKey == "arguments" {
					existing, _ := fnCurrent[fnKey].(string)
					fragment, ok := fnValue.(string)
					if !ok {
						fragment, ok = normalizeToolArguments(fnValue)
					}
					if ok {
						fnCurrent[fnKey] = existing + fragment
					}
					continue
				}
				if isEmptyToolChunkValue(fnCurrent[fnKey]) {
					fnCurrent[fnKey] = fnValue
				}
			}
		default:
			if isEmptyToolChunkValue(current[key]) {
				current[key] = value
			}
		}
	}
}

func isEmptyToolChunkValue(value any) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) == ""
}

func buildProviderToolCalls(toolChunks map[int]map[string]any) []provider.ToolCall {
	if len(toolChunks) == 0 {
		return nil
	}
	indexes := make([]int, 0, len(toolChunks))
	for index := range toolChunks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	calls := make([]provider.ToolCall, 0, len(indexes))
	for _, index := range indexes {
		data, err := json.Marshal(toolChunks[index])
		if err != nil {
			continue
		}
		var call provider.ToolCall
		if err := json.Unmarshal(data, &call); err != nil {
			continue
		}
		calls = append(calls, call)
	}
	return calls
}

func normalizeOpenAIChatResponseForVisualStudio(body []byte) []byte {
	// Visual Studio Copilot 适配：
	// 有些 OpenAI-compatible 上游会返回 finish_reason:""。Web 面板能显示 200，
	// 但 VS 客户端会把 finish_reason 当强枚举解析并抛
	// Unknown ChatFinishReason value。这里仅修正 finish_reason，不重建整个响应，
	// 以保留 provider_trace、reasoning_content 等上游扩展字段。
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}

	choices, ok := root["choices"].([]any)
	if !ok {
		return body
	}

	changed := false
	for _, item := range choices {
		choice, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if normalizeOpenAIFinishReason(choice) {
			changed = true
		}
	}
	if !changed {
		return body
	}

	normalized, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return normalized
}

func normalizeOpenAIStreamLineForVisualStudio(line string) string {
	return normalizeOpenAIStreamLineForVisualStudioWithTools(line, nil)
}

func normalizeOpenAIStreamLineForVisualStudioWithTools(line string, allowedTools map[string]struct{}) string {
	line = newOpenAIStreamToolSanitizer(allowedTools).normalizeLine(line)
	return normalizeOpenAIStreamFinishReasonForVisualStudio(line)
}

func normalizeOpenAIStreamLineForVisualStudioWithToolState(line string, sanitizer *openAIStreamToolSanitizer) string {
	if sanitizer != nil {
		line = sanitizer.normalizeLine(line)
	}
	return normalizeOpenAIStreamFinishReasonForVisualStudio(line)
}

func normalizeOpenAIStreamFinishReasonForVisualStudio(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") || !strings.HasPrefix(trimmed, "data:") {
		return line
	}

	payload := strings.TrimSpace(trimmed[5:])
	if payload == "" || payload == "[DONE]" {
		return line
	}

	normalized := normalizeOpenAIChatResponseForVisualStudio([]byte(payload))
	if string(normalized) == payload {
		return line
	}

	// Visual Studio Copilot 适配：
	// 流式路径由 OpenAI .NET SDK 逐个 SSE chunk 反序列化，不能只修最终 JSON。
	// 这里保留 SSE 协议外壳，仅修正 data JSON 内 VS 无法接受的 finish_reason。
	return "data: " + string(normalized)
}

type openAIStreamToolSanitizer struct {
	allowedTools  map[string]struct{}
	toolChunks    map[int]map[int]map[string]any
	toolKinds     map[int]string
	finishReasons map[int]string
	sawDone       bool
	started       bool
	err           error
}

func newOpenAIStreamToolSanitizer(allowedTools map[string]struct{}) *openAIStreamToolSanitizer {
	return &openAIStreamToolSanitizer{
		allowedTools:  allowedTools,
		toolChunks:    map[int]map[int]map[string]any{},
		toolKinds:     map[int]string{},
		finishReasons: map[int]string{},
	}
}

func (s *openAIStreamToolSanitizer) normalizeLine(line string) string {
	if s == nil {
		return line
	}
	if !s.started {
		line = strings.TrimPrefix(line, "\ufeff")
		s.started = true
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") || !strings.HasPrefix(trimmed, "data:") {
		return line
	}
	payload := strings.TrimSpace(trimmed[5:])
	if payload == "" {
		return line
	}
	if payload == "[DONE]" {
		s.sawDone = true
		finishChoices := []map[string]any{}
		choiceIndexes := make([]int, 0, len(s.toolChunks))
		for choiceIndex := range s.toolChunks {
			choiceIndexes = append(choiceIndexes, choiceIndex)
		}
		sort.Ints(choiceIndexes)
		for _, choiceIndex := range choiceIndexes {
			if s.finishReasons[choiceIndex] == "" && s.hasCompleteToolCalls(choiceIndex) {
				finishReason := s.toolFinishReason(choiceIndex)
				s.finishReasons[choiceIndex] = finishReason
				finishChoices = append(finishChoices, map[string]any{
					"index":         choiceIndex,
					"delta":         map[string]any{},
					"finish_reason": finishReason,
				})
			}
		}
		if len(finishChoices) > 0 {
			finishPayload, err := json.Marshal(map[string]any{"choices": finishChoices})
			if err == nil {
				return "data: " + string(finishPayload) + "\n\n" + line
			}
		}
		return line
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		s.err = fmt.Errorf("invalid SSE data payload: %w", err)
		return line
	}
	if streamErr, exists := root["error"]; exists && streamErr != nil {
		encoded, _ := json.Marshal(streamErr)
		s.err = fmt.Errorf("upstream SSE error: %s", sanitizeDiagnosticMessage(string(encoded)))
		return line
	}
	choices, _ := root["choices"].([]any)
	changed := false
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta, _ = choice["message"].(map[string]any)
		}
		if delta == nil {
			delta = map[string]any{}
		}
		choiceIndex := openAIStreamChoiceIndex(choice)
		choiceToolChunks := s.toolChunks[choiceIndex]
		if choiceToolChunks == nil {
			choiceToolChunks = map[int]map[string]any{}
			s.toolChunks[choiceIndex] = choiceToolChunks
		}
		if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
			s.toolKinds[choiceIndex] = "tool_calls"
			if normalized, _ := normalizeRawToolCalls(calls, s.allowedTools); normalized {
				changed = true
			}
			mergeOpenAIStreamToolCalls(choiceToolChunks, calls)
		}
		if functionCall, ok := delta["function_call"].(map[string]any); ok && functionCall != nil {
			if s.toolKinds[choiceIndex] == "" {
				s.toolKinds[choiceIndex] = "function_call"
			}
			if normalized, _ := normalizeRawFunctionCall(functionCall, s.allowedTools); normalized {
				changed = true
			}
			mergeOpenAIStreamToolCalls(choiceToolChunks, []any{map[string]any{
				"index":    float64(0),
				"id":       "function_call",
				"type":     "function",
				"function": functionCall,
			}})
		}
		if finishReason, ok := choice["finish_reason"].(string); ok && strings.TrimSpace(finishReason) != "" {
			s.finishReasons[choiceIndex] = visualStudioFinishReason(finishReason)
			if s.hasCompleteToolCalls(choiceIndex) && !isOpenAITruncationFinishReason(finishReason) {
				expectedFinish := s.toolFinishReason(choiceIndex)
				if s.finishReasons[choiceIndex] != expectedFinish {
					choice["finish_reason"] = expectedFinish
					changed = true
				}
				s.finishReasons[choiceIndex] = expectedFinish
			}
		}
	}
	if !changed {
		return line
	}
	normalized, err := json.Marshal(root)
	if err != nil {
		return line
	}
	return "data: " + string(normalized)
}

func (s *openAIStreamToolSanitizer) hasCompleteToolCalls(choiceIndex int) bool {
	if s == nil {
		return false
	}
	calls := buildProviderToolCalls(s.toolChunks[choiceIndex])
	return len(calls) > 0 && validateProviderToolCalls(calls) == nil
}

func (s *openAIStreamToolSanitizer) hasTrackedToolCalls() bool {
	if s == nil {
		return false
	}
	for _, chunks := range s.toolChunks {
		if len(chunks) > 0 {
			return true
		}
	}
	return false
}

func (s *openAIStreamToolSanitizer) toolFinishReason(choiceIndex int) string {
	if s != nil && s.toolKinds[choiceIndex] == "function_call" {
		return "function_call"
	}
	return "tool_calls"
}

func (s *openAIStreamToolSanitizer) validateFinal() error {
	if s == nil {
		return nil
	}
	if s.err != nil {
		return s.err
	}
	for choiceIndex, chunks := range s.toolChunks {
		calls := buildProviderToolCalls(chunks)
		if len(calls) == 0 {
			continue
		}
		finishReason := s.finishReasons[choiceIndex]
		if isOpenAITruncationFinishReason(finishReason) {
			return fmt.Errorf("SSE tool stream choice %d was truncated: finish_reason=%s", choiceIndex, finishReason)
		}
		if err := validateProviderToolCalls(calls); err != nil {
			return fmt.Errorf("SSE tool stream choice %d: %w", choiceIndex, err)
		}
		if finishReason == "" && !s.sawDone {
			return fmt.Errorf("SSE tool stream choice %d ended without finish_reason or [DONE]", choiceIndex)
		}
	}
	return nil
}

func openAIStreamChoiceIndex(choice map[string]any) int {
	if choice == nil {
		return 0
	}
	switch raw := choice["index"].(type) {
	case float64:
		return int(raw)
	case int:
		return raw
	}
	return 0
}

func openAIStreamToolCallIndex(call map[string]any) int {
	if call == nil {
		return 0
	}
	switch raw := call["index"].(type) {
	case float64:
		return int(raw)
	case int:
		return raw
	}
	return 0
}

func normalizeOpenAIFinishReason(choice map[string]any) bool {
	raw, exists := choice["finish_reason"]
	if !exists || raw == nil {
		return false
	}

	value, ok := raw.(string)
	if !ok {
		return false
	}

	normalized := visualStudioFinishReason(value)
	if normalized == value {
		return false
	}
	choice["finish_reason"] = normalized
	return true
}

func visualStudioFinishReason(value string) string {
	// Visual Studio Copilot 适配：
	// VS 已知可接受 OpenAI 标准结束原因；空字符串、"unknown" 或 provider 私有值
	// 会导致客户端失败，因此统一收敛为 stop。
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", "null", "unknown":
		return "stop"
	case "stop", "length", "tool_calls", "content_filter", "function_call":
		return normalized
	default:
		return "stop"
	}
}
