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
	if shouldWarnLargeChatRequest("/health", LargeRequestWarnBytes, 0) {
		t.Fatal("non-chat path must not warn")
	}
	if shouldWarnLargeChatRequest("/v1/chat/completions", LargeRequestWarnBytes-1, LargeRequestWarnBytes-1) {
		t.Fatal("below threshold must not warn")
	}
	if !shouldWarnLargeChatRequest("/v1/chat/completions", LargeRequestWarnBytes, 0) {
		t.Fatal("request bytes at threshold must warn")
	}
	if !shouldWarnLargeChatRequest("/api/chat", 0, LargeRequestWarnBytes) {
		t.Fatal("upstream bytes at threshold must warn")
	}
}

func TestObservedRequestBytesFallsBackToUpstreamSize(t *testing.T) {
	t.Parallel()
	if got := observedRequestBytes(1234, 9999); got != 1234 {
		t.Fatalf("prefer content-length: got %d", got)
	}
	if got := observedRequestBytes(-1, LargeRequestWarnBytes); got != LargeRequestWarnBytes {
		t.Fatalf("unknown content-length should use upstream bytes: got %d", got)
	}
	if got := observedRequestBytes(0, 512); got != 512 {
		t.Fatalf("zero content-length should use upstream bytes: got %d", got)
	}
	if got := observedRequestBytes(-1, 0); got != 0 {
		t.Fatalf("no sizes available should stay 0: got %d", got)
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


func TestAlternateChatModeFailureExposesBothErrorChains(t *testing.T) {
	t.Parallel()

	// 构造两个错误，各带不同的自定义标记类型，
	// 验证 errors.As 能从 alternateChatModeError.Unwrap() 的两个链中分别找到它们。
	type initialMarker struct{ error }
	type fallbackMarker struct{ error }

	initialErr := &initialMarker{errors.New("初始模式网络错误")}
	fallbackErr := &fallbackMarker{errors.New("备用模式超时")}

	wrapped := alternateChatModeFailure(initialErr, fallbackErr)
	if wrapped == nil {
		t.Fatal("alternateChatModeFailure must not return nil when both errors are non-nil")
	}
	if wrapped.Error() == "" {
		t.Fatal("alternateChatModeFailure Error() must not be empty")
	}

	// 验证 errors.As 能从 initial 链找到 initialMarker。
	var im *initialMarker
	if !errors.As(wrapped, &im) {
		t.Fatal("errors.As must find initialMarker from alternateChatModeError")
	}
	// 验证 errors.As 能从 fallback 链找到 fallbackMarker。
	var fm *fallbackMarker
	if !errors.As(wrapped, &fm) {
		t.Fatal("errors.As must find fallbackMarker from alternateChatModeError")
	}

	// 验证 nil fallback 只返回 initial。
	if got := alternateChatModeFailure(initialErr, nil); got != initialErr {
		t.Fatalf("alternateChatModeFailure(initialErr, nil) = %v, want initialErr", got)
	}
	// 验证 nil initial 只返回 fallback。
	if got := alternateChatModeFailure(nil, fallbackErr); got != fallbackErr {
		t.Fatalf("alternateChatModeFailure(nil, fallbackErr) = %v, want fallbackErr", got)
	}
	// 验证两个都 nil 返回 nil。
	if got := alternateChatModeFailure(nil, nil); got != nil {
		t.Fatalf("alternateChatModeFailure(nil, nil) = %v, want nil", got)
	}
}
