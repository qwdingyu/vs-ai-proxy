package proxy

import (
	"encoding/json"
	"testing"
)

func TestNormalizeOpenAIChatResponseForVisualStudioRewritesBlankFinishReason(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":""}]}`)

	normalized := normalizeOpenAIChatResponseForVisualStudio(body)

	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(normalized, &parsed); err != nil {
		t.Fatalf("unmarshal normalized response: %v", err)
	}
	if parsed.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", parsed.Choices[0].FinishReason)
	}
}

func TestVisualStudioFinishReasonPreservesKnownValues(t *testing.T) {
	for _, value := range []string{"stop", "length", "tool_calls", "content_filter", "function_call"} {
		if got := visualStudioFinishReason(value); got != value {
			t.Fatalf("visualStudioFinishReason(%q) = %q, want unchanged", value, got)
		}
	}
}
