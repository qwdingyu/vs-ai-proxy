package config

import "testing"

func TestDefaultConfigIncludesUseAIAsFirstProvider(t *testing.T) {
	cfg := DefaultConfig()

	if len(cfg.Providers) == 0 {
		t.Fatalf("default providers should not be empty")
	}
	useAI := cfg.Providers[0]
	if useAI.Name != UseAIProviderName {
		t.Fatalf("first provider = %q, want %q", useAI.Name, UseAIProviderName)
	}
	if useAI.BaseURL != UseAIProviderBaseURL {
		t.Fatalf("UseAI base_url = %q, want %q", useAI.BaseURL, UseAIProviderBaseURL)
	}
	if useAI.Type != UseAIProviderType {
		t.Fatalf("UseAI type = %q, want %q", useAI.Type, UseAIProviderType)
	}
	if !useAI.Enabled {
		t.Fatalf("UseAI should be enabled by default")
	}
	if useAI.Priority != UseAIProviderPriority {
		t.Fatalf("UseAI priority = %d, want %d", useAI.Priority, UseAIProviderPriority)
	}
}

func TestEnsureBuiltInProvidersMovesUseAIToFirstAndKeepsKey(t *testing.T) {
	cfg := &AppConfig{
		Providers: []ProviderConfig{
			{Name: "deepseek", Type: "openai", Priority: 1},
			{Name: "UseAI", Type: "custom", APIKey: "user-key", BaseURL: "https://wrong.example", Priority: 99},
		},
	}

	EnsureBuiltInProviders(cfg)

	if len(cfg.Providers) != 2 {
		t.Fatalf("providers len = %d, want 2: %#v", len(cfg.Providers), cfg.Providers)
	}
	useAI := cfg.Providers[0]
	if useAI.Name != UseAIProviderName {
		t.Fatalf("first provider = %q, want %q", useAI.Name, UseAIProviderName)
	}
	if useAI.APIKey != "user-key" {
		t.Fatalf("UseAI api key = %q, want user-key", useAI.APIKey)
	}
	if useAI.BaseURL != UseAIProviderBaseURL {
		t.Fatalf("UseAI base_url = %q, want %q", useAI.BaseURL, UseAIProviderBaseURL)
	}
	if cfg.Providers[1].Name != "deepseek" {
		t.Fatalf("second provider = %q, want deepseek", cfg.Providers[1].Name)
	}
}

func TestEnsureBuiltInProvidersUsesEnvKey(t *testing.T) {
	t.Setenv("PROVIDER_USEAI_API_KEY", "env-key")

	cfg := &AppConfig{}
	EnsureBuiltInProviders(cfg)

	if len(cfg.Providers) != 1 {
		t.Fatalf("providers len = %d, want 1", len(cfg.Providers))
	}
	if cfg.Providers[0].APIKey != "env-key" {
		t.Fatalf("UseAI api key = %q, want env-key", cfg.Providers[0].APIKey)
	}
}

func TestApplyEnvOverridesOverridesPort(t *testing.T) {
	t.Setenv("PROXY_PORT", "18080")

	cfg := DefaultConfig()
	cfg.Port = 11434
	applyEnvOverrides(cfg)

	if cfg.Port != 18080 {
		t.Fatalf("port = %d, want 18080", cfg.Port)
	}
}

func TestApplyEnvOverridesIgnoresInvalidPort(t *testing.T) {
	t.Setenv("PROXY_PORT", "not-a-number")

	cfg := DefaultConfig()
	cfg.Port = 11434
	applyEnvOverrides(cfg)

	if cfg.Port != 11434 {
		t.Fatalf("port = %d, want 11434", cfg.Port)
	}
}
