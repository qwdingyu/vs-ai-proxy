package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

var syntheticToolCallSequence atomic.Uint64

// newSyntheticToolCallID 为省略 OpenAI tool_call id 的兼容上游补充关联 ID。
// ID 只承担同一会话内 assistant tool_call 与后续 tool message 的关联，不包含业务数据。
func newSyntheticToolCallID() string {
	sequence := syntheticToolCallSequence.Add(1)
	return fmt.Sprintf("call_proxy_%x_%x", uint64(time.Now().UnixNano()), sequence)
}

// normalizeRawToolCalls 统一修正所有 raw JSON/SSE 工具调用的名称与参数类型。
// OpenAI wire contract 要求 arguments 是“包含 JSON 的字符串”；部分 Ollama/New API
// 实现会直接返回 object。这里用 encoding/json 生成稳定 JSON，禁止 fmt.Sprint(map)。
func normalizeRawToolCalls(calls []any, allowedTools map[string]struct{}) (changed bool, complete bool) {
	complete = len(calls) > 0
	seenIDs := map[string]struct{}{}
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok || call == nil {
			complete = false
			continue
		}
		if normalizeRawToolCallEnvelope(call) {
			changed = true
		}
		if id, _ := call["id"].(string); strings.TrimSpace(id) != "" {
			if _, duplicate := seenIDs[id]; duplicate {
				call["id"] = newSyntheticToolCallID()
				changed = true
			}
			seenIDs[call["id"].(string)] = struct{}{}
		}
		function, ok := call["function"].(map[string]any)
		if !ok || function == nil {
			complete = false
			continue
		}
		functionChanged, functionComplete := normalizeRawFunctionCall(function, allowedTools)
		changed = changed || functionChanged
		id, _ := call["id"].(string)
		typeName, _ := call["type"].(string)
		hasValidEnvelope := strings.TrimSpace(id) != "" && typeName == "function"
		if !hasValidEnvelope || !functionComplete {
			complete = false
		}
	}
	return changed, complete
}

func normalizeRawStreamToolCalls(calls []any, allowedTools map[string]struct{}) bool {
	changed := false
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		function, _ := call["function"].(map[string]any)
		if normalized, _ := normalizeRawFunctionCall(function, allowedTools); normalized {
			changed = true
		}
	}
	return changed
}

func normalizeRawToolCallEnvelope(call map[string]any) bool {
	if call == nil {
		return false
	}
	changed := false
	id, _ := call["id"].(string)
	if strings.TrimSpace(id) == "" {
		call["id"] = newSyntheticToolCallID()
		changed = true
	}
	typeName, _ := call["type"].(string)
	trimmedType := strings.TrimSpace(typeName)
	switch {
	case trimmedType == "":
		call["type"] = "function"
		changed = true
	case strings.EqualFold(trimmedType, "function") && typeName != "function":
		call["type"] = "function"
		changed = true
	}
	return changed
}

func normalizeProviderToolCallEnvelopes(calls []provider.ToolCall) {
	seenIDs := map[string]struct{}{}
	for index := range calls {
		call := &calls[index]
		if strings.TrimSpace(call.ID) == "" {
			call.ID = newSyntheticToolCallID()
		}
		if _, duplicate := seenIDs[call.ID]; duplicate {
			call.ID = newSyntheticToolCallID()
		}
		seenIDs[call.ID] = struct{}{}
		trimmedType := strings.TrimSpace(call.Type)
		missingType := trimmedType == ""
		isFunctionType := strings.EqualFold(trimmedType, "function")
		if missingType || isFunctionType {
			call.Type = "function"
		}
	}
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

func rawToolCallsComplete(calls []any) bool {
	if len(calls) == 0 {
		return false
	}
	seenIDs := map[string]struct{}{}
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		if call == nil {
			return false
		}
		id, _ := call["id"].(string)
		typeName, _ := call["type"].(string)
		function, _ := call["function"].(map[string]any)
		hasValidEnvelope := strings.TrimSpace(id) != "" && typeName == "function"
		if !hasValidEnvelope || !rawFunctionCallComplete(function) {
			return false
		}
		if _, duplicate := seenIDs[id]; duplicate {
			return false
		}
		seenIDs[id] = struct{}{}
	}
	return true
}

func rawFunctionCallComplete(function map[string]any) bool {
	if function == nil {
		return false
	}
	name, _ := function["name"].(string)
	arguments, ok := function["arguments"].(string)
	hasName := strings.TrimSpace(name) != ""
	hasArguments := ok && strings.TrimSpace(arguments) != "" && json.Valid([]byte(arguments))
	return hasName && hasArguments
}

func rawMessageHasResponsePayload(message map[string]any) bool {
	for _, key := range []string{"content", "reasoning_content", "thinking", "refusal"} {
		switch value := message[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return true
			}
		case []any:
			if len(value) > 0 {
				return true
			}
		case map[string]any:
			if len(value) > 0 {
				return true
			}
		}
	}
	return false
}

func validateProviderToolCalls(calls []provider.ToolCall) error {
	seenIDs := map[string]struct{}{}
	for index, call := range calls {
		if strings.TrimSpace(call.ID) == "" {
			return fmt.Errorf("incomplete tool call at index %d: missing id", index)
		}
		if call.Type != "function" {
			return fmt.Errorf("incomplete tool call at index %d: invalid type %q", index, call.Type)
		}
		if _, duplicate := seenIDs[call.ID]; duplicate {
			return fmt.Errorf("incomplete tool call at index %d: duplicate id %q", index, call.ID)
		}
		seenIDs[call.ID] = struct{}{}
		if err := validateProviderFunctionCall(call.Function, index); err != nil {
			return err
		}
	}
	return nil
}

func validateProviderFunctionCall(function provider.FunctionCall, index int) error {
	if strings.TrimSpace(function.Name) == "" {
		return fmt.Errorf("incomplete tool call at index %d: missing function name", index)
	}
	arguments := strings.TrimSpace(function.Arguments)
	if arguments == "" || !json.Valid([]byte(arguments)) {
		return fmt.Errorf("incomplete tool call at index %d: invalid function arguments", index)
	}
	return nil
}

// normalizeOllamaStreamToolCallEnvelopes 将 Ollama 省略的 id/type 补成
// Visual Studio 可关联的 OpenAI tool_calls envelope，并在多 chunk 间保持 ID 稳定。
func normalizeOllamaStreamToolCallEnvelopes(chunk map[string]any, stableIDs map[int]string) error {
	message, _ := chunk["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	for position, raw := range calls {
		call, _ := raw.(map[string]any)
		if call == nil {
			continue
		}
		callIndex := position
		if rawIndex, ok := call["index"].(float64); ok {
			callIndex = int(rawIndex)
		}
		id := stableIDs[callIndex]
		if id == "" {
			id, _ = call["id"].(string)
			if strings.TrimSpace(id) == "" || ollamaToolCallIDInUse(stableIDs, callIndex, id) {
				id = newSyntheticToolCallID()
			}
			stableIDs[callIndex] = id
		}
		call["id"] = id
		typeName, _ := call["type"].(string)
		trimmedType := strings.TrimSpace(typeName)
		missingType := trimmedType == ""
		isFunctionType := strings.EqualFold(trimmedType, "function")
		if !missingType && !isFunctionType {
			return fmt.Errorf("Ollama tool call %d 的 type 无效: %q", callIndex, typeName)
		}
		if missingType || isFunctionType {
			call["type"] = "function"
		}
	}
	return nil
}

func ollamaToolCallIDInUse(stableIDs map[int]string, currentIndex int, id string) bool {
	for index, existing := range stableIDs {
		if index != currentIndex && existing == id {
			return true
		}
	}
	return false
}

func isOpenAITruncationFinishReason(finishReason string) bool {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "length", "content_filter":
		return true
	default:
		return false
	}
}

// isToolCallFinishReason 表示客户端会据此等待可执行工具 payload 的结束原因。
// 该判断必须和 modern/legacy 两套工具字段同时使用，不能单独把它当成功终态。
func isToolCallFinishReason(finishReason string) bool {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "tool_calls", "function_call":
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
			delete(message, "function_call")
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
	if len(choices) == 0 {
		return fmt.Errorf("解析响应失败: OpenAI JSON 没有 choices")
	}
	for index, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			return fmt.Errorf("解析响应失败: invalid choice at index %d", index)
		}
		message, _ := choice["message"].(map[string]any)
		if message == nil {
			return fmt.Errorf("解析响应失败: choice %d 没有 message", index)
		}
		finishReason, ok := choice["finish_reason"].(string)
		hasFinishReason := ok && strings.TrimSpace(finishReason) != ""
		if !hasFinishReason || visualStudioFinishReason(finishReason) != finishReason {
			return fmt.Errorf("解析响应失败: choice %d 的 finish_reason 无效", index)
		}
		calls, hasModernCalls := message["tool_calls"].([]any)
		hasModernCalls = hasModernCalls && len(calls) > 0
		functionCall, hasLegacyCall := message["function_call"].(map[string]any)
		hasLegacyCall = hasLegacyCall && functionCall != nil
		if hasModernCalls && hasLegacyCall {
			return fmt.Errorf("解析响应失败: choice %d 同时包含 tool_calls 和 function_call", index)
		}
		hasToolCall := false
		if hasModernCalls {
			if !rawToolCallsComplete(calls) {
				return fmt.Errorf("解析响应失败: incomplete tool_calls at choice %d", index)
			}
			if finishReason != "tool_calls" {
				return fmt.Errorf("解析响应失败: choice %d tool_calls 的 finish_reason 必须为 tool_calls，实际为 %q", index, finishReason)
			}
			hasToolCall = true
		}
		if hasLegacyCall {
			if !rawFunctionCallComplete(functionCall) {
				return fmt.Errorf("解析响应失败: incomplete function_call at choice %d", index)
			}
			if finishReason != "function_call" {
				return fmt.Errorf("解析响应失败: choice %d function_call 的 finish_reason 必须为 function_call，实际为 %q", index, finishReason)
			}
			hasToolCall = true
		}
		if finishReason == "tool_calls" && !hasModernCalls {
			return fmt.Errorf("解析响应失败: choice %d 的 finish_reason=tool_calls 但没有 tool_calls", index)
		}
		if finishReason == "function_call" && !hasLegacyCall {
			return fmt.Errorf("解析响应失败: choice %d 的 finish_reason=function_call 但没有 function_call", index)
		}
		hasPayload := rawMessageHasResponsePayload(message)
		isEmptySuccess := !hasToolCall && !isOpenAITruncationFinishReason(finishReason) && !hasPayload
		if isEmptySuccess {
			return fmt.Errorf("解析响应失败: choice %d 没有文本、推理内容或工具调用", index)
		}
	}
	return nil
}
