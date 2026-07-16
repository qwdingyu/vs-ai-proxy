package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/requestmeta"
)

func TestOpenAIProviderUsesCapabilityPaths(t *testing.T) {
	seen := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path]++
		switch r.URL.Path {
		case "/v1beta/openai/models":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"gemini-test"}]}`))
		case "/v1beta/openai/chat/completions":
			var req ChatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode chat request: %v", err)
			}
			if _, ok := req.Extra["provider_routing"]; !ok {
				t.Fatalf("provider_routing extension field was not forwarded")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"id":"chatcmpl-test",
				"object":"chat.completion",
				"created":1,
				"model":"gemini-test",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
			}`))
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("google", "test-key", upstream.URL, true, time.Second)

	models, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if len(models) != 1 || models[0] != "gemini-test" {
		t.Fatalf("unexpected models: %#v", models)
	}

	resp, err := prov.Chat(context.Background(), &ChatRequest{
		Model:    "gemini-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"provider_routing": []byte(`{"allow_fallbacks":true}`),
		},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Model != "gemini-test" {
		t.Fatalf("unexpected response model: %s", resp.Model)
	}

	if seen["/v1beta/openai/models"] != 1 {
		t.Fatalf("models path was not used, seen=%#v", seen)
	}
	if seen["/v1beta/openai/chat/completions"] != 1 {
		t.Fatalf("chat path was not used, seen=%#v", seen)
	}
}

func TestOpenAIProviderChatRawPreservesToolFields(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	var req ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"glm-5.2",
		"messages":[{"role":"user","content":"create a file"}],
		"tools":[{"type":"function","strict":true,"function":{"name":"create_file","description":"Create file","parameters":{"type":"object"},"x-provider":"keep"}}],
		"tool_choice":"auto",
		"parallel_tool_calls":true
	}`), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	if _, err := prov.ChatRaw(context.Background(), &req); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}

	tools, _ := captured["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools missing from upstream request: %#v", captured)
	}
	tool := tools[0].(map[string]any)
	if tool["strict"] != true {
		t.Fatalf("tool strict missing: %#v", tool)
	}
	fn := tool["function"].(map[string]any)
	if fn["x-provider"] != "keep" {
		t.Fatalf("function extension missing: %#v", fn)
	}
	if captured["tool_choice"] != "auto" || captured["parallel_tool_calls"] != true {
		t.Fatalf("tool selection fields missing: %#v", captured)
	}
}

func TestOpenAIProviderForwardsRequestIDHeaders(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat_raw", true: "chat_stream"}[stream], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("X-Request-ID"); got != "req-upstream-1" {
					t.Fatalf("X-Request-ID = %q, want req-upstream-1", got)
				}
				if got := r.Header.Get("X-Proxy-Request-ID"); got != "req-upstream-1" {
					t.Fatalf("X-Proxy-Request-ID = %q, want req-upstream-1", got)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer upstream.Close()

			prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
			ctx := requestmeta.ContextWithRequestID(context.Background(), "req-upstream-1")
			req := &ChatRequest{Model: "gpt-5.5", Messages: []Message{{Role: "user", Content: "hi"}}}
			if stream {
				streamBody, err := prov.ChatStream(ctx, req)
				if err != nil {
					t.Fatalf("ChatStream returned error: %v", err)
				}
				_ = streamBody.Close()
				return
			}
			if _, err := prov.ChatRaw(ctx, req); err != nil {
				t.Fatalf("ChatRaw returned error: %v", err)
			}
		})
	}
}

func TestOpenAIProviderChatRawConvertsMaxOutputTokensForChatCompletions(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	var req ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"create files"}],
		"max_output_tokens":8192,
		"tools":[{"type":"function","function":{"name":"create_file","parameters":{"type":"object"}}}],
		"tool_choice":"auto"
	}`), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	if _, err := prov.ChatRaw(context.Background(), &req); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}
	if _, leaked := captured["max_output_tokens"]; leaked {
		t.Fatalf("max_output_tokens must not be sent to /chat/completions: %#v", captured)
	}
	if captured["max_tokens"] != float64(8192) {
		t.Fatalf("max_tokens = %#v, want 8192; body=%#v", captured["max_tokens"], captured)
	}
	if tools, _ := captured["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools should remain declared after parameter normalization: %#v", captured)
	}
}

func TestOpenAIProviderChatStreamConvertsMaxOutputTokensForChatCompletions(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		// 故意声明错误 Content-Type，覆盖真实兼容网关未正确标记 SSE 的情况。
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	var req ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"create files"}],
		"max_output_tokens":8192,
		"tools":[{"type":"function","function":{"name":"create_file","parameters":{"type":"object"}}}],
		"tool_choice":"auto"
	}`), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	stream, err := prov.ChatStream(context.Background(), &req)
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	_ = stream.Close()
	if _, leaked := captured["max_output_tokens"]; leaked {
		t.Fatalf("max_output_tokens must not be sent to streamed /chat/completions: %#v", captured)
	}
	if captured["max_tokens"] != float64(8192) || captured["stream"] != true {
		t.Fatalf("stream request was not normalized correctly: %#v", captured)
	}
	if tools, _ := captured["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools should remain declared after stream parameter normalization: %#v", captured)
	}
}

func TestOpenAIProviderConvertsMaxCompletionTokensForChatCompletions(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	req := ChatRequest{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: "user", Content: "create files"}},
		Extra: map[string]json.RawMessage{
			"max_completion_tokens": []byte(`6144`),
			"tool_choice":           []byte(`"auto"`),
		},
	}

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	if _, err := prov.ChatRaw(context.Background(), &req); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}
	if _, leaked := captured["max_completion_tokens"]; leaked {
		t.Fatalf("max_completion_tokens must not be sent to /chat/completions: %#v", captured)
	}
	if captured["max_tokens"] != float64(6144) {
		t.Fatalf("max_tokens = %#v, want 6144; body=%#v", captured["max_tokens"], captured)
	}
	if captured["tool_choice"] != "auto" {
		t.Fatalf("tool_choice should remain preserved: %#v", captured)
	}
}

func TestOpenAIProviderChatRawPreservesCommonToolMatrix(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	toolNames := []string{"create_file", "powershell", "git", "run_in_terminal", "apply_patch", "read_file"}
	tools := make([]map[string]any, 0, len(toolNames))
	for _, name := range toolNames {
		tools = append(tools, map[string]any{
			"type":   "function",
			"strict": true,
			"function": map[string]any{
				"name":        name,
				"description": "Tool " + name,
				"parameters": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"input": map[string]any{"type": "string"},
					},
				},
			},
		})
	}

	req := ChatRequest{
		Model:    "glm-5.2",
		Messages: []Message{{Role: "user", Content: "use tools"}},
		Extra: map[string]json.RawMessage{
			"tool_choice": []byte(`"auto"`),
		},
	}
	data, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	if err := json.Unmarshal(data, &req.Tools); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	if _, err := prov.ChatRaw(context.Background(), &req); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}

	capturedTools, _ := captured["tools"].([]any)
	if len(capturedTools) != len(toolNames) {
		t.Fatalf("tools len = %d, want %d; body=%#v", len(capturedTools), len(toolNames), captured)
	}
	seen := map[string]bool{}
	for _, raw := range capturedTools {
		tool := raw.(map[string]any)
		if tool["strict"] != true {
			t.Fatalf("strict flag lost for tool: %#v", tool)
		}
		fn := tool["function"].(map[string]any)
		seen[fn["name"].(string)] = true
		params := fn["parameters"].(map[string]any)
		if params["additionalProperties"] != false {
			t.Fatalf("schema additionalProperties lost: %#v", params)
		}
	}
	for _, name := range toolNames {
		if !seen[name] {
			t.Fatalf("tool %q missing from upstream request: %#v", name, capturedTools)
		}
	}
}

func TestOpenAIProviderChatReportsNonJSONBodyPreview(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("downstream temporarily unavailable"))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	_, err := prov.Chat(context.Background(), &ChatRequest{Model: "gpt-5.5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatalf("Chat should fail for non-JSON response")
	}
	if !strings.Contains(err.Error(), "body_preview") || !strings.Contains(err.Error(), "downstream temporarily unavailable") {
		t.Fatalf("error should include response preview, got: %v", err)
	}
}

func TestReadBoundedBodyRejectsOversizedResponse(t *testing.T) {
	body, err := readBoundedBody(strings.NewReader("12345"), 4)
	if !errors.Is(err, errProviderResponseBodyTooLarge) {
		t.Fatalf("readBoundedBody() error = %v, want response-too-large error", err)
	}
	if body != nil {
		t.Fatalf("readBoundedBody() body = %q, want nil on overflow", body)
	}
}

func TestReadBoundedBodyAllowsExactLimit(t *testing.T) {
	body, err := readBoundedBody(strings.NewReader("1234"), 4)
	if err != nil {
		t.Fatalf("readBoundedBody() error = %v", err)
	}
	if got, want := string(body), "1234"; got != want {
		t.Fatalf("readBoundedBody() = %q, want %q", got, want)
	}
}

func TestReadProviderResponseBodyTruncatesOversizedHTTPErrorWithoutLosingStatus(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Body: io.NopCloser(strings.NewReader(
			strings.Repeat("x", int(maxProviderErrorResponseBodyBytes)+1),
		)),
	}

	body, err := readProviderResponseBody(resp)
	if err != nil {
		t.Fatalf("readProviderResponseBody() error = %v", err)
	}
	if got := int64(len(body)); got != maxProviderErrorResponseBodyBytes {
		t.Fatalf("body length = %d, want truncated %d", got, maxProviderErrorResponseBodyBytes)
	}
}

func TestOpenAIProviderChatAcceptsSSEBodyForNonStreamRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Stream {
			t.Fatalf("request should be non-stream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":"!"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
			``,
		}, "\n")))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	resp, err := prov.Chat(context.Background(), &ChatRequest{Model: "gpt-5.5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "Hello!" {
		t.Fatalf("unexpected response: %#v", resp)
	}
}

func TestToolProtocolContractFallbackSSEParserPreservesToolCalls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"\ufeff" + `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
			``,
		}, "\n\n")))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	resp, err := prov.Chat(context.Background(), &ChatRequest{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: "user", Content: "read a.txt"}},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %#v, want one choice", resp.Choices)
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %#v, want one call", choice.Message.ToolCalls)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "read_file" {
		t.Fatalf("unexpected tool call: %#v", call)
	}
	if call.Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %q, want complete JSON object", call.Function.Arguments)
	}
}

func TestCollectOpenAIChatSSESupportsStandardMultilineEvents(t *testing.T) {
	stream := "\ufeff" + strings.Join([]string{
		"event: message",
		`data: {"choices":[{"delta":{"content":`,
		`data: "Hello"},"finish_reason":"stop"}]}`,
		``,
		"event: done",
		"data: [DONE]",
		``,
	}, "\n")

	resp, err := CollectOpenAIChatSSE(strings.NewReader(stream), "gpt-5.5", 1<<20)
	if err != nil {
		t.Fatalf("CollectOpenAIChatSSE returned error: %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "Hello" {
		t.Fatalf("content = %q, want Hello", got)
	}
	if got := resp.Choices[0].FinishReason; got != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got)
	}
}

func TestCollectOpenAIChatSSERejectsErrorEvent(t *testing.T) {
	stream := strings.Join([]string{
		"event: error",
		`data: {"message":"upstream unavailable"}`,
		``,
	}, "\n")

	_, err := CollectOpenAIChatSSE(strings.NewReader(stream), "gpt-5.5", 1<<20)
	if err == nil || !strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("CollectOpenAIChatSSE error = %v, want upstream error event", err)
	}
}

func TestCollectOpenAIChatSSEAggregatesLegacyFunctionCall(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"function_call":{"name":"read_file","arguments":"{\"path\":"}},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"function_call":{"arguments":"\"a.txt\"}"}},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"function_call"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	resp, err := CollectOpenAIChatSSE(strings.NewReader(stream), "gpt-4", 1<<20)
	if err != nil {
		t.Fatalf("CollectOpenAIChatSSE returned error: %v", err)
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "function_call" {
		t.Fatalf("finish_reason = %q, want function_call", choice.FinishReason)
	}
	if choice.Message.FunctionCall == nil {
		t.Fatal("legacy function_call was not preserved")
	}
	if got := choice.Message.FunctionCall.Name; got != "read_file" {
		t.Fatalf("function name = %q, want read_file", got)
	}
	if got := choice.Message.FunctionCall.Arguments; got != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %q, want complete JSON object", got)
	}
}

func TestOpenAIProviderChatSSEStopsReadingAfterDone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "\n")+"\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// 模拟部分网关在应用层 DONE 后仍保持 HTTP body；provider 必须
		// 以 DONE 结束聚合，不能把传输层 EOF 当成协议终态。
		<-r.Context().Done()
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability(
		"openai",
		"openai",
		"sk-test",
		upstream.URL,
		true,
		150*time.Millisecond,
	)
	result := make(chan struct {
		resp *ChatResponse
		err  error
	}, 1)
	go func() {
		resp, err := prov.Chat(context.Background(), &ChatRequest{
			Model:    "gpt-test",
			Messages: []Message{{Role: "user", Content: "hello"}},
		})
		result <- struct {
			resp *ChatResponse
			err  error
		}{resp: resp, err: err}
	}()

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("Chat returned error after [DONE]: %v", got.err)
		}
		if got.resp == nil || got.resp.Choices[0].Message.Content != "ok" {
			t.Fatalf("unexpected response after [DONE]: %#v", got.resp)
		}
	case <-time.After(800 * time.Millisecond):
		t.Fatal("Chat waited for transport EOF after [DONE]")
	}
}

func TestCollectOpenAIChatSSEPreservesRefusal(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"refusal":"policy "},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"refusal":"refusal"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	resp, err := CollectOpenAIChatSSE(strings.NewReader(body), "gpt-test", 0)
	if err != nil {
		t.Fatalf("collect refusal stream: %v", err)
	}
	if got := resp.Choices[0].Message.Refusal; got != "policy refusal" {
		t.Fatalf("refusal = %q, want policy refusal", got)
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal refusal response: %v", err)
	}
	if !strings.Contains(string(encoded), `"refusal":"policy refusal"`) {
		t.Fatalf("refusal was lost from response JSON: %s", encoded)
	}
}

func TestCollectOpenAIChatSSEAggregatesFragmentedToolIdentity(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{` +
			`"index":0,"id":"call_","type":"function","function":{` +
			`"name":"mcp_workspace_","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{` +
			`"index":0,"id":"42","function":{` +
			`"name":"symbol","arguments":"\"needle\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	resp, err := CollectOpenAIChatSSE(strings.NewReader(body), "gpt-test", 0)
	if err != nil {
		t.Fatalf("collect fragmented tool identity: %v", err)
	}
	call := resp.Choices[0].Message.ToolCalls[0]
	hasExpectedEnvelope := call.ID == "call_42" && call.Type == "function"
	hasExpectedFunction := call.Function.Name == "mcp_workspace_symbol" &&
		call.Function.Arguments == `{"query":"needle"}`
	if !hasExpectedEnvelope || !hasExpectedFunction {
		t.Fatalf("fragmented tool identity was not merged: %#v", call)
	}
}

func TestCollectOpenAIChatSSEAggregatesFragmentedLegacyFunctionName(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"function_call":{` +
			`"name":"workspace_","arguments":"{\"query\":"}},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"function_call":{` +
			`"name":"lookup","arguments":"\"needle\"}"}},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"function_call"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	resp, err := CollectOpenAIChatSSE(strings.NewReader(body), "gpt-test", 0)
	if err != nil {
		t.Fatalf("collect fragmented legacy function name: %v", err)
	}
	call := resp.Choices[0].Message.FunctionCall
	if call == nil {
		t.Fatal("fragmented legacy function call is missing")
	}
	hasExpectedFunction := call.Name == "workspace_lookup" &&
		call.Arguments == `{"query":"needle"}`
	if !hasExpectedFunction {
		t.Fatalf("fragmented legacy function name was not merged: %#v", call)
	}
}

func TestNormalizeAndValidateChatResponseToolsRejectsInvalidToolType(t *testing.T) {
	resp := &ChatResponse{Choices: []Choice{{
		Message: Message{ToolCalls: []ToolCall{{
			ID:   "call_invalid",
			Type: "computer",
			Function: FunctionCall{
				Name:      "create_file",
				Arguments: `{"path":"a.txt"}`,
			},
		}}},
		FinishReason: "tool_calls",
	}}}

	if err := normalizeAndValidateChatResponseTools(resp); err == nil {
		t.Fatal("invalid provider tool type must not pass the typed response boundary")
	}
}

func TestCollectOpenAIChatSSEHidesIncompleteToolCallOnTruncation(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	resp, err := CollectOpenAIChatSSE(strings.NewReader(stream), "gpt-5.5", 1<<20)
	if err != nil {
		t.Fatalf("CollectOpenAIChatSSE returned error: %v", err)
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "length" {
		t.Fatalf("finish_reason = %q, want length", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 0 {
		t.Fatalf("incomplete tool calls must not be exposed: %#v", choice.Message.ToolCalls)
	}
}

func TestCollectOpenAIChatSSERejectsIncompleteToolCallOnStop(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	_, err := CollectOpenAIChatSSE(strings.NewReader(stream), "gpt-5.5", 1<<20)
	if err == nil {
		t.Fatal("CollectOpenAIChatSSE should reject incomplete tool call on stop")
	}
}

func TestCollectOpenAIChatSSERejectsOversizedStream(t *testing.T) {
	_, err := CollectOpenAIChatSSE(strings.NewReader("data: {}\n\n"), "gpt-5.5", 8)
	if !errors.Is(err, ErrOpenAIStreamTooLarge) {
		t.Fatalf("CollectOpenAIChatSSE error = %v, want ErrOpenAIStreamTooLarge", err)
	}
}

func TestOpenAIProviderChatRejectsSSEErrorFrameAfterContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`,
			`data: {"error":{"message":"tool backend failed","type":"upstream_error"}}`,
			`data: [DONE]`,
			``,
		}, "\n\n")))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	_, err := prov.Chat(context.Background(), &ChatRequest{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: "user", Content: "read a.txt"}},
	})
	if err == nil || !strings.Contains(err.Error(), "tool backend failed") {
		t.Fatalf("Chat error = %v, want in-band upstream error", err)
	}
}

func TestOpenAIProviderChatRejectsTruncatedSSEToolArguments(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"preparing"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
			``,
		}, "\n\n")))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	_, err := prov.Chat(context.Background(), &ChatRequest{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: "user", Content: "read a.txt"}},
	})
	if err == nil {
		t.Fatal("Chat should reject truncated tool arguments")
	}
}

func TestOpenAIProviderChatRejectsIncompleteJSONToolArguments(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	_, err := prov.Chat(context.Background(), &ChatRequest{Model: "gpt-5.5"})
	if err == nil {
		t.Fatal("Chat should reject incomplete JSON tool arguments")
	}
}

func TestOpenAIProviderChatRawRejectsHTTP200ErrorObject(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"model unavailable","type":"upstream_error","code":"model_unavailable"}}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	_, err := prov.ChatRaw(context.Background(), &ChatRequest{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "model unavailable") {
		t.Fatalf("ChatRaw error = %v, want HTTP 200 error object", err)
	}
}

func TestOpenAIProviderChatRawRetriesTransientServerErrors(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"Service temporarily unavailable"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	if _, err := prov.ChatRaw(context.Background(), &ChatRequest{Model: "gpt-5.5", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("ChatRaw returned error after retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestOpenAIProviderCanDisableDefensiveRetriesAndHeaders(t *testing.T) {
	calls := 0
	var userAgent string
	var requestedWith string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		userAgent = r.Header.Get("User-Agent")
		requestedWith = r.Header.Get("X-Requested-With")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"Service temporarily unavailable"}}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	prov.SetDefenseEnabled(false)
	_, err := prov.ChatRaw(context.Background(), &ChatRequest{Model: "gpt-5.5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("ChatRaw should fail for upstream 503")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, disabled defense must not retry", calls)
	}
	if strings.Contains(userAgent, "vs-ai-proxy") || requestedWith != "" {
		t.Fatalf("defensive headers should be disabled, user-agent=%q x-requested-with=%q", userAgent, requestedWith)
	}
}

func TestOpenAIProviderSendsStableDefensiveUserAgent(t *testing.T) {
	var userAgent string
	var requestedWith string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.Header.Get("User-Agent")
		requestedWith = r.Header.Get("X-Requested-With")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	if _, err := prov.ChatRaw(context.Background(), &ChatRequest{Model: "gpt-5.5", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}
	if userAgent != "vs-ai-proxy" || requestedWith != "vs-ai-proxy" {
		t.Fatalf("unexpected defensive headers, user-agent=%q x-requested-with=%q", userAgent, requestedWith)
	}
}

func TestOpenAIProviderChatRawDoesNotRetryClientErrors(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	_, err := prov.ChatRaw(context.Background(), &ChatRequest{Model: "bad-model", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatalf("ChatRaw should fail for 400")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestOpenAIProviderChatRawDoesNotBlindRetryRateLimits(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	_, err := prov.ChatRaw(context.Background(), &ChatRequest{Model: "gpt-5.5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("ChatRaw should return the 429 error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, 429 without Retry-After must not be blindly retried", calls)
	}
}

func TestOpenAIProviderChatStreamRetriesTransientServerErrors(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"Service temporarily unavailable"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	stream, err := prov.ChatStream(context.Background(), &ChatRequest{Model: "gpt-5.5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("ChatStream returned error after retry: %v", err)
	}
	stream.Close()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestOpenAIProviderRetriesShareOneOperationTimeout(t *testing.T) {
	prov := NewOpenAIProviderWithCapability(
		"openai",
		"openai",
		"sk-test",
		"https://example.invalid",
		true,
		2*time.Second,
	)
	deadlines := []time.Time{}
	missingDeadline := false
	prov.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			missingDeadline = true
			return nil, errors.New("retry request is missing operation deadline")
		}
		deadlines = append(deadlines, deadline)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"message":"temporarily unavailable"}}`,
			)),
			Request: req,
		}, nil
	})

	_, err := prov.ChatRaw(context.Background(), &ChatRequest{Model: "gpt-5.5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("ChatRaw should return the final transient error")
	}
	if missingDeadline {
		t.Fatal("retry request should have an operation deadline")
	}
	if len(deadlines) != prov.openAIProviderMaxAttempts() {
		t.Fatalf("retry calls = %d, want %d", len(deadlines), prov.openAIProviderMaxAttempts())
	}
	for index := 1; index < len(deadlines); index++ {
		if !deadlines[index].Equal(deadlines[0]) {
			t.Fatalf(
				"retry %d received a fresh deadline: first=%s current=%s",
				index,
				deadlines[0],
				deadlines[index],
			)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenAIProviderTimeoutReportsWaitingResponseHeaders(t *testing.T) {
	requestBytes := make(chan int, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			requestBytes <- 0
			return
		}
		requestBytes <- len(body)
		<-r.Context().Done()
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability(
		"openai",
		"openai",
		"sk-test",
		upstream.URL,
		true,
		5*time.Second,
	)
	_, err := prov.ChatStream(context.Background(), &ChatRequest{
		Model: "gpt-5.6-sol",
		Messages: []Message{{
			Role:    "user",
			Content: strings.Repeat("x", 1<<20),
		}},
	})
	if err == nil {
		t.Fatal("ChatStream should time out while the upstream withholds response headers")
	}
	if !strings.Contains(err.Error(), "upstream_stage=waiting_response_headers") {
		t.Fatalf("timeout stage = %v, want waiting_response_headers", err)
	}
	if got := <-requestBytes; got < 1<<20 {
		t.Fatalf("upstream request bytes = %d, want at least 1 MiB", got)
	}
}

func TestOpenAIProviderTraceResetsForRedirectedHop(t *testing.T) {
	var requests int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.ReadAll(r.Body)
		<-r.Context().Done()
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability(
		"openai",
		"openai",
		"sk-test",
		upstream.URL,
		true,
		5*time.Second,
	)
	_, err := prov.ChatStream(context.Background(), &ChatRequest{
		Model:    "gpt-test",
		Messages: []Message{{Role: "user", Content: "redirect"}},
	})
	if err == nil {
		t.Fatal("redirected ChatStream should time out while the second hop withholds headers")
	}
	if !strings.Contains(err.Error(), "upstream_stage=waiting_response_headers") {
		t.Fatalf("redirected hop stage = %v, want waiting_response_headers", err)
	}
}

func TestOpenAIProviderHonorsLongerParentDeadlineThanClientDefault(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := prov.ChatRaw(ctx, &ChatRequest{Model: "slow-model", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("parent model deadline should override the shorter client default: %v", err)
	}
}

func TestShouldAttemptAlternateChatModeRejectsNonRecoverableErrors(t *testing.T) {
	for _, err := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		&providerHTTPError{StatusCode: http.StatusBadRequest, Message: "bad request"},
		&providerHTTPError{StatusCode: http.StatusTooManyRequests, Message: "rate limited"},
	} {
		if ShouldAttemptAlternateChatMode(err) {
			t.Fatalf("non-recoverable error should not switch modes: %v", err)
		}
	}
	if !ShouldAttemptAlternateChatMode(&providerHTTPError{StatusCode: http.StatusServiceUnavailable, Message: "unavailable"}) {
		t.Fatal("503 should allow one alternate-mode recovery attempt")
	}
}

func TestOpenAIProviderListModelsReportsNonJSONBodyPreview(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("downstream models endpoint unavailable"))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	_, err := prov.ListModels(context.Background())
	if err == nil {
		t.Fatalf("ListModels should fail for non-JSON response")
	}
	if !strings.Contains(err.Error(), "body_preview") || !strings.Contains(err.Error(), "downstream models endpoint unavailable") {
		t.Fatalf("error should include response preview, got: %v", err)
	}
}

func TestOpenAIProviderChatStreamReportsErrorBodyPreview(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream gateway rejected stream"))
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("openai", "openai", "sk-test", upstream.URL, true, time.Second)
	_, err := prov.ChatStream(context.Background(), &ChatRequest{Model: "gpt-5.5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatalf("ChatStream should fail for non-OK response")
	}
	if !strings.Contains(err.Error(), "API 错误 502") || !strings.Contains(err.Error(), "upstream gateway rejected stream") {
		t.Fatalf("error should include response preview, got: %v", err)
	}
}

func TestInferCapabilityNameSupportsProviderInstances(t *testing.T) {
	tests := []struct {
		name         string
		id           string
		providerName string
		baseURL      string
		providerType string
		want         string
	}{
		{name: "known id", id: "openrouter", providerType: "openai", want: "openrouter"},
		{name: "known display name", id: "team-a", providerName: "DeepSeek", providerType: "openai", want: "deepseek"},
		{name: "useai paid id", id: "useai-paid", baseURL: "https://api.eforge.xyz/v1", providerType: "openai", want: "useai"},
		{name: "ollama type", id: "local", providerType: "ollama", want: "ollama"},
		{name: "custom openai compatible", id: "sensenova", baseURL: "https://token.sensenova.cn/v1", providerType: "openai", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InferCapabilityName(tt.id, tt.providerName, tt.baseURL, tt.providerType)
			if got != tt.want {
				t.Fatalf("capability = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUseAIProviderUsesV1BaseURLWithoutDuplicatingPath(t *testing.T) {
	seen := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path]++
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"useai-model"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"id":"chatcmpl-useai",
				"object":"chat.completion",
				"created":1,
				"model":"useai-model",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
			}`))
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("UseAI", "", upstream.URL+"/v1", true, time.Second)

	models, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if len(models) != 1 || models[0] != "useai-model" {
		t.Fatalf("models = %#v, want useai-model", models)
	}

	if _, err := prov.ChatRaw(context.Background(), &ChatRequest{
		Model:    "useai-model",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}
	if seen["/v1/models"] != 1 {
		t.Fatalf("models path count = %d, want 1; seen=%#v", seen["/v1/models"], seen)
	}
	if seen["/v1/chat/completions"] != 1 {
		t.Fatalf("chat path count = %d, want 1; seen=%#v", seen["/v1/chat/completions"], seen)
	}
}

func TestCustomOpenAIProviderUsesV1BaseURLWithoutDuplicatingFallbackPath(t *testing.T) {
	seen := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path]++
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"custom-model"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"id":"chatcmpl-custom",
				"object":"chat.completion",
				"created":1,
				"model":"custom-model",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
			}`))
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("sensenova", "", "test-key", upstream.URL+"/v1", true, time.Second)

	models, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if len(models) != 1 || models[0] != "custom-model" {
		t.Fatalf("models = %#v, want custom-model", models)
	}

	if _, err := prov.ChatRaw(context.Background(), &ChatRequest{
		Model:    "custom-model",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}
	if seen["/v1/models"] != 1 {
		t.Fatalf("models path count = %d, want 1; seen=%#v", seen["/v1/models"], seen)
	}
	if seen["/v1/chat/completions"] != 1 {
		t.Fatalf("chat path count = %d, want 1; seen=%#v", seen["/v1/chat/completions"], seen)
	}
}

func TestOpenAIProviderOmitsAuthorizationHeaderWhenAPIKeyEmpty(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("UseAI", "", upstream.URL+"/v1", true, time.Second)
	if _, err := prov.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
}

func TestOpenAIProviderAddsOpenRouterHeaders(t *testing.T) {
	t.Setenv("PROVIDER_OPENROUTER_REFERER", "https://example.com")
	t.Setenv("PROVIDER_OPENROUTER_TITLE", "VS AI Proxy")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("HTTP-Referer"); got != "https://example.com" {
			t.Fatalf("HTTP-Referer = %q, want https://example.com", got)
		}
		if got := r.Header.Get("X-Title"); got != "VS AI Proxy" {
			t.Fatalf("X-Title = %q, want VS AI Proxy", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q, want application/json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":1,
			"model":"openrouter-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
		}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("openrouter", "test-key", upstream.URL, true, time.Second)
	if _, err := prov.ChatRaw(context.Background(), &ChatRequest{
		Model:    "openrouter-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}
}

func TestOpenAIProviderAddsOpenRouterFallbackHeaders(t *testing.T) {
	t.Setenv("OPENROUTER_HTTP_REFERER", "https://fallback.example")
	t.Setenv("OPENROUTER_X_TITLE", "Fallback Title")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("HTTP-Referer"); got != "https://fallback.example" {
			t.Fatalf("HTTP-Referer = %q, want fallback", got)
		}
		if got := r.Header.Get("X-Title"); got != "Fallback Title" {
			t.Fatalf("X-Title = %q, want fallback", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("openrouter", "test-key", upstream.URL, true, time.Second)
	models, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if len(models) != 1 || models[0] != "model-a" {
		t.Fatalf("models = %#v, want model-a", models)
	}
}

func TestOpenAIProviderDoesNotAddOpenRouterHeadersForOtherProviders(t *testing.T) {
	t.Setenv("PROVIDER_OPENROUTER_REFERER", "https://example.com")
	t.Setenv("PROVIDER_OPENROUTER_TITLE", "VS AI Proxy")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("HTTP-Referer"); got != "" {
			t.Fatalf("HTTP-Referer = %q, want empty", got)
		}
		if got := r.Header.Get("X-Title"); got != "" {
			t.Fatalf("X-Title = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":1,
			"model":"deepseek-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
		}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("deepseek", "test-key", upstream.URL, true, time.Second)
	if _, err := prov.Chat(context.Background(), &ChatRequest{
		Model:    "deepseek-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
}

func TestProviderHTTPClientDisablesHTTP2ForStreamingStability(t *testing.T) {
	prov := NewOpenAIProvider("deepseek", "test-key", "https://example.com", true, time.Second)
	transport, ok := prov.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", prov.Client.Transport)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatalf("ForceAttemptHTTP2 = true, want false")
	}
	if !transport.DisableCompression {
		t.Fatalf("DisableCompression = false, want true")
	}
	if prov.Client.Timeout != time.Second {
		t.Fatalf("timeout = %s, want configured timeout", prov.Client.Timeout)
	}
}

func TestOllamaProviderBuildsNativeChatRequest(t *testing.T) {
	temp := 0.3
	topP := 0.8
	topK := 40
	maxTokens := 2048
	contextLength := 8192
	prov := NewOllamaProvider("ollama", "http://localhost:11434", true, time.Second)

	req := prov.buildChatRequest(&ChatRequest{
		Model:           "llama",
		Temperature:     &temp,
		TopP:            &topP,
		TopK:            &topK,
		MaxTokens:       &maxTokens,
		ContextLength:   &contextLength,
		ReasoningEffort: "high",
		OptionsExtra: map[string]json.RawMessage{
			"num_keep":      []byte(`24`),
			"custom_option": []byte(`{"enabled":true}`),
			"temperature":   []byte(`2`),
		},
		Messages: []Message{{
			Role:      "assistant",
			Content:   "answer",
			Reasoning: "reason",
			Extra: map[string]json.RawMessage{
				"cache_control": []byte(`{"type":"ephemeral"}`),
			},
		}},
	}, true)

	if req["stream"] != true {
		t.Fatalf("stream = %#v, want true", req["stream"])
	}
	messages := req["messages"].([]map[string]any)
	if messages[0]["reasoning_content"] != "reason" {
		t.Fatalf("reasoning_content = %#v, want reason", messages[0]["reasoning_content"])
	}
	if _, ok := messages[0]["cache_control"]; !ok {
		t.Fatalf("cache_control message extension was not preserved: %#v", messages[0])
	}
	options := req["options"].(map[string]any)
	if options["temperature"] != temp {
		t.Fatalf("temperature = %#v, want %v", options["temperature"], temp)
	}
	if options["top_p"] != topP {
		t.Fatalf("top_p = %#v, want %v", options["top_p"], topP)
	}
	if options["top_k"] != topK {
		t.Fatalf("top_k = %#v, want %v", options["top_k"], topK)
	}
	if options["num_predict"] != maxTokens {
		t.Fatalf("num_predict = %#v, want %v", options["num_predict"], maxTokens)
	}
	if _, leaked := options["max_tokens"]; leaked {
		t.Fatalf("OpenAI max_tokens leaked into native Ollama options: %#v", options)
	}
	if options["num_ctx"] != contextLength {
		t.Fatalf("num_ctx = %#v, want %v", options["num_ctx"], contextLength)
	}
	if options["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", options["reasoning_effort"])
	}
	if options["num_keep"] != float64(24) {
		t.Fatalf("num_keep = %#v, want 24", options["num_keep"])
	}
	if _, ok := options["custom_option"]; !ok {
		t.Fatalf("custom_option was not preserved: %#v", options)
	}
}

func TestOllamaProviderPreservesGenericToolControlFields(t *testing.T) {
	prov := NewOllamaProvider("ollama", "http://localhost:11434", true, time.Second)
	req := prov.buildChatRequest(&ChatRequest{
		Model: "qwen3",
		Extra: map[string]json.RawMessage{
			"functions":           []byte(`[{"name":"legacy_tool","description":"legacy","parameters":{"type":"object"}}]`),
			"function_call":       []byte(`{"name":"legacy_tool"}`),
			"tool_choice":         []byte(`{"type":"function","function":{"name":"legacy_tool"}}`),
			"parallel_tool_calls": []byte(`true`),
		},
		Stop: []string{"END"},
	}, false)

	tools, ok := req["tools"].([]Tool)
	if !ok || len(tools) != 1 || tools[0].Function.Name != "legacy_tool" {
		t.Fatalf("legacy functions were not converted to tools: %#v", req["tools"])
	}
	if got, ok := req["tool_choice"].(map[string]any); !ok || got["type"] != "function" {
		t.Fatalf("tool_choice was not preserved: %#v", req["tool_choice"])
	}
	if got, ok := req["parallel_tool_calls"].(bool); !ok || !got {
		t.Fatalf("parallel_tool_calls was not preserved: %#v", req["parallel_tool_calls"])
	}
	if got, ok := req["function_call"].(map[string]any); !ok || got["name"] != "legacy_tool" {
		t.Fatalf("function_call was not preserved: %#v", req["function_call"])
	}
	options, ok := req["options"].(map[string]any)
	if !ok {
		t.Fatalf("Ollama options were not created: %#v", req["options"])
	}
	if got, ok := options["stop"].([]string); !ok || len(got) != 1 || got[0] != "END" {
		t.Fatalf("stop was not mapped to options.stop: %#v", options["stop"])
	}
	if _, leaked := req["stop"]; leaked {
		t.Fatalf("OpenAI stop leaked to unsupported Ollama top-level field: %#v", req)
	}
}

func TestToolProtocolContractOllamaChatPreservesToolCalls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"qwen3",
			"message":{"role":"assistant","content":"","tool_calls":[{
				"id":"call_1",
				"type":"function",
				"function":{"name":"read_file","arguments":{"path":"a.txt"}}
			}]},
			"done":true,
			"done_reason":"tool_calls"
		}`))
	}))
	defer upstream.Close()

	prov := NewOllamaProvider("ollama", upstream.URL, true, time.Second)
	resp, err := prov.Chat(context.Background(), &ChatRequest{
		Model:    "qwen3",
		Messages: []Message{{Role: "user", Content: "read a.txt"}},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %#v, want one choice", resp.Choices)
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %#v, want one call", choice.Message.ToolCalls)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "read_file" {
		t.Fatalf("unexpected tool call: %#v", call)
	}
	if call.Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %q, want JSON object string", call.Function.Arguments)
	}
}

func TestOllamaProviderChatInfersToolFinishWhenDoneReasonIsStop(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"qwen3",
			"message":{"role":"assistant","content":"","tool_calls":[{
				"id":"call_1","type":"function",
				"function":{"name":"read_file","arguments":{"path":"a.txt"}}
			}]},
			"done":true,
			"done_reason":"stop"
		}`))
	}))
	defer upstream.Close()

	prov := NewOllamaProvider("ollama", upstream.URL, true, time.Second)
	resp, err := prov.Chat(context.Background(), &ChatRequest{Model: "qwen3"})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("Ollama tool finish was not repaired: %#v", choice)
	}
}

func TestOllamaProviderChatRejectsIncompleteToolArguments(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"qwen3",
			"message":{"role":"assistant","content":"","tool_calls":[{
				"id":"call_1","type":"function",
				"function":{"name":"read_file","arguments":"{\"path\":"}
			}]},
			"done":true,
			"done_reason":"stop"
		}`))
	}))
	defer upstream.Close()

	prov := NewOllamaProvider("ollama", upstream.URL, true, time.Second)
	_, err := prov.Chat(context.Background(), &ChatRequest{Model: "qwen3"})
	if err == nil {
		t.Fatal("Chat should reject incomplete Ollama tool arguments")
	}
}

func TestOllamaProviderForwardsRequestIDHeaders(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat_raw", true: "chat_stream"}[stream], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/chat" {
					t.Fatalf("unexpected path: %s", r.URL.Path)
				}
				if got := r.Header.Get("X-Request-ID"); got != "req-ollama-1" {
					t.Fatalf("X-Request-ID = %q, want req-ollama-1", got)
				}
				if got := r.Header.Get("X-Proxy-Request-ID"); got != "req-ollama-1" {
					t.Fatalf("X-Proxy-Request-ID = %q, want req-ollama-1", got)
				}
				w.Header().Set("Content-Type", "application/json")
				if stream {
					_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"ok"},"done":true}`))
					return
				}
				_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"ok"},"done":true}`))
			}))
			defer upstream.Close()

			prov := NewOllamaProvider("ollama", upstream.URL, true, time.Second)
			ctx := requestmeta.ContextWithRequestID(context.Background(), "req-ollama-1")
			req := &ChatRequest{Model: "llama", Messages: []Message{{Role: "user", Content: "hi"}}}
			if stream {
				body, err := prov.ChatStream(ctx, req)
				if err != nil {
					t.Fatalf("ChatStream returned error: %v", err)
				}
				_ = body.Close()
				return
			}
			if _, err := prov.ChatRaw(ctx, req); err != nil {
				t.Fatalf("ChatRaw returned error: %v", err)
			}
		})
	}
}

func TestOllamaProviderConvertsOpenAIMultimodalContentToImages(t *testing.T) {
	prov := NewOllamaProvider("ollama", "http://localhost:11434", true, time.Second)
	rawContent := json.RawMessage(`[
		{"type":"text","text":"Describe this"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}
	]`)

	req := prov.buildChatRequest(&ChatRequest{
		Model: "llava",
		Messages: []Message{{
			Role:       "user",
			ContentRaw: rawContent,
		}},
	}, false)

	messages := req["messages"].([]map[string]any)
	if messages[0]["content"] != "Describe this" {
		t.Fatalf("content = %#v, want Describe this", messages[0]["content"])
	}
	images, ok := messages[0]["images"].([]string)
	if !ok || len(images) != 1 {
		t.Fatalf("images = %#v, want one image", messages[0]["images"])
	}
	if images[0] != "data:image/png;base64,AA==" {
		t.Fatalf("image = %q, want data url", images[0])
	}
}
