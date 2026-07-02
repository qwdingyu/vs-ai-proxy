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

	ctxLength := intValue(modelCfg.ContextLength, defaultContextLength)
	maxOutput := intValue(modelCfg.MaxOutputTokens, defaultMaxOutputTokens)
	supportsTools := boolValue(modelCfg.SupportsTools, true)
	supportsVision := boolValue(modelCfg.SupportsVision, false)
	family := coalesceString(providerName, modelCfg.Provider, "api")
	exec := executionMapFromModelConfig(modelCfg)

	if catalog != nil {
		if profile, ok := catalog.Profile(model, providerName); ok {
			if profile.ContextLength != nil && *profile.ContextLength > 0 {
				ctxLength = *profile.ContextLength
			}
			if profile.MaxOutputTokens != nil && *profile.MaxOutputTokens > 0 {
				maxOutput = *profile.MaxOutputTokens
			}
			if profile.SupportsTools != nil {
				supportsTools = *profile.SupportsTools
			}
			if profile.SupportsVision != nil {
				supportsVision = *profile.SupportsVision
			}
			family = coalesceString(profile.Family, family)
			mergeProfileExecution(exec, profile)
		}
	}

	return converter.BuildOllamaShowResponse(
		model,
		ctxLength,
		maxOutput,
		family,
		supportsTools,
		supportsVision,
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

func intValue(value *int, fallback int) int {
	if value == nil || *value <= 0 {
		return fallback
	}
	return *value
}

func boolValue(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}
