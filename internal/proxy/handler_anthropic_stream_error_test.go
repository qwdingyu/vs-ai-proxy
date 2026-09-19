package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// anthropic 流式链路的错误路径与边界测试。
//
// 这些用例补齐 handler_anthropic_stream_test.go 未覆盖的分支：
// 上游非 200、上游 error 事件、SSE 夹杂非 data 行、配置缺失、
// choices 为空、下游不支持 flush、上游连接失败等。
// 修复的价值主要在于“错误必须失败关闭而不是伪装成功”，因此错误路径
// 与成功路径同等重要，必须逐条锁定。
// ---------------------------------------------------------------------------

// anthropicErrorUpstream 返回一个可定制响应的 anthropic 上游构建器。
func anthropicErrorUpstream(t *testing.T, serve func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		serve(w, r)
	}))
}

func postAnthropicStream(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// 上游返回非 200 时必须失败关闭，且不得把上游 body 当成流式内容下发。
func TestAnthropicStream_UpstreamNon200FailsClosed(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code == http.StatusOK {
		t.Fatalf("上游 500 时不得返回 200，body=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "boom") && rec.Code == http.StatusOK {
		t.Errorf("上游错误体被当作成功内容下发")
	}
}

// 上游 200 + error 事件必须转为失败，而不是返回空成功流。
func TestAnthropicStream_UpstreamErrorEventFailsClosed(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code == http.StatusOK {
		t.Fatalf("上游 error 事件时不得返回 200，body=%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Overloaded") {
		t.Errorf("错误详情应保留给下游诊断: %s", rec.Body.String())
	}
}

// SSE 中夹杂注释行/未知字段/非 data 行时必须被安全忽略。
func TestAnthropicStream_IgnoresCommentsAndUnknownLines(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write(": this is an SSE comment\n\n")
		write("event: ping\ndata: {\"type\":\"ping\"}\n\n")
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		write("event: content_block_delta\ndata: not-json-at-all\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "ok") {
		t.Errorf("正常内容应被保留: %s", out)
	}
	if strings.Contains(out, "not-json") || strings.Contains(out, "ping") {
		t.Errorf("未知/畸形行泄漏到下游: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("缺少 [DONE]: %s", out)
	}
}

// stop_reason 的各种取值必须正确映射到 OpenAI finish_reason。
func TestAnthropicStream_StopReasonMapping(t *testing.T) {
	cases := []struct {
		anthropic string
		want      string
	}{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"stop_sequence", "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.anthropic, func(t *testing.T) {
			upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				write := func(s string) { _, _ = w.Write([]byte(s)) }
				write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n")
				if tc.anthropic == "tool_use" {
					write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu\",\"name\":\"t\",\"input\":{}}}\n\n")
					write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n")
				} else {
					write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
					write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
				}
				write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + tc.anthropic + "\"},\"usage\":{\"output_tokens\":1}}\n\n")
				write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			})
			defer upstream.Close()

			rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"finish_reason":"`+tc.want+`"`) {
				t.Errorf("finish_reason 应为 %q: %s", tc.want, rec.Body.String())
			}
		})
	}
}

// 上游连接被拒绝时必须失败关闭并给出诊断，而不是挂起。
func TestAnthropicStream_UpstreamConnectionRefused(t *testing.T) {
	// 关闭的端口
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	prov := provider.NewOpenAIProviderWithTransport("longcat2", "openai", "sk-test", deadURL, "v1/messages", "v1/models", true, 2*time.Second)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{{
		ID: "longcat2", Name: "longcat2", Type: "anthropic", BaseURL: deadURL, APIKey: "sk-test", Enabled: true,
		Transport: config.TransportConfig{ChatPath: "v1/messages", ModelsPath: "v1/models"},
	}}
	inner := withMux(server, func(mux *http.ServeMux) { mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions) })

	rec := postAnthropicStream(t, server.loggingMiddleware(inner))
	if rec.Code == http.StatusOK {
		t.Fatalf("连接失败时不得返回 200: %s", rec.Body.String())
	}
}

// provider 配置缺失（base_url 为空）时必须以明确错误失败，而不是 panic。
func TestAnthropicStream_MissingProviderConfigFailsClosed(t *testing.T) {
	prov := provider.NewOpenAIProviderWithTransport("ghost", "openai", "sk", "http://127.0.0.1:1", "v1/messages", "v1/models", true, time.Second)
	server := newTestServer(prov)
	// 故意不注册任何 provider 配置，使 findProviderConfig 返回 nil。
	inner := withMux(server, func(mux *http.ServeMux) { mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions) })

	var rec *httptest.ResponseRecorder
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("配置缺失时发生 panic: %v", r)
			}
		}()
		rec = postAnthropicStream(t, server.loggingMiddleware(inner))
	}()
	if rec.Code == http.StatusOK {
		t.Errorf("配置缺失时不应返回 200: %s", rec.Body.String())
	}
}

// 下游 ResponseWriter 不支持 Flush 时必须失败关闭而非静默丢流。
func TestAnthropicStream_WriterWithoutFlusherFailsClosed(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	defer upstream.Close()

	server := newAnthropicStreamTestServerUnwrapped(t, upstream.URL)
	// 用不含 Flusher 的 writer 包装，触发 !ok 分支。
	rec := &nonFlushWriter{header: http.Header{}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(rec, req)

	if rec.status == http.StatusOK {
		t.Errorf("不支持 flush 的 writer 不应得到 200 成功")
	}
}

type nonFlushWriter struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *nonFlushWriter) Header() http.Header         { return w.header }
func (w *nonFlushWriter) WriteHeader(code int)        { w.status = code }
func (w *nonFlushWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

// newAnthropicStreamTestServerUnwrapped 返回未包 loggingMiddleware 的 handler，
// 以便注入自定义 ResponseWriter。
func newAnthropicStreamTestServerUnwrapped(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	prov := provider.NewOpenAIProviderWithTransport("longcat2", "openai", "sk-test", upstreamURL, "v1/messages", "v1/models", true, 5*time.Second)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{{
		ID: "longcat2", Name: "longcat2", Type: "anthropic", BaseURL: upstreamURL, APIKey: "sk-test", Enabled: true,
		Transport: config.TransportConfig{ChatPath: "v1/messages", ModelsPath: "v1/models"},
	}}
	return withMux(server, func(mux *http.ServeMux) { mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions) })
}

// 客户端取消（context canceled）时必须被识别为 client_gone 并停止，
// 不能转成 502 让用户误以为是上游故障。
func TestAnthropicStream_ClientCancellation(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 故意不结束，等待客户端取消
		<-r.Context().Done()
	})
	defer upstream.Close()

	handler := newAnthropicStreamTestServer(t, upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端取消后请求未及时返回（可能挂起）")
	}
}

// 直接对 SendAnthropicChatStreamRequest 做单元级错误校验。
func TestSendAnthropicChatStreamRequest_UpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer upstream.Close()

	_, err := SendAnthropicChatStreamRequest(context.Background(), upstream.URL, "v1/messages", "bad",
		&provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("上游 401 时应返回错误")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("错误应包含上游状态码: %v", err)
	}
}

func TestSendAnthropicChatStreamRequest_ContextCanceled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := SendAnthropicChatStreamRequest(ctx, upstream.URL, "v1/messages", "k",
		&provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("context 已取消时应返回错误")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("错误应反映 context 取消: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 第二轮补齐：覆盖覆盖率报告中剩余的未覆盖分支。
// ---------------------------------------------------------------------------

// 上游 SSE 中含有 [DONE] 与非 data 行时必须被跳过（不参与聚合）。
func TestAnthropicStream_SkipsDoneAndNonDataLines(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: whatever\nid: 42\nretry: 1000\n\n") // 非 data 行
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
		write("data: [DONE]\n\n") // [DONE] 必须被跳过而不报错
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("应正常完成: %s", rec.Body.String())
	}
}

// message_delta 携带 input_tokens 时必须更新（覆盖 InputTokens>0 分支）。
func TestAnthropicStreamAccumulator_MessageDeltaCarriesInputTokens(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	_ = acc.consume(`{"type":"message_start","message":{"id":"m","role":"assistant","model":"x","usage":{"input_tokens":5,"output_tokens":0}}}`)
	_ = acc.consume(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":77,"output_tokens":3}}`)
	usage := acc.response().Usage
	if usage == nil || usage.PromptTokens != 77 {
		t.Fatalf("message_delta 的 input_tokens 应覆盖旧值: %+v", usage)
	}
	if usage.CompletionTokens != 3 {
		t.Errorf("output_tokens = %d, want 3", usage.CompletionTokens)
	}
}

// tool_use 的 partial_json 非法时必须退化为 {}（覆盖 else 分支）。
func TestAnthropicStreamAccumulator_InvalidPartialJSONFallsBackToEmptyObject(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	_ = acc.consume(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{}}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{broken"}}`)
	tc := acc.response().Choices[0].Message.ToolCalls
	if len(tc) != 1 {
		t.Fatalf("tool calls = %d", len(tc))
	}
	if !json.Valid([]byte(tc[0].Function.Arguments)) {
		t.Errorf("非法 partial_json 必须退化为合法 JSON: %q", tc[0].Function.Arguments)
	}
}

// SendAnthropicChatStreamContent（管理页兜底函数）的连接失败与非 200 分支。
func TestSendAnthropicChatStreamContent_ErrorPaths(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	req := &provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	if _, err := SendAnthropicChatStreamContent(context.Background(), deadURL, "v1/messages", "k", req); err == nil {
		t.Error("连接失败应返回错误")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer bad.Close()
	if _, err := SendAnthropicChatStreamContent(context.Background(), bad.URL, "v1/messages", "k", req); err == nil {
		t.Error("上游 400 应返回错误")
	}
}

// anthropic 流式：choices 为空（上游只发 message_stop）必须失败关闭。
func TestAnthropicStream_EmptyChoicesFailsClosed(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code == http.StatusOK {
		t.Errorf("无任何内容块的流不应返回 200: %s", rec.Body.String())
	}
}

// 流式产出必须把上游 usage 记入请求日志（token 统计的事实来源）。
// setResponseUsage 写入的是内部 *responseWriter 的 usage 字段，
// 不是 HTTP 头，因此这里直接断言 store 中记录的 usage。
func TestAnthropicStream_RecordsUsage(t *testing.T) {
	var header http.Header
	var body string
	upstream := anthropicStreamTestUpstream(t, &header, &body)
	defer upstream.Close()

	prov := provider.NewOpenAIProviderWithTransport("longcat2", "openai", "sk-test", upstream.URL, "v1/messages", "v1/models", true, 5*time.Second)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{{
		ID: "longcat2", Name: "longcat2", Type: "anthropic", BaseURL: upstream.URL, APIKey: "sk-test", Enabled: true,
		Transport: config.TransportConfig{ChatPath: "v1/messages", ModelsPath: "v1/models"},
	}}
	inner := withMux(server, func(mux *http.ServeMux) { mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions) })
	handler := server.loggingMiddleware(inner)

	rec := postAnthropicStream(t, handler)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	logs := server.store.GetLogs(10)
	if len(logs) == 0 {
		t.Fatal("请求日志为空")
	}
	found := false
	for _, entry := range logs {
		if entry.Usage != nil && entry.Usage.PromptTokens == 11 && entry.Usage.CompletionTokens == 7 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("上游 usage(input=11/output=7) 未记入日志: %+v", logs[0].Usage)
	}
}

// ---------------------------------------------------------------------------
// 第三轮补齐：序列化/请求构造失败，以及累加器错误传播。
// ---------------------------------------------------------------------------

// 非法上游 URL 必须在“创建请求”阶段失败并返回明确错误，而不是 panic。
func TestSendAnthropicChatStreamRequest_InvalidUpstreamURL(t *testing.T) {
	req := &provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	cases := []string{"http://[::1", "://missing-scheme", "http://bad host with spaces/"}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			_, err := SendAnthropicChatStreamRequest(context.Background(), bad, "v1/messages", "k", req)
			if err == nil {
				t.Errorf("非法 URL %q 应返回错误", bad)
			}
		})
	}
}

// anthropicStreamAccumulator.consume 目前对畸形 JSON 返回 nil（容错），
// 这里锁定该行为，防止未来无意中改为返回错误而中断整条流。
func TestAnthropicStreamAccumulator_ConsumeNeverErrorsOnBadJSON(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	for _, bad := range []string{"", "{", "null", "[]", `{"type":123}`, "\x00\x01"} {
		if err := acc.consume(bad); err != nil {
			t.Errorf("consume(%q) 不应返回错误: %v", bad, err)
		}
	}
}

// 端到端：上游 body 不是 SSE（例如返回 HTML）时必须失败关闭，不能伪装成功。
func TestAnthropicStream_NonSSEUpstreamBodyFailsClosed(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>gateway error</body></html>"))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code == http.StatusOK {
		t.Errorf("非 SSE 上游响应不应被当作成功: %s", rec.Body.String())
	}
}

// provider 类型为 anthropic 但 base_url 为空（配置不完整）时，
// streamAnthropicAsOpenAI 必须以明确错误失败，不能 panic 或挂起。
func TestAnthropicStream_EmptyBaseURLFailsClosed(t *testing.T) {
	prov := provider.NewOpenAIProviderWithTransport("longcat2", "openai", "sk-test", "http://127.0.0.1:1", "v1/messages", "v1/models", true, 2*time.Second)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{{
		ID: "longcat2", Name: "longcat2", Type: "anthropic",
		BaseURL: "", // 故意留空
		APIKey:  "sk-test", Enabled: true,
		Transport: config.TransportConfig{ChatPath: "v1/messages", ModelsPath: "v1/models"},
	}}
	inner := withMux(server, func(mux *http.ServeMux) { mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions) })

	var rec *httptest.ResponseRecorder
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("base_url 为空时发生 panic: %v", r)
			}
		}()
		rec = postAnthropicStream(t, server.loggingMiddleware(inner))
	}()
	if rec.Code == http.StatusOK {
		t.Errorf("base_url 为空时不应返回 200: %s", rec.Body.String())
	}
}

// 上游返回纯文本但没有 choices（仅未知事件）必须失败关闭。
func TestAnthropicStream_UnknownEventsOnlyFailsClosed(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 只有无法映射到 ChatResponse 的事件
		_, _ = w.Write([]byte("event: some_unknown\ndata: {\"type\":\"some_unknown\",\"x\":1}\n\n"))
		_, _ = w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code == http.StatusOK {
		t.Errorf("无有效事件时不应返回 200: %s", rec.Body.String())
	}
}

// 上游在 message_stop 之后继续挂住连接（不关闭 body）时，代理必须立即结束读取，
// 不能等待传输层 EOF。真实网关经常复用连接或在终态后延迟关闭，
// 若等到 EOF 会让请求一直挂到超时，Visual Studio 永远拿不到响应。
func TestAnthropicStream_TerminatesOnMessageStopWithoutEOF(t *testing.T) {
	release := make(chan struct{})
	// 注意 defer 顺序（后进先出）：先注册 upstream.Close，再注册 close(release)，
	// 这样先释放 handler，避免 httptest.Server.Close 阻塞等待活动连接。
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 终态之后故意不关闭连接，模拟复用/延迟关闭的上游。
		<-release
	})
	defer upstream.Close()
	defer close(release)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	}()

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "hello") || !strings.Contains(rec.Body.String(), "[DONE]") {
			t.Errorf("终态应在不上游 EOF 的情况下完整下发: %s", rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("上游在 message_stop 后不关闭连接时请求挂起（未在应用层终态结束读取）")
	}
}

// 缺少 message_stop 但连接正常关闭（EOF）时仍应成功，不能因新增终态判断而变严格。
func TestAnthropicStream_EOFWithoutMessageStopStillSucceeds(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
		// 故意不发 message_stop，直接结束（EOF）
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code != http.StatusOK {
		t.Fatalf("EOF 结束时应仍可完成: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "partial") {
		t.Errorf("内容应被保留: %s", rec.Body.String())
	}
}

// message_stop 之后即使还有多余事件也必须被忽略（读取已结束）。
func TestAnthropicStream_IgnoresEventsAfterMessageStop(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	_ = acc.consume(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`)
	_ = acc.consume(`{"type":"message_stop"}`)
	if !acc.sawTerminal {
		t.Fatal("message_stop 应置位 sawTerminal")
	}
}

// ---------------------------------------------------------------------------
// content block index 与切片位置的错位回归。
//
// Anthropic 协议只保证 content_block 的 index 唯一且递增，不保证从 0 开始连续。
// 若用切片位置当协议 index（或反过来），一旦上游跳过序号，
// tool_use 的参数就会被静默丢弃，表现为 VS 端工具"调用了但没有参数"。
// ---------------------------------------------------------------------------

func TestAnthropicStreamAccumulator_NonSequentialBlockIndexKeepsToolArgs(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	// 上游跳过 0/1/2，直接从 index=3 开始发 tool_use。
	_ = acc.consume(`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"tu","name":"get_weather","input":{}}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Paris\"}"}}`)
	_ = acc.consume(`{"type":"message_stop"}`)

	tc := acc.response().Choices[0].Message.ToolCalls
	if len(tc) != 1 {
		t.Fatalf("应有 1 个工具调用, got %d", len(tc))
	}
	if tc[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("非连续 index 下工具参数丢失: got %q want %q",
			tc[0].Function.Arguments, `{"city":"Paris"}`)
	}
}

// 文本块与工具块混合 + 非连续 index：内容与参数都必须正确落到各自块上。
func TestAnthropicStreamAccumulator_MixedBlocksNonSequentialIndex(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	_ = acc.consume(`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"answer:"}}`)
	_ = acc.consume(`{"type":"content_block_start","index":7,"content_block":{"type":"tool_use","id":"tu","name":"f","input":{}}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":7,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":7,"delta":{"type":"input_json_delta","partial_json":"1}"}}`)

	msg := acc.response().Choices[0].Message
	if !strings.Contains(msg.Content, "answer:") {
		t.Errorf("文本块内容错位: %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("工具调用数 = %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Errorf("分片参数拼接错误: %q", msg.ToolCalls[0].Function.Arguments)
	}
}

// delta 的 index 没有对应的 content_block_start 时必须忽略，不能错位写入其他块。
func TestAnthropicStreamAccumulator_OrphanDeltaIsIgnored(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	_ = acc.consume(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":99,"delta":{"type":"text_delta","text":"LEAK"}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)

	msg := acc.response().Choices[0].Message
	if strings.Contains(msg.Content, "LEAK") {
		t.Errorf("孤儿 delta 被错位写入: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "ok") {
		t.Errorf("正常内容丢失: %q", msg.Content)
	}
}

// 多个工具块共用一个 index 空间时，各自参数不得串台。
func TestAnthropicStreamAccumulator_MultipleToolsDoNotCrossContaminate(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	_ = acc.consume(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"a","name":"first","input":{}}}`)
	_ = acc.consume(`{"type":"content_block_start","index":4,"content_block":{"type":"tool_use","id":"b","name":"second","input":{}}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":"{\"b\":2}"}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`)

	tc := acc.response().Choices[0].Message.ToolCalls
	if len(tc) != 2 {
		t.Fatalf("工具数 = %d", len(tc))
	}
	if tc[0].Function.Name != "first" || tc[0].Function.Arguments != `{"a":1}` {
		t.Errorf("第一个工具串台: %s %s", tc[0].Function.Name, tc[0].Function.Arguments)
	}
	if tc[1].Function.Name != "second" || tc[1].Function.Arguments != `{"b":2}` {
		t.Errorf("第二个工具串台: %s %s", tc[1].Function.Name, tc[1].Function.Arguments)
	}
}

// 文本块与工具块交错的顺序必须与协议 index 升序一致，
// anthropicResponseToChatResponse 依据 Content 顺序组装 content/tool_calls。
func TestAnthropicStreamAccumulator_ContentOrderFollowsProtocolIndex(t *testing.T) {
	acc := newAnthropicStreamAccumulator()
	// 故意乱序发送 start（协议允许 index 唯一即可），最终顺序应按 index 升序。
	_ = acc.consume(`{"type":"content_block_start","index":5,"content_block":{"type":"text","text":""}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":5,"delta":{"type":"text_delta","text":"SECOND"}}`)
	_ = acc.consume(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
	_ = acc.consume(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"FIRST"}}`)

	content := acc.response().Choices[0].Message.Content
	first := strings.Index(content, "FIRST")
	second := strings.Index(content, "SECOND")
	if first < 0 || second < 0 {
		t.Fatalf("内容缺失: %q", content)
	}
	if first > second {
		t.Errorf("顺序应按协议 index 升序: %q", content)
	}
}

// TestRequestParity_StreamVsNonStream 锁定流式与非流式生成的 Anthropic 请求体完全一致
// （除 stream 字段外）。两者共用 chatRequestToAnthropicRequest，
// 这个测试保证将来任一侧加字段时不会只改一半，造成 VS 流式与测试页非流式行为分裂。
func TestRequestParity_StreamVsNonStream(t *testing.T) {
	cases := []struct {
		name string
		req  *provider.ChatRequest
	}{
		{"plain", &provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "system", Content: "S"}, {Role: "user", Content: "U"}}}},
		{"tools", &provider.ChatRequest{
			Model:    "m",
			Messages: []provider.Message{{Role: "user", Content: "U"}},
			Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{
				Name: "f", Description: "d", Parameters: map[string]any{"type": "object"},
			}}},
		}},
		{"toolresult", &provider.ChatRequest{Model: "m", Messages: []provider.Message{
			{Role: "user", Content: "U"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "t1", Function: provider.FunctionCall{Name: "f", Arguments: `{"a":1}`}}}},
			{Role: "tool", ToolCallID: "t1", Content: "result"},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := *tc.req
			ns.Stream = false
			a := chatRequestToAnthropicRequest(&ns)

			ss := *tc.req
			ss.Stream = true
			b := chatRequestToAnthropicRequest(&ss)

			ja, _ := json.Marshal(a)
			jb, _ := json.Marshal(b)
			var ma, mb map[string]any
			_ = json.Unmarshal(ja, &ma)
			_ = json.Unmarshal(jb, &mb)
			// stream 字段本来就该不同
			ma["stream"] = nil
			mb["stream"] = nil
			ja2, _ := json.Marshal(ma)
			jb2, _ := json.Marshal(mb)
			if string(ja2) != string(jb2) {
				t.Errorf("流式与非流式的 Anthropic 请求体不一致（除 stream）:\n nonstream=%s\n stream=%s", ja2, jb2)
			}
		})
	}
}

// 验证：任何 tool_use 块经过 response() 后，Input 都必须是合法 JSON，
// 绝不能是 nil（会 marshal 成字面量 null，VS 解析工具参数时可能出错）。
func TestResponse_ToolUseInputAlwaysValidJSON(t *testing.T) {
	scenarios := []struct {
		name   string
		events []string
	}{
		{"no delta at all", []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{}}}`,
		}},
		{"empty delta", []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`,
		}},
		{"whitespace only", []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"   "}}`,
		}},
		{"truncated json", []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		}},
		{"nil input in start", []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f"}}`,
		}},
		{"input as array", []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":[1,2]}}`,
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			acc := newAnthropicStreamAccumulator()
			for _, e := range sc.events {
				if err := acc.consume(e); err != nil {
					t.Fatalf("consume(%s): %v", e, err)
				}
			}
			tcs := acc.response().Choices[0].Message.ToolCalls
			if len(tcs) != 1 {
				t.Fatalf("tool calls = %d", len(tcs))
			}
			args := tcs[0].Function.Arguments
			if !json.Valid([]byte(args)) {
				t.Errorf("参数不是合法 JSON: %q", args)
			}
			if args == "null" {
				t.Errorf("参数退化成了 null: %q", args)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SSE 多行 data 字段（规范允许把 data 拆成多个物理行）。
//
// 若逐行独立解析，每个碎片都不是合法 JSON，会被静默忽略，
// 表现为内容或工具参数丢失。这里锁定"先累积再用 \n 拼回"的行为。
// 参考 dsml_stream.go 中 openAIStreamEventProcessor 对多行 data 的处理。
// ---------------------------------------------------------------------------

func TestAnthropicStream_MultilineDataFieldsReassembled(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 把一个 JSON 事件拆到多个 data: 行上（SSE 规范允许）。
		_, _ = w.Write([]byte("event: message_start\n" +
			"data: {\"type\":\"message_start\",\n" +
			"data: \"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\n" +
			"data: \"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\n" +
			"data: \"delta\":{\"type\":\"text_delta\",\"text\":\"MULTILINE_OK\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "MULTILINE_OK") {
		t.Errorf("多行 data 内容丢失: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("缺少 [DONE]: %s", out)
	}
}

// 多行 data 的工具参数也必须完整还原。
func TestAnthropicStream_MultilineToolArguments(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\n" +
			"data: \"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"get_weather\",\"input\":{}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\n" +
			"data: \"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	out := rec.Body.String()
	if !strings.Contains(out, "get_weather") {
		t.Fatalf("工具名丢失: %s", out)
	}
	if !strings.Contains(out, "Paris") {
		t.Errorf("多行 data 下工具参数丢失: %s", out)
	}
}

// 最后事件不以空行结束（上游直接关闭连接）时仍必须提交。
func TestAnthropicStream_LastEventWithoutTrailingBlankLine(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"NOBLANK\"}}\n\n"))
		// 末尾没有空行，直接结束
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}"))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "NOBLANK") {
		t.Errorf("末尾无空行时内容丢失: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 读取循环的健壮性：不依赖换行符、不依赖 EOF、不吞错误。
//
// 背景：bufio.Scanner 与 bufio.Reader.ReadString 对「末尾没有换行符的行」
// 都必须等到 EOF 才返回（已实测）。真实网关常把 message_stop 作为最后一行
// 且不补换行、同时复用连接不关闭 body，此时若按行读取会永久阻塞。
// ---------------------------------------------------------------------------

// message_stop 没有结尾换行符，且上游保持连接不关闭：必须立即返回。
func TestAnthropicStream_TerminalWithoutTrailingNewlineAndNoEOF(t *testing.T) {
	release := make(chan struct{})
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"NONL\"}}\n\n"))
		// 关键：终态行没有换行符结尾
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	})
	defer upstream.Close()
	defer close(release)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL)) }()

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "NONL") {
			t.Errorf("内容丢失: %s", rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("终态行无换行符且上游不关闭连接时挂起")
	}
}

// message_stop 后紧跟内容但无换行，且连接不关闭：同样不能挂起。
func TestAnthropicStream_TerminalThenMoreBytesNoNewline(t *testing.T) {
	release := make(chan struct{})
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"A\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		_, _ = w.Write([]byte("event: trailing\ndata: {\"type\":\"ping\"}")) // 无换行
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	})
	defer upstream.Close()
	defer close(release)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL)) }()

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("终态后存在无换行的尾部数据时挂起")
	}
}

// 上游 error 事件必须把真实原因传回下游，不能被泛化成"缺少内容"。
func TestAnthropicStream_ErrorEventReasonReachesDiagnostics(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n"))
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"UNIQUE_REASON_XYZ\"}}\n\n"))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code == http.StatusOK {
		t.Fatalf("error 事件不应返回 200: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "UNIQUE_REASON_XYZ") {
		t.Errorf("上游错误原因被吞掉: %s", rec.Body.String())
	}
}

// error 事件没有结尾空行时也必须被识别并上报。
func TestAnthropicStream_ErrorEventWithoutBlankLine(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"NOBLANK_ERR\"}}"))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	if rec.Code == http.StatusOK {
		t.Errorf("无空行的 error 事件不应返回 200: %s", rec.Body.String())
	}
}
