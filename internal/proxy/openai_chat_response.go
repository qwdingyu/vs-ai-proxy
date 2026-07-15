package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
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
	hasNoPayload := strings.TrimSpace(message.Content) == "" &&
		strings.TrimSpace(message.Reasoning) == "" &&
		strings.TrimSpace(message.Refusal) == "" &&
		len(message.ToolCalls) == 0 && message.FunctionCall == nil
	if hasNoPayload && !isOpenAITruncationFinishReason(resp.Choices[0].FinishReason) {
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
	if err := validateProviderResponseToolContract(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func validateProviderResponseToolContract(resp *provider.ChatResponse) error {
	if resp == nil || len(resp.Choices) == 0 {
		return fmt.Errorf("chat response has no choices")
	}
	for choiceIndex, choice := range resp.Choices {
		message := choice.Message
		finishReason := strings.TrimSpace(choice.FinishReason)
		if finishReason == "" || visualStudioFinishReason(finishReason) != finishReason {
			return fmt.Errorf("choice %d 的 finish_reason 无效", choiceIndex)
		}
		hasModernCalls := len(message.ToolCalls) > 0
		hasLegacyCall := message.FunctionCall != nil
		if hasModernCalls && hasLegacyCall {
			return fmt.Errorf("choice %d 同时包含 tool_calls 和 function_call", choiceIndex)
		}
		if hasModernCalls {
			if err := validateProviderToolCalls(message.ToolCalls); err != nil {
				return fmt.Errorf("choice %d: %w", choiceIndex, err)
			}
			if finishReason != "tool_calls" {
				return fmt.Errorf("choice %d tool_calls 的 finish_reason 必须为 tool_calls，实际为 %q", choiceIndex, finishReason)
			}
		}
		if hasLegacyCall {
			if err := validateProviderFunctionCall(*message.FunctionCall, 0); err != nil {
				return fmt.Errorf("choice %d: %w", choiceIndex, err)
			}
			if finishReason != "function_call" {
				return fmt.Errorf("choice %d function_call 的 finish_reason 必须为 function_call，实际为 %q", choiceIndex, finishReason)
			}
		}
		if finishReason == "tool_calls" && !hasModernCalls {
			return fmt.Errorf("choice %d 的 finish_reason=tool_calls 但没有 tool_calls", choiceIndex)
		}
		if finishReason == "function_call" && !hasLegacyCall {
			return fmt.Errorf("choice %d 的 finish_reason=function_call 但没有 function_call", choiceIndex)
		}
		hasText := strings.TrimSpace(message.Content) != "" ||
			strings.TrimSpace(message.Reasoning) != "" ||
			strings.TrimSpace(message.Refusal) != ""
		hasPayload := hasText || hasModernCalls || hasLegacyCall
		if !hasPayload && !isOpenAITruncationFinishReason(finishReason) {
			return fmt.Errorf("choice %d 没有文本、推理内容或工具调用", choiceIndex)
		}
	}
	return nil
}

func normalizeProviderSpecificToolCalls(resp *provider.ChatResponse, allowedTools map[string]struct{}) {
	if resp != nil {
		for i := range resp.Choices {
			choice := &resp.Choices[i]
			msg := &choice.Message
			canonicalizeProviderToolCallNames(msg.ToolCalls, allowedTools)
			canonicalizeFunctionCallName(msg.FunctionCall, allowedTools)
			if isOpenAITruncationFinishReason(choice.FinishReason) {
				choice.FinishReason = visualStudioFinishReason(choice.FinishReason)
				msg.ToolCalls = nil
				msg.FunctionCall = nil
				continue
			}
			normalizeProviderToolCallEnvelopes(msg.ToolCalls)
			if len(msg.ToolCalls) > 0 && validateProviderToolCalls(msg.ToolCalls) == nil {
				choice.FinishReason = "tool_calls"
			}
			if msg.FunctionCall != nil && validateProviderFunctionCall(*msg.FunctionCall, 0) == nil {
				choice.FinishReason = "function_call"
			}
			if len(msg.ToolCalls) == 0 && msg.FunctionCall == nil {
				choice.FinishReason = visualStudioFinishReason(choice.FinishReason)
				if isToolCallFinishReason(choice.FinishReason) {
					// 工具终态却没有对应 payload 会让 VS 进入无法执行的工具分支。
					// 已有普通文本时按普通完成处理；空响应仍由最终契约校验拒绝。
					choice.FinishReason = "stop"
				}
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
		if finishReason, exists := choice["finish_reason"]; !exists || finishReason == nil {
			choice["finish_reason"] = "stop"
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
		if finishReason, _ := choice["finish_reason"].(string); isToolCallFinishReason(finishReason) {
			choice["finish_reason"] = "stop"
			changed = true
		}
		if finishReason, _ := choice["finish_reason"].(string); isOpenAITruncationFinishReason(finishReason) {
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
	if err := validateProviderResponseToolContract(resp); err != nil {
		return err
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
	if choice.Message.Refusal != "" {
		delta["refusal"] = choice.Message.Refusal
	}
	if len(choice.Message.ToolCalls) > 0 {
		delta["tool_calls"] = choice.Message.ToolCalls
	}
	// 流式失败后的非流式兜底仍可能得到 legacy function_call。
	// 只发送 finish_reason=function_call 而省略 delta.function_call 会让
	// Visual Studio 收到“调用结束”却没有可执行工具参数，表现为工具偶发失效。
	if choice.Message.FunctionCall != nil {
		delta["function_call"] = choice.Message.FunctionCall
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
	for position, raw := range chunks {
		chunk, ok := raw.(map[string]any)
		if !ok || chunk == nil {
			continue
		}
		index, _ := openAIStreamToolCallIndex(chunk, position)
		current := acc[index]
		if current == nil {
			current = map[string]any{"index": float64(index)}
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
				switch fnKey {
				case "arguments":
					existing, _ := fnCurrent[fnKey].(string)
					fragment, ok := fnValue.(string)
					if !ok {
						fragment, ok = normalizeToolArguments(fnValue)
					}
					if ok {
						fnCurrent[fnKey] = existing + fragment
					}
				case "name":
					mergeOpenAIIdentityFragment(fnCurrent, fnKey, fnValue)
				default:
					if isEmptyToolChunkValue(fnCurrent[fnKey]) {
						fnCurrent[fnKey] = fnValue
					}
				}
			}
		case "id":
			mergeOpenAIIdentityFragment(current, key, value)
		default:
			if isEmptyToolChunkValue(current[key]) {
				current[key] = value
			}
		}
	}
}

func mergeOpenAIIdentityFragment(target map[string]any, key string, value any) {
	fragment, ok := value.(string)
	if !ok || fragment == "" {
		return
	}
	existing, _ := target[key].(string)
	merged := mergedOpenAIIdentity(existing, fragment)
	if merged != existing {
		target[key] = merged
	}
}

func mergedOpenAIIdentity(existing, fragment string) string {
	switch {
	case existing == "":
		return fragment
	case existing == fragment:
		return existing
	case strings.HasPrefix(fragment, existing):
		// 少数兼容上游发送累计值而不是纯 delta。
		return fragment
	default:
		return existing + fragment
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
	syntheticIDs  map[int]map[int]string
	seenToolIDs   map[string]string
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
		syntheticIDs:  map[int]map[int]string{},
		seenToolIDs:   map[string]string{},
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
		calls, hasModernCalls := delta["tool_calls"].([]any)
		hasModernCalls = hasModernCalls && len(calls) > 0
		functionCall, hasLegacyCall := delta["function_call"].(map[string]any)
		hasLegacyCall = hasLegacyCall && functionCall != nil
		if hasModernCalls && hasLegacyCall {
			s.err = fmt.Errorf("SSE choice %d 同时包含 tool_calls 和 function_call", choiceIndex)
			return line
		}
		if hasModernCalls {
			if s.toolKinds[choiceIndex] == "function_call" {
				s.err = fmt.Errorf("SSE choice %d 混用了 tool_calls 和 function_call", choiceIndex)
				return line
			}
			s.toolKinds[choiceIndex] = "tool_calls"
			if s.normalizeStreamToolCallEnvelopes(choiceIndex, calls, choiceToolChunks) {
				changed = true
			}
			if normalizeRawStreamToolCalls(calls, s.allowedTools) {
				changed = true
			}
			mergeOpenAIStreamToolCalls(choiceToolChunks, calls)
		}
		if hasLegacyCall {
			if s.toolKinds[choiceIndex] == "tool_calls" {
				s.err = fmt.Errorf("SSE choice %d 混用了 tool_calls 和 function_call", choiceIndex)
				return line
			}
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
			if len(choiceToolChunks) == 0 && isToolCallFinishReason(s.finishReasons[choiceIndex]) {
				// 没有任何工具分片时，工具 finish 只能视为上游结束原因误标。
				// 已跟踪但不完整的工具不能走这里，仍会在 validateFinal 中失败。
				choice["finish_reason"] = "stop"
				s.finishReasons[choiceIndex] = "stop"
				changed = true
			}
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

func (s *openAIStreamToolSanitizer) normalizeStreamToolCallEnvelopes(
	choiceIndex int,
	calls []any,
	currentCalls map[int]map[string]any,
) bool {
	if s.seenToolIDs == nil {
		s.seenToolIDs = map[string]string{}
	}
	// owner 快照只代表事件开始前已经建立的跨事件关联。同一事件内重复 ID
	// 仍应由后续 duplicate 修复逻辑生成 synthetic ID，不能误判成跨事件冲突。
	priorToolIDOwners := make(map[string]string, len(s.seenToolIDs))
	for id, owner := range s.seenToolIDs {
		priorToolIDOwners[id] = owner
	}
	generated := s.syntheticIDs[choiceIndex]
	if generated == nil {
		generated = map[int]string{}
		s.syntheticIDs[choiceIndex] = generated
	}
	// 先预留本事件中所有合法显式 index。缺失 index 的工具如果直接使用
	// 数组位置，可能与稍后出现的显式 index 冲突并再次被合并。
	reservedIndexes := make(map[int]struct{}, len(calls))
	eventToolIDOwners := make(map[string]string, len(calls))
	for position, raw := range calls {
		call, _ := raw.(map[string]any)
		if index, valid := openAIStreamToolCallIndex(call, position); valid {
			reservedIndexes[index] = struct{}{}
		}
	}
	changed := false
	for position, raw := range calls {
		call, _ := raw.(map[string]any)
		if call == nil {
			continue
		}
		callIndex, validIndex := openAIStreamToolCallIndex(call, position)
		incomingID, _ := call["id"].(string)
		if validIndex && strings.TrimSpace(incomingID) != "" {
			_, seenInEvent := eventToolIDOwners[incomingID]
			if !seenInEvent {
				if knownIndex, known := toolCallIndexFromOwners(priorToolIDOwners, choiceIndex, incomingID); known && knownIndex != callIndex {
					// 同一个已知 ID 不能在后续分片切换到另一个显式 index；
					// 这会把两个工具的参数和名称错误拼接，必须拒绝矛盾上游。
					s.err = fmt.Errorf(
						"SSE choice %d 的工具 id %q 属于 index %d，但续片声明 index %d",
						choiceIndex,
						incomingID,
						knownIndex,
						callIndex,
					)
					return changed
				}
			}
		}
		if !validIndex {
			if knownIndex, known := toolCallIndexFromOwners(priorToolIDOwners, choiceIndex, incomingID); known {
				// 后续分片可能只重复 id 而省略 index；优先使用首个
				// 分片登记的 owner，不能按本事件位置猜测归属。
				callIndex = knownIndex
				validIndex = true
				call["index"] = float64(callIndex)
				changed = true
			}
		}
		if !validIndex && strings.TrimSpace(incomingID) == "" && len(currentCalls) == 1 {
			// 续片没有 id，但当前 choice 只有一个已知工具时归属唯一。
			// 必须恢复实际 index，不能假设唯一工具一定使用 index 0。
			for existingIndex := range currentCalls {
				callIndex = existingIndex
			}
			validIndex = true
			call["index"] = float64(callIndex)
			changed = true
		}
		if !validIndex {
			if len(currentCalls) > 0 {
				// 已有工具状态时，未知 id 既可能是新工具，也可能是 identity
				// 片段。宁可拒绝残缺流，也不能猜测后静默合并到错误工具。
				s.err = fmt.Errorf("SSE choice %d 的工具续片缺少有效 index，且无法通过 id 唯一关联", choiceIndex)
				return changed
			}
			// 少数兼容上游省略 index 或发送了错误类型；同一 SSE 事件内
			// 从数组位置开始选择未被显式 index 占用的值，避免并行工具合并。
			for {
				if _, occupied := reservedIndexes[callIndex]; !occupied {
					break
				}
				callIndex++
			}
			call["index"] = float64(callIndex)
			changed = true
		}
		reservedIndexes[callIndex] = struct{}{}
		current := currentCalls[callIndex]
		callKey := fmt.Sprintf("%d:%d", choiceIndex, callIndex)
		if strings.TrimSpace(incomingID) != "" {
			eventToolIDOwners[incomingID] = callKey
		}
		if generated[callIndex] != "" {
			if current != nil {
				if _, exists := call["id"]; exists {
					// 已向下游发送 synthetic id 后不能再混入迟到的上游 id。
					delete(call, "id")
					changed = true
				}
			}
			continue
		}
		if current == nil {
			id, _ := call["id"].(string)
			if strings.TrimSpace(id) == "" {
				id = newSyntheticToolCallID()
				call["id"] = id
				generated[callIndex] = id
				changed = true
			} else if s.toolCallIDInUse(choiceIndex, callIndex, id, callKey) {
				id = newSyntheticToolCallID()
				call["id"] = id
				generated[callIndex] = id
				changed = true
			}
			s.reserveToolCallID(id, callKey)
			typeName, _ := call["type"].(string)
			trimmedType := strings.TrimSpace(typeName)
			missingType := trimmedType == ""
			nonCanonicalFunctionType := strings.EqualFold(trimmedType, "function") && typeName != "function"
			if missingType || nonCanonicalFunctionType {
				call["type"] = "function"
				changed = true
			}
			continue
		}
		// 少数兼容上游会把 id 拆成累计分片。保留不会造成冲突的分片；
		// 如果合并后的候选 ID 已被同 choice 的其他工具占用，则丢弃迟到片段，
		// 保留当前已发送的关联 ID，避免整条工具流在终态校验时失败。
		if incomingID, ok := call["id"].(string); ok && strings.TrimSpace(incomingID) != "" {
			currentID, _ := current["id"].(string)
			candidateID := mergedOpenAIIdentity(currentID, incomingID)
			if candidateID != currentID && s.toolCallIDInUse(choiceIndex, callIndex, candidateID, callKey) {
				delete(call, "id")
				changed = true
			} else {
				s.reserveToolCallID(candidateID, callKey)
			}
		}
	}
	return changed
}

func (s *openAIStreamToolSanitizer) knownToolCallIndex(choiceIndex int, id string) (int, bool) {
	if s == nil {
		return 0, false
	}
	return toolCallIndexFromOwners(s.seenToolIDs, choiceIndex, id)
}

func toolCallIndexFromOwners(owners map[string]string, choiceIndex int, id string) (int, bool) {
	if strings.TrimSpace(id) == "" {
		return 0, false
	}
	owner, exists := owners[id]
	if !exists {
		return 0, false
	}
	prefix := strconv.Itoa(choiceIndex) + ":"
	if !strings.HasPrefix(owner, prefix) {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(owner, prefix))
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func (s *openAIStreamToolSanitizer) toolCallIDInUse(choiceIndex, callIndex int, id, currentKey string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	if owner, exists := s.seenToolIDs[id]; exists && owner != currentKey {
		return true
	}
	for otherChoice, calls := range s.toolChunks {
		for otherIndex, call := range calls {
			if otherChoice == choiceIndex && otherIndex == callIndex {
				continue
			}
			if existing, _ := call["id"].(string); existing == id {
				return true
			}
		}
	}
	return false
}

func (s *openAIStreamToolSanitizer) reserveToolCallID(id, owner string) {
	if s == nil || strings.TrimSpace(id) == "" {
		return
	}
	for existing, existingOwner := range s.seenToolIDs {
		if existingOwner == owner && existing != id {
			delete(s.seenToolIDs, existing)
		}
	}
	s.seenToolIDs[id] = owner
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

func openAIStreamToolCallIndex(call map[string]any, fallback int) (int, bool) {
	if call == nil {
		return fallback, false
	}
	switch raw := call["index"].(type) {
	case float64:
		index := int(raw)
		if raw >= 0 && float64(index) == raw {
			return index, true
		}
	case int:
		if raw >= 0 {
			return raw, true
		}
	case int64:
		index := int(raw)
		if raw >= 0 && int64(index) == raw {
			return index, true
		}
	case json.Number:
		value, err := raw.Int64()
		index := int(value)
		if err == nil && value >= 0 && int64(index) == value {
			return index, true
		}
	}
	return fallback, false
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
