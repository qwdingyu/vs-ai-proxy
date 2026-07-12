package requestmeta

import (
	"context"
	"testing"
)

func TestContextWithRequestIDRoundTrip(t *testing.T) {
	ctx := ContextWithRequestID(context.Background(), " req-123 ")
	if got := RequestIDFromContext(ctx); got != "req-123" {
		t.Fatalf("RequestIDFromContext() = %q, want req-123", got)
	}
}

func TestContextWithRequestIDPreservesExistingMetadata(t *testing.T) {
	ctx := ContextWithRequestID(context.Background(), "req-1")
	ctx = ContextWithRequestID(ctx, "req-2")
	if got := RequestIDFromContext(ctx); got != "req-2" {
		t.Fatalf("RequestIDFromContext() = %q, want req-2", got)
	}
}
