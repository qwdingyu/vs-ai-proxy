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
		streamChunk["choices"] = []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": getFinishReason(chunk),
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

	out, err := json.Marshal(streamChunk)
	if err != nil {
		return nil, fmt.Errorf("convert ollama stream chunk to openai failed: %w", err)
	}
	return out, nil
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

	if v, ok := src["options"]; ok && v != nil {
		dst["options"] = v
	} else if hasOptions(src) {
		dst["options"] = buildOptions(src)
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

	resp := map[string]any{
		"id":      buildChatCompletionID(),
		"object":  "chat.completion",
		"created": created,
		"model":   coalesceString(getString(src, "model"), requestModel),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": getFinishReason(src),
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
	capabilities := []string{"completion", "tools"}
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

func hasOptions(src map[string]any) bool {
	_, ok := src["options"]
	return ok
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

func normalizeMessages(messages []any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, item := range messages {
		m, ok := item.(map[string]any)
		if !ok || m == nil {
			continue
		}

		msg := map[string]any{
			"role":    coalesceString(getString(m, "role")),
			"content": coalesceString(getString(m, "content")),
		}
		if tc, ok := m["tool_calls"]; ok && tc != nil {
			msg["tool_calls"] = tc
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
		message["tool_calls"] = v
	}
	if reasoning, ok := messageSrc["thinking"].(string); ok && reasoning != "" {
		message["reasoning_content"] = reasoning
	} else if reasoning, ok := messageSrc["reasoning_content"].(string); ok && reasoning != "" {
		message["reasoning_content"] = reasoning
	}

	return message
}

func getFinishReason(src map[string]any) string {
	switch v := src["done_reason"].(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "stop"
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
