package proxy

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

type healthResponse struct {
	Status               string   `json:"status"`
	Model                string   `json:"model"`
	AvailableModels      []string `json:"available_models"`
	Providers            []string `json:"providers"`
	ModelsLastRefreshUTC string   `json:"models_last_refresh_utc"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	_, registry, catalog := s.snapshot()
	resp := buildHealthResponse(registry, catalog)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func buildHealthResponse(registry *provider.Registry, catalog *provider.ModelCatalog) healthResponse {
	resp := healthResponse{
		Status:               "ok",
		AvailableModels:      []string{},
		Providers:            []string{},
		ModelsLastRefreshUTC: "",
	}
	if registry != nil {
		resp.Model = registry.DefaultModel()
		resp.Providers = registry.ProviderNames()
		resp.ModelsLastRefreshUTC = formatHealthTime(registry.ModelsLastRefresh())
	}
	if catalog != nil {
		entries := catalog.AllEntries()
		resp.AvailableModels = make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.Enabled {
				resp.AvailableModels = append(resp.AvailableModels, entry.Model)
			}
		}
		if last := catalog.LastRefresh(); !last.IsZero() {
			resp.ModelsLastRefreshUTC = formatHealthTime(last)
		}
	}
	return resp
}

func formatHealthTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
