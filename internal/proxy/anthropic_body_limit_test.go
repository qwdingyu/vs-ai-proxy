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
// /v1/messages 请求体上限
//
// 原实现用 io.LimitReader 静默截断超限请求：截断后 JSON 解析必然失败，
// 用户看到的是"解析请求失败: unexpected end of JSON input"，看不出真实原因是
// "请求过大"；而 /v1/chat/completions 对同样情况返回 413 + 明确文案。
// 现统一为 http.MaxBytesReader + 413。
// ---------------------------------------------------------------------------

func newAnthropicBodyLimitHandler(t *testing.T) http.Handler {
	t.Helper()
	prov := provider.NewOpenAIProviderWithTransport("longcat2", "openai", "sk", "http://127.0.0.1:1",
		"v1/messages", "v1/models", true, 5e9)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{{
		ID: "longcat2", Name: "longcat2", Type: "anthropic", BaseURL: "http://127.0.0.1:1",
		APIKey: "sk", Enabled: true,
		Transport: config.TransportConfig{ChatPath: "v1/messages", ModelsPath: "v1/models"},
	}}
	return server.loggingMiddleware(withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
	}))
}

// 超过上限必须返回 413，且错误信息点明"请求过大"，不能是 JSON 解析错误。
func TestAnthropicBodyLimit_OversizeReturns413(t *testing.T) {
	handler := newAnthropicBodyLimitHandler(t)

	big := strings.Repeat("A", 2*maxAnthropicRequestBodyBytes)
	body := `{"model":"LongCat-2.0","max_tokens":16,"messages":[{"role":"user","content":"` + big + `"}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	msg := rec.Body.String()
	if !strings.Contains(msg, "限制") {
		t.Errorf("错误信息应点明超限，实际: %s", msg)
	}
	if strings.Contains(msg, "unexpected end of JSON") {
		t.Errorf("错误信息不应是误导性的 JSON 解析错误: %s", msg)
	}
}

// 未声明 Content-Length 的超限请求（分块传输）也必须被 MaxBytesReader 拦下。
func TestAnthropicBodyLimit_OversizeWithoutContentLengthReturns413(t *testing.T) {
	handler := newAnthropicBodyLimitHandler(t)

	big := strings.Repeat("A", 2*maxAnthropicRequestBodyBytes)
	body := `{"model":"LongCat-2.0","max_tokens":16,"messages":[{"role":"user","content":"` + big + `"}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.ContentLength = -1 // 模拟 chunked：长度未知
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413（长度未知时也必须拦截）; body=%s", rec.Code, rec.Body.String())
	}
}

// 上限之内的正常请求不得被误伤。
func TestAnthropicBodyLimit_NormalRequestPasses(t *testing.T) {
	handler := newAnthropicBodyLimitHandler(t)

	body := `{"model":"LongCat-2.0","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// 上游不可达，会返回错误；关键是**不能**是 413。
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("正常大小请求被误判为超限: %s", rec.Body.String())
	}
}

// 空请求体仍返回 400。
func TestAnthropicBodyLimit_EmptyBodyReturns400(t *testing.T) {
	handler := newAnthropicBodyLimitHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// 跨入口一致性：/v1/messages 与 /v1/chat/completions 的请求体上限必须相同。
// 历史上 /v1/messages 是 1 MiB、主路径是 32 MiB，导致同一个大上下文请求
// 换入口就一个 413 一个成功。
func TestAnthropicBodyLimit_MatchesChatCompletionsLimit(t *testing.T) {
	if maxAnthropicRequestBodyBytes != maxChatRequestBodyBytes {
		t.Fatalf("/v1/messages 上限 %d 与 /v1/chat/completions 上限 %d 不一致，用户会看到换入口行为不同",
			maxAnthropicRequestBodyBytes, maxChatRequestBodyBytes)
	}
	if maxAnthropicRequestBodyBytes < 8<<20 {
		t.Errorf("/v1/messages 上限仅 %d MiB，对处理大代码库的 Anthropic 原生客户端过于严格",
			maxAnthropicRequestBodyBytes>>20)
	}
}

// 上限之下的大请求（2 MiB）必须能通过校验进入转发阶段，不被 413 拒绝。
func TestAnthropicBodyLimit_TwoMiBRequestPasses(t *testing.T) {
	handler := newAnthropicBodyLimitHandler(t)

	big := strings.Repeat("A", 2*1024*1024)
	body := `{"model":"LongCat-2.0","max_tokens":16,"messages":[{"role":"user","content":"` + big + `"}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("2 MiB 请求被 413 拒绝，但 /v1/chat/completions 会接受：%s", rec.Body.String())
	}
}
