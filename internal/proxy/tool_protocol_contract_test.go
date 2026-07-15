package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// TestToolProtocolContract 固化 VS Copilot 工具调用最容易失真的协议边界。
// 这些用例必须进入发布门禁，禁止再以临时 overlay 测试替代。
func TestToolProtocolContract(t *testing.T) {
	allowedCreateFile := map[string]struct{}{"create_file": {}}

	t.Run("complete tool calls override upstream stop finish", func(t *testing.T) {
		body := []byte(strings.Join([]string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "\n"))

		converted, err := openAIStreamBodyToChatResponse(body, "gpt-test", allowedCreateFile)
		if err != nil {
			t.Fatalf("aggregate tool stream: %v", err)
		}
		var got provider.ChatResponse
		if err := json.Unmarshal(converted, &got); err != nil {
			t.Fatalf("decode aggregated response: %v", err)
		}
		if got.Choices[0].FinishReason != "tool_calls" {
			t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
		}
	})

	t.Run("direct stream rejects in-band error", func(t *testing.T) {
		stream := "data: {\"error\":{\"message\":\"upstream unavailable\"}}\n\ndata: [DONE]\n\n"
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		chatReq := toolProtocolChatRequest("create_file")

		if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err == nil {
			t.Fatalf("direct stream accepted upstream error; body=%s", rec.Body.String())
		}
	})

	t.Run("direct stream rejects SSE error event", func(t *testing.T) {
		stream := strings.Join([]string{
			`event: error`,
			`data: {"message":"tool backend unavailable"}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()

		if err := server.streamOpenAI(rec, req, prov, toolProtocolChatRequest("create_file"), rec); err == nil {
			t.Fatalf("direct stream accepted SSE error event; body=%s", rec.Body.String())
		}
	})

	t.Run("direct stream accepts multiline SSE event", func(t *testing.T) {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":`,
			`data: {"content":"ok"},"finish_reason":"stop"}]}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()

		if err := server.streamOpenAI(rec, req, prov, toolProtocolChatRequest("create_file"), rec); err != nil {
			t.Fatalf("direct stream rejected valid multiline SSE: %v; body=%s", err, rec.Body.String())
		}
	})

	t.Run("direct stream rejects truncated tool call", func(t *testing.T) {
		stream := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		chatReq := toolProtocolChatRequest("create_file")

		if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err == nil {
			t.Fatalf("direct stream accepted truncated tool call; body=%s", rec.Body.String())
		}
	})

	t.Run("late truncated tool call is never exposed", func(t *testing.T) {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"` + strings.Repeat("x", dsmlStreamProbeLimit+1) + `"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_late","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"late.txt\"}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()

		if err := server.streamOpenAI(rec, req, prov, toolProtocolChatRequest("create_file"), rec); err == nil {
			t.Fatalf("direct stream accepted late truncated tool call; body=%s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), `"tool_calls"`) {
			t.Fatalf("late truncated tool call leaked to client: %s", rec.Body.String())
		}
	})

	t.Run("direct stream repairs complete tool finish", func(t *testing.T) {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()

		if err := server.streamOpenAI(rec, req, prov, toolProtocolChatRequest("create_file"), rec); err != nil {
			t.Fatalf("stream OpenAI response: %v", err)
		}
		_, finishReason := parseOpenAIStreamToolCalls(t, rec.Body.String())
		if finishReason != "tool_calls" {
			t.Fatalf("finish_reason = %q, want tool_calls; body=%s", finishReason, rec.Body.String())
		}
	})

	t.Run("direct stream completes a valid finish without upstream DONE", func(t *testing.T) {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		if err := server.streamOpenAI(rec, req, prov, toolProtocolChatRequest("create_file"), rec); err != nil {
			t.Fatalf("valid finish-only stream returned error: %v", err)
		}
		if !strings.Contains(rec.Body.String(), "data: [DONE]\n\n") {
			t.Fatalf("proxy did not normalize missing DONE: %s", rec.Body.String())
		}
	})

	t.Run("direct stream repairs duplicate tool IDs", func(t *testing.T) {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"tool_calls":[` +
				`{"index":0,"id":"same","type":"function","function":{"name":"create_file","arguments":"{}"}},` +
				`{"index":1,"id":"same","type":"function","function":{"name":"create_file","arguments":"{}"}}` +
				`]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		if err := server.streamOpenAI(rec, req, prov, toolProtocolChatRequest("create_file"), rec); err != nil {
			t.Fatalf("stream OpenAI response: %v", err)
		}
		calls, _ := parseOpenAIStreamToolCalls(t, rec.Body.String())
		if len(calls) != 2 || calls[0].ID == calls[1].ID {
			t.Fatalf("duplicate tool IDs were not repaired: %#v; body=%s", calls, rec.Body.String())
		}
	})

	t.Run("direct stream repairs a collision after fragmented tool IDs", func(t *testing.T) {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_first","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		if err := server.streamOpenAI(rec, req, prov, toolProtocolChatRequest("create_file"), rec); err != nil {
			t.Fatalf("stream OpenAI response: %v; body=%s", err, rec.Body.String())
		}
		calls, _ := parseOpenAIStreamToolCalls(t, rec.Body.String())
		if len(calls) != 1 || calls[0].ID != "call_firstcall_late" {
			t.Fatalf("identity fragments were not merged: %#v; body=%s", calls, rec.Body.String())
		}
	})

	t.Run("direct stream repairs collision against a fragmented final ID", func(t *testing.T) {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_","type":"function","function":{"name":"create_file","arguments":"{}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"shared"},{"index":1,"id":"call_shared","type":"function","function":{"name":"create_file","arguments":"{}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		if err := server.streamOpenAI(rec, req, prov, toolProtocolChatRequest("create_file"), rec); err != nil {
			t.Fatalf("stream OpenAI response: %v; body=%s", err, rec.Body.String())
		}
		calls, _ := parseOpenAIStreamToolCalls(t, rec.Body.String())
		if len(calls) != 2 || calls[0].ID != "call_shared" || calls[0].ID == calls[1].ID {
			t.Fatalf("fragmented final ID collision was not repaired: %#v; body=%s", calls, rec.Body.String())
		}
	})

	t.Run("direct stream preserves legacy function finish", func(t *testing.T) {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"function_call":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()

		if err := server.streamOpenAI(rec, req, prov, toolProtocolChatRequest("create_file"), rec); err != nil {
			t.Fatalf("stream legacy OpenAI response: %v", err)
		}
		if !strings.Contains(rec.Body.String(), `"finish_reason":"function_call"`) {
			t.Fatalf("legacy finish reason was not preserved: %s", rec.Body.String())
		}
	})

	t.Run("direct stream tool state is isolated per choice", func(t *testing.T) {
		sanitizer := newOpenAIStreamToolSanitizer(map[string]struct{}{"create_file": {}})
		normalizeOpenAIStreamLineForVisualStudioWithToolState(
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":null}]}`,
			sanitizer,
		)

		finish := normalizeOpenAIStreamLineForVisualStudioWithToolState(
			`data: {"choices":[{"index":1,"delta":{},"finish_reason":"stop"}]}`,
			sanitizer,
		)
		if !strings.Contains(finish, `"index":1`) || !strings.Contains(finish, `"finish_reason":"stop"`) {
			t.Fatalf("choice 0 tool state leaked into choice 1: %s", finish)
		}
	})

	t.Run("late DSML is normalized", func(t *testing.T) {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{"reasoning_content":"thinking"},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{"content":"<|DSML|tool_calls><|DSML|invoke name=\"get_file\"><|DSML|parameter name=\"filename\">a.cs</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>"},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		chatReq := toolProtocolChatRequest("get_file")
		chatReq.Model = "step-router-v1"

		if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err != nil {
			t.Fatalf("stream OpenAI response: %v", err)
		}
		if rec.Header().Get("X-Proxy-Tool-Call-Normalization") != "dsml" {
			t.Fatalf("normalization header = %q, want dsml", rec.Header().Get("X-Proxy-Tool-Call-Normalization"))
		}
		if !strings.Contains(rec.Body.String(), `"name":"get_file"`) {
			t.Fatalf("late DSML was passed through as text; body=%s", rec.Body.String())
		}
	})

	t.Run("raw object arguments become JSON string", func(t *testing.T) {
		body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_file","arguments":{"path":"a.txt"}}}]},"finish_reason":"tool_calls"}]}`)
		out := normalizeProviderSpecificToolCallsInOpenAIJSON(body, allowedCreateFile)
		var root map[string]any
		if err := json.Unmarshal(out, &root); err != nil {
			t.Fatalf("decode normalized response: %v", err)
		}
		choice := root["choices"].([]any)[0].(map[string]any)
		message := choice["message"].(map[string]any)
		call := message["tool_calls"].([]any)[0].(map[string]any)
		function := call["function"].(map[string]any)
		arguments, ok := function["arguments"].(string)
		if !ok || !json.Valid([]byte(arguments)) {
			t.Fatalf("arguments = %#v, want JSON string", function["arguments"])
		}
	})

	t.Run("typed tool calls require matching finish reason and unique IDs", func(t *testing.T) {
		resp := &provider.ChatResponse{Choices: []provider.Choice{{
			Message: provider.Message{ToolCalls: []provider.ToolCall{
				{ID: "duplicate", Type: "function", Function: provider.FunctionCall{Name: "create_file", Arguments: `{}`}},
				{ID: "duplicate", Type: "function", Function: provider.FunctionCall{Name: "create_file", Arguments: `{}`}},
			}},
			FinishReason: "stop",
		}}}
		normalizeProviderSpecificToolCalls(resp, nil)
		if err := validateProviderResponseToolContract(resp); err != nil {
			t.Fatalf("normalized tool calls must satisfy the contract: %v", err)
		}
		calls := resp.Choices[0].Message.ToolCalls
		if calls[0].ID == calls[1].ID || resp.Choices[0].FinishReason != "tool_calls" {
			t.Fatalf("tool envelope was not repaired: %#v", resp.Choices[0])
		}
	})

	t.Run("tool finish without payload becomes ordinary stop", func(t *testing.T) {
		for _, finishReason := range []string{"tool_calls", "function_call"} {
			t.Run(finishReason, func(t *testing.T) {
				resp := &provider.ChatResponse{Choices: []provider.Choice{{
					Message:      provider.Message{Content: "ordinary answer"},
					FinishReason: finishReason,
				}}}
				normalizeProviderSpecificToolCalls(resp, nil)
				if resp.Choices[0].FinishReason != "stop" {
					t.Fatalf("typed finish_reason = %q, want stop", resp.Choices[0].FinishReason)
				}
				if err := validateProviderResponseToolContract(resp); err != nil {
					t.Fatalf("normalized typed response was rejected: %v", err)
				}

				raw := []byte(`{"choices":[{"message":{"role":"assistant","content":"ordinary answer"},"finish_reason":"` + finishReason + `"}]}`)
				raw = normalizeProviderSpecificToolCallsInOpenAIJSON(raw, nil)
				if err := validateOpenAIChatResponseBody(raw); err != nil {
					t.Fatalf("normalized raw response was rejected: %v; body=%s", err, raw)
				}
				var decoded provider.ChatResponse
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatalf("decode normalized raw response: %v", err)
				}
				if decoded.Choices[0].FinishReason != "stop" {
					t.Fatalf("raw finish_reason = %q, want stop", decoded.Choices[0].FinishReason)
				}
			})
		}
	})

	t.Run("direct stream does not expose phantom tool finish", func(t *testing.T) {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"ordinary answer"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
			``,
		}, "\n")
		server := &Server{}
		prov := &fakeStreamProvider{name: "openai", body: stream}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()

		if err := server.streamOpenAI(rec, req, prov, toolProtocolChatRequest("create_file"), rec); err != nil {
			t.Fatalf("stream response with ordinary content was rejected: %v", err)
		}
		if strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) ||
			!strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
			t.Fatalf("phantom tool finish was exposed: %s", rec.Body.String())
		}
	})

	t.Run("raw tool calls override empty finish", func(t *testing.T) {
		body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":""}]}`)
		body = normalizeOpenAIChatResponseForVisualStudio(body)
		body = normalizeProviderSpecificToolCallsInOpenAIJSON(body, allowedCreateFile)

		var got provider.ChatResponse
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode normalized response: %v", err)
		}
		if got.Choices[0].FinishReason != "tool_calls" {
			t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
		}
	})

	t.Run("declared tool spelling is exact", func(t *testing.T) {
		body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":"TOOL_CALLS"}]}`)
		body = normalizeProviderSpecificToolCallsInOpenAIJSON(body, map[string]struct{}{"Create_File": {}})

		var got provider.ChatResponse
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode normalized response: %v", err)
		}
		choice := got.Choices[0]
		if choice.FinishReason != "tool_calls" || choice.Message.ToolCalls[0].Function.Name != "Create_File" {
			t.Fatalf("normalized choice = %#v, want exact declared name and canonical finish", choice)
		}
	})

	t.Run("truncation never exposes executable tool call", func(t *testing.T) {
		body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":"length"}]}`)
		body = normalizeProviderSpecificToolCallsInOpenAIJSON(body, allowedCreateFile)

		var got provider.ChatResponse
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode normalized response: %v", err)
		}
		choice := got.Choices[0]
		if choice.FinishReason != "length" || len(choice.Message.ToolCalls) != 0 {
			t.Fatalf("truncated choice = %#v, want length without executable tools", choice)
		}
	})

	t.Run("truncated DSML never becomes executable tool call", func(t *testing.T) {
		resp := &provider.ChatResponse{Choices: []provider.Choice{{
			Message:      provider.Message{Content: dsmlAdvisorSample},
			FinishReason: "length",
		}}}
		normalizeProviderSpecificToolCalls(resp, map[string]struct{}{"get_file": {}})
		choice := resp.Choices[0]
		if choice.FinishReason != "length" || len(choice.Message.ToolCalls) != 0 {
			t.Fatalf("truncated DSML choice = %#v, want length without executable tools", choice)
		}

		raw := []byte(`{"choices":[{"message":{"role":"assistant","content":` + strconv.Quote(dsmlAdvisorSample) + `},"finish_reason":"content_filter"}]}`)
		raw = normalizeProviderSpecificToolCallsInOpenAIJSON(raw, map[string]struct{}{"get_file": {}})
		var root map[string]any
		if err := json.Unmarshal(raw, &root); err != nil {
			t.Fatalf("decode normalized DSML response: %v", err)
		}
		choiceMap := root["choices"].([]any)[0].(map[string]any)
		messageMap := choiceMap["message"].(map[string]any)
		if _, exists := messageMap["tool_calls"]; exists || choiceMap["finish_reason"] != "content_filter" {
			t.Fatalf("raw truncated DSML became executable: %s", raw)
		}

		ollama := []byte(`{"message":{"role":"assistant","content":` + strconv.Quote(dsmlAdvisorSample) + `},"done_reason":"length"}`)
		ollama = normalizeDSMLToolCallsInOllamaJSON(ollama, map[string]struct{}{"get_file": {}})
		var ollamaRoot map[string]any
		if err := json.Unmarshal(ollama, &ollamaRoot); err != nil {
			t.Fatalf("decode normalized Ollama DSML response: %v", err)
		}
		ollamaMessage := ollamaRoot["message"].(map[string]any)
		if _, exists := ollamaMessage["tool_calls"]; exists || ollamaRoot["done_reason"] != "length" {
			t.Fatalf("Ollama truncated DSML became executable: %s", ollama)
		}
	})

	t.Run("UTF-8 BOM preserves first tool chunk", func(t *testing.T) {
		body := append([]byte{0xef, 0xbb, 0xbf}, []byte(strings.Join([]string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}, "\n"))...)

		converted, err := openAIStreamBodyToChatResponse(body, "gpt-test", allowedCreateFile)
		if err != nil {
			t.Fatalf("aggregate BOM-prefixed SSE: %v", err)
		}
		var got provider.ChatResponse
		if err := json.Unmarshal(converted, &got); err != nil {
			t.Fatalf("decode aggregated response: %v", err)
		}
		if len(got.Choices[0].Message.ToolCalls) != 1 {
			t.Fatalf("BOM dropped first tool chunk: %#v", got.Choices[0])
		}
	})

	t.Run("SSE object arguments become JSON string", func(t *testing.T) {
		body := []byte(strings.Join([]string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_file","arguments":{"path":"a.txt"}}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}, "\n"))

		converted, err := openAIStreamBodyToChatResponse(body, "gpt-test", allowedCreateFile)
		if err != nil {
			t.Fatalf("aggregate object arguments: %v", err)
		}
		var got provider.ChatResponse
		if err := json.Unmarshal(converted, &got); err != nil {
			t.Fatalf("decode aggregated response: %v", err)
		}
		arguments := got.Choices[0].Message.ToolCalls[0].Function.Arguments
		if arguments != `{"path":"a.txt"}` {
			t.Fatalf("arguments = %q, want canonical JSON string", arguments)
		}
	})

	t.Run("DSML aggregation is bounded", func(t *testing.T) {
		scanner := bufio.NewScanner(strings.NewReader("data: first\ndata: second\n"))
		var raw bytes.Buffer
		buffered := []string{}

		err := drainOpenAIStreamProbeWithLimit(scanner, &raw, &buffered, 12)
		if !errors.Is(err, errOpenAIStreamTooLarge) {
			t.Fatalf("drain error = %v, want stream-too-large", err)
		}
	})

	t.Run("direct SSE event buffering is bounded", func(t *testing.T) {
		rec := httptest.NewRecorder()
		processor := newOpenAIStreamEventProcessor(
			rec,
			rec,
			newStreamReasoningAccumulator(),
			newOpenAIStreamToolSanitizer(allowedCreateFile),
		)
		processor.maxBytes = 12

		err := processor.consumeLine("data: " + strings.Repeat("x", 20))
		if !errors.Is(err, errOpenAIStreamTooLarge) {
			t.Fatalf("event buffer error = %v, want stream-too-large", err)
		}
	})

	t.Run("malformed Ollama tool stream is rejected", func(t *testing.T) {
		server := &Server{}
		prov := &fakeStreamProvider{
			name: "ollama",
			body: `{"message":{"role":"assistant","tool_calls":[`,
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()

		err := server.streamOllamaToOpenAI(
			rec,
			req,
			prov,
			toolProtocolChatRequest("create_file"),
			rec,
		)
		if err == nil {
			t.Fatalf("malformed Ollama stream returned success: %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "[DONE]") {
			t.Fatalf("malformed Ollama stream emitted DONE: %s", rec.Body.String())
		}
	})

	t.Run("OpenAI to Ollama rejects SSE error instead of emitting done", func(t *testing.T) {
		server := &Server{}
		prov := &fakeStreamProvider{
			name: "openai",
			body: `data: {"error":{"message":"tool backend unavailable"}}` + "\n" + `data: [DONE]` + "\n",
		}
		req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
		rec := httptest.NewRecorder()

		err := server.streamOpenAIToOllama(
			rec,
			req,
			prov,
			toolProtocolChatRequest("create_file"),
			rec,
		)
		if err == nil {
			t.Fatalf("malformed OpenAI stream returned success: %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), `"done":true`) {
			t.Fatalf("malformed OpenAI stream emitted done: %s", rec.Body.String())
		}
	})

	t.Run("HTTP 200 error object is rejected", func(t *testing.T) {
		prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, "")
		prov.rawBody = []byte(`{"error":{"message":"upstream failed","type":"server_error"}}`)
		server := newOpenServer(prov)
		handler := withMux(server, func(mux *http.ServeMux) {
			mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
		})
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
		if rec.Code < http.StatusBadRequest {
			t.Fatalf("raw error object returned HTTP %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func toolProtocolChatRequest(toolName string) *provider.ChatRequest {
	return &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{
			Type: "function",
			Function: provider.ToolFunc{
				Name: toolName,
			},
		}},
	}
}
