package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// messages 必填校验（三条入口一致）
//
// 原实现只在 /v1/messages 校验，/v1/chat/completions 与 /api/chat 会把
// 空消息列表原样发给上游：既消耗上游配额/计费，又让用户看到上游的模糊报错
// 而不是明确的本地校验失败。OpenAI 官方对空 messages 返回 400。
// ---------------------------------------------------------------------------

func newEmptyMessagesProbe(t *testing.T) (http.Handler, *string) {
	t.Helper()
	var captured string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		captured = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)

	prov := provider.NewOpenAIProviderWithTransport("p", "openai", "k", upstream.URL,
		"v1/chat/completions", "v1/models", true, 5e9)
	server := newTestServer(prov)
	return server.loggingMiddleware(withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
		mux.HandleFunc("/api/chat", server.handleOllamaChat)
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
	})), &captured
}

// 空 messages 必须 400 拒绝，且不得把请求转发到上游。
func TestEmptyMessages_RejectedOnAllEntries(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"chat/completions 空数组", "/v1/chat/completions", `{"model":"m","messages":[]}`},
		{"chat/completions 缺字段", "/v1/chat/completions", `{"model":"m"}`},
		{"ollama 空数组", "/api/chat", `{"model":"m","messages":[]}`},
		{"ollama 缺字段", "/api/chat", `{"model":"m"}`},
		{"v1/messages 空数组", "/v1/messages", `{"model":"m","max_tokens":16,"messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handler, captured := newEmptyMessagesProbe(t)
			*captured = ""
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if *captured != "" {
				t.Errorf("空 messages 不应转发到上游，实际收到: %s", *captured)
			}
			// 错误必须点明 messages 问题，便于用户定位。
			if !strings.Contains(rec.Body.String(), "messages") {
				t.Errorf("错误信息未点明 messages 字段: %s", rec.Body.String())
			}
		})
	}
}

// 正常 messages 不得被误伤。
func TestEmptyMessages_ValidRequestStillWorks(t *testing.T) {
	handler, _ := newEmptyMessagesProbe(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "messages 字段是必填") {
		t.Fatalf("正常请求被误判为 messages 缺失: %s", rec.Body.String())
	}
}

// 诊断头必须带上 invalid_request_error，便于客户端与日志归因。
func TestEmptyMessages_SetsDiagnosticHeaders(t *testing.T) {
	handler, _ := newEmptyMessagesProbe(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Proxy-Error-Code"); got != "invalid_request_error" {
		t.Errorf("X-Proxy-Error-Code = %q, want invalid_request_error", got)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v; body=%s", err, rec.Body.String())
	}
	if body.Error.Code != "invalid_request_error" {
		t.Errorf("error.code = %q, want invalid_request_error", body.Error.Code)
	}
}
