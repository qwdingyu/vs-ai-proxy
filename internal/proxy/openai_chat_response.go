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
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "null", "unknown":
		return "stop"
	case "stop", "length", "tool_calls", "content_filter", "function_call":
		return strings.TrimSpace(value)
	default:
		return "stop"
	}
}
