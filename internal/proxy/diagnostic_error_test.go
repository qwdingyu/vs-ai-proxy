package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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
		`openai stream error: API 错误 401`:       "upstream_auth_error",
		`ollama stream error: Ollama 错误 403`:    "upstream_auth_error",
		`openai stream error: API 错误 400`:       "upstream_request_error",
		`openai stream error: API 错误 404`:       "upstream_request_error",
		`openai stream error: API 错误 413`:       "upstream_payload_too_large",
		`openai stream error: API 错误 429`:       "upstream_rate_limit",
		`openai stream error: API 错误 503`:       "upstream_server_error",
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

func TestNewAttemptDiagnosticExtractsNetworkPeer(t *testing.T) {
	err := errors.New(`请求失败: Post "https://api.eforge.xyz/v1/chat/completions": write tcp 192.168.1.11:57874->104.21.57.81:443: use of closed network connection`)
	attempt := newAttemptDiagnostic("useai", "step-router-v1", err)

	if attempt.Category != "network_error" {
		t.Fatalf("category = %q, want network_error", attempt.Category)
	}
	if attempt.Peer != "104.21.57.81:443" {
		t.Fatalf("peer = %q, want 104.21.57.81:443", attempt.Peer)
	}
}

func TestNetworkErrorHintMentionsPeerWhenPresent(t *testing.T) {
	diag := allCandidatesFailedDiagnostic("UseAI - step-router-v1", "step-router-v1", 1, []attemptDiagnostic{{
		Provider: "useai",
		Upstream: "step-router-v1",
		Category: "network_error",
		Message:  "use of closed network connection",
		Peer:     "104.21.57.81:443",
	}})

	if !strings.Contains(diag.Details.Hint, "104.21.57.81:443") || !strings.Contains(diag.Details.Hint, "Cloudflare/CDN") {
		t.Fatalf("hint should explain network peer/CDN: %q", diag.Details.Hint)
	}
}

func TestNewAttemptDiagnosticClassifiesWrappedContextCanceledAsClientGone(t *testing.T) {
	err := errors.Join(context.Canceled, errors.New(`openai stream error: API 错误 500: {"error":{"code":"do_request_failed"}}`))
	attempt := newAttemptDiagnostic("useai", "gpt-5.5", err)
	if attempt.Category != "client_gone" {
		t.Fatalf("category = %q, want client_gone; message=%s", attempt.Category, attempt.Message)
	}
}

func TestDiagnosticHintCoversSpecificUpstreamCategories(t *testing.T) {
	for _, category := range []string{
		"upstream_auth_error",
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

func TestPayloadTooLargeHintMentionsProviderSpecificLimit(t *testing.T) {
	diag := allCandidatesFailedDiagnostic("UseAI - deepseek-v4-flash", "deepseek-v4-flash", 1, []attemptDiagnostic{{
		Provider: "useai",
		Upstream: "deepseek-v4-flash",
		Category: "upstream_payload_too_large",
		Message:  "API 错误 413",
	}})

	if diag.Message != "当前提供商请求失败" {
		t.Fatalf("single-candidate message = %q, want 当前提供商请求失败", diag.Message)
	}
	if !strings.Contains(diag.Details.Hint, "不是代理或 nginx 全局限制") {
		t.Fatalf("hint should avoid global nginx misdiagnosis: %q", diag.Details.Hint)
	}
	if !strings.Contains(diag.Details.Hint, "useai/deepseek-v4-flash") {
		t.Fatalf("hint should identify provider/model: %q", diag.Details.Hint)
	}
}
