package proxy

import (
	"fmt"
	"net/http"
)

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	_, registry, catalog := s.snapshot()
	stats := s.store.GetStatistics()

	providerCount := 0
	if registry != nil {
		providerCount = len(registry.ProviderNames())
	}

	modelCount := 0
	if catalog != nil {
		modelCount = len(catalog.AllEntries())
	} else if registry != nil {
		modelCount = len(registry.AllModels())
	}

	type metric struct {
		name  string
		value string
		help  string
		typ   string
	}

	metrics := []metric{
		{name: "vs_ai_proxy_requests_total", value: fmt.Sprintf("%d", stats.TotalRequests), help: "Total proxied requests.", typ: "counter"},
		{name: "vs_ai_proxy_requests_success_total", value: fmt.Sprintf("%d", stats.SuccessCount), help: "Successful proxied requests.", typ: "counter"},
		{name: "vs_ai_proxy_requests_failure_total", value: fmt.Sprintf("%d", stats.FailureCount), help: "Failed proxied requests.", typ: "counter"},
		{name: "vs_ai_proxy_request_latency_ms_average", value: fmt.Sprintf("%.3f", stats.AvgLatencyMs), help: "Average proxied request latency in milliseconds.", typ: "gauge"},
		{name: "vs_ai_proxy_providers_configured", value: fmt.Sprintf("%d", providerCount), help: "Enabled providers currently registered.", typ: "gauge"},
		{name: "vs_ai_proxy_models_available", value: fmt.Sprintf("%d", modelCount), help: "Known models currently exposed by the proxy.", typ: "gauge"},
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, m := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n", m.name, m.help)
		fmt.Fprintf(w, "# TYPE %s %s\n", m.name, m.typ)
		fmt.Fprintf(w, "%s %s\n", m.name, m.value)
	}
}
