package proxy

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// /v1/messages + anthropic provider + stream=true 的直通流式契约
//
// 这是 /v1/messages 入口的核心分支（handleAnthropicPassthroughStream，约 90 行），
// 负责把上游 Anthropic SSE 原样透传给 Anthropic 原生客户端。
// 此前只有 NonStream 版本测试，以及一个只校验"上游请求头"的用例——
// 下游内容与终态是否完整没有任何断言。
// ---------------------------------------------------------------------------

func newPassthroughStreamHandler(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	prov := provider.NewOpenAIProviderWithTransport("longcat2", "openai", "sk", upstreamURL,
		"v1/messages", "v1/models", true, 5e9)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{{
		ID: "longcat2", Name: "longcat2", Type: "anthropic", BaseURL: upstreamURL,
		APIKey: "sk", Enabled: true,
		Transport: config.TransportConfig{ChatPath: "v1/messages", ModelsPath: "v1/models"},
	}}
	return server.loggingMiddleware(withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
	}))
}

// 完整 Anthropic SSE 必须原样透传：内容与 message_stop 终态都不能丢。
func TestAnthropicPassthroughStream_RelaysFullEventStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"PASS\"}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	handler := newPassthroughStreamHandler(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"LongCat-2.0","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	out := rec.Body.String()
	for _, want := range []string{"message_start", "content_block_start", "content_block_delta", "PASS", "message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("下游缺少 %q:\n%s", want, out)
		}
	}
	// 直通路径不得把 Anthropic 事件改写成 OpenAI 格式。
	if strings.Contains(out, "chat.completion.chunk") || strings.Contains(out, "[DONE]") {
		t.Errorf("直通路径不应把 Anthropic 流转成 OpenAI 格式:\n%s", out)
	}
}

// 上游非 200 时必须失败关闭，不能把错误体当事件流下发。
func TestAnthropicPassthroughStream_UpstreamNon200FailsClosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
	}))
	defer upstream.Close()

	handler := newPassthroughStreamHandler(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"LongCat-2.0","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("上游 401 被伪装成成功下发:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bad key") {
		t.Errorf("上游错误原因未上报: %s", rec.Body.String())
	}
}

// 增量性：上游分片到达时下游必须即时可见，不能聚合成一次性输出。
func TestAnthropicPassthroughStream_IsIncremental(t *testing.T) {
	const (
		upstreamHold = 10 * time.Second
		assertWithin = 2500 * time.Millisecond
	)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"FIRST\"}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-time.After(upstreamHold):
		}
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newPassthroughStreamHandler(t, upstream.URL))
	defer proxy.Close()
	defer releaseUpstream()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages",
		strings.NewReader(`{"model":"LongCat-2.0","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	seen := make(chan struct{})
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, readErr := reader.ReadString('\n')
			if strings.Contains(line, "FIRST") {
				close(seen)
				return
			}
			if readErr != nil {
				return
			}
		}
	}()

	select {
	case <-seen:
	case <-time.After(assertWithin):
		t.Fatalf("下游在 %s 内未收到首个增量：直通流式被聚合", assertWithin)
	}
	releaseUpstream()
}
