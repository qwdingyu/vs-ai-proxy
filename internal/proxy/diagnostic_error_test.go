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
		`openai stream error: API 错误 401`:             "upstream_auth_error",
		`ollama stream error: Ollama 错误 403`:          "upstream_auth_error",
		`openai stream error: API 错误 400`:             "upstream_request_error",
		`openai stream error: API 错误 404`:             "upstream_request_error",
		`openai stream error: API 错误 413`:             "upstream_payload_too_large",
		`openai stream error: API 错误 429`:             "upstream_rate_limit",
		`openai stream error: API 错误 503`:             "upstream_server_error",
		`openai stream error: context canceled`:       "client_gone",
		`client_gone`:                                 "client_gone",
		`context deadline exceeded`:                   "timeout",
		`解析响应失败: invalid character`:                   "proxy_parse_error",
	}

	for message, want := range tests {
		if got := classifyProxyError(message); got != want {
			t.Fatalf("%q classified as %q, want %q", message, got, want)
		}
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
