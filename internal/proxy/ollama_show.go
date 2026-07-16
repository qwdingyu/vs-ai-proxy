package proxy

import (
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/converter"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

const (
	defaultContextLength   = 128000
	defaultMaxOutputTokens = 4096
)

func (s *Server) buildOllamaShowBody(
	cfg *config.AppConfig,
	registry *provider.Registry,
	catalog *provider.ModelCatalog,
	model string,
) ([]byte, error) {
	providerName := s.resolveProviderName(registry, model)
	upstreamModel := model
	if candidates := registry.ResolveCandidates(model); len(candidates) > 0 {
		upstreamModel = coalesceString(candidates[0].UpstreamID, candidates[0].ModelID, upstreamModel)
		if candidates[0].Provider != nil && candidates[0].Provider.Provider != nil {
			providerName = candidates[0].Provider.Provider.Name()
		}
	}
	profile, _ := effectiveModelProfile(cfg, catalog, upstreamModel, model, providerName)
	meta := modelCapabilitiesFromProfile(profile)
	exec := map[string]any{}
	mergeProfileExecution(exec, profile)

	return converter.BuildOllamaShowResponse(
		model,
		provider.ModelBasename(upstreamModel),
		meta.contextLength,
		meta.inputTokenLimit,
		meta.maxOutputTokens,
		meta.family,
		meta.supportsTools,
		meta.supportsVision,
		exec,
	)
}

func mergeProfileExecution(exec map[string]any, profile provider.ModelProfile) {
	if profile.Temperature != nil {
		exec["temperature"] = *profile.Temperature
	}
	if profile.TopP != nil {
		exec["top_p"] = *profile.TopP
	}
	if profile.MaxTokens != nil {
		exec["max_tokens"] = *profile.MaxTokens
	}
	if strings.TrimSpace(profile.ReasoningEffort) != "" {
		exec["reasoning_effort"] = profile.ReasoningEffort
	}
	if profile.TimeoutSeconds != nil {
		exec["timeout_seconds"] = *profile.TimeoutSeconds
	}
}
