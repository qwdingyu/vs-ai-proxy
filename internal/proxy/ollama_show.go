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
	model string,
) ([]byte, error) {
	providerName := s.resolveProviderName(registry, model)
	modelCfg, _ := findModelConfig(cfg, model, model, providerName)

	ctxLength := intValue(modelCfg.ContextLength, defaultContextLength)
	maxOutput := intValue(modelCfg.MaxOutputTokens, defaultMaxOutputTokens)

	supportsTools := boolValue(modelCfg.SupportsTools, true)
	supportsVision := boolValue(modelCfg.SupportsVision, false)

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

	family := coalesceString(providerName, modelCfg.Provider, "api")
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
