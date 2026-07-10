package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

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

func openAIStreamBodyToChatResponse(body []byte, model string) ([]byte, error) {
	if !bytes.Contains(body, []byte("data:")) {
		return nil, fmt.Errorf("response is not an SSE body")
	}
	// 真实代理非流式路径会优先 raw 透传 OpenAI 响应，以保留 provider 扩展字段。
	// 但 useai/gpt-5.5 这类上游可能在 stream=false 时返回 SSE；VS/普通 OpenAI 客户端
	// 此时期待 JSON 而不是 data: 行，所以必须在写给下游前聚合成标准 chat.completion。
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var content strings.Builder
	finishReason := "stop"
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "[DONE]" {
			break
		}
		chunk, err := parseOpenAIStreamPayload(payload)
		if err != nil {
			continue
		}
		content.WriteString(chunk.Content)
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(content.String()) == "" {
		return nil, fmt.Errorf("SSE response has no text content")
	}
	resp := provider.ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-sse-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: content.String()},
			FinishReason: visualStudioFinishReason(finishReason),
		}},
	}
	return json.Marshal(resp)
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
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var content strings.Builder
	var reasoning strings.Builder
	toolChunks := map[int]map[string]any{}
	finishReason := "stop"

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "[DONE]" {
			break
		}
		chunk, err := parseOpenAIStreamPayload(payload)
		if err != nil {
			continue
		}
		content.WriteString(chunk.Content)
		reasoning.WriteString(chunk.Reasoning)
		mergeOpenAIStreamToolCalls(toolChunks, chunk.ToolCalls)
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	message := provider.Message{Role: "assistant", Content: content.String(), Reasoning: reasoning.String()}
	if calls := buildProviderToolCalls(toolChunks); len(calls) > 0 {
		message.ToolCalls = calls
	}
	if len(message.ToolCalls) == 0 {
		calls, cleaned := parseDSMLToolCalls(message.Content, allowedTools)
		if len(calls) > 0 {
			message.Content = cleaned
			message.ToolCalls = calls
			finishReason = "tool_calls"
		}
	}
	if strings.TrimSpace(message.Content) == "" && strings.TrimSpace(message.Reasoning) == "" && len(message.ToolCalls) == 0 {
		return nil, fmt.Errorf("SSE response has no content or tool calls")
	}

	resp := &provider.ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-sse-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []provider.Choice{{
			Index:        0,
			Message:      message,
			FinishReason: visualStudioFinishReason(finishReason),
		}},
	}
	normalizeProviderSpecificToolCalls(resp, allowedTools)
	return resp, nil
}

func normalizeProviderSpecificToolCalls(resp *provider.ChatResponse, allowedTools map[string]struct{}) {
	normalizeDSMLToolCallsInChatResponse(resp, allowedTools)
}

func normalizeProviderSpecificToolCallsInOpenAIJSON(body []byte, allowedTools map[string]struct{}) []byte {
	if len(allowedTools) == 0 {
		return body
	}
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
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			kept, removed := sanitizeRawToolCalls(calls, allowedTools)
			if len(removed) > 0 {
				if len(kept) > 0 {
					message["tool_calls"] = kept
				} else {
					delete(message, "tool_calls")
					message["content"] = appendToolSanitizationNotice(asString(message["content"]), removed)
					choice["finish_reason"] = "stop"
				}
				changed = true
			}
			continue
		}
		if functionCall, ok := message["function_call"].(map[string]any); ok && functionCall != nil {
			name, _ := functionCall["name"].(string)
			if !isAllowedDSMLTool(name, allowedTools) {
				delete(message, "function_call")
				message["content"] = appendToolSanitizationNotice(asString(message["content"]), []string{name})
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

func sanitizeRawToolCalls(calls []any, allowedTools map[string]struct{}) ([]any, []string) {
	kept := make([]any, 0, len(calls))
	removed := []string{}
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		function, _ := call["function"].(map[string]any)
		name, _ := function["name"].(string)
		if isAllowedDSMLTool(name, allowedTools) {
			kept = append(kept, raw)
			continue
		}
		if strings.TrimSpace(name) == "" {
			name = "<empty>"
		}
		removed = append(removed, name)
	}
	return kept, removed
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
					fnCurrent[fnKey] = existing + fmt.Sprint(fnValue)
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
	line = sanitizeOpenAIStreamLineToolCalls(line, allowedTools)
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

func sanitizeOpenAIStreamLineToolCalls(line string, allowedTools map[string]struct{}) string {
	if len(allowedTools) == 0 {
		return line
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") || !strings.HasPrefix(trimmed, "data:") {
		return line
	}
	payload := strings.TrimSpace(trimmed[5:])
	if payload == "" || payload == "[DONE]" {
		return line
	}
	var root map[string]any
	if json.Unmarshal([]byte(payload), &root) != nil {
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
			continue
		}
		if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
			kept, removed := sanitizeOpenAIStreamDeltaToolCalls(calls, allowedTools)
			if len(removed) > 0 {
				if len(kept) > 0 {
					delta["tool_calls"] = kept
				} else {
					delete(delta, "tool_calls")
					delta["content"] = appendToolSanitizationNotice(asString(delta["content"]), removed)
				}
				changed = true
			}
		}
		if functionCall, ok := delta["function_call"].(map[string]any); ok && functionCall != nil {
			name, _ := functionCall["name"].(string)
			// OpenAI 流式 function_call 可能把 name 和 arguments 拆在不同 chunk；
			// 只有当前 chunk 明确给出非法 name 时才能拦截，arguments 续片必须放行。
			if strings.TrimSpace(name) != "" && !isAllowedDSMLTool(name, allowedTools) {
				delete(delta, "function_call")
				delta["content"] = appendToolSanitizationNotice(asString(delta["content"]), []string{name})
				changed = true
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

func sanitizeOpenAIStreamDeltaToolCalls(calls []any, allowedTools map[string]struct{}) ([]any, []string) {
	kept := make([]any, 0, len(calls))
	removed := []string{}
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		function, _ := call["function"].(map[string]any)
		name, _ := function["name"].(string)
		// OpenAI SSE 的 tool_calls 是增量协议：首片通常带 name，后续片经常只带
		// index/arguments。续片没有独立语义，不能按“空工具名”拦截，否则会把
		// get_file/grep_search/create_file 等合法工具的参数片段改写成普通文本。
		if strings.TrimSpace(name) == "" {
			kept = append(kept, raw)
			continue
		}
		if isAllowedDSMLTool(name, allowedTools) {
			kept = append(kept, raw)
			continue
		}
		removed = append(removed, name)
	}
	return kept, removed
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
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "null", "unknown":
		return "stop"
	case "stop", "length", "tool_calls", "content_filter", "function_call":
		return strings.TrimSpace(value)
	default:
		return "stop"
	}
}
