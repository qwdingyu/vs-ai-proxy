package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

type rawOllamaChatProvider interface {
	ChatRaw(ctx context.Context, req *provider.ChatRequest) ([]byte, error)
}

// normalizeOllamaNativeChatResponse 校验并最小修复非流式 Ollama 终态。
// 该路径最终返回原生 body，不能只验证转换后的 OpenAI 临时结构；否则 done=false、
// 工具终态错误或残缺 arguments 仍可能原样 200 给 /api/chat 客户端。arguments 保持
// Ollama 要求的对象形态，其他耗时/扩展字段不参与重建。
func normalizeOllamaNativeChatResponse(body []byte) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("invalid Ollama JSON: %w", err)
	}
	if rawError, exists := root["error"]; exists && rawError != nil {
		encoded, _ := json.Marshal(rawError)
		return nil, fmt.Errorf("Ollama 返回 error: %s", sanitizeDiagnosticMessage(string(encoded)))
	}
	done, ok := root["done"].(bool)
	if !ok || !done {
		return nil, fmt.Errorf("Ollama 非流式响应未以 done=true 结束")
	}
	message, ok := root["message"].(map[string]any)
	if !ok || message == nil {
		return nil, fmt.Errorf("Ollama 响应缺少 message")
	}

	changed := false
	doneReason, _ := root["done_reason"].(string)
	toolCalls, hasToolCalls := message["tool_calls"].([]any)
	if hasToolCalls && len(toolCalls) > 0 {
		if isOpenAITruncationFinishReason(doneReason) {
			delete(message, "tool_calls")
			changed = true
			hasToolCalls = false
		} else {
			if normalizeErr := normalizeNativeOllamaToolCalls(toolCalls); normalizeErr != nil {
				return nil, normalizeErr
			}
			// helper 会原地规范 string arguments 和重复 ID；工具响应统一
			// 重新序列化，确保修复进入实际写给 /api/chat 客户端的 body。
			changed = true
			if doneReason != "tool_calls" {
				root["done_reason"] = "tool_calls"
				changed = true
			}
		}
	} else if hasToolCalls && toolCalls == nil {
		return nil, fmt.Errorf("Ollama message.tool_calls 类型无效")
	}

	if !hasToolCalls || len(toolCalls) == 0 {
		normalizedReason := visualStudioFinishReason(doneReason)
		if isToolCallFinishReason(normalizedReason) {
			if nativeOllamaMessageHasPayload(message) {
				root["done_reason"] = "stop"
				changed = true
			} else {
				return nil, fmt.Errorf("Ollama 工具终态缺少 tool_calls payload")
			}
		} else if normalizedReason != doneReason {
			root["done_reason"] = normalizedReason
			changed = true
		}
	}

	if !changed {
		return body, nil
	}
	normalized, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("marshal normalized Ollama response: %w", err)
	}
	return normalized, nil
}

func normalizeNativeOllamaToolCalls(calls []any) error {
	seenIDs := map[string]struct{}{}
	for index, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok || call == nil {
			return fmt.Errorf("Ollama tool_calls[%d] 类型无效", index)
		}
		function, ok := call["function"].(map[string]any)
		if !ok || function == nil {
			return fmt.Errorf("Ollama tool_calls[%d] 缺少 function", index)
		}
		name, _ := function["name"].(string)
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("Ollama tool_calls[%d] 缺少函数名", index)
		}
		arguments, exists := function["arguments"]
		if !exists {
			return fmt.Errorf("Ollama tool_calls[%d] 缺少 arguments", index)
		}
		if argumentText, isString := arguments.(string); isString {
			var object map[string]any
			if err := json.Unmarshal([]byte(argumentText), &object); err != nil || object == nil {
				return fmt.Errorf("Ollama tool_calls[%d] arguments 不是有效 JSON 对象", index)
			}
			function["arguments"] = object
		} else if object, isObject := arguments.(map[string]any); !isObject || object == nil {
			return fmt.Errorf("Ollama tool_calls[%d] arguments 必须是 JSON 对象", index)
		}
		if id, _ := call["id"].(string); strings.TrimSpace(id) != "" {
			if _, duplicate := seenIDs[id]; duplicate {
				id = newSyntheticToolCallID()
				call["id"] = id
			}
			seenIDs[id] = struct{}{}
		}
	}
	return nil
}

func nativeOllamaMessageHasPayload(message map[string]any) bool {
	for _, key := range []string{"content", "thinking", "reasoning_content"} {
		if value, ok := message[key].(string); ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func ensureOllamaContentFromThinking(body []byte) []byte {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}

	message, _ := root["message"].(map[string]any)
	if message == nil {
		return body
	}

	content, _ := message["content"].(string)
	if content != "" {
		return body
	}
	thinking, _ := message["thinking"].(string)
	if thinking == "" {
		thinking, _ = message["reasoning_content"].(string)
	}
	if thinking == "" {
		return body
	}

	message["content"] = thinking
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func buildOllamaChatResponse(model string, resp *provider.ChatResponse) map[string]any {
	message := map[string]any{
		"role":    "assistant",
		"content": "",
	}
	if resp != nil && len(resp.Choices) > 0 {
		src := resp.Choices[0].Message
		content := src.Content
		if content == "" && src.Refusal != "" {
			content = src.Refusal
		}
		if content == "" && src.Reasoning != "" {
			content = src.Reasoning
		}
		message["content"] = content
		if src.Reasoning != "" {
			message["thinking"] = src.Reasoning
			message["reasoning_content"] = src.Reasoning
		}
		if len(src.ToolCalls) > 0 {
			message["tool_calls"] = src.ToolCalls
		} else if src.FunctionCall != nil {
			message["tool_calls"] = []map[string]any{{
				"id":       "function_call",
				"type":     "function",
				"function": src.FunctionCall,
			}}
		}
	}

	out := map[string]any{
		"model":      model,
		"created_at": time.Now().Format(time.RFC3339),
		"message":    message,
		"done":       true,
	}
	if _, ok := message["tool_calls"]; ok {
		out["done_reason"] = "tool_calls"
	}
	if resp != nil && resp.Usage != nil {
		out["prompt_eval_count"] = resp.Usage.PromptTokens
		out["eval_count"] = resp.Usage.CompletionTokens
	}
	return out
}
