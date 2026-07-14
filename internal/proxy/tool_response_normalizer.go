package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// normalizeRawToolCalls 统一修正所有 raw JSON/SSE 工具调用的名称与参数类型。
// OpenAI wire contract 要求 arguments 是“包含 JSON 的字符串”；部分 Ollama/New API
// 实现会直接返回 object。这里用 encoding/json 生成稳定 JSON，禁止 fmt.Sprint(map)。
func normalizeRawToolCalls(calls []any, allowedTools map[string]struct{}) (changed bool, complete bool) {
	complete = len(calls) > 0
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok || call == nil {
			complete = false
			continue
		}
		function, ok := call["function"].(map[string]any)
		if !ok || function == nil {
			complete = false
			continue
		}
		functionChanged, functionComplete := normalizeRawFunctionCall(function, allowedTools)
		changed = changed || functionChanged
		complete = complete && functionComplete
	}
	return changed, complete
}

func normalizeRawFunctionCall(function map[string]any, allowedTools map[string]struct{}) (changed bool, complete bool) {
	if function == nil {
		return false, false
	}
	if canonicalizeRawFunctionName(function, allowedTools) {
		changed = true
	}
	name, _ := function["name"].(string)
	arguments, validArguments := normalizeToolArguments(function["arguments"])
	if validArguments {
		if current, ok := function["arguments"].(string); !ok || current != arguments {
			function["arguments"] = arguments
			changed = true
		}
	}
	return changed, strings.TrimSpace(name) != "" && validArguments
}

func normalizeToolArguments(value any) (string, bool) {
	switch arguments := value.(type) {
	case string:
		trimmed := strings.TrimSpace(arguments)
		return arguments, trimmed != "" && json.Valid([]byte(trimmed))
	case nil:
		return "", false
	default:
		encoded, err := json.Marshal(arguments)
		if err != nil || !json.Valid(encoded) {
			return "", false
		}
		return string(encoded), true
	}
}

func validateProviderToolCalls(calls []provider.ToolCall) error {
	for index, call := range calls {
		if strings.TrimSpace(call.Function.Name) == "" {
			return fmt.Errorf("incomplete tool call at index %d: missing function name", index)
		}
		arguments := strings.TrimSpace(call.Function.Arguments)
		if arguments == "" || !json.Valid([]byte(arguments)) {
			return fmt.Errorf("incomplete tool call at index %d: invalid function arguments", index)
		}
	}
	return nil
}

func isOpenAITruncationFinishReason(finishReason string) bool {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "length", "content_filter":
		return true
	default:
		return false
	}
}

// normalizeRawToolChoice 同时处理类型和结束语义：完整工具调用必须以
// tool_calls/function_call 结束；被 length/content_filter 截断的调用不能暴露给客户端执行。
func normalizeRawToolChoice(choice, message map[string]any, allowedTools map[string]struct{}) bool {
	if choice == nil || message == nil {
		return false
	}
	finishReason, _ := choice["finish_reason"].(string)
	if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
		changed, complete := normalizeRawToolCalls(calls, allowedTools)
		if isOpenAITruncationFinishReason(finishReason) {
			delete(message, "tool_calls")
			return true
		}
		if complete {
			if finishReason != "tool_calls" {
				choice["finish_reason"] = "tool_calls"
				changed = true
			}
			return changed
		}
		return changed
	}

	functionCall, ok := message["function_call"].(map[string]any)
	if !ok || functionCall == nil {
		return false
	}
	changed, complete := normalizeRawFunctionCall(functionCall, allowedTools)
	if isOpenAITruncationFinishReason(finishReason) {
		delete(message, "function_call")
		return true
	}
	if complete {
		if finishReason != "function_call" {
			choice["finish_reason"] = "function_call"
			changed = true
		}
		return changed
	}
	return changed
}

// validateOpenAIChatResponseBody 在 raw 透传前检查上游的 HTTP 200 正文。
// 200 + {"error":...} 仍然是失败；残缺工具参数也不能伪装成成功响应。
func validateOpenAIChatResponseBody(body []byte) error {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return fmt.Errorf("解析响应失败: invalid OpenAI JSON: %w", err)
	}
	if rawError, exists := root["error"]; exists && rawError != nil {
		encoded, _ := json.Marshal(rawError)
		return fmt.Errorf("解析响应失败: upstream returned error object: %s", sanitizeDiagnosticMessage(string(encoded)))
	}

	choices, _ := root["choices"].([]any)
	for index, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if message == nil {
			continue
		}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			_, complete := normalizeRawToolCalls(calls, nil)
			if !complete {
				return fmt.Errorf("解析响应失败: incomplete tool_calls at choice %d", index)
			}
		}
		if functionCall, ok := message["function_call"].(map[string]any); ok && functionCall != nil {
			_, complete := normalizeRawFunctionCall(functionCall, nil)
			if !complete {
				return fmt.Errorf("解析响应失败: incomplete function_call at choice %d", index)
			}
		}
	}
	return nil
}
