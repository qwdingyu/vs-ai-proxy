package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

func TestMetricsEndpointExposesPrometheusText(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), nil, store.New(10), log.New(nil, log.LevelError, false))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	srv.handleMetrics(rec, req)

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"vs_ai_proxy_requests_total",
		"vs_ai_proxy_requests_success_total",
		"vs_ai_proxy_requests_failure_total",
		"vs_ai_proxy_request_latency_ms_average",
		"vs_ai_proxy_providers_configured",
		"vs_ai_proxy_models_available",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q: %s", want, body)
		}
	}
}
