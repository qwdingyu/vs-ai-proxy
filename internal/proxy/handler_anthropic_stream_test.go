package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// anthropic 类型 provider 的 /v1/chat/completions 流式回归测试
//
// 背景（v0.2.76 之前的真实缺陷）：
// Visual Studio Copilot 固定使用 stream=true。它是 *OpenAIProvider，而
// handleChatCompletions 的流式分支在 anthropic 判断之前就 return，因此
//   1) 上游收到 OpenAI 线格式请求体（无 system 顶层字段、无 anthropic-version）；
//   2) 上游的 Anthropic SSE 被原样透传给 VS（没有 choices、没有 [DONE]）。
// 管理测试页因为固定 stream=false 而走了正确分支，所以表现为“测试页可用、
// VS Copilot 不可用”。
//
// 这些用例锁定修复后的契约，防止再次退化。
// ---------------------------------------------------------------------------

// anthropicStreamTestUpstream 提供一个 Anthropic Messages 端点，
// 记录收到的请求，并回放一段真实的 event-based SSE。
func anthropicStreamTestUpstream(t *testing.T, capturedHeader *http.Header, capturedBody *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			// 供 registry 模型发现使用
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		*capturedBody = string(buf[:n])
		*capturedHeader = r.Header.Clone()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\",\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
}

func newAnthropicStreamTestServer(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	prov := provider.NewOpenAIProviderWithTransport(
		"longcat2", "openai", "sk-test", upstreamURL,
		"v1/messages", "v1/models", true, 5*time.Second,
	)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{{
		ID: "longcat2", Name: "longcat2", Type: "anthropic",
		BaseURL: upstreamURL, APIKey: "sk-test", Enabled: true,
		Transport: config.TransportConfig{ChatPath: "v1/messages", ModelsPath: "v1/models"},
	}}
	inner := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})
	return server.loggingMiddleware(inner)
}

// TestHandleChatCompletions_AnthropicProviderStreamEmitsOpenAISSE 是本次缺陷的
// 主回归用例：VS Copilot 的 stream=true 必须拿到 OpenAI SSE 契约。
func TestHandleChatCompletions_AnthropicProviderStreamEmitsOpenAISSE(t *testing.T) {
	var header http.Header
	var body string
	upstream := anthropicStreamTestUpstream(t, &header, &body)
	defer upstream.Close()

	handler := newAnthropicStreamTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":true,"messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()

	if !strings.Contains(out, "[DONE]") {
		t.Errorf("下游缺少 [DONE] 终态，Visual Studio 会挂起: %s", out)
	}
	if !strings.Contains(out, "chat.completion.chunk") {
		t.Errorf("下游缺少 OpenAI chunk 结构: %s", out)
	}
	if !strings.Contains(out, "Hello") {
		t.Errorf("下游缺少聚合后的文本内容: %s", out)
	}
	// Anthropic 原生事件名绝不能泄漏到下游。
	for _, leaked := range []string{"content_block_delta", "message_start", "content_block_start"} {
		if strings.Contains(out, leaked) {
			t.Errorf("Anthropic SSE 事件 %q 泄漏到下游: %s", leaked, out)
		}
	}
}

// TestHandleChatCompletions_AnthropicProviderStreamSendsAnthropicRequest 锁定上游
// 请求必须是真正的 Anthropic Messages 线格式。
func TestHandleChatCompletions_AnthropicProviderStreamSendsAnthropicRequest(t *testing.T) {
	var header http.Header
	var body string
	upstream := anthropicStreamTestUpstream(t, &header, &body)
	defer upstream.Close()

	handler := newAnthropicStreamTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":true,"messages":[{"role":"system","content":"SYS"},{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var sent map[string]any
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("上游请求不是 JSON: %v (body=%q)", err, body)
	}
	if _, ok := sent["system"]; !ok {
		t.Errorf("上游请求缺少 Anthropic 顶层 system 字段: %s", body)
	}
	if sent["max_tokens"] == nil {
		t.Errorf("上游请求缺少 Anthropic 必需的 max_tokens: %s", body)
	}
	if got := header.Get("anthropic-version"); got == "" {
		t.Errorf("上游请求缺少 anthropic-version 头")
	}
	if got := header.Get("x-api-key"); got != "sk-test" {
		t.Errorf("x-api-key = %q, want sk-test（必须来自 provider 配置）", got)
	}
}

// TestHandleChatCompletions_AnthropicProviderStreamToolUse 覆盖流式工具调用，
// 这是 VS Copilot 最关键的场景。
func TestHandleChatCompletions_AnthropicProviderStreamToolUse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_t\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\",\"input\":{}}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"Paris\\\"}\"}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":9}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	handler := newAnthropicStreamTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":true,"messages":[{"role":"user","content":"weather?"}],`+
			`"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "get_weather") {
		t.Errorf("下游缺少工具名: %s", out)
	}
	if !strings.Contains(out, "tool_calls") {
		t.Errorf("下游缺少 tool_calls: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("下游缺少 [DONE]: %s", out)
	}
	// 工具“参数”必须完整传递。只断言工具名会漏掉 input_json_delta 聚合失败的场景，
	// 而 VS Copilot 恰恰依赖 arguments 才能执行工具，所以这里必须校验参数。
	if !strings.Contains(out, "Paris") {
		t.Errorf("工具参数丢失（应包含 city=Paris）: %s", out)
	}
	// finish_reason 必须为 tool_calls，否则 VS 不会发起工具执行。
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason 应为 tool_calls: %s", out)
	}
}

// TestHandleChatCompletions_AnthropicProviderStreamEmptyUpstreamFailsClosed
// 空上游流必须失败关闭，不能返回一个没有终态的 200（否则 VS 会挂起）。
func TestHandleChatCompletions_AnthropicProviderStreamEmptyUpstreamFailsClosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler := newAnthropicStreamTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("返回了没有终态的 200，Visual Studio 会挂起: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// anthropicStreamAccumulator 单元测试
// ---------------------------------------------------------------------------

func TestAnthropicStreamAccumulator_InterleavedToolBlocks(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	for _, e := range []string{
		`{"type":"message_start","message":{"id":"m","role":"assistant","model":"x","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"alpha","input":{}}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t2","name":"beta","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"b\":2}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
	} {
		if err := acc.consume(e); err != nil {
			t.Fatalf("consume(%s): %v", e, err)
		}
	}
	tc := acc.response().Choices[0].Message.ToolCalls
	if len(tc) != 2 {
		t.Fatalf("tool calls = %d, want 2: %+v", len(tc), tc)
	}
	if tc[0].Function.Name != "alpha" || tc[0].Function.Arguments != `{"a":1}` {
		t.Errorf("tool0 = %s / %s", tc[0].Function.Name, tc[0].Function.Arguments)
	}
	if tc[1].Function.Name != "beta" || tc[1].Function.Arguments != `{"b":2}` {
		t.Errorf("tool1 = %s / %s", tc[1].Function.Name, tc[1].Function.Arguments)
	}
}

func TestAnthropicStreamAccumulator_IgnoresMalformedAndUnknownEvents(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	for _, e := range []string{
		`not json`,
		`{"type":"ping"}`,
		`{"type":"some_future_event","payload":{"x":1}}`,
		`{"type":"content_block_delta","index":99,"delta":{"type":"text_delta","text":"ignored"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
	} {
		if err := acc.consume(e); err != nil {
			t.Fatalf("必须容忍未知/畸形事件 %s: %v", e, err)
		}
	}
	if got := acc.response().Choices[0].Message.Content; got != "ok" {
		t.Errorf("content = %q, want ok", got)
	}
}

func TestAnthropicStreamAccumulator_ToolUseWithoutInputDelta(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	for _, e := range []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"noargs","input":{}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
	} {
		_ = acc.consume(e)
	}
	tc := acc.response().Choices[0].Message.ToolCalls
	if len(tc) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(tc))
	}
	if !json.Valid([]byte(tc[0].Function.Arguments)) {
		t.Errorf("arguments 不是合法 JSON: %q", tc[0].Function.Arguments)
	}
}

func TestAnthropicStreamAccumulator_ThinkingMapsToReasoning(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	for _, e := range []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","text":"pondering"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
	} {
		_ = acc.consume(e)
	}
	msg := acc.response().Choices[0].Message
	if msg.Content != "answer" {
		t.Errorf("content = %q, want answer", msg.Content)
	}
	if msg.Reasoning != "pondering" {
		t.Errorf("reasoning = %q, want pondering", msg.Reasoning)
	}
}

func TestAnthropicStreamAccumulator_UsageKeepsInputTokens(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	_ = acc.consume(`{"type":"message_start","message":{"id":"m","role":"assistant","model":"x","usage":{"input_tokens":42,"output_tokens":0}}}`)
	_ = acc.consume(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`)
	usage := acc.response().Usage
	if usage == nil {
		t.Fatal("usage 为 nil")
	}
	if usage.PromptTokens != 42 || usage.CompletionTokens != 9 || usage.TotalTokens != 51 {
		t.Errorf("usage = %d/%d/%d, want 42/9/51", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	}
}
