package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

type testUpstreamHTTPError struct {
	status int
	body   string
}

func (e testUpstreamHTTPError) Error() string                 { return "upstream request failed" }
func (e testUpstreamHTTPError) UpstreamHTTPStatusCode() int   { return e.status }
func (e testUpstreamHTTPError) UpstreamHTTPErrorBody() []byte { return []byte(e.body) }

type testUpstreamAttemptsError struct {
	message  string
	attempts []provider.UpstreamAttempt
}

func (e testUpstreamAttemptsError) Error() string { return e.message }

func (e testUpstreamAttemptsError) UpstreamAttempts() []provider.UpstreamAttempt {
	return append([]provider.UpstreamAttempt(nil), e.attempts...)
}

func TestSanitizeDiagnosticMessageRedactsSecretsAndTruncates(t *testing.T) {
	message := "request failed: Bearer secret-token-value-123456 api_key:secret-api-key-123456 sk-testsecret1234567890 " +
		strings.Repeat("x", maxDiagnosticMessageBytes+20)

	got := sanitizeDiagnosticMessage(message)

	if strings.Contains(got, "secret-token-value") ||
		strings.Contains(got, "secret-api-key") ||
		strings.Contains(got, "sk-testsecret") {
		t.Fatalf("diagnostic message leaked secret: %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("diagnostic message = %q, want redaction marker", got)
	}
	if len(got) > maxDiagnosticMessageBytes+len("...<truncated>") {
		t.Fatalf("diagnostic message len = %d, want bounded", len(got))
	}
}

func TestClassifyProxyErrorDistinguishesNetworkAndUpstreamStatus(t *testing.T) {
	tests := map[string]string{
		`请求失败: dial tcp: connect: connection refused`: "network_error",
		`请求失败: Post "https://api.eforge.xyz/v1/chat/completions": write tcp 192.168.1.11:57874->104.21.57.81:443: use of closed network connection`: "network_error",
		`openai stream error: API 错误 401`:                                          "upstream_auth_error",
		`ollama stream error: Ollama 错误 403`:                                       "upstream_auth_error",
		`openai stream error: API 错误 400`:                                          "upstream_request_error",
		`openai stream error: API 错误 400: unsupported parameter; context canceled`: "upstream_request_error",
		`openai stream error: API 错误 404`:                                          "upstream_request_error",
		`openai stream error: API 错误 413`:                                          "upstream_payload_too_large",
		`openai stream error: API 错误 413: payload too large; context canceled`:     "upstream_payload_too_large",
		`openai stream error: API 错误 429`:                                          "upstream_rate_limit",
		`openai stream error: API 错误 503`:                                          "upstream_server_error",
		`openai stream error: API 错误 403: {"error":{"message":"You've reached your usage limit for this billing cycle","type":"access_terminated_error"}}`:           "upstream_quota_exhausted",
		`openai stream error: API 错误 400: {"error":{"message":"the message at position 39 with role 'assistant' must not be empty","type":"invalid_request_error"}}`: "upstream_message_error",
		`openai stream error: context canceled`: "client_gone",
		`client_gone`:                           "client_gone",
		`context deadline exceeded`:             "timeout",
		`解析响应失败: invalid character`:             "proxy_parse_error",
	}

	for message, want := range tests {
		if got := classifyProxyError(message); got != want {
			t.Fatalf("%q classified as %q, want %q", message, got, want)
		}
	}
}

func TestNewAttemptDiagnosticUsesStructuredUpstreamErrorDetails(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "billing quota in 403 body",
			err: testUpstreamHTTPError{
				status: http.StatusForbidden,
				body:   `{"error":{"message":"You've reached your usage limit for this billing cycle","type":"access_terminated_error"}}`,
			},
			want: "upstream_quota_exhausted",
		},
		{
			name: "empty assistant history message",
			err: testUpstreamHTTPError{
				status: http.StatusBadRequest,
				body:   `{"error":{"message":"the message at position 39 with role 'assistant' must not be empty","type":"invalid_request_error"}}`,
			},
			want: "upstream_message_error",
		},
		{
			name: "ordinary forbidden remains auth",
			err:  testUpstreamHTTPError{status: http.StatusForbidden, body: `{"error":{"message":"forbidden"}}`},
			want: "upstream_auth_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt := newAttemptDiagnostic("test", "model", 10, tt.err)
			if attempt.Category != tt.want {
				t.Fatalf("category = %q, want %q", attempt.Category, tt.want)
			}
		})
	}
}

func TestUserFacingDiagnosticCopyIsConcise(t *testing.T) {
	tests := []struct {
		code       string
		wantReason string
		wantAction string
	}{
		{code: "upstream_quota_exhausted", wantReason: "上游额度已用完", wantAction: "等待额度刷新，或充值/升级套餐。"},
		{code: "upstream_message_error", wantReason: "会话消息无效", wantAction: "新建会话后重试。"},
		{code: "client_gone", wantReason: "客户端已取消", wantAction: "重新发送；若反复出现，请新建会话。"},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			diag := summarizeLogDiagnostic(tt.code, http.StatusBadGateway, 100, 0, 0, "", "", "", "")
			if diag.Reason != tt.wantReason || diag.Action != tt.wantAction {
				t.Fatalf("diagnostic = %#v, want reason=%q action=%q", diag, tt.wantReason, tt.wantAction)
			}
			if len([]rune(diag.Reason+diag.Action)) > 40 {
				t.Fatalf("user-facing copy is too long: %#v", diag)
			}
		})
	}
}

func TestClassifyProxyErrorPrioritizesExplicitHTTPStatusOverTransportText(t *testing.T) {
	tests := []struct {
		message string
		want    string
	}{
		{`openai stream error: API 错误 401: auth failed; context canceled`, "upstream_auth_error"},
		{`openai stream error: API 错误 403: forbidden; context deadline exceeded`, "upstream_auth_error"},
		{`openai stream error: API 错误 400: unsupported parameter; timeout`, "upstream_request_error"},
		{`openai stream error: API 错误 404: model_not_found; client_gone`, "upstream_request_error"},
		{`openai stream error: API 错误 413: payload too large; use of closed network connection`, "upstream_payload_too_large"},
		{`openai stream error: API 错误 429: rate limited; context canceled`, "upstream_rate_limit"},
		{`openai stream error: API 错误 500: upstream error; context canceled`, "upstream_server_error"},
		{`openai stream error: API 错误 503: unavailable; timeout`, "upstream_server_error"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := classifyProxyError(tt.message); got != tt.want {
				t.Fatalf("classifyProxyError(%q) = %q, want %q", tt.message, got, tt.want)
			}
		})
	}
}

func TestNewAttemptDiagnosticExtractsNetworkPeer(t *testing.T) {
	err := errors.New(`请求失败: Post "https://api.eforge.xyz/v1/chat/completions": write tcp 192.168.1.11:57874->104.21.57.81:443: use of closed network connection`)
	attempt := newAttemptDiagnostic("useai", "step-router-v1", 1234.5, err)

	if attempt.Category != "network_error" {
		t.Fatalf("category = %q, want network_error", attempt.Category)
	}
	if attempt.ElapsedMs != 1234.5 {
		t.Fatalf("elapsed = %v, want 1234.5", attempt.ElapsedMs)
	}
	if attempt.Peer != "104.21.57.81:443" {
		t.Fatalf("peer = %q, want 104.21.57.81:443", attempt.Peer)
	}
}

func TestNewAttemptDiagnosticKeepsElapsedMsForSlowCandidates(t *testing.T) {
	attempt := newAttemptDiagnostic("useai2", "gpt-5.5", 9876.5, errors.New("context deadline exceeded"))
	if attempt.ElapsedMs != 9876.5 {
		t.Fatalf("elapsed = %v, want 9876.5", attempt.ElapsedMs)
	}
	if attempt.Category != "timeout" {
		t.Fatalf("category = %q, want timeout", attempt.Category)
	}
}

func TestNetworkErrorHintKeepsPeerInStructuredAttempt(t *testing.T) {
	diag := allCandidatesFailedDiagnostic("UseAI - step-router-v1", "step-router-v1", 1, []attemptDiagnostic{{
		Provider:  "useai",
		Upstream:  "step-router-v1",
		Category:  "network_error",
		Message:   "use of closed network connection",
		ElapsedMs: 13227,
		Peer:      "104.21.57.81:443",
	}})

	if diag.Details.Hint != "检查网络和上游地址后重试。" {
		t.Fatalf("hint = %q, want concise recovery", diag.Details.Hint)
	}
	if len(diag.Details.Attempts) != 1 || diag.Details.Attempts[0].Peer != "104.21.57.81:443" {
		t.Fatalf("structured peer missing: %#v", diag.Details.Attempts)
	}
}

func TestAttemptsSummaryIncludesProviderModelElapsedAndCategory(t *testing.T) {
	summary := attemptsSummary([]attemptDiagnostic{
		{Provider: "useai", Upstream: "gpt-5.5", Category: "upstream_server_error", ElapsedMs: 654},
		{Provider: "useai2", Upstream: "gpt-5.5", Category: "client_deadline_reached", ElapsedMs: 99_995},
	})
	for _, want := range []string{"useai/gpt-5.5 654ms upstream_server_error", "useai2/gpt-5.5 100s client_deadline_reached"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary = %q, want contains %q", summary, want)
		}
	}
}

func TestWriteProxyDiagnosticErrorSetsAttemptsSummaryHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	diag := allCandidatesFailedDiagnostic("UseAI - gpt-5.5", "gpt-5.5", 1, []attemptDiagnostic{{
		Provider:  "useai",
		Upstream:  "gpt-5.5",
		Category:  "upstream_server_error",
		ElapsedMs: 654,
	}})

	writeProxyDiagnosticError(rec, http.StatusBadGateway, diag)

	if got := rec.Header().Get("X-Proxy-Attempts-Summary"); !strings.Contains(got, "useai/gpt-5.5 654ms upstream_server_error") {
		t.Fatalf("attempts summary header = %q", got)
	}
}

func TestNewAttemptDiagnosticIncludesStructuredUpstreamAttempts(t *testing.T) {
	err := testUpstreamAttemptsError{
		message: "请求失败: use of closed network connection",
		attempts: []provider.UpstreamAttempt{
			{
				Stage:   "writing_request",
				Elapsed: 120 * time.Millisecond,
				Error:   "write tcp 127.0.0.1:1->104.21.57.81:443: use of closed network connection",
			},
			{
				Stage:   "connecting",
				Elapsed: 220 * time.Millisecond,
				Error:   "dial tcp: connect: connection refused",
			},
		},
	}

	attempt := newAttemptDiagnostic("useai", "deepseek-v4-flash", 6500, err)

	if attempt.Category != "network_error" {
		t.Fatalf("category = %q, want network_error", attempt.Category)
	}
	if len(attempt.UpstreamAttempts) != 2 {
		t.Fatalf("upstream attempts = %#v, want 2 attempts", attempt.UpstreamAttempts)
	}
	if attempt.UpstreamAttempts[0].Stage != "writing_request" {
		t.Fatalf("first upstream stage = %q, want writing_request", attempt.UpstreamAttempts[0].Stage)
	}
	summary := attemptsSummary([]attemptDiagnostic{attempt})
	if !strings.Contains(summary, "upstream_attempts=2 last=connecting/network_error") {
		t.Fatalf("attempts summary = %q, want structured upstream attempts", summary)
	}

	rec := httptest.NewRecorder()
	writeProxyDiagnosticError(
		rec,
		http.StatusBadGateway,
		allCandidatesFailedDiagnostic(
			"UseAI - deepseek-v4-flash",
			"deepseek-v4-flash",
			1,
			[]attemptDiagnostic{attempt},
		),
	)
	if got := rec.Header().Get("X-Proxy-Stream-State"); got != "upstream_connecting" {
		t.Fatalf("stream state = %q, want upstream_connecting", got)
	}
}

func TestWaitingForResponseHeadersIsReportedAsUpstreamNoResponse(t *testing.T) {
	err := testUpstreamAttemptsError{
		message: "请求失败: EOF",
		attempts: []provider.UpstreamAttempt{{
			Stage:   "waiting_response_headers",
			Elapsed: 3200 * time.Millisecond,
			Error:   "upstream_stage=waiting_response_headers: EOF",
		}},
	}

	attempt := newAttemptDiagnostic("useai", "deepseek-v4-flash", 3200, err)
	if attempt.Category != "upstream_no_response" {
		t.Fatalf("category = %q, want upstream_no_response", attempt.Category)
	}
	summary := attemptsSummary([]attemptDiagnostic{attempt})
	if !strings.Contains(summary, "last=waiting_response_headers/upstream_no_response") {
		t.Fatalf("attempts summary = %q, want response-header stage", summary)
	}
	diag := summarizeLogDiagnostic(
		attempt.Category,
		http.StatusBadGateway,
		3200,
		400008,
		400000,
		"",
		"upstream_waiting_response_headers",
		"",
		"",
	)
	if diag.Reason != "上游接收后未响应" {
		t.Fatalf("reason = %q, want 上游接收后未响应", diag.Reason)
	}
	if !strings.Contains(diag.Summary, "未返回响应头") {
		t.Fatalf("summary = %q, want response-header wording", diag.Summary)
	}
}

func TestUpstreamStreamInterruptedHasSpecificDiagnostic(t *testing.T) {
	attempt := newAttemptDiagnostic(
		"useai",
		"deepseek-v4-flash",
		5600,
		errors.New("上游流中断: OpenAI SSE: unexpected EOF"),
	)

	if attempt.Category != "upstream_stream_interrupted" {
		t.Fatalf("category = %q, want upstream_stream_interrupted", attempt.Category)
	}
	diag := summarizeLogDiagnostic(
		attempt.Category,
		http.StatusBadGateway,
		5600,
		400008,
		400000,
		"",
		"upstream_connected",
		"",
		"",
	)
	if diag.Reason != "上游响应流中断" {
		t.Fatalf("reason = %q, want 上游响应流中断", diag.Reason)
	}
	if !strings.Contains(diag.Summary, "响应流中断") {
		t.Fatalf("summary = %q, want stream interruption wording", diag.Summary)
	}
}

func TestMissingStreamTerminalStateClassifiesAsUpstreamInterrupted(t *testing.T) {
	for _, message := range []string{
		"解析响应失败: OpenAI SSE: OpenAI SSE 在 finish_reason 或 [DONE] 之前结束",
		"解析 Ollama 流失败: Ollama 流在 done=true 或 [DONE] 之前结束",
	} {
		attempt := newAttemptDiagnostic("useai", "deepseek-v4-flash", 123, errors.New(message))
		if attempt.Category != "upstream_stream_interrupted" {
			t.Fatalf("category = %q, want upstream_stream_interrupted; message=%s", attempt.Category, message)
		}
	}
}

func TestDiagnosticHeaderValueRemovesLineBreaksAndRedactsSecrets(t *testing.T) {
	got := diagnosticHeaderValue("API key sk-testsecret1234567890\nnext line")
	if strings.Contains(got, "\n") || strings.Contains(got, "\r") {
		t.Fatalf("header contains line break: %q", got)
	}
	if strings.Contains(got, "sk-testsecret") || !strings.Contains(got, "<redacted>") {
		t.Fatalf("header did not redact secret: %q", got)
	}
}

func TestNewAttemptDiagnosticClassifiesWrappedContextCanceledAsClientGone(t *testing.T) {
	err := errors.Join(context.Canceled, errors.New(`openai stream error: API 错误 500: {"error":{"code":"do_request_failed"}}`))
	attempt := newAttemptDiagnostic("useai", "gpt-5.5", 2345.6, err)
	if attempt.Category != "upstream_server_error" {
		t.Fatalf("category = %q, want upstream_server_error; message=%s", attempt.Category, attempt.Message)
	}
}

func TestDiagnosticHintCoversSpecificUpstreamCategories(t *testing.T) {
	for _, category := range []string{
		"upstream_quota_exhausted",
		"upstream_auth_error",
		"upstream_message_error",
		"upstream_payload_too_large",
		"upstream_rate_limit",
		"upstream_request_error",
		"upstream_server_error",
	} {
		if got := diagnosticHint(category); got == "" || got == diagnosticHint("provider_error") {
			t.Fatalf("hint for %s = %q, want specific non-default hint", category, got)
		}
	}
}

func TestPayloadTooLargeHintIsConcise(t *testing.T) {
	diag := allCandidatesFailedDiagnostic("UseAI - deepseek-v4-flash", "deepseek-v4-flash", 1, []attemptDiagnostic{{
		Provider: "useai",
		Upstream: "deepseek-v4-flash",
		Category: "upstream_payload_too_large",
		Message:  "API 错误 413",
	}})

	if diag.Message != "当前提供商请求失败" {
		t.Fatalf("single-candidate message = %q, want 当前提供商请求失败", diag.Message)
	}
	if diag.Details.Hint != "减少会话历史、文件或附件后重试。" {
		t.Fatalf("hint = %q, want concise recovery", diag.Details.Hint)
	}
}

func TestPayloadTooLargeKeepsProvidersInStructuredAttempts(t *testing.T) {
	diag := allCandidatesFailedDiagnostic("shared", "shared", 2, []attemptDiagnostic{
		{Provider: "useai", Upstream: "shared", Category: "upstream_payload_too_large", Message: "API 错误 413"},
		{Provider: "backup", Upstream: "shared", Category: "network_error", Message: "use of closed network connection"},
	})

	if diag.Code != "upstream_payload_too_large" {
		t.Fatalf("code = %q, want upstream_payload_too_large", diag.Code)
	}
	if len(diag.Details.Attempts) != 2 || diag.Details.Attempts[0].Provider != "useai" || diag.Details.Attempts[1].Provider != "backup" {
		t.Fatalf("structured attempts lost provider details: %#v", diag.Details.Attempts)
	}
}

func TestAllCandidatesFailedDiagnosticUsesMostActionableCategory(t *testing.T) {
	attempts := []attemptDiagnostic{
		{Provider: "useai", Upstream: "model-a", Category: "network_error", Message: "use of closed network connection"},
		{Provider: "usecpa", Upstream: "model-a", Category: "upstream_payload_too_large", Message: "API 错误 413"},
		{Provider: "backup", Upstream: "model-a", Category: "provider_error", Message: "unknown"},
	}

	diag := allCandidatesFailedDiagnostic("model-a", "model-a", 3, attempts)

	if diag.Code != "upstream_payload_too_large" {
		t.Fatalf("code = %q, want upstream_payload_too_large", diag.Code)
	}
	if !strings.Contains(diag.Details.Hint, "减少会话历史") {
		t.Fatalf("hint = %q, want payload guidance", diag.Details.Hint)
	}
}

func TestPrimaryFailureCategoryPrioritizesConfigurationErrors(t *testing.T) {
	tests := []struct {
		name     string
		attempts []attemptDiagnostic
		want     string
	}{
		{
			name: "request error beats later network error",
			attempts: []attemptDiagnostic{
				{Category: "network_error"},
				{Category: "upstream_request_error"},
			},
			want: "upstream_request_error",
		},
		{
			name: "rate limit beats server error",
			attempts: []attemptDiagnostic{
				{Category: "upstream_server_error"},
				{Category: "upstream_rate_limit"},
			},
			want: "upstream_rate_limit",
		},
		{
			name:     "empty attempts fallback",
			attempts: nil,
			want:     "provider_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := primaryFailureCategory(tt.attempts); got != tt.want {
				t.Fatalf("primaryFailureCategory = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSummarizeLogDiagnosticGivesOperatorReadyReasonAndAction(t *testing.T) {
	tests := []struct {
		code       string
		statusCode int
		elapsedMs  float64
		bytes      int64
		upstream   int64
		peer       string
		stream     string
		wantReason string
		wantAction string
		wantInSum  string
	}{
		{
			code:       "client_deadline_reached",
			statusCode: 499,
			elapsedMs:  99_995,
			bytes:      634_054,
			upstream:   642_181,
			wantReason: "客户端等待超时",
			wantAction: "减少会话内容",
			wantInSum:  "上游体 627.1 KB",
		},
		{
			code:       "upstream_payload_too_large",
			statusCode: 502,
			elapsedMs:  4_985,
			bytes:      1_132_802,
			wantReason: "请求内容过大",
			wantAction: "减少会话历史",
			wantInSum:  "上游拒绝大请求",
		},
		{
			code:       "network_error",
			statusCode: 502,
			elapsedMs:  13_227,
			peer:       "104.21.57.81:443",
			wantReason: "无法连接上游",
			wantAction: "检查网络",
			wantInSum:  "104.21.57.81:443",
		},
		{
			code:       "network_error",
			statusCode: 502,
			elapsedMs:  22_510,
			bytes:      634_054,
			upstream:   642_181,
			wantReason: "无法连接上游",
			wantAction: "检查网络",
			wantInSum:  "请求体 619.2 KB",
		},
		{
			code:       "upstream_request_error",
			statusCode: 502,
			elapsedMs:  11228,
			wantReason: "上游不接受本次请求",
			wantAction: "模型名",
			wantInSum:  "上游拒绝请求",
		},
		{
			code:       "upstream_rate_limit",
			statusCode: 502,
			elapsedMs:  1807,
			wantReason: "请求过于频繁",
			wantAction: "等待 15-30 秒",
			wantInSum:  "上游限流",
		},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			diag := summarizeLogDiagnostic(tt.code, tt.statusCode, tt.elapsedMs, tt.bytes, tt.upstream, tt.peer, tt.stream, "", "")
			if diag.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", diag.Reason, tt.wantReason)
			}
			if !strings.Contains(diag.Action, tt.wantAction) {
				t.Fatalf("action = %q, want contains %q", diag.Action, tt.wantAction)
			}
			if !strings.Contains(diag.Summary, tt.wantInSum) {
				t.Fatalf("summary = %q, want contains %q", diag.Summary, tt.wantInSum)
			}
		})
	}
}

func TestSummarizeLogDiagnosticLeavesSuccessfulRequestsEmpty(t *testing.T) {
	diag := summarizeLogDiagnostic("", 200, 12, 512, 128, "", "", "", "")
	if diag.Reason != "" || diag.Action != "" || diag.Summary != "" {
		t.Fatalf("success diagnostic = %#v, want empty", diag)
	}
}

func TestSummarizeLogDiagnosticDoesNotAttachContextPressureToUnrelatedErrors(t *testing.T) {
	for _, code := range []string{"upstream_auth_error", "upstream_request_error", "upstream_rate_limit"} {
		t.Run(code, func(t *testing.T) {
			diag := summarizeLogDiagnostic(code, 502, 1200, 2*1024*1024, 2*1024*1024, "", "", "", "")
			if strings.Contains(diag.Action, "旧 session") || strings.Contains(diag.Summary, "大上下文") {
				t.Fatalf("%s should not include context pressure hint: %#v", code, diag)
			}
		})
	}
}

func TestSummarizeLogDiagnosticDoesNotAddToolJargon(t *testing.T) {
	diag := summarizeLogDiagnostic(
		"client_gone",
		499,
		52_649,
		790*1024,
		824*1024,
		"",
		"downstream_started",
		"declared: adapt_plan,create_file,get_file",
		"",
	)

	if diag.Action != "重新发送；若反复出现，请新建会话。" {
		t.Fatalf("action = %q, want concise recovery", diag.Action)
	}
	for _, unwanted := range []string{"工具", "499/context canceled", "provider", "channel"} {
		if strings.Contains(diag.Action, unwanted) || strings.Contains(diag.Summary, unwanted) {
			t.Fatalf("diagnostic = %#v, contains jargon %q", diag, unwanted)
		}
	}
}

func TestSummarizeLogDiagnosticKeepsTimeoutActionConcise(t *testing.T) {
	diag := summarizeLogDiagnostic(
		"timeout",
		http.StatusBadGateway,
		89_000,
		790*1024,
		824*1024,
		"",
		"upstream_connected",
		"declared: create_file,get_file",
		"",
	)

	if diag.Reason != "上游响应超时" || diag.Action != "减少会话内容，或切换响应更快的模型。" {
		t.Fatalf("timeout diagnostic = %#v, want concise recovery", diag)
	}
}
