package proxy

import (
	"context"
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
// /v1/messages 直通路径的配置快照纪律
//
// s.config 由 Reconfigure 在 s.mu 保护下替换，snapshot() 在 s.mu.RLock 下读取。
// 但 forwardAnthropicRequest / handleAnthropicPassthroughStream 曾经直接读
// s.config，配置热更新与在途请求并发时构成数据竞争。
//
// 本测试用 -race 检测：并发执行"配置热更新"与"/v1/messages 直通请求"。
// 竞争存在时 `go test -race` 会报告 WARNING: DATA RACE 并失败。
// 注意：必须在 -race 下运行才有意义（make release-check 会跑 -race ./...）。
// ---------------------------------------------------------------------------

func newAnthropicPassthroughTestServer(t *testing.T, upstreamURL string) (*Server, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_x","type":"message","role":"assistant","model":"LongCat-2.0","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)

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
	ts := httptest.NewServer(server.loggingMiddleware(withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
	})))
	t.Cleanup(ts.Close)
	return server, ts
}

// 并发：配置热更新 × /v1/messages 直通请求。
// 若实现绕过快照直接读 s.config，-race 会报数据竞争。
func TestAnthropicPassthrough_ConfigSnapshotUnderReconfigure(t *testing.T) {
	server, ts := newAnthropicPassthroughTestServer(t, "http://127.0.0.1:1")

	body := `{"model":"LongCat-2.0","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 持续热更新配置
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			next := config.CloneAppConfig(server.config)
			next.Providers = append([]config.ProviderConfig{}, server.config.Providers...)
			server.Reconfigure(next)
		}
	}()

	// 持续发起直通请求（直到收到 response 或错误都无所谓，重点是并发访问配置）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(body))
			if err != nil {
				cancel()
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			cancel()
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// 非流式直通（forwardAnthropicRequest）也要走快照：直接调用它并并发热更新。
func TestForwardAnthropicRequest_ConfigSnapshotUnderReconfigure(t *testing.T) {
	server, _ := newAnthropicPassthroughTestServer(t, "http://127.0.0.1:1")

	prov := provider.NewOpenAIProviderWithTransport(
		"longcat2", "openai", "sk-test", "http://127.0.0.1:1",
		"v1/messages", "v1/models", true, time.Second,
	)
	originalBody := []byte(`{"model":"LongCat-2.0","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			next := config.CloneAppConfig(server.config)
			next.Providers = append([]config.ProviderConfig{}, server.config.Providers...)
			server.Reconfigure(next)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			select {
			case <-stop:
				return
			default:
			}
			cfg, _, _ := server.snapshot()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			// 上游不可达，必然返回错误——这里只关心配置读取是否走快照。
			_ = server.forwardAnthropicRequest(context.Background(), cfg, rec, req, prov, originalBody, "LongCat-2.0")
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
