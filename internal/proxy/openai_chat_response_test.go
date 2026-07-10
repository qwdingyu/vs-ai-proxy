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

func TestOpenAIStreamBodyToChatResponseConvertsEmptyChoiceSSE(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"id":"","object":"chat.completion.chunk","model":"gpt-5.5","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":0,"total_tokens":12}}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	converted, err := openAIStreamBodyToChatResponse(body, "gpt-5.5")
	if err != nil {
		t.Fatalf("openAIStreamBodyToChatResponse returned error: %v", err)
	}
	if strings.Contains(string(converted), "data:") {
		t.Fatalf("converted response must be JSON, got %s", string(converted))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(converted, &parsed); err != nil {
		t.Fatalf("unmarshal converted response: %v", err)
	}
	if len(parsed.Choices) != 1 || parsed.Choices[0].Message.Role != "assistant" || parsed.Choices[0].FinishReason != "stop" {
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
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop after all tool calls were blocked", resp.Choices[0].FinishReason)
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
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "grep_search" {
		t.Fatalf("declared tool call should remain: %#v", message.ToolCalls)
	}
	if !strings.Contains(message.Content, "powershell") {
		t.Fatalf("blocked tool notice missing: %#v", message)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls while declared calls remain", resp.Choices[0].FinishReason)
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

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONBlocksToolsWhenRequestDeclaresNone(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, nil)
	if strings.Contains(string(normalized), `"tool_calls"`) || strings.Contains(string(normalized), `"finish_reason":"tool_calls"`) {
		t.Fatalf("tool calls must be blocked when request declares no tools: %s", normalized)
	}
	if !strings.Contains(string(normalized), "powershell") || !strings.Contains(string(normalized), `"finish_reason":"stop"`) {
		t.Fatalf("blocked tool notice/stop finish missing: %s", normalized)
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

func TestNormalizeOpenAIStreamLineBlocksNamedToolWhenRequestDeclaresNone(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(nil)
	line := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if strings.Contains(line, `"tool_calls"`) || !strings.Contains(line, "Proxy blocked undeclared tool calls: powershell") {
		t.Fatalf("named stream tool must be blocked when request declares no tools: %s", line)
	}
}

func TestNormalizeProviderSpecificToolCallsInOpenAIJSONReportsEmptyToolNameWithoutEnglishPlaceholder(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_empty","type":"function","function":{"arguments":"{\"query\":\"needle\"}"}}]},"finish_reason":"tool_calls"}]}`)
	normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"grep_search": {}})
	if strings.Contains(string(normalized), "<empty>") {
		t.Fatalf("empty tool name should not expose English placeholder: %s", normalized)
	}
	if !strings.Contains(string(normalized), "空工具名") || !strings.Contains(string(normalized), `"finish_reason":"stop"`) {
		t.Fatalf("empty tool name should produce localized notice and stop finish: %s", normalized)
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

func TestNormalizeOpenAIStreamLineDropsContinuationAfterBlockedToolCall(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"grep_search": {}})
	first := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if strings.Contains(first, `"tool_calls"`) || !strings.Contains(first, "powershell") {
		t.Fatalf("undeclared named chunk should be blocked with notice: %s", first)
	}

	second := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Remove-Item\"}"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if strings.Contains(second, `"tool_calls"`) || strings.Contains(second, "Remove-Item") || strings.Contains(second, "<empty>") {
		t.Fatalf("continuation for blocked tool must be silently dropped: %s", second)
	}

	finish := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		sanitizer,
	)
	if strings.Contains(finish, `"finish_reason":"tool_calls"`) || !strings.Contains(finish, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason must become stop when all tool calls were blocked: %s", finish)
	}
}

func TestNormalizeOpenAIStreamLineDropsLegacyContinuationAfterBlockedFunctionCall(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"grep_search": {}})
	first := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{"function_call":{"name":"powershell","arguments":"{\"command\":"}},"finish_reason":null}]}`,
		sanitizer,
	)
	if strings.Contains(first, `"function_call"`) || !strings.Contains(first, "powershell") {
		t.Fatalf("undeclared legacy named chunk should be blocked with notice: %s", first)
	}

	second := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{"function_call":{"arguments":"\"Remove-Item\"}"}},"finish_reason":null}]}`,
		sanitizer,
	)
	if strings.Contains(second, `"function_call"`) || strings.Contains(second, "Remove-Item") || strings.Contains(second, "<empty>") {
		t.Fatalf("legacy continuation for blocked tool must be silently dropped: %s", second)
	}

	finish := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		sanitizer,
	)
	if strings.Contains(finish, `"finish_reason":"tool_calls"`) || !strings.Contains(finish, `"finish_reason":"stop"`) {
		t.Fatalf("legacy finish_reason must become stop when function_call was blocked: %s", finish)
	}
}

func TestNormalizeOpenAIStreamLineTracksToolStatePerChoice(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"grep_search": {}})
	first := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_grep","type":"function","function":{"name":"grep_search","arguments":"{\"query\":"}}]},"finish_reason":null},{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(first, `"name":"grep_search"`) || strings.Contains(first, `"name":"powershell"`) {
		t.Fatalf("mixed choices should keep declared tool and block undeclared tool: %s", first)
	}

	continuation := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"needle\"}"}}]},"finish_reason":null},{"index":1,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Remove-Item\"}"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(continuation, "needle") || strings.Contains(continuation, "Remove-Item") || strings.Contains(continuation, "<empty>") {
		t.Fatalf("continuations must be scoped by choice index: %s", continuation)
	}

	finish := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"},{"index":1,"delta":{},"finish_reason":"tool_calls"}]}`,
		sanitizer,
	)
	if !strings.Contains(finish, `"index":0`) || !strings.Contains(finish, `"finish_reason":"tool_calls"`) {
		t.Fatalf("choice 0 should keep tool_calls finish: %s", finish)
	}
	if !strings.Contains(finish, `"index":1`) || !strings.Contains(finish, `"finish_reason":"stop"`) {
		t.Fatalf("choice 1 should become stop after all tools blocked: %s", finish)
	}
}

func TestNormalizeOpenAIStreamLineHandlesMixedToolsWithinChoice(t *testing.T) {
	sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"create_file": {}})
	first := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}},{"index":1,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(first, `"name":"create_file"`) || strings.Contains(first, `"name":"powershell"`) {
		t.Fatalf("mixed tool chunk should keep declared tool and block undeclared tool: %s", first)
	}

	continuation := normalizeOpenAIStreamLineForVisualStudioWithToolState(
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}},{"index":1,"function":{"arguments":"\"Remove-Item\"}"}}]},"finish_reason":null}]}`,
		sanitizer,
	)
	if !strings.Contains(continuation, "a.txt") || strings.Contains(continuation, "Remove-Item") || strings.Contains(continuation, "空工具名") {
		t.Fatalf("mixed continuations should preserve only declared tool arguments: %s", continuation)
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
