package converter

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

var ErrStreamDone = fmt.Errorf("stream done")

// ParseOllamaStreamChunk 解析单行 Ollama 流式响应数据。
func ParseOllamaStreamChunk(line string) (map[string]any, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") {
		return nil, fmt.Errorf("skip comment/empty line")
	}

	payload := trimmed
	if strings.HasPrefix(trimmed, "data:") {
		payload = strings.TrimSpace(trimmed[5:])
	}

	if payload == "[DONE]" {
		return nil, ErrStreamDone
	}

	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return nil, fmt.Errorf("unmarshal ollama chunk failed: %w", err)
	}
	return data, nil
}

// ConvertOllamaChunkToOpenAISSE 将单条 Ollama chat chunk 转换为 OpenAI SSE 行。
func ConvertOllamaChunkToOpenAISSE(chunk map[string]any, requestModel string) ([]byte, error) {
	if chunk == nil {
		return nil, fmt.Errorf("nil chunk")
	}

	message := buildAssistantMessage(chunk)
	created := int64(getFloat(chunk, "created_at", float64(timeNowUnix())))
	delta := map[string]any{
		"role":    message["role"],
		"content": message["content"],
	}
	if v, ok := message["reasoning_content"]; ok && v != "" {
		delta["reasoning_content"] = v
	}
	if v, ok := message["tool_calls"]; ok && v != nil {
		delta["tool_calls"] = v
	}

	streamChunk := map[string]any{
		"id":      buildChatCompletionID(),
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   coalesceString(getString(chunk, "model"), requestModel),
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	}

	if done, _ := chunk["done"].(bool); done {
		finishReason := getFinishReason(chunk)
		finishDelta := map[string]any{}
		if finishReason == "tool_calls" {
			if calls, ok := message["tool_calls"]; ok && calls != nil {
				finishDelta["tool_calls"] = calls
			}
		}
		streamChunk["choices"] = []map[string]any{{
			"index":         0,
			"delta":         finishDelta,
			"finish_reason": finishReason,
		}}

		promptTokens := int(getFloat(chunk, "prompt_eval_count", 0))
		completionTokens := int(getFloat(chunk, "eval_count", 0))
		if promptTokens != 0 || completionTokens != 0 {
			streamChunk["usage"] = map[string]any{
				"prompt_tokens":     promptTokens,
				"completion_tokens": completionTokens,
				"total_tokens":      promptTokens + completionTokens,
			}
		}
	}

	payload, err := json.Marshal(streamChunk)
	if err != nil {
		return nil, fmt.Errorf("convert ollama stream chunk to openai failed: %w", err)
	}
	return []byte("data: " + string(payload) + "\n"), nil
}

// OpenAI2OllamaChatRequest 将 OpenAI 聊天请求转换为 Ollama chat 请求。
func OpenAI2OllamaChatRequest(body []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}

	dst := map[string]any{
		"model":    getString(src, "model"),
		"messages": normalizeMessages(getSlice(src, "messages")),
		"stream":   getBool(src, "stream"),
	}

	// OpenAI 的采样参数和 stop 必须映射到 Ollama options；stop 放在请求
	// 顶层会被原生 Ollama 忽略。显式 options 的值优先于顶层兼容字段。
	options := buildOptions(src)
	if rawOptions, exists := src["options"]; exists && rawOptions != nil {
		explicitOptions, ok := rawOptions.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("convert openai to ollama chat request failed: options must be an object")
		}
		for key, value := range explicitOptions {
			options[key] = value
		}
	}
	if stop, exists := src["stop"]; exists && stop != nil {
		options["stop"] = stop
	}
	if len(options) > 0 {
		dst["options"] = options
	}
	if v, ok := src["tools"]; ok && v != nil {
		dst["tools"] = v
	} else if functions, ok := src["functions"]; ok && functions != nil {
		dst["tools"] = toolsFromLegacyFunctions(functions)
	}
	for _, field := range []string{"tool_choice", "parallel_tool_calls", "function_call"} {
		if value, ok := src[field]; ok && value != nil {
			dst[field] = value
		}
	}

	out, err := json.Marshal(dst)
	if err != nil {
		return nil, fmt.Errorf("convert openai to ollama chat request failed: %w", err)
	}
	return out, nil
}

// OllamaChatResponse2OpenAI 将单条 Ollama chat 响应转换为 OpenAI chat completion 响应。
func OllamaChatResponse2OpenAI(body []byte, requestModel string) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}

	message := buildAssistantMessage(src)
	created := int64(getFloat(src, "created_at", float64(timeNowUnix())))
	finishReason := getFinishReason(src)
	if isTruncatedFinishReason(finishReason) {
		delete(message, "tool_calls")
	}

	resp := map[string]any{
		"id":      buildChatCompletionID(),
		"object":  "chat.completion",
		"created": created,
		"model":   coalesceString(getString(src, "model"), requestModel),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}

	promptTokens := int(getFloat(src, "prompt_eval_count", 0))
	completionTokens := int(getFloat(src, "eval_count", 0))
	if promptTokens != 0 || completionTokens != 0 {
		resp["usage"] = map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		}
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("convert ollama chat response to openai failed: %w", err)
	}
	return out, nil
}

// BuildOllamaShowResponse 构建 /api/show 的 Ollama 展示响应。
func BuildOllamaShowResponse(model string, ctxLength, maxOutput int, family string, supportsTools, supportsVision bool, exec map[string]any) ([]byte, error) {
	capabilities := []string{"completion"}
	if supportsTools {
		capabilities = append(capabilities, "tools")
	}
	if supportsVision {
		capabilities = append(capabilities, "vision")
	}

	resp := map[string]any{
		"model":       model,
		"modified_at": timeNowRFC3339(),
		"size":        3826793677,
		"digest":      "sha256:" + strings.Repeat("0", 64),
		"license":     "NIM API",
		"modelfile":   "FROM " + model,
		"parameters":  buildParametersString(ctxLength, maxOutput, exec),
		"template":    "{{ .Prompt }}",
		"details": map[string]any{
			"parent_model":       "",
			"format":             "api",
			"family":             coalesceString(family, "api"),
			"families":           []string{coalesceString(family, "api")},
			"parameter_size":     "api",
			"quantization_level": "none",
		},
		"model_info": map[string]any{
			"general.architecture":   coalesceString(family, "api"),
			"general.basename":       model,
			"general.context_length": ctxLength,
			"context_length":         ctxLength,
			"max_output_tokens":      maxOutput,
			"input_token_limit":      ctxLength,
			"output_token_limit":     maxOutput,
			"supports_tools":         supportsTools,
			"supports_tool_calls":    supportsTools,
			"supports_vision":        supportsVision,
			"supports_images":        supportsVision,
		},
		"capabilities":           capabilities,
		"context_length":         ctxLength,
		"max_output_tokens":      maxOutput,
		"input_token_limit":      ctxLength,
		"output_token_limit":     maxOutput,
		"supports_tools":         supportsTools,
		"supports_tool_calls":    supportsTools,
		"supports_vision":        supportsVision,
		"supports_images":        supportsVision,
		"recommended_parameters": buildRecommendedParameters(exec),
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("build ollama show response failed: %w", err)
	}
	return out, nil
}

func getString(src map[string]any, key string) string {
	v, _ := src[key].(string)
	return v
}

func getBool(src map[string]any, key string) bool {
	v, ok := src[key].(bool)
	return ok && v
}

func getFloat(src map[string]any, key string, def float64) float64 {
	switch v := src[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, err := v.Float64()
		if err == nil {
			return f
		}
	}
	return def
}

func getSlice(src map[string]any, key string) []any {
	v, _ := src[key].([]any)
	return v
}

func coalesceString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func buildOptions(src map[string]any) map[string]any {
	options := map[string]any{}
	copyIfPresent(options, src, "temperature")
	copyIfPresent(options, src, "top_p")
	copyIfPresent(options, src, "max_tokens")
	copyIfPresent(options, src, "top_k")
	copyIfPresent(options, src, "reasoning_effort")
	return options
}

func copyIfPresent(dst map[string]any, src map[string]any, key string) {
	if v, ok := src[key]; ok && v != nil {
		dst[key] = v
	}
}

func toolsFromLegacyFunctions(functions any) []map[string]any {
	items, ok := functions.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		fn, ok := item.(map[string]any)
		if !ok || fn == nil {
			continue
		}
		out = append(out, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return out
}

func normalizeMessages(messages []any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, item := range messages {
		m, ok := item.(map[string]any)
		if !ok || m == nil {
			continue
		}

		msg := make(map[string]any, len(m))
		for key, value := range m {
			if value != nil {
				msg[key] = value
			}
		}
		msg["role"] = coalesceString(getString(m, "role"))
		if _, exists := msg["content"]; !exists {
			msg["content"] = ""
		}
		out = append(out, msg)
	}
	return out
}

func buildAssistantMessage(src map[string]any) map[string]any {
	messageSrc := src
	if nested, ok := src["message"].(map[string]any); ok && nested != nil {
		messageSrc = nested
	}

	message := map[string]any{
		"role":    "assistant",
		"content": coalesceString(getString(messageSrc, "content"), getString(src, "message")),
	}

	if v, ok := messageSrc["tool_calls"]; ok && v != nil {
		message["tool_calls"] = normalizeToolCallArguments(v)
	}
	if reasoning, ok := messageSrc["thinking"].(string); ok && reasoning != "" {
		message["reasoning_content"] = reasoning
	} else if reasoning, ok := messageSrc["reasoning_content"].(string); ok && reasoning != "" {
		message["reasoning_content"] = reasoning
	}

	return message
}

func normalizeToolCallArguments(value any) any {
	calls, ok := value.([]any)
	if !ok {
		return value
	}

	normalized := make([]any, 0, len(calls))
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			normalized = append(normalized, item)
			continue
		}

		callCopy := make(map[string]any, len(call))
		for key, field := range call {
			callCopy[key] = field
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			normalized = append(normalized, callCopy)
			continue
		}

		functionCopy := make(map[string]any, len(function))
		for key, field := range function {
			functionCopy[key] = field
		}
		arguments, exists := function["arguments"]
		if exists && arguments != nil {
			if _, isString := arguments.(string); !isString {
				// OpenAI 工具协议要求 arguments 是 JSON 字符串；Ollama 原生接口常直接返回对象。
				if encoded, err := json.Marshal(arguments); err == nil {
					functionCopy["arguments"] = string(encoded)
				}
			}
		}
		callCopy["function"] = functionCopy
		normalized = append(normalized, callCopy)
	}

	return normalized
}

func getFinishReason(src map[string]any) string {
	finishReason := "stop"
	switch v := src["done_reason"].(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			finishReason = strings.ToLower(strings.TrimSpace(v))
		}
	}
	if isTruncatedFinishReason(finishReason) {
		return finishReason
	}
	messageSrc := src
	if nested, ok := src["message"].(map[string]any); ok && nested != nil {
		messageSrc = nested
	}
	if calls, ok := messageSrc["tool_calls"].([]any); ok && len(calls) > 0 {
		return "tool_calls"
	}
	return finishReason
}

func isTruncatedFinishReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "content_filter":
		return true
	default:
		return false
	}
}

func buildChatCompletionID() string {
	return fmt.Sprintf("chatcmpl-%d", timeNowUnix())
}

func timeNowUnix() int64 {
	return timeNow().Unix()
}

func timeNowRFC3339() string {
	return timeNow().Format(timeRFC3339)
}

func buildParametersString(ctxLength, maxOutput int, exec map[string]any) string {
	parts := []string{
		fmt.Sprintf("num_ctx %d", ctxLength),
		fmt.Sprintf("num_predict %d", maxOutput),
	}
	if v, ok := exec["temperature"].(float64); ok {
		parts = append(parts, fmt.Sprintf("temperature %g", v))
	}
	if v, ok := exec["top_p"].(float64); ok {
		parts = append(parts, fmt.Sprintf("top_p %g", v))
	}
	if v, ok := exec["max_tokens"].(int); ok {
		parts = append(parts, fmt.Sprintf("max_tokens %d", v))
	}
	if v, ok := exec["reasoning_effort"].(string); ok && strings.TrimSpace(v) != "" {
		parts = append(parts, fmt.Sprintf("reasoning_effort %s", v))
	}
	return strings.Join(parts, "\n")
}

func buildRecommendedParameters(exec map[string]any) map[string]any {
	out := map[string]any{}
	copyIfPresent(out, exec, "temperature")
	copyIfPresent(out, exec, "top_p")
	copyIfPresent(out, exec, "max_tokens")
	copyIfPresent(out, exec, "reasoning_effort")
	copyIfPresent(out, exec, "timeout_seconds")
	return out
}

// Placeholder time function so tests can override deterministic behavior.
var timeNow = func() time.Time { return time.Now() }

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"
