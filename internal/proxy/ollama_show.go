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
	modelCfg, _ := findModelConfig(cfg, model, model, providerName)

	meta := modelCapabilityMeta(cfg, catalog, model, model, providerName)
	exec := executionMapFromModelConfig(modelCfg)

	if catalog != nil {
		if profile, ok := catalog.Profile(model, providerName); ok {
			mergeProfileExecution(exec, profile)
		}
	}

	return converter.BuildOllamaShowResponse(
		model,
		meta.contextLength,
		meta.maxOutputTokens,
		meta.family,
		meta.supportsTools,
		meta.supportsVision,
		exec,
	)
}

func executionMapFromModelConfig(modelCfg config.ModelConfig) map[string]any {
	exec := map[string]any{}
	if modelCfg.Temperature != nil {
		exec["temperature"] = *modelCfg.Temperature
	}
	if modelCfg.TopP != nil {
		exec["top_p"] = *modelCfg.TopP
	}
	if modelCfg.MaxTokens != nil {
		exec["max_tokens"] = *modelCfg.MaxTokens
	}
	if strings.TrimSpace(modelCfg.ReasoningEffort) != "" {
		exec["reasoning_effort"] = modelCfg.ReasoningEffort
	}
	if modelCfg.TimeoutSeconds != nil {
		exec["timeout_seconds"] = *modelCfg.TimeoutSeconds
	}
	return exec
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
