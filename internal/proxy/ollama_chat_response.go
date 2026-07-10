package proxy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

type rawOllamaChatProvider interface {
	ChatRaw(ctx context.Context, req *provider.ChatRequest) ([]byte, error)
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
