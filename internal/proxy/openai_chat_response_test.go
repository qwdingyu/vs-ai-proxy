package proxy

import (
	"encoding/json"
	"strings"
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

func TestOpenAIStreamBodyToChatResponseAggregatesSSE(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"!"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5")
	if err != nil {
		t.Fatalf("openAIStreamBodyToChatResponse returned error: %v", err)
	}

	var parsed struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(converted, &parsed); err != nil {
		t.Fatalf("unmarshal converted response: %v", err)
	}
	if parsed.Model != "gpt-5.5" || parsed.Choices[0].Message.Content != "Hello!" || parsed.Choices[0].FinishReason != "stop" {
		t.Fatalf("converted response = %s", string(converted))
	}
}

func TestCollectOpenAIStreamReaderAggregatesToolCallChunks(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	resp, err := collectOpenAIStreamReader(stream, "gpt-5.5")
	if err != nil {
		t.Fatalf("collectOpenAIStreamReader returned error: %v", err)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls missing: %#v", resp)
	}
	call := resp.Choices[0].Message.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "create_file" || call.Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("unexpected tool call: %#v", call)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
}

func TestVisualStudioFinishReasonPreservesKnownValues(t *testing.T) {
	for _, value := range []string{"stop", "length", "tool_calls", "content_filter", "function_call"} {
		if got := visualStudioFinishReason(value); got != value {
			t.Fatalf("visualStudioFinishReason(%q) = %q, want unchanged", value, got)
		}
	}
}
