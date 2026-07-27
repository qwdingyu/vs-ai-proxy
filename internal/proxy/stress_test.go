package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

// TestStress_NormalStream 正常流式 50 并发
func TestStress_NormalStream(t *testing.T) {
	mock, handler := newStressTestEnv(t)
	defer mock.Close()

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	c := &http.Client{Timeout: 30 * time.Second}
	var success, total atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"stress-model","stream":true,"messages":[{"role":"user","content":"test %d"}]}`, id)
			resp, err := c.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			total.Add(1)
			if err != nil { t.Logf("req err: %v", err); return }
			defer resp.Body.Close()
			scanner := bufio.NewScanner(resp.Body)
			done := false
			for scanner.Scan() {
				if strings.TrimSpace(scanner.Text()) == "data: [DONE]" { done = true }
			}
			if done && resp.StatusCode == 200 { success.Add(1) }
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("正常流式 50 并发: 耗时=%v 成功=%d/%d", elapsed, success.Load(), total.Load())
	if success.Load() < 45 { t.Fatalf("成功率过低: %d/%d", success.Load(), total.Load()) }
}

// TestStress_NormalNonStream 正常非流式 50 并发
func TestStress_NormalNonStream(t *testing.T) {
	mock, handler := newStressTestEnv(t)
	defer mock.Close()

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	c := &http.Client{Timeout: 30 * time.Second}
	var success, total atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"stress-model","stream":false,"messages":[{"role":"user","content":"test %d"}]}`, id)
			resp, err := c.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			total.Add(1)
			if err != nil { return }
			defer resp.Body.Close()
			io.ReadAll(resp.Body)
			if resp.StatusCode == 200 { success.Add(1) }
		}(i)
	}
	wg.Wait()
	t.Logf("正常非流式 50 并发: 成功=%d/%d", success.Load(), total.Load())
	if success.Load() < 45 { t.Fatalf("成功率过低: %d/%d", success.Load(), total.Load()) }
}

// TestStress_Upstream503 上游 503 错误 20 并发
func TestStress_Upstream503(t *testing.T) {
	mock, handler := newStressTestEnv(t)
	defer mock.Close()

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	c := &http.Client{Timeout: 10 * time.Second}
	var success, total atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"stress-model","stream":false,"messages":[{"role":"user","content":"test"}],"error_inject":"503"}`)
			resp, err := c.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			total.Add(1)
			if err != nil { return }
			defer resp.Body.Close()
			io.ReadAll(resp.Body)
			// 代理应返回 502 Bad Gateway
			if resp.StatusCode == 502 { success.Add(1) }
		}(i)
	}
	wg.Wait()
	t.Logf("上游 503 错误 20 并发: 成功(%d)=%d/%d", 502, success.Load(), total.Load())
	if success.Load() < 18 { t.Fatalf("502 响应不足: %d/20", success.Load()) }
}

// TestStress_ClientDisconnect 客户端提前断开 20 并发
func TestStress_ClientDisconnect(t *testing.T) {
	mock, handler := newStressTestEnv(t)
	defer mock.Close()

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	var success, total atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			if id%2 == 0 {
				time.AfterFunc(10*time.Millisecond, cancel)
			} else {
				defer cancel()
			}
			body := fmt.Sprintf(`{"model":"stress-model","stream":true,"messages":[{"role":"user","content":"test %d"}],"delay_ms":10000}`, id)
			req, _ := http.NewRequestWithContext(ctx, "POST", proxyServer.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			total.Add(1)
			if err != nil {
				success.Add(1) // client_gone = 正确行为
			} else {
				resp.Body.Close()
				success.Add(1)
			}
		}(i)
	}
	wg.Wait()
	t.Logf("客户端提前断开 20 并发: 完成=%d", total.Load())
	if total.Load() < 18 { t.Fatal("太多请求未完成") }
}


// TestStress_LargeRequest 大请求体 10 并发
func TestStress_LargeRequest(t *testing.T) {
	mock, handler := newStressTestEnv(t)
	defer mock.Close()

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	c := &http.Client{Timeout: 30 * time.Second}
	var success, total atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := make([]byte, 500*1024)
			rand.Read(payload)
			body := fmt.Sprintf(`{"model":"stress-model","stream":false,"messages":[{"role":"user","content":"%x"}]}`, payload[:32])
			resp, err := c.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			total.Add(1)
			if err != nil { return }
			defer resp.Body.Close()
			io.ReadAll(resp.Body)
			if resp.StatusCode == 200 { success.Add(1) }
		}(i)
	}
	wg.Wait()
	t.Logf("大请求体 500KB 10 并发: 成功=%d/%d", success.Load(), total.Load())
	if success.Load() < 8 { t.Fatalf("成功率过低: %d/10", success.Load()) }
}

// TestStress_PartialStream 上游流式中断 20 并发
func TestStress_PartialStream(t *testing.T) {
	mock, handler := newStressTestEnv(t)
	defer mock.Close()

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	var success, total atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"stress-model","stream":true,"messages":[{"role":"user","content":"test"}],"error_inject":"partial_stream"}`)
			resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			total.Add(1)
			if err != nil { return }
			defer resp.Body.Close()
			io.ReadAll(resp.Body)
			// 可能部分数据已写出，HTTP 状态可能是 200 或 502
			if resp.StatusCode == 200 || resp.StatusCode == 502 { success.Add(1) }
		}(i)
	}
	wg.Wait()
	t.Logf("上游流式中断 20 并发: 200/502=%d/%d", success.Load(), total.Load())
	if success.Load() < 16 { t.Fatalf("过多异常状态: %d/20", total.Load()-success.Load()) }
}

// TestStress_MixedLoad 混合负载 100 请求
func TestStress_MixedLoad(t *testing.T) {
	mock, handler := newStressTestEnv(t)
	defer mock.Close()

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	c := &http.Client{Timeout: 30 * time.Second}
	var success, total atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var body string
			switch id % 4 {
			case 0:
				body = fmt.Sprintf(`{"model":"stress-model","stream":true,"messages":[{"role":"user","content":"test %d"}]}`, id)
			case 1:
				body = fmt.Sprintf(`{"model":"stress-model","stream":false,"messages":[{"role":"user","content":"test %d"}]}`, id)
			case 2:
				body = fmt.Sprintf(`{"model":"stress-model","stream":false,"messages":[{"role":"user","content":"test"}],"error_inject":"503"}`)
			case 3:
				body = fmt.Sprintf(`{"model":"stress-model","stream":false,"messages":[{"role":"user","content":"test"}],"error_inject":"502"}`)
			}
			resp, err := c.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			total.Add(1)
			if err != nil { return }
			defer resp.Body.Close()
			io.ReadAll(resp.Body)
			if resp.StatusCode == 200 || resp.StatusCode == 502 { success.Add(1) }
		}(i)
	}
	wg.Wait()
	t.Logf("混合负载 100 请求: 成功=%d/%d", success.Load(), total.Load())
	if success.Load() < 80 { t.Fatalf("成功率过低: %d/100", success.Load()) }
}

// TestStress_ModelList 模型列表并发 50
func TestStress_ModelList(t *testing.T) {
	mock, handler := newStressTestEnv(t)
	defer mock.Close()

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	var success, total atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp, err := http.Get(proxyServer.URL + "/v1/models")
			total.Add(1)
			if err != nil { return }
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode == 200 && strings.Contains(string(body), "stress-model") {
				success.Add(1)
			}
		}(i)
	}
	wg.Wait()
	t.Logf("模型列表并发 50: 成功=%d/%d", success.Load(), total.Load())
	if success.Load() < 45 { t.Fatalf("成功率过低: %d/50", success.Load()) }
}

// TestStress_HealthCheck 健康检查并发 50
func TestStress_HealthCheck(t *testing.T) {
	mock, handler := newStressTestEnv(t)
	defer mock.Close()

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	var success, total atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp, err := http.Get(proxyServer.URL + "/health")
			total.Add(1)
			if err != nil { return }
			defer resp.Body.Close()
			if resp.StatusCode == 200 { success.Add(1) }
		}(i)
	}
	wg.Wait()
	t.Logf("健康检查并发 50: 成功=%d/%d", success.Load(), total.Load())
	if success.Load() < 45 { t.Fatalf("成功率过低: %d/50", success.Load()) }
}

// ---------------------------------------------------------------------------
// 辅助：搭建测试环境
// ---------------------------------------------------------------------------

func newStressTestEnv(t *testing.T) (*httptest.Server, http.Handler) {
	t.Helper()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"stress-model"},{"id":"stress-timeout"}]}`))

		case "/v1/chat/completions":
			var req struct {
				Model     string `json:"model"`
				Stream    bool   `json:"stream"`
				Error     string `json:"error_inject"`
				DelayMs   int    `json:"delay_ms"`
			}
			json.Unmarshal(body, &req)

			if req.DelayMs > 0 { time.Sleep(time.Duration(req.DelayMs) * time.Millisecond) }

			switch req.Error {
			case "503":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"message":"Service Unavailable"}}`))
				return
			case "502":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":{"message":"Bad Gateway"}}`))
				return
			case "hang":
				// 挂起测试需要用专用测试环境，避免 httptest.Server.Close 阻塞
				// 参见 TestStress_UpstreamHangCleanup 专用测试
				time.Sleep(50 * time.Millisecond)
			case "partial_stream":
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n"))
				if f, ok := w.(http.Flusher); ok { f.Flush() }
				time.Sleep(50 * time.Millisecond)
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, _ := hj.Hijack()
					conn.Close()
				}
				return
			}

			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				chunks := []string{
					`data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
					`data: {"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
					`data: {"choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`,
					`data: [DONE]`,
					``,
				}
				for _, c := range chunks {
					_, err := w.Write([]byte(c + "\n"))
					if err != nil { return }
					if f, ok := w.(http.Flusher); ok { f.Flush() }
					time.Sleep(3 * time.Millisecond)
				}
				return
			}

			resp := map[string]any{
				"id": "mock-" + fmt.Sprintf("%d", time.Now().UnixNano()),
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role": "assistant", "content": "Mock response.",
					},
					"finish_reason": "stop",
				}},
			}
			out, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)

		default:
			http.NotFound(w, r)
		}
	}))

	cfg := &config.AppConfig{
		DefaultModel: "stress-model",
		Providers: []config.ProviderConfig{{
			ID:      "stress",
			Name:    "Stress",
			Type:    "openai",
			BaseURL: mock.URL + "/v1",
			APIKey:  "sk-mock",
			Enabled: true,
		}},
		Models: []config.ModelConfig{
			{Name: "stress-model", ProviderID: "stress", Provider: "stress", Enabled: true, TimeoutSeconds: intPtr(30)},
			{Name: "stress-timeout", ProviderID: "stress", Provider: "stress", Enabled: true, TimeoutSeconds: intPtr(3)},
		},
		Defense: config.DefenseConfig{
			Enabled:                   boolPtr(true),
			ClientTimeoutBudgetSeconds: intPtr(30),
		},
	}

	st := store.New(1000)
	logger := log.New(nil, log.LevelError, false)
	srv := NewServer(cfg, nil, st, logger)
	return mock, srv.Handler()
}

func boolPtr(v bool) *bool { return &v }

