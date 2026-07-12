package requestmeta

import (
	"context"
	"strings"
)

type contextKey struct{}

// Metadata carries request-scoped observability fields across proxy/provider layers.
// Keep it intentionally small for now so we can extend it without multiplying context keys.
type Metadata struct {
	RequestID string
}

// ContextWithRequestID stores a request id in context.
func ContextWithRequestID(ctx context.Context, requestID string) context.Context {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ctx
	}
	meta := FromContext(ctx)
	meta.RequestID = requestID
	return context.WithValue(ctx, contextKey{}, meta)
}

// FromContext extracts request metadata from context.
func FromContext(ctx context.Context) Metadata {
	if ctx == nil {
		return Metadata{}
	}
	meta, _ := ctx.Value(contextKey{}).(Metadata)
	return meta
}

// RequestIDFromContext returns the request id if present.
func RequestIDFromContext(ctx context.Context) string {
	return strings.TrimSpace(FromContext(ctx).RequestID)
}
