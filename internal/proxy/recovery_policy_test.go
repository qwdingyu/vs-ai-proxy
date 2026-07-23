package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// TestRecoveryBoundaryMatrix 用一张表锁住默认恢复边界：
// 哪些 error_code 必须停止候选切换，以及半截流后如何记内部失败。
func TestRecoveryBoundaryMatrix(t *testing.T) {
	t.Parallel()

	stopCases := []struct {
		category string
		wantStop bool
	}{
		{"client_gone", true},
		{"upstream_quota_exhausted", true},
		{"upstream_auth_error", true},
		{"upstream_rate_limit", true},
		{"upstream_payload_too_large", true},
		{"upstream_message_error", true},
		{"upstream_request_error", true},
		{"upstream_no_response", true},
		{"upstream_stream_interrupted", true},
		// 明确 5xx 仍可走配置内的恢复路径（例如候选健康排序后的下一次请求），
		// 但不得在已提交 POST 后无脑重放；这里只断言 stop 列表本身。
		{"upstream_server_error", false},
		{"network_error", false},
		{"", false},
	}
	for _, tc := range stopCases {
		tc := tc
		t.Run("stop/"+tc.category, func(t *testing.T) {
			t.Parallel()
			if got := shouldStopCandidateFallback(tc.category); got != tc.wantStop {
				t.Fatalf("shouldStopCandidateFallback(%q)=%v, want %v", tc.category, got, tc.wantStop)
			}
		})
	}

	// 半截流失败：客户端 HTTP 状态保持已写出状态，但内部 statusCode 必须变 502。
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}
	attempt := attemptDiagnostic{
		Provider:  "useai",
		Upstream:  "deepseek-v4-flash",
		Category:  "upstream_stream_interrupted",
		ElapsedMs: 12.5,
	}
	markWrittenStreamFailure(rw, attempt)
	if rw.statusCode != http.StatusBadGateway {
		t.Fatalf("internal statusCode=%d, want 502 after written stream failure", rw.statusCode)
	}
	if got := rw.Header().Get("X-Proxy-Error-Code"); got != "upstream_stream_interrupted" {
		t.Fatalf("X-Proxy-Error-Code=%q, want upstream_stream_interrupted", got)
	}
	// 尚未调用 WriteHeader 时 Recorder 默认仍是 200 语义；本函数刻意不 WriteHeader。
	if rec.Code != http.StatusOK && rec.Code != 0 {
		// httptest.ResponseWriter.Code 在未 WriteHeader 前为 200。
		t.Fatalf("client-facing recorder code unexpectedly changed to %d", rec.Code)
	}
}

func TestRequestLogIsSuccessRequiresEmptyErrorCode(t *testing.T) {
	t.Parallel()
	if !requestLogIsSuccess(http.StatusOK, "") {
		t.Fatal("200 without error_code must count as success")
	}
	if requestLogIsSuccess(http.StatusOK, "upstream_stream_interrupted") {
		t.Fatal("200 with explicit error_code must not count as success")
	}
	if requestLogIsSuccess(http.StatusBadGateway, "") {
		t.Fatal("502 must not count as success")
	}
	if requestLogIsSuccess(http.StatusBadGateway, "upstream_no_response") {
		t.Fatal("502 with error_code must not count as success")
	}
}

func TestShouldWarnLargeChatRequestThreshold(t *testing.T) {
	t.Parallel()
	if shouldWarnLargeChatRequest("/health", largeRequestWarnBytes, 0) {
		t.Fatal("non-chat path must not warn")
	}
	if shouldWarnLargeChatRequest("/v1/chat/completions", largeRequestWarnBytes-1, largeRequestWarnBytes-1) {
		t.Fatal("below threshold must not warn")
	}
	if !shouldWarnLargeChatRequest("/v1/chat/completions", largeRequestWarnBytes, 0) {
		t.Fatal("request bytes at threshold must warn")
	}
	if !shouldWarnLargeChatRequest("/api/chat", 0, largeRequestWarnBytes) {
		t.Fatal("upstream bytes at threshold must warn")
	}
}

func TestCanAttemptAlternateChatModeRequiresDefenseAndLiveClient(t *testing.T) {
	t.Parallel()
	enabled := true
	disabled := false
	cfgOn := &config.AppConfig{Defense: config.DefenseConfig{Enabled: &enabled}}
	cfgOff := &config.AppConfig{Defense: config.DefenseConfig{Enabled: &disabled}}
	// 简化 5xx 错误（无 UpstreamAttempts）历史上允许模式互切。
	err5xx := errors.New("api 错误 503 service unavailable")
	if !canAttemptAlternateChatMode(cfgOn, context.Background(), err5xx) {
		// 若当前 provider 策略已收紧到拒绝字符串启发式，则至少保证 Defense 关闭时一定拒绝。
		if provider.ShouldAttemptAlternateChatMode(err5xx) {
			t.Fatal("defense-on live client should allow alternate mode when provider allows")
		}
	}
	if canAttemptAlternateChatMode(cfgOff, context.Background(), err5xx) {
		t.Fatal("defense-off must never allow alternate chat mode")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if canAttemptAlternateChatMode(cfgOn, canceled, err5xx) {
		t.Fatal("canceled client must never allow alternate chat mode")
	}
}
