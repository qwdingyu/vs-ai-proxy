package proxy

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// copilotContractToolNames 同时覆盖 VS Copilot 常见工具族和任意扩展工具。
// 生产实现不得依赖这份名称表；所有名称必须走同一套 OpenAI 工具协议边界。
var copilotContractToolNames = []string{
	"create_file",
	"edit_file",
	"apply_patch",
	"get_file",
	"file_search",
	"grep_search",
	"code_search",
	"list_files",
	"delete_files",
	"run_command_in_terminal",
	"powershell",
	"git",
	"find_symbol",
	"adapt_plan",
	"ask_question",
	"detect_memories",
	"mcp_workspace_symbol_42",
}

func TestCopilotDeclaredToolContractMatrixNonStream(t *testing.T) {
	for _, toolName := range copilotContractToolNames {
		toolName := toolName
		t.Run(toolName, func(t *testing.T) {
			arguments := `{"value":"needle","nested":{"enabled":true}}`
			responseBody := marshalTestJSON(t, map[string]any{
				"id":     "chatcmpl-contract",
				"object": "chat.completion",
				"model":  "gpt-test",
				"choices": []any{map[string]any{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "",
						// 部分 Ollama/OpenAI-compatible 上游省略 id/type；
						// 代理对 VS 输出时必须补齐标准 envelope。
						"tool_calls": []any{map[string]any{
							"function": map[string]any{"name": toolName, "arguments": arguments},
						}},
					},
					"finish_reason": "stop",
				}},
			})

			prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, "")
			prov.rawBody = responseBody
			server := newOpenServer(prov)
			handler := withMux(server, func(mux *http.ServeMux) {
				mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
			})

			requestBody := marshalCopilotContractRequest(t, toolName, false)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(requestBody)))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var resp provider.ChatResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("VS client decode response: %v; body=%s", err, rec.Body.String())
			}
			assertExecutableCopilotToolCall(
				t,
				resp.Choices[0].Message.ToolCalls,
				toolName,
				arguments,
			)
			if resp.Choices[0].FinishReason != "tool_calls" {
				t.Fatalf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
			}
			assertCopilotRequestContractPreserved(t, prov.lastReq, toolName)
		})
	}
}

func TestCopilotDeclaredToolContractStreamHandlesArbitraryFragmentation(t *testing.T) {
	toolName := "mcp_workspace_symbol_42"
	arguments := `{"query":"needle","paths":["a.cs","b.cs"]}`
	nameFragments := []string{"mcp_workspace_", "symbol_", "42"}
	argumentFragments := []string{arguments[:11], arguments[11:29], arguments[29:]}
	events := make([]string, 0, len(nameFragments)+2)
	for index := range nameFragments {
		events = append(events, "data: "+string(marshalTestJSON(t, map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": 0,
						"function": map[string]any{
							"name":      nameFragments[index],
							"arguments": argumentFragments[index],
						},
					}},
				},
				"finish_reason": nil,
			}},
		})))
	}
	events = append(events,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"data: [DONE]",
		"",
	)

	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, strings.Join(events, "\n"))
	rec := postCopilotContractStream(t, prov, toolName)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	calls, finishReason := parseOpenAIStreamToolCalls(t, rec.Body.String())
	assertExecutableCopilotToolCall(
		t,
		calls,
		toolName,
		arguments,
	)
	if finishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; body=%s", finishReason, rec.Body.String())
	}
}

func TestCopilotDirectStreamAcceptsLargeSingleToolEvent(t *testing.T) {
	toolName := "mcp_generate_source"
	argumentBytes, err := json.Marshal(map[string]any{
		"path":    "generated.cs",
		"content": strings.Repeat("x", 5<<20),
	})
	if err != nil {
		t.Fatalf("marshal large arguments: %v", err)
	}
	event := "data: " + string(marshalTestJSON(t, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    "call_large",
					"type":  "function",
					"function": map[string]any{
						"name":      toolName,
						"arguments": string(argumentBytes),
					},
				}},
			},
			"finish_reason": nil,
		}},
	}))
	stream := strings.Join([]string{
		event,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"data: [DONE]",
		"",
	}, "\n")

	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, stream)
	rec := postCopilotContractStream(t, prov, toolName)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body_prefix=%s", rec.Code, responsePrefix(rec.Body.String(), 300))
	}
	calls, finishReason := parseOpenAIStreamToolCalls(t, rec.Body.String())
	assertExecutableCopilotToolCall(
		t,
		calls,
		toolName,
		string(argumentBytes),
	)
	if finishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finishReason)
	}
}

func TestCopilotOllamaStreamRepairsOpenAIToolEnvelope(t *testing.T) {
	toolName := "mcp_workspace_symbol_42"
	arguments := `{"query":"needle"}`
	stream := strings.Join([]string{
		`{"model":"gpt-test","message":{"role":"assistant","content":"","tool_calls":[{` +
			`"function":{"name":"` + toolName + `","arguments":{"query":"needle"}}}]},"done":false}`,
		`{"model":"gpt-test","message":{"role":"assistant","content":""},"done":true,"done_reason":"tool_calls"}`,
		"",
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "ollama", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{
			Type:     "function",
			Function: provider.ToolFunc{Name: toolName},
		}},
	}

	if err := server.streamOllamaToOpenAI(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("stream Ollama to OpenAI: %v", err)
	}
	calls, finishReason := parseOpenAIStreamToolCalls(t, rec.Body.String())
	assertExecutableCopilotToolCall(
		t,
		calls,
		toolName,
		arguments,
	)
	if finishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; body=%s", finishReason, rec.Body.String())
	}
}

func TestCopilotOllamaStreamRejectsEmptyOrUnterminatedSuccess(t *testing.T) {
	tests := []struct {
		name   string
		stream string
	}{
		{
			name:   "done without payload",
			stream: `{"message":{"role":"assistant","content":""},"done":true}`,
		},
		{
			name:   "content without terminal event",
			stream: `{"message":{"role":"assistant","content":"partial"},"done":false}`,
		},
		{
			name: "invalid tool type",
			stream: `{"message":{"role":"assistant","content":"","tool_calls":[{` +
				`"id":"call_invalid","type":"computer","function":{` +
				`"name":"custom_tool","arguments":{}}}]},"done":true}`,
		},
		{
			name: "truncated tool call",
			stream: strings.Join([]string{
				`{"message":{"role":"assistant","content":"","tool_calls":[{` +
					`"function":{"name":"custom_tool","arguments":{"path":"a.txt"}}}]},"done":false}`,
				`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"length"}`,
			}, "\n"),
		},
		{
			name: "incomplete tool call",
			stream: strings.Join([]string{
				`{"message":{"role":"assistant","content":"","tool_calls":[{` +
					`"function":{"name":"custom_tool","arguments":""}}]},"done":false}`,
				`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"tool_calls"}`,
			}, "\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{}
			prov := &fakeStreamProvider{name: "ollama", body: tt.stream}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			rec := httptest.NewRecorder()
			err := server.streamOllamaToOpenAI(
				rec,
				req,
				prov,
				&provider.ChatRequest{Model: "gpt-test"},
				rec,
			)
			if err == nil {
				t.Fatalf("invalid Ollama stream returned success: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "[DONE]") {
				t.Fatalf("invalid Ollama stream emitted DONE: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), `"tool_calls"`) {
				t.Fatalf("invalid Ollama stream exposed tool calls: %s", rec.Body.String())
			}
		})
	}
}

func TestCopilotRawResponseRequiresChoiceAndRepairsMissingTerminalReason(t *testing.T) {
	if err := validateOpenAIChatResponseBody([]byte(`{"choices":[]}`)); err == nil {
		t.Fatal("empty choices must not be accepted as a successful chat response")
	}
	emptyStop := []byte(
		`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`,
	)
	if err := validateOpenAIChatResponseBody(emptyStop); err == nil {
		t.Fatal("empty assistant stop response must not be accepted as a successful chat response")
	}

	body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
	body = normalizeProviderSpecificToolCallsInOpenAIJSON(body, nil)
	if err := validateOpenAIChatResponseBody(body); err != nil {
		t.Fatalf("normalized text response should be valid: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decode normalized response: %v", err)
	}
	choice := root["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %#v, want stop; body=%s", choice["finish_reason"], body)
	}

	typed := &provider.ChatResponse{Choices: []provider.Choice{{
		Message: provider.Message{Role: "assistant", Content: "ok"},
	}}}
	normalizeProviderSpecificToolCalls(typed, nil)
	if typed.Choices[0].FinishReason != "stop" {
		t.Fatalf("typed finish_reason = %q, want stop", typed.Choices[0].FinishReason)
	}

	missingEnvelope := []byte(
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{` +
			`"function":{"name":"custom_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
	)
	if err := validateOpenAIChatResponseBody(missingEnvelope); err == nil {
		t.Fatal("validator must reject a tool call whose id/type were not normalized into the wire body")
	}
	normalizedEnvelope := normalizeProviderSpecificToolCallsInOpenAIJSON(
		missingEnvelope,
		map[string]struct{}{"custom_tool": {}},
	)
	if err := validateOpenAIChatResponseBody(normalizedEnvelope); err != nil {
		t.Fatalf("normalized tool envelope should pass validation: %v; body=%s", err, normalizedEnvelope)
	}
	truncatedMixedCalls := []byte(
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{` +
			`"id":"call_1","type":"function","function":{"name":"custom_tool","arguments":"{}"}}],` +
			`"function_call":{"name":"custom_tool","arguments":"{}"}},"finish_reason":"length"}]}`,
	)
	normalizedTruncated := normalizeProviderSpecificToolCallsInOpenAIJSON(truncatedMixedCalls, nil)
	if strings.Contains(string(normalizedTruncated), "tool_calls") ||
		strings.Contains(string(normalizedTruncated), "function_call") {
		t.Fatalf("truncated response must not expose executable calls: %s", normalizedTruncated)
	}
	mixedCalls := []byte(
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{` +
			`"id":"call_1","type":"function","function":{"name":"custom_tool","arguments":"{}"}}],` +
			`"function_call":{"name":"custom_tool","arguments":"{}"}},"finish_reason":"tool_calls"}]}`,
	)
	if err := validateOpenAIChatResponseBody(mixedCalls); err == nil {
		t.Fatal("raw modern/legacy mixed response must not pass validation")
	}

	invalidTypeStream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{` +
			`"index":0,"id":"call_invalid","type":"computer","function":{` +
			`"name":"custom_tool","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		"data: [DONE]",
		"",
	}, "\n")
	if _, err := collectOpenAIStreamReader(strings.NewReader(invalidTypeStream), "gpt-test", nil); err == nil {
		t.Fatal("aggregated fallback must reject an invalid tool type")
	}
}

func TestCopilotToolStreamRandomizedChunkBoundaries(t *testing.T) {
	rng := rand.New(rand.NewSource(20260714))
	for iteration := 0; iteration < 100; iteration++ {
		toolNames := []string{
			fmt.Sprintf("mcp_workspace_query_%d", iteration),
			fmt.Sprintf("extension_symbol_lookup_%d", iteration),
			fmt.Sprintf("custom_terminal_action_%d", iteration),
		}
		arguments := make([]string, len(toolNames))
		ids := make([]string, len(toolNames))
		firstCalls := make([]any, 0, len(toolNames))
		secondCalls := make([]any, 0, len(toolNames))
		for index, toolName := range toolNames {
			argumentBytes, err := json.Marshal(map[string]any{
				"iteration": iteration,
				"index":     index,
				"value":     strings.Repeat("x", 16+iteration%23),
			})
			if err != nil {
				t.Fatalf("marshal randomized arguments: %v", err)
			}
			arguments[index] = string(argumentBytes)
			ids[index] = fmt.Sprintf("call_%d_%d", iteration, index)
			nameSplit := 1 + rng.Intn(len(toolName)-1)
			argumentSplit := 1 + rng.Intn(len(arguments[index])-1)
			firstCalls = append(firstCalls, map[string]any{
				"index": index,
				"id":    ids[index],
				"type":  "function",
				"function": map[string]any{
					"name":      toolName[:nameSplit],
					"arguments": arguments[index][:argumentSplit],
				},
			})
			secondCalls = append(secondCalls, map[string]any{
				"index": index,
				"function": map[string]any{
					"name":      toolName[nameSplit:],
					"arguments": arguments[index][argumentSplit:],
				},
			})
		}

		stream := strings.Join([]string{
			"data: " + string(marshalTestJSON(t, streamToolDelta(firstCalls, nil))),
			"data: " + string(marshalTestJSON(t, streamToolDelta(secondCalls, nil))),
			"data: " + string(marshalTestJSON(t, streamToolDelta(nil, "stop"))),
			"data: [DONE]",
			"",
		}, "\n")
		prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, stream)
		rec := postCopilotContractStreamForTools(t, prov, toolNames)
		if rec.Code != http.StatusOK {
			t.Fatalf("iteration %d: status=%d body=%s", iteration, rec.Code, responsePrefix(rec.Body.String(), 300))
		}
		calls, finishReason := parseOpenAIStreamToolCalls(t, rec.Body.String())
		if len(calls) != len(toolNames) || finishReason != "tool_calls" {
			t.Fatalf("iteration %d: calls=%#v finish=%q", iteration, calls, finishReason)
		}
		for index := range calls {
			call := calls[index]
			hasExpectedEnvelope := call.ID == ids[index] && call.Type == "function"
			hasExpectedFunction := call.Function.Name == toolNames[index] &&
				call.Function.Arguments == arguments[index]
			if !hasExpectedEnvelope || !hasExpectedFunction {
				t.Fatalf("iteration %d call %d changed: %#v", iteration, index, calls[index])
			}
		}
	}
}

func TestCopilotDirectStreamRejectsEmptyOrUnterminatedSuccess(t *testing.T) {
	tests := []struct {
		name   string
		stream string
	}{
		{
			name: "done without payload",
			stream: strings.Join([]string{
				`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
				"data: [DONE]",
				"",
			}, "\n"),
		},
		{
			name:   "content without terminal event",
			stream: `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`,
		},
		{
			name: "invalid tool type",
			stream: strings.Join([]string{
				`data: {"choices":[{"delta":{"tool_calls":[{` +
					`"index":0,"id":"call_invalid","type":"computer","function":{` +
					`"name":"custom_tool","arguments":"{}"}}]},"finish_reason":null}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				"data: [DONE]",
				"",
			}, "\n"),
		},
		{
			name: "modern and legacy calls across events",
			stream: strings.Join([]string{
				`data: {"choices":[{"delta":{"tool_calls":[{` +
					`"index":0,"id":"call_mixed","type":"function","function":{` +
					`"name":"custom_tool","arguments":"{}"}}]},"finish_reason":null}]}`,
				`data: {"choices":[{"delta":{"function_call":{` +
					`"name":"custom_tool","arguments":"{}"}},"finish_reason":null}]}`,
				"data: [DONE]",
				"",
			}, "\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{}
			prov := &fakeStreamProvider{name: "openai", body: tt.stream}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			rec := httptest.NewRecorder()
			if err := server.streamOpenAI(rec, req, prov, &provider.ChatRequest{Model: "gpt-test"}, rec); err == nil {
				t.Fatalf("invalid stream returned success: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "[DONE]") {
				t.Fatalf("invalid stream emitted DONE: %s", rec.Body.String())
			}
		})
	}
}

func TestCopilotExplicitTruncationTerminalMayBeEmpty(t *testing.T) {
	server := &Server{}
	prov := &fakeStreamProvider{
		name: "openai",
		body: strings.Join([]string{
			`data: {"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
			"data: [DONE]",
			"",
		}, "\n"),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	if err := server.streamOpenAI(rec, req, prov, &provider.ChatRequest{Model: "gpt-test"}, rec); err != nil {
		t.Fatalf("explicit content_filter terminal should remain valid: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"finish_reason":"content_filter"`) {
		t.Fatalf("content_filter terminal was not preserved: %s", rec.Body.String())
	}

	ollamaProv := &fakeStreamProvider{
		name: "ollama",
		body: `{"message":{"role":"assistant","content":""},` +
			`"done":true,"done_reason":"content_filter"}`,
	}
	ollamaRec := httptest.NewRecorder()
	if err := server.streamOllamaToOpenAI(
		ollamaRec,
		req,
		ollamaProv,
		&provider.ChatRequest{Model: "gpt-test"},
		ollamaRec,
	); err != nil {
		t.Fatalf("Ollama content_filter terminal should remain valid: %v", err)
	}
	if !strings.Contains(ollamaRec.Body.String(), `"finish_reason":"content_filter"`) {
		t.Fatalf("Ollama content_filter terminal was not preserved: %s", ollamaRec.Body.String())
	}

	typed := &provider.ChatResponse{Choices: []provider.Choice{{
		Message:      provider.Message{Role: "assistant"},
		FinishReason: " LENGTH ",
	}}}
	normalizeProviderSpecificToolCalls(typed, nil)
	if err := validateProviderResponseToolContract(typed); err != nil {
		t.Fatalf("typed length terminal should remain valid: %v", err)
	}
	if typed.Choices[0].FinishReason != "length" {
		t.Fatalf("typed finish_reason = %q, want length", typed.Choices[0].FinishReason)
	}
}

func TestCopilotTypedFallbackRejectsEmptyStop(t *testing.T) {
	resp := &provider.ChatResponse{Choices: []provider.Choice{{
		Message:      provider.Message{Role: "assistant", Content: ""},
		FinishReason: "stop",
	}}}
	rec := httptest.NewRecorder()
	if err := writeOpenAIChatResponseAsSSE(rec, rec, resp); err == nil {
		t.Fatalf("empty typed fallback returned success: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("empty typed fallback emitted DONE: %s", rec.Body.String())
	}
}

func TestCopilotOpenAIToOllamaRejectsUnterminatedSuccess(t *testing.T) {
	server := &Server{}
	prov := &fakeStreamProvider{
		name: "openai",
		body: `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()
	err := server.streamOpenAIToOllama(
		rec,
		req,
		prov,
		&provider.ChatRequest{Model: "gpt-test"},
		rec,
	)
	if err == nil {
		t.Fatalf("unterminated OpenAI stream returned Ollama success: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"done":true`) {
		t.Fatalf("unterminated OpenAI stream emitted done=true: %s", rec.Body.String())
	}
}

func TestCopilotOpenAIToOllamaUsesLogicalToolEvents(t *testing.T) {
	toolName := "custom_tool"
	tests := []struct {
		name        string
		stream      string
		wantError   bool
		wantTool    bool
		wantContent string
	}{
		{
			name: "tool call terminated only by done",
			stream: strings.Join([]string{
				`data: {"choices":[{"delta":{"tool_calls":[{` +
					`"index":0,"id":"call_done","type":"function","function":{` +
					`"name":"custom_tool","arguments":"{}"}}]},"finish_reason":null}]}`,
				"data: [DONE]",
				"",
			}, "\n"),
			wantTool: true,
		},
		{
			name: "multiline content event",
			stream: strings.Join([]string{
				`data: {"choices":[`,
				`data: {"delta":{"content":"hello"},"finish_reason":"stop"}]}`,
				"",
				"data: [DONE]",
				"",
			}, "\n"),
			wantContent: "hello",
		},
		{
			name: "truncated tool call",
			stream: strings.Join([]string{
				`data: {"choices":[{"delta":{"tool_calls":[{` +
					`"index":0,"id":"call_length","type":"function","function":{` +
					`"name":"custom_tool","arguments":"{}"}}]},"finish_reason":null}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
				"data: [DONE]",
				"",
			}, "\n"),
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{}
			prov := &fakeStreamProvider{name: "openai", body: tt.stream}
			req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
			rec := httptest.NewRecorder()
			chatReq := &provider.ChatRequest{
				Model: "gpt-test",
				Tools: []provider.Tool{{
					Type:     "function",
					Function: provider.ToolFunc{Name: toolName},
				}},
			}
			err := server.streamOpenAIToOllama(rec, req, prov, chatReq, rec)
			if tt.wantError {
				if err == nil {
					t.Fatalf("invalid stream returned success: %s", rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), toolName) ||
					strings.Contains(rec.Body.String(), `"done":true`) {
					t.Fatalf("invalid stream exposed success payload: %s", rec.Body.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("valid logical stream failed: %v", err)
			}
			if tt.wantTool && !strings.Contains(rec.Body.String(), toolName) {
				t.Fatalf("tool call was lost: %s", rec.Body.String())
			}
			if tt.wantContent != "" && !strings.Contains(rec.Body.String(), tt.wantContent) {
				t.Fatalf("content was lost: %s", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"done":true`) {
				t.Fatalf("valid stream did not emit done=true: %s", rec.Body.String())
			}
		})
	}
}

func postCopilotContractStream(t *testing.T, prov *fakeProvider, toolName string) *httptest.ResponseRecorder {
	return postCopilotContractStreamForTools(t, prov, []string{toolName})
}

func postCopilotContractStreamForTools(
	t *testing.T,
	prov *fakeProvider,
	toolNames []string,
) *httptest.ResponseRecorder {
	t.Helper()
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})
	tools := make([]any, 0, len(toolNames))
	for _, toolName := range toolNames {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       toolName,
				"parameters": map[string]any{"type": "object"},
			},
		})
	}
	body := marshalTestJSON(t, map[string]any{
		"model":    "gpt-test",
		"messages": []any{map[string]any{"role": "user", "content": "use tools"}},
		"tools":    tools,
		"stream":   true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func streamToolDelta(calls []any, finishReason any) map[string]any {
	delta := map[string]any{}
	if len(calls) > 0 {
		delta["tool_calls"] = calls
	}
	return map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
}

func marshalCopilotContractRequest(t *testing.T, toolName string, stream bool) []byte {
	t.Helper()
	return marshalTestJSON(t, map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "use the declared tool"},
			map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":             "call_history",
					"type":           "function",
					"provider_state": map[string]any{"opaque": true},
					"function": map[string]any{
						"name":      toolName,
						"arguments": `{"value":"history"}`,
					},
				}},
			},
			map[string]any{
				"role":         "tool",
				"tool_call_id": "call_history",
				"content":      `{"ok":true}`,
			},
		},
		"tools": []any{map[string]any{
			"type":             "function",
			"provider_options": map[string]any{"trace": true},
			"function": map[string]any{
				"name":        toolName,
				"description": "Generic Copilot contract tool",
				"strict":      true,
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"value": map[string]any{"type": "string"}},
					"required":   []string{"value"},
				},
			},
		}},
		"tool_choice": map[string]any{
			"type":     "function",
			"function": map[string]any{"name": toolName},
		},
		"parallel_tool_calls": true,
		"stream":              stream,
	})
}

func assertCopilotRequestContractPreserved(t *testing.T, req *provider.ChatRequest, toolName string) {
	t.Helper()
	if req == nil || len(req.Tools) != 1 || req.Tools[0].Function.Name != toolName {
		t.Fatalf("declared tool lost: %#v", req)
	}
	if string(req.Tools[0].Function.Extra["strict"]) != "true" || len(req.Tools[0].Extra["provider_options"]) == 0 {
		t.Fatalf("tool schema extensions lost: %#v", req.Tools[0])
	}
	if len(req.Messages) != 3 || len(req.Messages[1].ToolCalls) != 1 || req.Messages[2].ToolCallID != "call_history" {
		t.Fatalf("tool history lost: %#v", req.Messages)
	}
	if len(req.Messages[1].ToolCalls[0].Extra["provider_state"]) == 0 {
		t.Fatalf("tool call provider state lost: %#v", req.Messages[1].ToolCalls[0])
	}
	for _, field := range []string{"tool_choice", "parallel_tool_calls"} {
		if len(req.Extra[field]) == 0 {
			t.Fatalf("top-level %s lost: %#v", field, req.Extra)
		}
	}
	if req.MaxTokens != nil || req.Temperature != nil {
		t.Fatalf(
			"proxy invented output/sampling defaults: max_tokens=%v temperature=%v",
			req.MaxTokens,
			req.Temperature,
		)
	}
}

func assertExecutableCopilotToolCall(
	t *testing.T,
	calls []provider.ToolCall,
	toolName string,
	arguments string,
) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v, want exactly one", calls)
	}
	call := calls[0]
	if strings.TrimSpace(call.ID) == "" || call.Type != "function" {
		t.Fatalf("tool envelope is not executable: %#v", call)
	}
	hasExpectedPayload := call.Function.Name == toolName && call.Function.Arguments == arguments
	if !hasExpectedPayload || !json.Valid([]byte(call.Function.Arguments)) {
		t.Fatalf("tool payload changed: got=%#v want_name=%q want_arguments=%q", call, toolName, arguments)
	}
}

func marshalTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal test JSON: %v", err)
	}
	return body
}

func responsePrefix(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return fmt.Sprintf("%s...", value[:max])
}
