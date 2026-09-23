package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// anthropic-version 头的兜底
//
// Anthropic Messages API 要求请求必须携带 anthropic-version，缺失或为空会 400。
// 项目里存在两类链路，历史上写法不一致：
//   - OpenAI→Anthropic 转换路径：硬编码 "2023-06-01"（正确）
//   - /v1/messages 直通路径：直接转发客户端头（客户端不带时透传空串 → 上游 400）
//
// 现已统一到 setAnthropicVersionHeader：优先透传客户端值，缺省回落默认版本。
// ---------------------------------------------------------------------------

func newVersionProbeUpstream(t *testing.T, captured *http.Header, nonStreamBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		if captured != nil {
			*captured = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nonStreamBody))
	}))
}

const versionProbeMessage = `{"id":"msg_v","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func newAnthropicVersionTestHandler(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	prov := provider.NewOpenAIProviderWithTransport(
		"longcat2", "openai", "sk-test", upstreamURL,
		"v1/messages", "v1/models", true, 5e9,
	)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{{
		ID: "longcat2", Name: "longcat2", Type: "anthropic", BaseURL: upstreamURL,
		APIKey: "sk-test", Enabled: true,
		Transport: config.TransportConfig{ChatPath: "v1/messages", ModelsPath: "v1/models"},
	}}
	return server.loggingMiddleware(withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	}))
}

// 直通路径：客户端不带 anthropic-version 时，上游必须收到默认版本而不是空串。
func TestAnthropicVersionHeader_PassthroughFallsBackToDefault(t *testing.T) {
	var captured http.Header
	upstream := newVersionProbeUpstream(t, &captured, versionProbeMessage)
	defer upstream.Close()
	handler := newAnthropicVersionTestHandler(t, upstream.URL)

	body := `{"model":"LongCat-2.0","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	// 刻意不设置 anthropic-version
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := captured.Get("anthropic-version"); got != defaultAnthropicAPIVersion {
		t.Fatalf("上游 anthropic-version = %q, want %q（空值会被真实 Anthropic 端点 400 拒绝）",
			got, defaultAnthropicAPIVersion)
	}
}

// 直通路径：客户端显式提供的版本必须被透传，不能被默认值覆盖。
func TestAnthropicVersionHeader_PassthroughPreservesClientValue(t *testing.T) {
	var captured http.Header
	upstream := newVersionProbeUpstream(t, &captured, versionProbeMessage)
	defer upstream.Close()
	handler := newAnthropicVersionTestHandler(t, upstream.URL)

	body := `{"model":"LongCat-2.0","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	req.Header.Set("anthropic-version", "2024-01-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := captured.Get("anthropic-version"); got != "2024-01-01" {
		t.Fatalf("上游 anthropic-version = %q, want 透传客户端的 2024-01-01", got)
	}
}

// 直通路径：客户端传空白值也应回落默认，不能把空白透传上去。
func TestAnthropicVersionHeader_BlankClientValueFallsBack(t *testing.T) {
	var captured http.Header
	upstream := newVersionProbeUpstream(t, &captured, versionProbeMessage)
	defer upstream.Close()
	handler := newAnthropicVersionTestHandler(t, upstream.URL)

	body := `{"model":"LongCat-2.0","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	req.Header.Set("anthropic-version", "   ")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := captured.Get("anthropic-version"); got != defaultAnthropicAPIVersion {
		t.Fatalf("上游 anthropic-version = %q, want 默认 %q", got, defaultAnthropicAPIVersion)
	}
}

// OpenAI→Anthropic 转换路径（/v1/chat/completions + anthropic provider）
// 必须始终带默认版本，与直通路径行为一致。
func TestAnthropicVersionHeader_ConversionPathAlwaysSetsDefault(t *testing.T) {
	var captured http.Header
	upstream := newVersionProbeUpstream(t, &captured, versionProbeMessage)
	defer upstream.Close()
	handler := newAnthropicVersionTestHandler(t, upstream.URL)

	body := `{"model":"LongCat-2.0","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := captured.Get("anthropic-version"); got != defaultAnthropicAPIVersion {
		t.Fatalf("转换路径上游 anthropic-version = %q, want %q", got, defaultAnthropicAPIVersion)
	}
}

// setAnthropicVersionHeader 的单元边界。
func TestSetAnthropicVersionHeader(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", defaultAnthropicAPIVersion},
		{"   ", defaultAnthropicAPIVersion},
		{"\t\n", defaultAnthropicAPIVersion},
		{"2023-06-01", "2023-06-01"},
		{" 2024-01-01 ", "2024-01-01"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid", nil)
		setAnthropicVersionHeader(req, c.in)
		if got := req.Header.Get("anthropic-version"); got != c.want {
			t.Errorf("setAnthropicVersionHeader(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 上游请求头一致性：流式链路必须声明 Accept: text/event-stream
//
// 项目里两条 Anthropic 流式链路历史上写法不一致：
//   - OpenAI→Anthropic 转换路径：设置了 Accept
//   - /v1/messages 直通路径：未设置（上游收到空值）
// 部分网关依据 Accept 决定返回 SSE 还是聚合 JSON，缺失时可能拿到非流式响应。
// ---------------------------------------------------------------------------

func newStreamingProbeUpstream(t *testing.T, captured *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		if captured != nil {
			*captured = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
}

// 直通流式（/v1/messages + anthropic provider + stream=true）必须声明 Accept。
func TestAnthropicUpstreamHeaders_PassthroughStreamSetsAccept(t *testing.T) {
	var captured http.Header
	upstream := newStreamingProbeUpstream(t, &captured)
	defer upstream.Close()
	handler := newAnthropicVersionTestHandler(t, upstream.URL)

	body := `{"model":"LongCat-2.0","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := captured.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("直通流式上游 Accept = %q, want text/event-stream", got)
	}
	if got := captured.Get("Content-Type"); got != "application/json" {
		t.Errorf("直通流式上游 Content-Type = %q, want application/json", got)
	}
}

// 转换流式（/v1/chat/completions + anthropic provider + stream=true）同样必须声明 Accept。
func TestAnthropicUpstreamHeaders_ConversionStreamSetsAccept(t *testing.T) {
	var captured http.Header
	upstream := newStreamingProbeUpstream(t, &captured)
	defer upstream.Close()
	handler := newAnthropicVersionTestHandler(t, upstream.URL)

	body := `{"model":"LongCat-2.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := captured.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("转换流式上游 Accept = %q, want text/event-stream", got)
	}
}

// 非流式链路不应声明 Accept: text/event-stream（否则网关可能返回 SSE）。
func TestAnthropicUpstreamHeaders_NonStreamDoesNotRequestSSE(t *testing.T) {
	var captured http.Header
	upstream := newVersionProbeUpstream(t, &captured, versionProbeMessage)
	defer upstream.Close()
	handler := newAnthropicVersionTestHandler(t, upstream.URL)

	body := `{"model":"LongCat-2.0","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := captured.Get("Accept"); got == "text/event-stream" {
		t.Fatalf("非流式链路不应声明 Accept: text/event-stream，实际 = %q", got)
	}
}
