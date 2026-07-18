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

func TestMetricsExposeUpstreamTokenCounters(t *testing.T) {
	st := store.New(10)
	st.AddLog(store.RequestLog{
		Provider: "kimi", Model: "kimi-for-coding", Upstream: "kimi-for-coding", StatusCode: 200, IsSuccess: true,
		Usage: &store.TokenUsage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13, CachedTokens: 4, ReasoningTokens: 2, Source: "upstream"},
	})
	srv := NewServer(config.DefaultConfig(), nil, st, log.New(nil, log.LevelError, false))
	rec := httptest.NewRecorder()
	srv.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	for _, want := range []string{
		"vs_ai_proxy_token_usage_requests_total 1",
		"vs_ai_proxy_token_usage_reported_total 1",
		"vs_ai_proxy_prompt_tokens_total 10",
		"vs_ai_proxy_completion_tokens_total 3",
		"vs_ai_proxy_tokens_total 13",
		"vs_ai_proxy_cached_tokens_total 4",
		"vs_ai_proxy_reasoning_tokens_total 2",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("metrics body missing %q:\n%s", want, rec.Body.String())
		}
	}
}
