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

	resp, err := collectOpenAIStreamReader(stream, "gpt-5.5", map[string]struct{}{"create_file": {}})
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

func TestCollectOpenAIStreamReaderAggregatesShellToolArguments(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_git","type":"function","function":{"name":"git","arguments":"{\"args\":[\"status\","}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"--short\"],\"cwd\":\"C:\\\\repo\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":\"Get-ChildItem"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":" -Force\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	resp, err := collectOpenAIStreamReader(stream, "gpt-5.5", map[string]struct{}{"git": {}, "powershell": {}})
	if err != nil {
		t.Fatalf("collectOpenAIStreamReader returned error: %v", err)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 2 {
		t.Fatalf("tool calls missing: %#v", resp)
	}

	gitCall := resp.Choices[0].Message.ToolCalls[0]
	if gitCall.Function.Name != "git" || gitCall.Function.Arguments != `{"args":["status","--short"],"cwd":"C:\\repo"}` {
		t.Fatalf("unexpected git call: %#v", gitCall)
	}
	powershellCall := resp.Choices[0].Message.ToolCalls[1]
	if powershellCall.Function.Name != "powershell" || powershellCall.Function.Arguments != `{"command":"Get-ChildItem -Force"}` {
		t.Fatalf("unexpected powershell call: %#v", powershellCall)
	}
}

func TestCollectOpenAIStreamReaderBlocksUndeclaredToolCalls(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":\"Remove-Item -Recurse\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	resp, err := collectOpenAIStreamReader(stream, "gpt-5.5", map[string]struct{}{"git": {}})
	if err != nil {
		t.Fatalf("collectOpenAIStreamReader returned error: %v", err)
	}
	message := resp.Choices[0].Message
	if len(message.ToolCalls) != 0 {
		t.Fatalf("undeclared tool call must be blocked: %#v", message.ToolCalls)
	}
	if !strings.Contains(message.Content, "Proxy blocked undeclared tool calls: powershell") {
		t.Fatalf("blocked tool notice missing: %#v", message)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONBlocksUndeclaredTools(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"git": {}})

	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []any  `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(normalized, &parsed); err != nil {
		t.Fatalf("unmarshal normalized response: %v; body=%s", err, normalized)
	}
	choice := parsed.Choices[0]
	if len(choice.Message.ToolCalls) != 0 {
		t.Fatalf("undeclared raw tool call must be removed: %s", normalized)
	}
	if choice.FinishReason != "stop" || !strings.Contains(choice.Message.Content, "powershell") {
		t.Fatalf("blocked tool should become visible text with stop finish: %s", normalized)
	}
}

func TestNormalizeOpenAIStreamLineBlocksUndeclaredToolCalls(t *testing.T) {
	line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":null}]}`
	normalized := normalizeOpenAIStreamLineForVisualStudioWithTools(line, map[string]struct{}{"git": {}})
	if strings.Contains(normalized, "tool_calls") {
		t.Fatalf("undeclared stream tool call must be removed: %s", normalized)
	}
	if !strings.Contains(normalized, "Proxy blocked undeclared tool calls: powershell") {
		t.Fatalf("blocked stream tool notice missing: %s", normalized)
	}
}

func TestVisualStudioFinishReasonPreservesKnownValues(t *testing.T) {
	for _, value := range []string{"stop", "length", "tool_calls", "content_filter", "function_call"} {
		if got := visualStudioFinishReason(value); got != value {
			t.Fatalf("visualStudioFinishReason(%q) = %q, want unchanged", value, got)
		}
	}
}
