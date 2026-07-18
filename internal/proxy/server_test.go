package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

func TestAuthMiddlewareRequiresBearerToken(t *testing.T) {
	server := &Server{proxyKey: "secret"}
	handler := server.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", authHeader: "Basic secret", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", authHeader: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "valid token", authHeader: "Bearer secret", wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestChatHandlersRejectRequestBodiesOverSharedLimit(t *testing.T) {
	server := &Server{}
	tests := []struct {
		name    string
		path    string
		handler http.HandlerFunc
	}{
		{name: "openai", path: "/v1/chat/completions", handler: server.handleChatCompletions},
		{name: "ollama", path: "/api/chat", handler: server.handleOllamaChat},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader("{}"))
			req.ContentLength = maxChatRequestBodyBytes + 1
			rec := httptest.NewRecorder()

			tt.handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestReadRequestBodyRejectsUnknownLengthOverflow(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("12345"))
	req.ContentLength = -1
	rec := httptest.NewRecorder()

	body, ok := readRequestBody(rec, req, 4)
	if ok || body != nil {
		t.Fatalf("readRequestBody() = %q, %v; want rejected", body, ok)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddlewareAllowsOpenProxyWithoutKey(t *testing.T) {
	server := &Server{}
	handler := server.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestLoggingMiddlewareCapturesProviderAndModelHeaders(t *testing.T) {
	st := store.New(10)
	server := &Server{store: st, logger: log.New(nil, log.LevelError, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Proxy-Provider", "UseAI")
		w.Header().Set("X-Proxy-Requested-Model", "useai-model")
		w.Header().Set("X-Proxy-Upstream-Model", "upstream-model")
		w.WriteHeader(http.StatusOK)
	}))

	body := `{"model":"useai-model"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Request-ID", "req-123-abc")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Proxy-Request-ID"); got != "req-123-abc" {
		t.Fatalf("response request id = %q, want req-123-abc", got)
	}

	logs := st.GetLogs(1)
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	if logs[0].RequestID != "req-123-abc" {
		t.Fatalf("request id = %q, want req-123-abc", logs[0].RequestID)
	}
	if logs[0].Provider != "UseAI" {
		t.Fatalf("provider = %q, want UseAI", logs[0].Provider)
	}
	if logs[0].Model != "useai-model" {
		t.Fatalf("model = %q, want useai-model", logs[0].Model)
	}
	if logs[0].Upstream != "upstream-model" {
		t.Fatalf("upstream = %q, want upstream-model", logs[0].Upstream)
	}
	if logs[0].RequestBytes != int64(len(body)) {
		t.Fatalf("request bytes = %d, want %d", logs[0].RequestBytes, len(body))
	}
}

func TestLoggingMiddlewareWritesRequestIDAndErrorCodeToServerLog(t *testing.T) {
	var buf strings.Builder
	st := store.New(10)
	server := &Server{store: st, logger: log.New(&buf, log.LevelInfo, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setResponseLogFields(w, "useai", "gpt-5.5", "gpt-5.5")
		w.Header().Set("X-Proxy-Error-Code", "upstream_server_error")
		w.Header().Set("X-Proxy-Attempts-Summary", "useai/gpt-5.5 13s upstream_server_error")
		w.WriteHeader(http.StatusBadGateway)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5"}`))
	req.Header.Set("X-Request-ID", "req-log-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	for _, want := range []string{
		"request_id=req-log-1",
		"provider=useai",
		"requested_model=gpt-5.5",
		"upstream=gpt-5.5",
		"error_code=upstream_server_error",
		`reason="上游服务暂不可用"`,
		`action="稍后重试，或切换模型。"`,
		`attempts="useai/gpt-5.5 13s upstream_server_error"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("server log = %q, want contains %q", out, want)
		}
	}
}

func TestLoggingMiddlewareWritesProviderModelToSuccessfulServerLog(t *testing.T) {
	var buf strings.Builder
	st := store.New(10)
	server := &Server{store: st, logger: log.New(&buf, log.LevelInfo, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setResponseLogFields(w, "xiaomimimo", "mimo-v2.5", "mimo-v2.5")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"mimo-v2.5"}`))
	req.Header.Set("X-Request-ID", "req-mimo-ok")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	for _, want := range []string{
		"POST /v1/chat/completions - 200",
		"request_id=req-mimo-ok",
		"provider=xiaomimimo",
		"requested_model=mimo-v2.5",
		"upstream=mimo-v2.5",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("server log = %q, want contains %q", out, want)
		}
	}
}

func TestProviderAttemptWarningUsesShortUserFacingReason(t *testing.T) {
	var buf strings.Builder
	server := &Server{logger: log.New(&buf, log.LevelWarn, false)}
	server.logProviderAttemptFailure("req-kimi", "kimi-for-coding", "kimi-for-coding", "kimi", attemptDiagnostic{
		Category: "upstream_quota_exhausted",
		Message:  `API 错误 403: {"error":{"message":"long upstream error"}}`,
	})

	out := buf.String()
	if !strings.Contains(out, "模型 kimi-for-coding（kimi）候选尝试失败: request_id=req-kimi provider=kimi requested_model=kimi-for-coding upstream=kimi-for-coding reason=上游额度已用完") {
		t.Fatalf("warning = %q, want concise reason", out)
	}
	if strings.Contains(out, "API 错误") || strings.Contains(out, "long upstream error") {
		t.Fatalf("warning leaked technical upstream body: %q", out)
	}
}

func TestProviderAttemptDebugLogKeepsSanitizedUpstreamEvidence(t *testing.T) {
	var buf strings.Builder
	server := &Server{logger: log.New(&buf, log.LevelDebug, false)}
	server.logProviderAttemptFailure("req-debug", "requested-model", "test-model", "test", attemptDiagnostic{
		Category: "upstream_auth_error",
		Message:  "API 错误 403: Bearer secret-token-value-123456 denied",
	})

	out := buf.String()
	if !strings.Contains(out, "[DEBUG]") || !strings.Contains(out, "request_id=req-debug") || !strings.Contains(out, "requested_model=requested-model") || !strings.Contains(out, "upstream=test-model") || !strings.Contains(out, "API 错误 403") {
		t.Fatalf("debug evidence missing: %q", out)
	}
	if strings.Contains(out, "secret-token-value") || !strings.Contains(out, "<redacted>") {
		t.Fatalf("debug evidence was not sanitized: %q", out)
	}
}

func TestConsoleRouteSuffixQuotesNonCompactModelNames(t *testing.T) {
	suffix := consoleRouteSuffix("useai", "UseAI - step-router-v1", "模型 step-router-v1")

	for _, want := range []string{
		"provider=useai",
		`requested_model="UseAI - step-router-v1"`,
		`upstream="模型 step-router-v1"`,
	} {
		if !strings.Contains(suffix, want) {
			t.Fatalf("route suffix = %q, want contains %q", suffix, want)
		}
	}
	if strings.Contains(suffix, "\n") || strings.Contains(suffix, "\r") {
		t.Fatalf("route suffix must stay single-line: %q", suffix)
	}
}

func TestConsoleDiagnosticSuffixKeepsTechnicalFieldsCompact(t *testing.T) {
	diag := summarizeLogDiagnostic("network_error", http.StatusBadGateway, 22_510, 634_054, 642_181, "104.21.57.81:443", "upstream_connecting", "", "")
	suffix := consoleDiagnosticSuffix(diag, "useai/step-router-v1 23s network_error", 634_054, 642_181)

	for _, want := range []string{
		`reason="无法连接上游"`,
		`action="检查网络和上游地址后重试。"`,
		`attempts="useai/step-router-v1 23s network_error"`,
		`request_bytes="619.2 KB"`,
		`upstream_bytes="627.1 KB"`,
	} {
		if !strings.Contains(suffix, want) {
			t.Fatalf("suffix = %q, want contains %q", suffix, want)
		}
	}
	if strings.Contains(suffix, "summary=") || strings.Contains(suffix, "旧 session") {
		t.Fatalf("suffix contains redundant explanation: %q", suffix)
	}
}

func TestLoggingMiddlewareKeepsLargeRequestErrorConcise(t *testing.T) {
	var buf strings.Builder
	st := store.New(10)
	server := &Server{store: st, logger: log.New(&buf, log.LevelInfo, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setResponseLogFields(w, "useai", "UseAI - step-router-v1", "step-router-v1")
		if rw, ok := w.(*responseWriter); ok {
			rw.upstreamBytes = 642_181
		}
		w.Header().Set("X-Proxy-Error-Code", "network_error")
		w.Header().Set("X-Proxy-Attempts-Summary", "useai/step-router-v1 23s network_error")
		w.Header().Set("X-Proxy-Network-Peer", "104.21.57.81:443")
		setProxyStreamState(w, "upstream_connecting")
		w.WriteHeader(http.StatusBadGateway)
	}))

	body := strings.Repeat("x", 634_054)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Request-ID", "req-pressure-console")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	for _, want := range []string{
		"request_id=req-pressure-console",
		"error_code=network_error",
		`reason="无法连接上游"`,
		`action="检查网络和上游地址后重试。"`,
		`request_bytes="619.2 KB"`,
		`upstream_bytes="627.1 KB"`,
		`attempts="useai/step-router-v1 23s network_error"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("server log = %q, want contains %q", out, want)
		}
	}

	logs := st.GetLogs(1)
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	if strings.Contains(logs[0].ErrorAction, "session") || strings.Contains(logs[0].ErrorAction, "上下文") {
		t.Fatalf("web/store action should stay concise: %#v", logs[0])
	}
}

func TestLoggingMiddlewareCapturesReportedUsageThroughStreamWriter(t *testing.T) {
	st := store.New(10)
	server := &Server{store: st, logger: log.New(nil, log.LevelError, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setResponseLogFields(w, "kimi", "kimi-for-coding", "kimi-for-coding")
		streamWriter := &streamAttemptWriter{ResponseWriter: w}
		setResponseUsage(streamWriter, &provider.Usage{
			PromptTokens: 12, CompletionTokens: 4, TotalTokens: 16,
			PromptTokensDetails:     &provider.PromptTokensDetails{CachedTokens: 5},
			CompletionTokensDetails: &provider.CompletionTokensDetails{ReasoningTokens: 2},
		})
		streamWriter.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	logs := st.GetLogs(1)
	if len(logs) != 1 || logs[0].Usage == nil {
		t.Fatalf("logs = %#v, want one log with usage", logs)
	}
	usage := logs[0].Usage
	if usage.PromptTokens != 12 || usage.CompletionTokens != 4 || usage.TotalTokens != 16 || usage.CachedTokens != 5 || usage.ReasoningTokens != 2 {
		t.Fatalf("usage = %#v, want full upstream details", usage)
	}
}

func TestStreamAccumulatorUsesLastUsageSnapshot(t *testing.T) {
	acc := newStreamReasoningAccumulator()
	acc.consumeOpenAISSELine(`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
	acc.consumeOpenAISSELine(`data: {"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":1}}}`)
	if acc.usage == nil || acc.usage.TotalTokens != 10 {
		t.Fatalf("usage = %#v, want final snapshot total=10", acc.usage)
	}
	if acc.usage.PromptTokensDetails == nil || acc.usage.PromptTokensDetails.CachedTokens != 3 {
		t.Fatalf("usage details = %#v, want cached=3", acc.usage)
	}
}

func TestStreamAccumulatorCapturesOllamaTerminalUsage(t *testing.T) {
	acc := newStreamReasoningAccumulator()
	acc.consumeOllamaChunk(map[string]any{
		"done": true, "done_reason": "stop", "prompt_eval_count": float64(9), "eval_count": float64(3),
	})
	if acc.usage == nil || acc.usage.PromptTokens != 9 || acc.usage.CompletionTokens != 3 || acc.usage.TotalTokens != 12 {
		t.Fatalf("usage = %#v, want Ollama terminal usage 9/3/12", acc.usage)
	}
}

func TestLoggingMiddlewareWritesDiagnosticsWhenErrorCodeHeaderMissing(t *testing.T) {
	var buf strings.Builder
	st := store.New(10)
	server := &Server{store: st, logger: log.New(&buf, log.LevelInfo, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodGet, "/broken", nil)
	req.Header.Set("X-Request-ID", "req-missing-code")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	for _, want := range []string{
		"request_id=req-missing-code",
		"error_code=provider_error",
		`reason="请求失败"`,
		`action="检查上游配置后重试。"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("server log = %q, want contains %q", out, want)
		}
	}
	logs := st.GetLogs(1)
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	if logs[0].ErrorCode != "provider_error" || logs[0].ErrorReason != "请求失败" {
		t.Fatalf("store fallback diagnostics missing: %#v", logs[0])
	}
}

func TestFallbackLogErrorCodeClassifiesHTTPStatus(t *testing.T) {
	tests := map[int]string{
		http.StatusBadRequest:            "upstream_request_error",
		http.StatusNotFound:              "upstream_request_error",
		http.StatusMethodNotAllowed:      "upstream_request_error",
		http.StatusUnauthorized:          "upstream_auth_error",
		http.StatusForbidden:             "upstream_auth_error",
		http.StatusTooManyRequests:       "upstream_rate_limit",
		http.StatusRequestEntityTooLarge: "upstream_payload_too_large",
		http.StatusInternalServerError:   "provider_error",
		http.StatusBadGateway:            "provider_error",
		http.StatusOK:                    "",
	}
	for statusCode, want := range tests {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			if got := fallbackLogErrorCode(statusCode); got != want {
				t.Fatalf("fallbackLogErrorCode(%d) = %q, want %q", statusCode, got, want)
			}
		})
	}
}

func TestLoggingMiddlewareClassifiesHTTPErrorWithoutProxyHeaders(t *testing.T) {
	var buf strings.Builder
	st := store.New(10)
	server := &Server{store: st, logger: log.New(&buf, log.LevelInfo, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad json", http.StatusBadRequest)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	req.Header.Set("X-Request-ID", "req-bad-json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	if !strings.Contains(out, "error_code=upstream_request_error") || !strings.Contains(out, `reason="上游不接受本次请求"`) {
		t.Fatalf("server log = %q, want request-error diagnostics", out)
	}
	logs := st.GetLogs(1)
	if len(logs) != 1 || logs[0].ErrorCode != "upstream_request_error" || logs[0].ErrorReason != "上游不接受本次请求" {
		t.Fatalf("store diagnostics = %#v, want upstream_request_error", logs)
	}
}

func TestLoggingMiddlewareClassifiesUnauthorizedWithoutProxyHeaders(t *testing.T) {
	var buf strings.Builder
	st := store.New(10)
	server := &Server{proxyKey: "secret", store: st, logger: log.New(&buf, log.LevelInfo, false)}
	handler := server.loggingMiddleware(server.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Request-ID", "req-unauthorized")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	if !strings.Contains(out, "error_code=upstream_auth_error") || !strings.Contains(out, `reason="上游鉴权失败"`) {
		t.Fatalf("server log = %q, want auth diagnostics", out)
	}
	logs := st.GetLogs(1)
	if len(logs) != 1 || logs[0].ErrorCode != "upstream_auth_error" || logs[0].ErrorReason != "上游鉴权失败" {
		t.Fatalf("store diagnostics = %#v, want upstream_auth_error", logs)
	}
}

func TestConsoleDiagnosticSuffixRedactsAndSingleLinesValues(t *testing.T) {
	diag := logDiagnosticSummary{
		Reason:  "上游服务异常\nsecond line",
		Action:  "检查 API key sk-testsecret1234567890",
		Summary: "Bearer secret-token-value-123456\r\nsummary",
	}
	suffix := consoleDiagnosticSuffix(diag, "", 0, 0)

	if strings.Contains(suffix, "sk-testsecret") || strings.Contains(suffix, "secret-token-value") {
		t.Fatalf("suffix leaked secret: %q", suffix)
	}
	if strings.Contains(suffix, "\n") || strings.Contains(suffix, "\r") {
		t.Fatalf("suffix contains line break: %q", suffix)
	}
	if !strings.Contains(suffix, "<redacted>") {
		t.Fatalf("suffix should contain redaction marker: %q", suffix)
	}
}

func TestRequestIDFromRequestFallsBackWhenHeaderSanitizesToEmpty(t *testing.T) {
	start := time.Unix(0, 12345)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("X-Request-ID", "中文请求")

	if got := requestIDFromRequest(req, start); got != "12345" {
		t.Fatalf("request id = %q, want timestamp fallback", got)
	}
}

func TestLoggingMiddlewareStoresStructuredDiagnostics(t *testing.T) {
	st := store.New(10)
	server := &Server{store: st, logger: log.New(nil, log.LevelError, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Proxy-Provider", "useai")
		w.Header().Set("X-Proxy-Requested-Model", "UseAI - gpt-5.5")
		w.Header().Set("X-Proxy-Upstream-Model", "gpt-5.5")
		w.Header().Set("X-Proxy-Error-Code", "upstream_server_error")
		w.Header().Set("X-Proxy-Error-Message", "当前提供商请求失败")
		w.Header().Set("X-Proxy-Error-Hint", "检查 new-api/sub2api 渠道健康度。")
		w.Header().Set("X-Proxy-Attempts-Summary", "useai/gpt-5.5 13s upstream_server_error")
		w.WriteHeader(http.StatusBadGateway)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	logs := st.GetLogs(1)
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	entry := logs[0]
	if entry.AttemptsSummary != "useai/gpt-5.5 13s upstream_server_error" {
		t.Fatalf("attempts summary = %q", entry.AttemptsSummary)
	}
	if entry.ErrorReason != "上游服务暂不可用" {
		t.Fatalf("error reason = %q, want 上游服务暂不可用", entry.ErrorReason)
	}
	if entry.ErrorAction != "稍后重试，或切换模型。" || !strings.Contains(entry.DiagnosticSummary, "5xx") {
		t.Fatalf("diagnostic fields missing useful guidance: %#v", entry)
	}
}

func TestSetUpstreamRequestBytesRecordsSerializedProviderBodySize(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec}
	req := &provider.ChatRequest{Model: "gpt-test", Messages: []provider.Message{{Role: "user", Content: "hi"}}, Stream: true}

	setUpstreamRequestBytes(rw, req)

	if rw.upstreamBytes <= 0 {
		t.Fatalf("upstream bytes = %d, want positive", rw.upstreamBytes)
	}
}

func TestLoggingMiddlewareMarksClientGoneAfterPartialStream(t *testing.T) {
	st := store.New(10)
	server := &Server{store: st, logger: log.New(nil, log.LevelError, false)}
	ctx, cancel := context.WithCancel(context.Background())
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: partial\n\n"))
		cancel()
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	logs := st.GetLogs(1)
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	if logs[0].StatusCode != 499 || logs[0].IsSuccess {
		t.Fatalf("log status = %d success=%v, want 499 false", logs[0].StatusCode, logs[0].IsSuccess)
	}
	if logs[0].ErrorCode != "client_gone" {
		t.Fatalf("error_code = %q, want client_gone", logs[0].ErrorCode)
	}
}

func TestEnrichClientGoneDiagnosticsNearClientDeadline(t *testing.T) {
	code, message, hint := enrichClientGoneDiagnostics(499, 99_995, 1_075_855, "client_gone", "客户端取消", "原始提示")

	if code != "client_deadline_reached" {
		t.Fatalf("code = %q, want client_deadline_reached", code)
	}
	if message != "客户端等待超时。" {
		t.Fatalf("message = %q, want concise deadline", message)
	}
	if hint != "减少会话内容，或切换响应更快的模型。" {
		t.Fatalf("hint = %q, want concise recovery", hint)
	}
}

func TestEnrichClientGoneDiagnosticsKeepsShortCancelAsClientGone(t *testing.T) {
	code, message, hint := enrichClientGoneDiagnostics(499, 18_000, 1024, "client_gone", "客户端取消", "原始提示")

	if code != "client_gone" || message != "客户端取消" || hint != "原始提示" {
		t.Fatalf("short cancel should remain unchanged: code=%q message=%q hint=%q", code, message, hint)
	}
}

func TestRequestCancelReasonDistinguishesDeadlineAndShortCancel(t *testing.T) {
	if got := requestCancelReason(499, 100_043, "client_deadline_reached", context.Canceled); got != "client_deadline_reached" {
		t.Fatalf("deadline reason = %q", got)
	}
	if got := requestCancelReason(499, 18_000, "client_gone", context.Canceled); got != "client_canceled" {
		t.Fatalf("short cancel reason = %q", got)
	}
	if got := requestCancelReason(502, 18_000, "network_error", nil); got != "" {
		t.Fatalf("non-499 reason = %q, want empty", got)
	}
}

func TestLoggingMiddlewareCapturesToolDiagnosticsWithoutArguments(t *testing.T) {
	st := store.New(10)
	server := &Server{store: st, logger: log.New(nil, log.LevelError, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setProxyDiagnosticHeader(w, "X-Proxy-Request-Tools", "declared: git,powershell")
		setProxyDiagnosticHeader(w, "X-Proxy-Response-Tools", "returned: powershell")
		setProxyFallbackMode(w, "stream-to-nonstream")
		setProxyToolNormalization(w, "dsml")
		setProxyStreamState(w, "upstream_connected")
		setProxyDiagnosticHeader(w, "X-Proxy-Network-Peer", "104.21.57.81:443")
		setTimeoutDiagnostic(w, 300, 90)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	logs := st.GetLogs(1)
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	logEntry := logs[0]
	if logEntry.RequestTools != "declared: git,powershell" || logEntry.ResponseTools != "returned: powershell" {
		t.Fatalf("tool diagnostics missing: %#v", logEntry)
	}
	if logEntry.FallbackMode != "stream-to-nonstream" || logEntry.Normalization != "dsml" {
		t.Fatalf("recovery diagnostics missing: %#v", logEntry)
	}
	if logEntry.StreamState != "upstream_connected" || logEntry.NetworkPeer != "104.21.57.81:443" {
		t.Fatalf("transport diagnostics missing: %#v", logEntry)
	}
	if logEntry.ConfiguredTimeoutSeconds != 300 || logEntry.EffectiveTimeoutSeconds != 90 {
		t.Fatalf("timeout diagnostics missing: %#v", logEntry)
	}
	if strings.Contains(logEntry.RequestTools+logEntry.ResponseTools, "Get-ChildItem") {
		t.Fatalf("tool diagnostics must not include command arguments: %#v", logEntry)
	}
}

func TestBuildRegistryUsesConfiguredProviderIDs(t *testing.T) {
	server := testRegistryServer()
	registry := server.buildRegistry(&config.AppConfig{
		DefaultModel: "model-a",
		Providers: []config.ProviderConfig{{
			ID:      "useai-paid",
			Name:    "UseAI Paid",
			BaseURL: "https://api.eforge.xyz/v1",
			Type:    "openai",
			Enabled: true,
		}},
	})

	if !containsString(registry.ProviderNames(), "useai-paid") {
		t.Fatalf("providers = %#v, want useai-paid", registry.ProviderNames())
	}
	if containsString(registry.ProviderNames(), "UseAI Paid") {
		t.Fatalf("providers = %#v, registry should use stable provider id, not display name", registry.ProviderNames())
	}
}

func TestBuildRegistryDoesNotRegisterProviderFromEnvironment(t *testing.T) {
	t.Setenv("PROVIDER_DEEPSEEK_API_KEY", "env-key")
	t.Setenv("PROVIDER_OLLAMA_BASE_URL", "http://127.0.0.1:11434")
	t.Setenv("DEEPSEEK_API_KEY", "legacy-key")

	server := testRegistryServer()
	registry := server.buildRegistry(&config.AppConfig{DefaultModel: "model-a"})

	if got := registry.ProviderNames(); len(got) != 0 {
		t.Fatalf("providers = %#v, want none because provider env discovery is intentionally disabled", got)
	}
}

func TestServerConfigDirFollowsConfigManagerPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	mgr, err := config.NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	server := NewServer(mgr.Get(), mgr, store.New(10), log.New(nil, log.LevelError, false))

	if got, want := server.configDir(), filepath.Dir(path); got != want {
		t.Fatalf("configDir() = %q, want %q", got, want)
	}
}

func TestModelTimeoutSecondsUsesSafeDefaultBudget(t *testing.T) {
	configured, effective := modelTimeoutSeconds(&config.AppConfig{}, "gpt-test", "gpt-test", "useai", provider.ModelProfile{}, false)
	if configured != defaultModelTimeoutSeconds || effective != config.DefaultClientTimeoutBudgetSeconds {
		t.Fatalf("timeout = configured:%d effective:%d, want configured:%d effective:%d", configured, effective, defaultModelTimeoutSeconds, config.DefaultClientTimeoutBudgetSeconds)
	}
}

func TestModelTimeoutSecondsCapsLongProfileBeforeClientDeadline(t *testing.T) {
	longTimeout := 300
	profile := provider.ModelProfile{TimeoutSeconds: &longTimeout}

	configured, effective := modelTimeoutSeconds(&config.AppConfig{}, "gpt-5.5", "gpt-5.5", "useai2", profile, true)
	if configured != longTimeout || effective != config.DefaultClientTimeoutBudgetSeconds {
		t.Fatalf("timeout = configured:%d effective:%d, want configured:%d effective:%d", configured, effective, longTimeout, config.DefaultClientTimeoutBudgetSeconds)
	}
}

func TestModelTimeoutSecondsPreservesShortModelOverride(t *testing.T) {
	shortTimeout := 25
	cfg := &config.AppConfig{Models: []config.ModelConfig{{Name: "step-router-v1", ProviderID: "useai", TimeoutSeconds: &shortTimeout, Enabled: true}}}

	configured, effective := modelTimeoutSeconds(cfg, "step-router-v1", "step-router-v1", "useai", provider.ModelProfile{}, false)
	if configured != shortTimeout || effective != shortTimeout {
		t.Fatalf("timeout = configured:%d effective:%d, want %d", configured, effective, shortTimeout)
	}
}

func TestModelTimeoutSecondsUsesConfiguredClientBudget(t *testing.T) {
	budget := 60
	cfg := &config.AppConfig{Defense: config.DefenseConfig{ClientTimeoutBudgetSeconds: &budget}}

	configured, effective := modelTimeoutSeconds(cfg, "gpt-5.5", "gpt-5.5", "useai", provider.ModelProfile{}, false)
	if configured != defaultModelTimeoutSeconds || effective != budget {
		t.Fatalf("timeout = configured:%d effective:%d, want configured:%d effective:%d", configured, effective, defaultModelTimeoutSeconds, budget)
	}
}

func TestModelTimeoutSecondsPrefersExplicitModelConfig(t *testing.T) {
	modelTimeout := 25
	profileTimeout := 120
	cfg := &config.AppConfig{Models: []config.ModelConfig{{
		Name:           "gpt-5.4",
		ProviderID:     "useai2",
		TimeoutSeconds: &modelTimeout,
		Enabled:        true,
	}}}
	profile := provider.ModelProfile{TimeoutSeconds: &profileTimeout}

	configured, effective := modelTimeoutSeconds(
		cfg,
		"gpt-5.4",
		"gpt-5.4",
		"useai2",
		profile,
		true,
	)
	if configured != modelTimeout || effective != modelTimeout {
		t.Fatalf("timeout = configured:%d effective:%d, want explicit %d", configured, effective, modelTimeout)
	}
}

func TestProfileForProviderFallsBackToCapabilityProfile(t *testing.T) {
	dir := t.TempDir()
	selectionDir := filepath.Join(dir, "model-selection")
	if err := os.MkdirAll(selectionDir, 0755); err != nil {
		t.Fatalf("mkdir model-selection: %v", err)
	}
	if err := os.WriteFile(filepath.Join(selectionDir, "openai.json"), []byte(`{
		"provider":"openai",
		"models":[{
			"match":"gpt-5",
			"priority":1,
			"enabled":true,
			"execution":{"timeout_seconds":120,"max_tokens":8192}
		}]
	}`), 0644); err != nil {
		t.Fatalf("write model selection: %v", err)
	}

	registry := provider.NewRegistry("gpt-5.5", time.Minute)
	prov := provider.NewOpenAIProviderWithCapability("useai2", "useai", "sk-test", "https://api.eforge.xyz/v1", true, time.Second)
	registry.Add(&provider.ProviderEntry{Provider: prov, Models: []string{"gpt-5.5"}, Priority: 1})
	registry.SetModels("useai2", []string{"gpt-5.5"})
	catalog := provider.NewModelCatalog(registry, dir, time.Minute)

	profile, ok := profileForProvider(catalog, "gpt-5.5", prov)
	if !ok {
		t.Fatalf("expected gpt profile for useai2 capability fallback")
	}
	if profile.TimeoutSeconds == nil || *profile.TimeoutSeconds != 120 {
		t.Fatalf("timeout_seconds = %v, want 120 from openai gpt-5 profile", profile.TimeoutSeconds)
	}
}

func TestProfileForProviderKeepsExactProviderSelectionHighestPriority(t *testing.T) {
	dir := t.TempDir()
	selectionDir := filepath.Join(dir, "model-selection")
	if err := os.MkdirAll(selectionDir, 0755); err != nil {
		t.Fatalf("mkdir model-selection: %v", err)
	}
	if err := os.WriteFile(filepath.Join(selectionDir, "useai2.json"), []byte(`{
		"provider":"useai2",
		"models":[{
			"match":"gpt-5.5",
			"priority":1,
			"enabled":true,
			"execution":{"context_length":222222,"timeout_seconds":25}
		}]
	}`), 0644); err != nil {
		t.Fatalf("write model selection: %v", err)
	}

	registry := provider.NewRegistry("gpt-5.5", time.Minute)
	prov := provider.NewOpenAIProviderWithCapability("useai2", "useai", "sk-test", "https://api.eforge.xyz/v1", true, time.Second)
	registry.Add(&provider.ProviderEntry{Provider: prov, Models: []string{"gpt-5.5"}, Priority: 1})
	registry.SetModels("useai2", []string{"gpt-5.5"})
	catalog := provider.NewModelCatalog(registry, dir, time.Minute)

	profile, ok := profileForProvider(catalog, "gpt-5.5", prov)
	if !ok {
		t.Fatalf("expected profile")
	}
	if profile.ContextLength == nil || *profile.ContextLength != 222222 {
		t.Fatalf("context_length = %v, want exact provider 222222", profile.ContextLength)
	}
	if profile.TimeoutSeconds == nil || *profile.TimeoutSeconds != 25 {
		t.Fatalf("timeout_seconds = %v, want exact provider 25", profile.TimeoutSeconds)
	}
}

func TestStreamOpenAIToOllamaWritesNDJSON(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()

	err := server.streamOpenAIToOllama(
		rec,
		req,
		prov,
		&provider.ChatRequest{Model: "gpt-test"},
		rec,
	)
	if err != nil {
		t.Fatalf("streamOpenAIToOllama returned error: %v", err)
	}

	if contentType := rec.Header().Get("Content-Type"); contentType != "application/x-ndjson" {
		t.Fatalf("content type = %q, want application/x-ndjson", contentType)
	}
	body := rec.Body.String()
	if strings.Contains(body, "data:") {
		t.Fatalf("expected NDJSON without SSE data prefix, got %q", body)
	}
	if !strings.Contains(body, `"done":false`) || !strings.Contains(body, `"done":true`) {
		t.Fatalf("expected streaming and final chunks, got %q", body)
	}
}

func TestParseOpenAIStreamPayloadConvertsLegacyFunctionCall(t *testing.T) {
	chunk, err := parseOpenAIStreamPayload(`{"choices":[{"delta":{"function_call":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}},"finish_reason":null}]}`)
	if err != nil {
		t.Fatalf("parseOpenAIStreamPayload returned error: %v", err)
	}
	if len(chunk.ToolCalls) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(chunk.ToolCalls))
	}
	call, _ := chunk.ToolCalls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "powershell" {
		t.Fatalf("function call not converted: %#v", chunk.ToolCalls)
	}
}

func TestParseOpenAIStreamPayloadConvertsLegacyGitFunctionCall(t *testing.T) {
	chunk, err := parseOpenAIStreamPayload(`{"choices":[{"delta":{"function_call":{"name":"git","arguments":"{\"args\":[\"status\",\"--short\"],\"cwd\":\"C:\\\\repo\"}"}},"finish_reason":null}]}`)
	if err != nil {
		t.Fatalf("parseOpenAIStreamPayload returned error: %v", err)
	}
	if len(chunk.ToolCalls) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(chunk.ToolCalls))
	}
	call, _ := chunk.ToolCalls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "git" || fn["arguments"] != `{"args":["status","--short"],"cwd":"C:\\repo"}` {
		t.Fatalf("legacy git function call not converted: %#v", chunk.ToolCalls)
	}
}

func TestStreamOpenAIToOllamaPreservesToolCallArgumentChunks(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()

	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "create_file"}}},
	}
	err := server.streamOpenAIToOllama(rec, req, prov, chatReq, rec)
	if err != nil {
		t.Fatalf("streamOpenAIToOllama returned error: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("tool call chunks missing from Ollama stream: %s", body)
	}
	if !strings.Contains(body, `"arguments":"{\"path\":"`) || !strings.Contains(body, `"arguments":"\"a.txt\"}"`) {
		t.Fatalf("tool call argument chunks were not preserved: %s", body)
	}
	if !strings.Contains(body, `"done_reason":"tool_calls"`) {
		t.Fatalf("tool_calls finish reason missing: %s", body)
	}
}

func TestStreamOpenAIToOllamaPassesThroughUndeclaredToolContinuation(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Remove-Item\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "grep_search"}}},
	}

	if err := server.streamOpenAIToOllama(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("streamOpenAIToOllama returned error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "powershell") || !strings.Contains(body, "Remove-Item") {
		t.Fatalf("undeclared tool continuation should pass through to Ollama client in stable mode: %s", body)
	}
	if strings.Contains(body, "Proxy blocked undeclared tool calls") || strings.Contains(body, "<empty>") {
		t.Fatalf("stable mode must not inject block notice: %s", body)
	}
}

func TestStreamOllamaPassthroughWritesNDJSON(t *testing.T) {
	stream := `{"model":"llama","message":{"role":"assistant","content":"hi"},"done":false}` + "\n"
	server := &Server{}
	prov := &fakeStreamProvider{name: "ollama", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()

	err := server.streamOllamaPassthrough(
		rec,
		req,
		prov,
		&provider.ChatRequest{Model: "llama"},
		rec,
	)
	if err != nil {
		t.Fatalf("streamOllamaPassthrough returned error: %v", err)
	}

	if contentType := rec.Header().Get("Content-Type"); contentType != "application/x-ndjson" {
		t.Fatalf("content type = %q, want application/x-ndjson", contentType)
	}
	if rec.Body.String() != stream {
		t.Fatalf("body = %q, want %q", rec.Body.String(), stream)
	}
}

func TestStreamOpenAIHandlesLargeSSELine(t *testing.T) {
	largeContent := strings.Repeat("x", 80*1024)
	stream := `data: {"choices":[{"delta":{"content":"` + largeContent + `"},"finish_reason":null}]}` + "\n" +
		`data: [DONE]` + "\n"
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	err := server.streamOpenAI(rec, req, prov, &provider.ChatRequest{Model: "gpt-test"}, rec)
	if err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), largeContent) {
		t.Fatalf("large content was not preserved")
	}
}

func TestStreamOpenAINormalizesBlankFinishReasonForVisualStudio(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":""}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	err := server.streamOpenAI(rec, req, prov, &provider.ChatRequest{Model: "gpt-test"}, rec)
	if err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}

	body := rec.Body.String()
	if strings.Contains(body, `"finish_reason":""`) {
		t.Fatalf("blank finish_reason leaked to Visual Studio stream: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason was not normalized to stop: %s", body)
	}
}

func TestStreamOpenAIPreservesDeclaredToolCallContinuationChunks(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_grep","type":"function","function":{"name":"grep_search","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"needle\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "grep_search"}}},
	}

	if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Proxy blocked undeclared tool calls") || strings.Contains(body, "<empty>") {
		t.Fatalf("declared stream tool continuation was incorrectly blocked: %s", body)
	}
	if !strings.Contains(body, `"name":"grep_search"`) || !strings.Contains(body, `"arguments":"\"needle\"}"`) {
		t.Fatalf("declared stream tool chunks were not preserved: %s", body)
	}
}

func TestStreamOpenAIPassesThroughUndeclaredToolContinuation(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Remove-Item\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "grep_search"}}},
	}

	if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"powershell"`) || !strings.Contains(body, "Remove-Item") {
		t.Fatalf("undeclared tool continuation should pass through in stable mode: %s", body)
	}
	if strings.Contains(body, "Proxy blocked undeclared tool calls") || strings.Contains(body, "<empty>") {
		t.Fatalf("stable mode must not inject block notice: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason should remain tool_calls in stable mode: %s", body)
	}
}

func TestStreamOpenAIConvertsSuccessfulDSMLStreamToToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"<｜DSML｜tool_calls> <｜DSML｜invoke name=\"get_file\">"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":" <｜DSML｜parameter name=\"filename\" string=\"true\">a.cs</｜DSML｜parameter> </｜DSML｜invoke> </｜DSML｜tool_calls>"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "step-router-v1",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "get_file"}}},
	}

	err := server.streamOpenAI(rec, req, prov, chatReq, rec)
	if err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<｜DSML｜") || !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"name":"get_file"`) {
		t.Fatalf("DSML stream was not normalized: %s", body)
	}
	if got := rec.Header().Get("X-Proxy-Tool-Call-Normalization"); got != "dsml" {
		t.Fatalf("normalization header = %q, want dsml", got)
	}
}

func TestStreamOpenAIProbePreservesOrdinarySSE(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "get_file"}}},
	}

	if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	want := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	if rec.Body.String() != want {
		t.Fatalf("ordinary SSE was not normalized to complete events during DSML probe:\n got: %q\nwant: %q", rec.Body.String(), want)
	}
	if got := rec.Header().Get("X-Proxy-Tool-Call-Normalization"); got != "" {
		t.Fatalf("ordinary stream should not report DSML normalization: %q", got)
	}
}

func TestStreamOpenAILeavesUndeclaredDSMLAsText(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"<｜DSML｜tool_calls><｜DSML｜invoke name=\"delete_file\"></｜DSML｜invoke></｜DSML｜tool_calls>"},"finish_reason":null}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "get_file"}}},
	}

	if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	want := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"<｜DSML｜tool_calls><｜DSML｜invoke name=\"delete_file\"></｜DSML｜invoke></｜DSML｜tool_calls>"},"finish_reason":null}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	if rec.Body.String() != want {
		t.Fatalf("undeclared DSML text changed beyond SSE framing: got %q want %q", rec.Body.String(), want)
	}
	if got := rec.Header().Get("X-Proxy-Tool-Call-Normalization"); got != "" {
		t.Fatalf("rejected DSML must not report normalization: %q", got)
	}
}

func TestStreamOllamaPassthroughHandlesLargeNDJSONLine(t *testing.T) {
	largeContent := strings.Repeat("x", 80*1024)
	stream := `{"model":"llama","message":{"role":"assistant","content":"` + largeContent + `"},"done":false}` + "\n"
	server := &Server{}
	prov := &fakeStreamProvider{name: "ollama", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()

	err := server.streamOllamaPassthrough(rec, req, prov, &provider.ChatRequest{Model: "llama"}, rec)
	if err != nil {
		t.Fatalf("streamOllamaPassthrough returned error: %v", err)
	}
	if rec.Body.String() != stream {
		t.Fatalf("large NDJSON line was not preserved")
	}
}

type fakeStreamProvider struct {
	name string
	body string
}

func (p *fakeStreamProvider) Name() string {
	return p.name
}

func (p *fakeStreamProvider) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("not implemented")
}

func (p *fakeStreamProvider) ChatStream(context.Context, *provider.ChatRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(p.body)), nil
}

func (p *fakeStreamProvider) ListModels(context.Context) ([]string, error) {
	return []string{}, nil
}

func (p *fakeStreamProvider) IsEnabled() bool {
	return true
}

func testRegistryServer() *Server {
	return &Server{logger: log.New(nil, log.LevelError, false)}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
