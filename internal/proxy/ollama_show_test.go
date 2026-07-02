package proxy

import (
	"encoding/json"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

func TestReconfigureUpdatesConfigSnapshot(t *testing.T) {
	server := NewServer(&config.AppConfig{
		Port:         11434,
		DefaultModel: "old-model",
		Providers: []config.ProviderConfig{{
			Name:     "old",
			Type:     "ollama",
			BaseURL:  "http://127.0.0.1:1",
			Enabled:  false,
			Priority: 1,
		}},
	}, nil, store.New(10), log.New(nil, log.LevelError, false))

	next := &config.AppConfig{
		Port:         11434,
		DefaultModel: "new-model",
		Providers: []config.ProviderConfig{{
			Name:     "new",
			Type:     "ollama",
			BaseURL:  "http://127.0.0.1:1",
			Enabled:  false,
			Priority: 1,
		}},
	}
	server.Reconfigure(next)

	cfg, registry, _ := server.snapshot()
	if cfg.DefaultModel != "new-model" {
		t.Fatalf("default model = %q, want new-model", cfg.DefaultModel)
	}
	if registry.DefaultModel() != "new-model" {
		t.Fatalf("registry default model = %q, want new-model", registry.DefaultModel())
	}
}

func TestBuildOllamaShowBodyUsesModelConfig(t *testing.T) {
	ctxLength := 200000
	maxOutput := 32000
	maxTokens := 8192
	temp := 0.2
	topP := 0.8
	timeout := 45
	supportsTools := true
	supportsVision := true

	cfg := &config.AppConfig{
		DefaultModel: "deepseek-v4-pro",
		Models: []config.ModelConfig{{
			Name:            "deepseek-v4-pro",
			Provider:        "deepseek",
			ContextLength:   &ctxLength,
			MaxOutputTokens: &maxOutput,
			MaxTokens:       &maxTokens,
			Temperature:     &temp,
			TopP:            &topP,
			ReasoningEffort: "high",
			TimeoutSeconds:  &timeout,
			SupportsTools:   &supportsTools,
			SupportsVision:  &supportsVision,
			Enabled:         true,
		}},
	}
	server := &Server{}
	registry := providerRegistryForShowTest()

	body, err := server.buildOllamaShowBody(cfg, registry, provider.NewModelCatalog(registry, "", 0), "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("buildOllamaShowBody returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode show body: %v", err)
	}
	if got["context_length"] != float64(ctxLength) {
		t.Fatalf("context_length = %#v, want %d", got["context_length"], ctxLength)
	}
	if got["max_output_tokens"] != float64(maxOutput) {
		t.Fatalf("max_output_tokens = %#v, want %d", got["max_output_tokens"], maxOutput)
	}
	params := got["recommended_parameters"].(map[string]any)
	if params["max_tokens"] != float64(maxTokens) {
		t.Fatalf("recommended max_tokens = %#v, want %d", params["max_tokens"], maxTokens)
	}
	if params["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", params["reasoning_effort"])
	}
	if params["timeout_seconds"] != float64(timeout) {
		t.Fatalf("timeout_seconds = %#v, want %d", params["timeout_seconds"], timeout)
	}
}

func providerRegistryForShowTest() *provider.Registry {
	return provider.NewRegistry("deepseek-v4-pro", 0)
}
