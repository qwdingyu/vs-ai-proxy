package proxy

import (
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
