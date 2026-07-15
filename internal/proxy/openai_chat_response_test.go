package proxy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
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

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", nil)
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

func TestOpenAIStreamBodyToChatResponseDoesNotTreatJSONContentAsSSE(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"literal data: text"},"finish_reason":"stop"}]}`)

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", nil)
	if err == nil || converted != nil {
		t.Fatalf("plain JSON containing data: must not be converted: converted=%s err=%v", string(converted), err)
	}
}

func TestOpenAIStreamBodyToChatResponseAcceptsLeadingWhitespaceSSE(t *testing.T) {
	body := []byte("\n\t data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n")

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", nil)
	if err != nil {
		t.Fatalf("openAIStreamBodyToChatResponse returned error: %v", err)
	}
	if !strings.Contains(string(converted), `"content":"ok"`) {
		t.Fatalf("converted response = %s, want content ok", string(converted))
	}
}

func TestOpenAIStreamBodyToChatResponseAcceptsStandardSSEMetadataPreamble(t *testing.T) {
	body := []byte(strings.Join([]string{
		``,
		`: upstream keepalive`,
		`event: completion.chunk`,
		`id: chunk-1`,
		`retry: 1000`,
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", nil)
	if err != nil {
		t.Fatalf("openAIStreamBodyToChatResponse returned error: %v", err)
	}
	if !strings.Contains(string(converted), `"content":"ok"`) {
		t.Fatalf("converted response = %s, want content ok", string(converted))
	}
}

func TestOpenAIStreamBodyToChatResponseAcceptsDataLineLargerThanDetectorBuffer(t *testing.T) {
	content := strings.Repeat("x", 70*1024)
	body := []byte(`data: {"choices":[{"delta":{"content":"` + content + `"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n")

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", nil)
	if err != nil {
		t.Fatalf("large SSE data line must be detected and aggregated: %v", err)
	}
	var parsed provider.ChatResponse
	if err := json.Unmarshal(converted, &parsed); err != nil {
		t.Fatalf("unmarshal converted response: %v", err)
	}
	if got := parsed.Choices[0].Message.Content; got != content {
		t.Fatalf("content length = %d, want %d", len(got), len(content))
	}
}

func TestLooksLikeSSEBodyRejectsOrdinaryBodiesWithSSELikeText(t *testing.T) {
	for _, body := range []string{
		`{"message":"data: embedded text"}`,
		"plain text\ndata: embedded later",
		"id: this is ordinary metadata",
		"event: audit\nordinary text",
	} {
		if looksLikeSSEBody([]byte(body)) {
			t.Fatalf("looksLikeSSEBody(%q) = true, want false", body)
		}
	}
}

func TestOpenAIStreamBodyToChatResponseRejectsEmptyChoiceSSE(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"id":"","object":"chat.completion.chunk","model":"gpt-5.5","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":0,"total_tokens":12}}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	if converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", nil); err == nil {
		t.Fatalf("empty SSE choices returned success: %s", string(converted))
	}
}

func TestOpenAIStreamBodyToChatResponsePreservesToolCalls(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"docs/a.md\",\"content\":\"ok\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", map[string]struct{}{"create_file": {}})
	if err != nil {
		t.Fatalf("openAIStreamBodyToChatResponse returned error: %v", err)
	}
	var parsed provider.ChatResponse
	if err := json.Unmarshal(converted, &parsed); err != nil {
		t.Fatalf("unmarshal converted response: %v; body=%s", err, string(converted))
	}
	if len(parsed.Choices) != 1 || parsed.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("converted response missing tool_calls finish: %s", string(converted))
	}
	calls := parsed.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls len = %d, want 1; body=%s", len(calls), string(converted))
	}
	call := calls[0]
	if call.ID != "call_create" || call.Function.Name != "create_file" || call.Function.Arguments != `{"path":"docs/a.md","content":"ok"}` {
		t.Fatalf("tool call = %#v, converted=%s", call, string(converted))
	}
}

func TestOpenAIStreamBodyToChatResponsePreservesExplicitTruncationFinishReason(t *testing.T) {
	for _, finishReason := range []string{"length", "content_filter"} {
		t.Run(finishReason, func(t *testing.T) {
			body := []byte(strings.Join([]string{
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"` + finishReason + `"}]}`,
				`data: [DONE]`,
				``,
			}, "\n"))

			converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", map[string]struct{}{"create_file": {}})
			if err != nil {
				t.Fatalf("openAIStreamBodyToChatResponse returned error: %v", err)
			}
			var parsed provider.ChatResponse
			if err := json.Unmarshal(converted, &parsed); err != nil {
				t.Fatalf("unmarshal converted response: %v; body=%s", err, string(converted))
			}
			if got := parsed.Choices[0].FinishReason; got != finishReason {
				t.Fatalf("finish_reason = %q, want %q; body=%s", got, finishReason, string(converted))
			}
			if calls := parsed.Choices[0].Message.ToolCalls; len(calls) != 0 {
				t.Fatalf("truncated tool calls must not be executable: %#v", calls)
			}
		})
	}
}

func TestOpenAIStreamBodyToChatResponseRejectsIncompleteToolCallWithStop(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", map[string]struct{}{"create_file": {}})
	if err == nil || converted != nil {
		t.Fatalf("incomplete tool call with stop must fail: converted=%s err=%v", string(converted), err)
	}
}

func TestOpenAIStreamBodyToChatResponseRejectsMalformedSSEPayload(t *testing.T) {
	body := []byte("event: completion.chunk\nid: broken\ndata: {not-json}\n\ndata: [DONE]\n")

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", nil)
	if err == nil || converted != nil {
		t.Fatalf("malformed SSE must fail aggregation: converted=%s err=%v", string(converted), err)
	}
}

func TestOpenAIStreamBodyToChatResponseRejectsSSEErrorEvent(t *testing.T) {
	body := []byte("data: {\"error\":{\"message\":\"upstream unavailable\",\"type\":\"server_error\"}}\n\ndata: [DONE]\n")

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", nil)
	if err == nil || converted != nil {
		t.Fatalf("SSE error event must fail aggregation: converted=%s err=%v", string(converted), err)
	}
	if !strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("error = %v, want upstream message", err)
	}
}

func TestOpenAIStreamBodyToChatResponseAcceptsEmptyDataHeartbeat(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data:`,
		``,
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", nil)
	if err != nil {
		t.Fatalf("empty data heartbeat must be ignored: %v", err)
	}
	if !strings.Contains(string(converted), `"content":"ok"`) {
		t.Fatalf("converted response = %s, want content ok", string(converted))
	}
}

func TestOpenAIStreamBodyToChatResponseRejectsUnterminatedToolCall(t *testing.T) {
	body := []byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`)

	converted, err := openAIStreamBodyToChatResponse(
		body,
		"gpt-5.5",
		map[string]struct{}{"create_file": {}},
	)
	if err == nil || converted != nil {
		t.Fatalf("unterminated tool stream must fail: converted=%s err=%v", string(converted), err)
	}
}

func TestOpenAIStreamBodyToChatResponseRejectsIncompleteExplicitToolCall(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5", map[string]struct{}{"create_file": {}})
	if err == nil || converted != nil {
		t.Fatalf("explicitly finished incomplete tool call must fail: converted=%s err=%v", string(converted), err)
	}
}

func TestAggregateOpenAIStreamReaderRejectsOversizedStream(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"first"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"second"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n"))

	_, err := aggregateOpenAIStreamReaderWithLimit(stream, "gpt-5.5", nil, 64)
	if !errors.Is(err, errOpenAIStreamTooLarge) {
		t.Fatalf("aggregate error = %v, want stream-too-large", err)
	}
}

func TestOpenAIStreamBodyToChatResponseInfersCompleteToolCallAtDone(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":null}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	converted, err := openAIStreamBodyToChatResponse(
		body,
		"gpt-5.5",
		map[string]struct{}{"create_file": {}},
	)
	if err != nil {
		t.Fatalf("complete tool call followed by DONE must be inferred: %v", err)
	}
	var parsed provider.ChatResponse
	if err := json.Unmarshal(converted, &parsed); err != nil {
		t.Fatalf("unmarshal converted response: %v", err)
	}
	if got := parsed.Choices[0].FinishReason; got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got)
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

func TestCollectOpenAIStreamReaderPassesThroughUndeclaredToolCalls(t *testing.T) {
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
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "powershell" {
		t.Fatalf("undeclared tool call should pass through in stable mode: %#v", message.ToolCalls)
	}
	if strings.Contains(message.Content, "Proxy blocked undeclared tool calls") {
		t.Fatalf("stable mode should not add block notice: %#v", message)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls after stable passthrough", resp.Choices[0].FinishReason)
	}
}

func TestCollectOpenAIStreamReaderKeepsToolCallsFinishWhenSomeDeclaredToolsRemain(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_grep","type":"function","function":{"name":"grep_search","arguments":"{\"query\":\"needle\"}"}},{"index":1,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	resp, err := collectOpenAIStreamReader(stream, "gpt-5.5", map[string]struct{}{"grep_search": {}})
	if err != nil {
		t.Fatalf("collectOpenAIStreamReader returned error: %v", err)
	}
	message := resp.Choices[0].Message
	if len(message.ToolCalls) != 2 || message.ToolCalls[0].Function.Name != "grep_search" || message.ToolCalls[1].Function.Name != "powershell" {
		t.Fatalf("all tool calls should remain in stable mode: %#v", message.ToolCalls)
	}
	if strings.Contains(message.Content, "Proxy blocked undeclared tool calls") {
		t.Fatalf("stable mode should not add block notice: %#v", message)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls while declared calls remain", resp.Choices[0].FinishReason)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONPassesThroughUndeclaredTools(t *testing.T) {
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
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("undeclared raw tool call should pass through in stable mode: %s", normalized)
	}
	if choice.FinishReason != "tool_calls" || strings.Contains(choice.Message.Content, "Proxy blocked undeclared tool calls") {
		t.Fatalf("stable mode should preserve tool call and finish reason: %s", normalized)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONCanonicalizesRunTestsAlias(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_test","type":"function","function":{"name":"run_tests","arguments":"{\"command\":\"go test ./...\"}"}}]},"finish_reason":"tool_calls"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"powershell": {}})

	if !strings.Contains(string(normalized), `"name":"powershell"`) || strings.Contains(string(normalized), `"name":"run_tests"`) {
		t.Fatalf("run_tests alias should canonicalize to powershell when declared: %s", normalized)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONDoesNotCanonicalizeToUndeclaredTarget(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_test","type":"function","function":{"name":"run_tests","arguments":"{\"command\":\"go test ./...\"}"}}]},"finish_reason":"tool_calls"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"get_file": {}})

	if !strings.Contains(string(normalized), `"name":"run_tests"`) || strings.Contains(string(normalized), `"name":"powershell"`) {
		t.Fatalf("run_tests alias must not canonicalize when no terminal tool is declared: %s", normalized)
	}
}

func TestNormalizeOpenAIStreamLineCanonicalizesRunTestsAlias(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"powershell": {}})
	line := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_test","type":"function","function":{"name":"run_tests","arguments":"{\"command\":"}}]},"finish_reason":null}]}`

	normalized := normalizeOpenAIStreamLineForVisualStudioWithToolState(line, sanitizer)

	if !strings.Contains(normalized, `"name":"powershell"`) || strings.Contains(normalized, `"name":"run_tests"`) {
		t.Fatalf("stream run_tests alias should canonicalize to powershell when declared: %s", normalized)
	}
}

func TestNormalizeProviderSpecificToolCallsCanonicalizesLegacyRunTestsAlias(t *testing.T) {
	resp := &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.Message{FunctionCall: &provider.FunctionCall{Name: "run_tests", Arguments: `{"command":"go test ./..."}`}}, FinishReason: "function_call"}}}

	normalizeProviderSpecificToolCalls(resp, map[string]struct{}{"powershell": {}})

	if resp.Choices[0].Message.FunctionCall == nil || resp.Choices[0].Message.FunctionCall.Name != "powershell" {
		t.Fatalf("legacy run_tests alias should canonicalize to powershell: %#v", resp.Choices[0].Message.FunctionCall)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONPassesThroughToolsWhenRequestDeclaresNone(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, nil)
	if !strings.Contains(string(normalized), `"tool_calls"`) || !strings.Contains(string(normalized), `"finish_reason":"tool_calls"`) {
		t.Fatalf("tool calls should pass through when request declares no tools in stable mode: %s", normalized)
	}
	if strings.Contains(string(normalized), "Proxy blocked undeclared tool calls") {
		t.Fatalf("stable mode should not rewrite tool call to notice: %s", normalized)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONAllowsLegacyDeclaredFunction(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","function_call":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}},"finish_reason":"function_call"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"powershell": {}})
	if !strings.Contains(string(normalized), `"function_call"`) || !strings.Contains(string(normalized), `"name":"powershell"`) {
		t.Fatalf("legacy declared function_call should be preserved: %s", normalized)
	}
	if strings.Contains(string(normalized), "Proxy blocked undeclared tool calls") || strings.Contains(string(normalized), `"finish_reason":"stop"`) {
		t.Fatalf("declared legacy function_call should not be blocked: %s", normalized)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONPassesThroughLegacyCreateFileWhenUndeclared(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","function_call":{"name":"create_file","arguments":"{\"path\":\"docs/08_当前程序架构与运行流程事实梳理.md\",\"content\":\"ok\"}"}},"finish_reason":"function_call"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"powershell": {}})

	if !strings.Contains(string(normalized), `"function_call"`) || !strings.Contains(string(normalized), `"name":"create_file"`) {
		t.Fatalf("legacy create_file must pass through in stable mode: %s", normalized)
	}
	if strings.Contains(string(normalized), "Proxy blocked undeclared tool calls") || strings.Contains(string(normalized), `"finish_reason":"stop"`) {
		t.Fatalf("legacy create_file should not be blocked: %s", normalized)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONPassesThroughLegacyGetFileWhenUndeclared(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","function_call":{"name":"get_file","arguments":"{\"filename\":\"docs/08_当前程序架构与运行流程事实梳理.md\"}"}},"finish_reason":"function_call"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"powershell": {}})

	if !strings.Contains(string(normalized), `"function_call"`) || !strings.Contains(string(normalized), `"name":"get_file"`) {
		t.Fatalf("legacy get_file must pass through in stable mode: %s", normalized)
	}
	if strings.Contains(string(normalized), "Proxy blocked undeclared tool calls") || strings.Contains(string(normalized), `"finish_reason":"stop"`) {
		t.Fatalf("legacy get_file should not be blocked: %s", normalized)
	}
}

func TestOpenAIToolCallCompatibilityMatrixKeepsKnownVSTools(t *testing.T) {
	// VS Copilot 在不同模型/网关组合下可能返回 OpenAI tool_calls、legacy
	// function_call、SSE 增量 tool_calls 或 SSE legacy function_call。这里用矩阵
	// 锁住核心原则：代理只做兼容性归一化，不因本地未声明而删除已命名工具。
	tests := []struct {
		name       string
		tool       string
		normalized func(string) string
		want       string
	}{
		{
			name: "json tool_calls create_file",
			tool: "create_file",
			normalized: func(tool string) string {
				body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"` + tool + `","arguments":"{\"path\":\"docs/a.md\",\"content\":\"ok\"}"}}]},"finish_reason":"tool_calls"}]}`)
				return string(normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"powershell": {}}))
			},
			want: `"tool_calls"`,
		},
		{
			name: "json tool_calls get_file",
			tool: "get_file",
			normalized: func(tool string) string {
				body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"` + tool + `","arguments":"{\"filename\":\"docs/a.md\"}"}}]},"finish_reason":"tool_calls"}]}`)
				return string(normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"powershell": {}}))
			},
			want: `"tool_calls"`,
		},
		{
			name: "json legacy function_call create_file",
			tool: "create_file",
			normalized: func(tool string) string {
				body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","function_call":{"name":"` + tool + `","arguments":"{\"path\":\"docs/a.md\",\"content\":\"ok\"}"}},"finish_reason":"function_call"}]}`)
				return string(normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"powershell": {}}))
			},
			want: `"function_call"`,
		},
		{
			name: "json legacy function_call get_file",
			tool: "get_file",
			normalized: func(tool string) string {
				body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","function_call":{"name":"` + tool + `","arguments":"{\"filename\":\"docs/a.md\"}"}},"finish_reason":"function_call"}]}`)
				return string(normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"powershell": {}}))
			},
			want: `"function_call"`,
		},
		{
			name: "stream tool_calls create_file",
			tool: "create_file",
			normalized: func(tool string) string {
				line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"` + tool + `","arguments":"{\"path\":\"docs/a.md\",\"content\":\"ok\"}"}}]},"finish_reason":null}]}`
				return normalizeOpenAIStreamLineForVisualStudioWithTools(line, map[string]struct{}{"powershell": {}})
			},
			want: `"tool_calls"`,
		},
		{
			name: "stream tool_calls get_file",
			tool: "get_file",
			normalized: func(tool string) string {
				line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"` + tool + `","arguments":"{\"filename\":\"docs/a.md\"}"}}]},"finish_reason":null}]}`
				return normalizeOpenAIStreamLineForVisualStudioWithTools(line, map[string]struct{}{"powershell": {}})
			},
			want: `"tool_calls"`,
		},
		{
			name: "stream legacy function_call create_file",
			tool: "create_file",
			normalized: func(tool string) string {
				line := `data: {"choices":[{"delta":{"function_call":{"name":"` + tool + `","arguments":"{\"path\":\"docs/a.md\",\"content\":\"ok\"}"}},"finish_reason":null}]}`
				return normalizeOpenAIStreamLineForVisualStudioWithTools(line, map[string]struct{}{"powershell": {}})
			},
			want: `"function_call"`,
		},
		{
			name: "stream legacy function_call get_file",
			tool: "get_file",
			normalized: func(tool string) string {
				line := `data: {"choices":[{"delta":{"function_call":{"name":"` + tool + `","arguments":"{\"filename\":\"docs/a.md\"}"}},"finish_reason":null}]}`
				return normalizeOpenAIStreamLineForVisualStudioWithTools(line, map[string]struct{}{"powershell": {}})
			},
			want: `"function_call"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized := tt.normalized(tt.tool)
			if !strings.Contains(normalized, tt.want) || !strings.Contains(normalized, `"name":"`+tt.tool+`"`) {
				t.Fatalf("tool call was not preserved: %s", normalized)
			}
			if strings.Contains(normalized, "Proxy blocked undeclared tool calls") || strings.Contains(normalized, "空工具名") || strings.Contains(normalized, `"finish_reason":"stop"`) {
				t.Fatalf("tool call should not be blocked or downgraded: %s", normalized)
			}
		})
	}
}

func TestNormalizeOpenAIStreamLinePassesThroughNamedToolWhenRequestDeclaresNone(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(nil)
	line := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(line, `"tool_calls"`) || !strings.Contains(line, `"name":"powershell"`) || strings.Contains(line, "Proxy blocked undeclared tool calls") {
		t.Fatalf("named stream tool should pass through when request declares no tools in stable mode: %s", line)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONReportsEmptyToolNameWithoutEnglishPlaceholder(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_empty","type":"function","function":{"arguments":"{\"query\":\"needle\"}"}}]},"finish_reason":"tool_calls"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"grep_search": {}})
	if strings.Contains(string(normalized), "<empty>") {
		t.Fatalf("empty tool name should not expose English placeholder: %s", normalized)
	}
	if strings.Contains(string(normalized), "空工具名") || !strings.Contains(string(normalized), `"finish_reason":"tool_calls"`) || !strings.Contains(string(normalized), `"tool_calls"`) {
		t.Fatalf("empty tool name should pass through without localized block notice in stable mode: %s", normalized)
	}
}

func TestNormalizeOpenAIStreamLinePassesThroughUndeclaredToolCalls(t *testing.T) {
	line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":null}]}`
	normalized := normalizeOpenAIStreamLineForVisualStudioWithTools(line, map[string]struct{}{"git": {}})
	if !strings.Contains(normalized, "tool_calls") || !strings.Contains(normalized, `"name":"powershell"`) || strings.Contains(normalized, "Proxy blocked undeclared tool calls") {
		t.Fatalf("undeclared stream tool call should pass through in stable mode: %s", normalized)
	}
}

func TestNormalizeOpenAIStreamLineAllowsArgumentOnlyToolCallChunks(t *testing.T) {
	line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"query\"}"}}]},"finish_reason":null}]}`
	normalized := normalizeOpenAIStreamLineForVisualStudioWithTools(line, map[string]struct{}{"grep_search": {}})
	if strings.Contains(normalized, "Proxy blocked undeclared tool calls") {
		t.Fatalf("argument-only stream chunk must not be blocked: %s", normalized)
	}
	if !strings.Contains(normalized, `"tool_calls"`) || !strings.Contains(normalized, `"arguments"`) {
		t.Fatalf("argument-only stream chunk must pass through unchanged enough for VS to merge it: %s", normalized)
	}
}

func TestNormalizeOpenAIStreamLineDowngradesToolFinishWithoutObservedPayload(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"create_file": {}})
	line := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`

	normalized := normalizeOpenAIStreamLineForVisualStudioWithToolState(line, sanitizer)

	if !strings.Contains(normalized, `"finish_reason":"stop"`) || strings.Contains(normalized, `"finish_reason":"tool_calls"`) {
		t.Fatalf("tool finish without observed payload must become stop: %s", normalized)
	}
}

func TestNormalizeOpenAIStreamLineAllowsLegacyFunctionCallArgumentChunks(t *testing.T) {
	line := `data: {"choices":[{"delta":{"function_call":{"arguments":"\"pattern\"}"}},"finish_reason":null}]}`
	normalized := normalizeOpenAIStreamLineForVisualStudioWithTools(line, map[string]struct{}{"grep_search": {}})
	if strings.Contains(normalized, "Proxy blocked undeclared tool calls") {
		t.Fatalf("legacy function_call argument-only chunk must not be blocked: %s", normalized)
	}
	if !strings.Contains(normalized, `"function_call"`) || !strings.Contains(normalized, `"arguments"`) {
		t.Fatalf("legacy argument-only chunk must pass through for client-side merge: %s", normalized)
	}
}

func TestNormalizeOpenAIStreamLinePreservesContinuationForUndeclaredToolCall(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"grep_search": {}})
	first := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(first, `"tool_calls"`) || !strings.Contains(first, `"name":"powershell"`) || strings.Contains(first, "Proxy blocked undeclared tool calls") {
		t.Fatalf("undeclared named chunk should pass through in stable mode: %s", first)
	}

	second := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Remove-Item\"}"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(second, `"tool_calls"`) || !strings.Contains(second, "Remove-Item") || strings.Contains(second, "<empty>") || strings.Contains(second, "Proxy blocked undeclared tool calls") {
		t.Fatalf("continuation for undeclared tool should pass through in stable mode: %s", second)
	}

	finish := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		sanitizer,
	)
	if !strings.Contains(finish, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason should remain tool_calls in stable mode: %s", finish)
	}
}

func TestNormalizeOpenAIStreamLinePreservesLegacyContinuationForUndeclaredFunctionCall(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"grep_search": {}})
	first := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{"function_call":{"name":"powershell","arguments":"{\"command\":"}},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(first, `"function_call"`) || !strings.Contains(first, "powershell") || strings.Contains(first, "Proxy blocked undeclared tool calls") {
		t.Fatalf("undeclared legacy named chunk should pass through in stable mode: %s", first)
	}

	second := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{"function_call":{"arguments":"\"Remove-Item\"}"}},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(second, `"function_call"`) || !strings.Contains(second, "Remove-Item") || strings.Contains(second, "<empty>") || strings.Contains(second, "Proxy blocked undeclared tool calls") {
		t.Fatalf("legacy continuation for undeclared tool should pass through in stable mode: %s", second)
	}

	finish := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		sanitizer,
	)
	if !strings.Contains(finish, `"finish_reason":"function_call"`) {
		t.Fatalf("legacy finish_reason should match function_call payload: %s", finish)
	}
}

func TestNormalizeOpenAIStreamLineTracksToolStatePerChoice(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"grep_search": {}})
	first := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_grep","type":"function","function":{"name":"grep_search","arguments":"{\"query\":"}}]},"finish_reason":null},{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(first, `"name":"grep_search"`) || !strings.Contains(first, `"name":"powershell"`) || strings.Contains(first, "Proxy blocked undeclared tool calls") {
		t.Fatalf("mixed choices should keep both declared and undeclared tools in stable mode: %s", first)
	}

	continuation := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"needle\"}"}}]},"finish_reason":null},{"index":1,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Remove-Item\"}"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(continuation, "needle") || !strings.Contains(continuation, "Remove-Item") || strings.Contains(continuation, "<empty>") || strings.Contains(continuation, "Proxy blocked undeclared tool calls") {
		t.Fatalf("continuations must pass through per choice index in stable mode: %s", continuation)
	}

	finish := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"},{"index":1,"delta":{},"finish_reason":"tool_calls"}]}`,
		sanitizer,
	)
	if !strings.Contains(finish, `"index":0`) || !strings.Contains(finish, `"finish_reason":"tool_calls"`) {
		t.Fatalf("choice 0 should keep tool_calls finish: %s", finish)
	}
	if !strings.Contains(finish, `"index":1`) || !strings.Contains(finish, `"finish_reason":"tool_calls"`) {
		t.Fatalf("choice 1 should keep tool_calls finish in stable mode: %s", finish)
	}
}

func TestNormalizeOpenAIStreamLineHandlesMixedToolsWithinChoice(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"create_file": {}})
	first := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}},{"index":1,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(first, `"name":"create_file"`) || !strings.Contains(first, `"name":"powershell"`) || strings.Contains(first, "Proxy blocked undeclared tool calls") {
		t.Fatalf("mixed tool chunk should keep all tools in stable mode: %s", first)
	}

	continuation := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}},{"index":1,"function":{"arguments":"\"Remove-Item\"}"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(continuation, "a.txt") || !strings.Contains(continuation, "Remove-Item") || strings.Contains(continuation, "空工具名") || strings.Contains(continuation, "Proxy blocked undeclared tool calls") {
		t.Fatalf("mixed continuations should preserve all tool arguments in stable mode: %s", continuation)
	}

	finish := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		sanitizer,
	)
	if !strings.Contains(finish, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish should remain tool_calls because a declared tool survived: %s", finish)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONReportsEmptyLegacyFunctionCall(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","function_call":{"arguments":"{\"query\":\"needle\"}"}},"finish_reason":"function_call"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"grep_search": {}})
	var parsed struct {
		Choices []struct {
			Message struct {
				Content      string         `json:"content"`
				FunctionCall map[string]any `json:"function_call"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(normalized, &parsed); err != nil {
		t.Fatalf("unmarshal normalized response: %v; body=%s", err, normalized)
	}
	choice := parsed.Choices[0]
	if choice.Message.FunctionCall != nil {
		t.Fatalf("empty legacy function_call should be removed: %s", normalized)
	}
	if strings.Contains(choice.Message.Content, "<empty>") || !strings.Contains(choice.Message.Content, "Proxy blocked undeclared tool calls: 空工具名") || choice.FinishReason != "stop" {
		t.Fatalf("empty legacy function_call should produce clear notice and stop finish: %s", normalized)
	}
}

func TestVisualStudioFinishReasonPreservesKnownValues(t *testing.T) {
	for _, value := range []string{"stop", "length", "tool_calls", "content_filter", "function_call"} {
		if got := visualStudioFinishReason(value); got != value {
			t.Fatalf("visualStudioFinishReason(%q) = %q, want unchanged", value, got)
		}
	}
}
