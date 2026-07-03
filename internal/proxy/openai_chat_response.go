package proxy

import (
	"context"
	"encoding/json"
	"strings"

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
