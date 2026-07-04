package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigIncludesUseAIAsFirstProvider(t *testing.T) {
	cfg := DefaultConfig()

	if len(cfg.Providers) == 0 {
		t.Fatalf("default providers should not be empty")
	}
	useAI := cfg.Providers[0]
	if useAI.ID != UseAIProviderID {
		t.Fatalf("UseAI id = %q, want %q", useAI.ID, UseAIProviderID)
	}
	if useAI.Name != UseAIProviderName {
		t.Fatalf("first provider = %q, want %q", useAI.Name, UseAIProviderName)
	}
	if useAI.DisplayName != UseAIProviderName {
		t.Fatalf("UseAI display_name = %q, want %q", useAI.DisplayName, UseAIProviderName)
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

func TestDefaultConfigPathUsesXDGStyleHomeConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/tmp/test-home")

	if got, want := DefaultConfigPath(), "/tmp/test-home/.config/vs-ai-proxy/config.json"; got != want {
		t.Fatalf("DefaultConfigPath() = %q, want %q", got, want)
	}
}

func TestDefaultConfigPathUsesXDGConfigHomeWhenSet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-config")
	t.Setenv("HOME", "/tmp/test-home")

	if got, want := DefaultConfigPath(), "/tmp/xdg-config/vs-ai-proxy/config.json"; got != want {
		t.Fatalf("DefaultConfigPath() = %q, want %q", got, want)
	}
}

func TestEnsureBuiltInProvidersMovesUseAIToFirstAndPreservesConfigValues(t *testing.T) {
	cfg := &AppConfig{
		Providers: []ProviderConfig{
			{Name: "deepseek", Type: "openai", Priority: 1},
			{Name: "UseAI", DisplayName: "UseAI Free", Type: "custom", APIKey: "user-key", BaseURL: "https://custom.example/v1", Priority: 99},
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
	if useAI.BaseURL != "https://custom.example/v1" {
		t.Fatalf("UseAI base_url = %q, want configured base_url", useAI.BaseURL)
	}
	if useAI.DisplayName != "UseAI Free" {
		t.Fatalf("UseAI display_name = %q, want UseAI Free", useAI.DisplayName)
	}
	if useAI.Priority != 99 {
		t.Fatalf("UseAI priority = %d, want 99", useAI.Priority)
	}
	if cfg.Providers[1].Name != "deepseek" {
		t.Fatalf("second provider = %q, want deepseek", cfg.Providers[1].Name)
	}
}

func TestEnsureBuiltInProvidersDoesNotReadProviderEnvKey(t *testing.T) {
	t.Setenv("PROVIDER_USEAI_API_KEY", "env-key")

	cfg := &AppConfig{}
	EnsureBuiltInProviders(cfg)

	if len(cfg.Providers) != 1 {
		t.Fatalf("providers len = %d, want 1", len(cfg.Providers))
	}
	if cfg.Providers[0].APIKey != "" {
		t.Fatalf("UseAI api key = %q, want empty because provider env is no longer a config source", cfg.Providers[0].APIKey)
	}
}

func TestNormalizeProviderAndModelAddsStableIDs(t *testing.T) {
	provider := NormalizeProvider(ProviderConfig{Name: "UseAI Paid"})
	if provider.ID != "useai-paid" {
		t.Fatalf("provider id = %q, want useai-paid", provider.ID)
	}
	if provider.DisplayName != "UseAI Paid" {
		t.Fatalf("display name = %q, want UseAI Paid", provider.DisplayName)
	}

	model := NormalizeModel(ModelConfig{Name: "model-a", Provider: "UseAI Paid"})
	if model.ProviderID != "useai-paid" {
		t.Fatalf("model provider_id = %q, want useai-paid", model.ProviderID)
	}
}

func TestApplyEnvOverridesUsesPort(t *testing.T) {
	t.Setenv("PORT", "18080")
	t.Setenv("PROXY_PORT", "19090")

	cfg := DefaultConfig()
	cfg.Port = 12345
	applyEnvOverrides(cfg)

	if cfg.Port != 18080 {
		t.Fatalf("port = %d, want 18080", cfg.Port)
	}
}

func TestApplyEnvOverridesFallsBackToLegacyProxyPort(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("PROXY_PORT", "19090")

	cfg := DefaultConfig()
	cfg.Port = 12345
	applyEnvOverrides(cfg)

	if cfg.Port != 19090 {
		t.Fatalf("port = %d, want 19090", cfg.Port)
	}
}

func TestApplyEnvOverridesIgnoresInvalidPort(t *testing.T) {
	t.Setenv("PORT", "not-a-number")

	cfg := DefaultConfig()
	cfg.Port = 12345
	applyEnvOverrides(cfg)

	if cfg.Port != 12345 {
		t.Fatalf("port = %d, want 12345", cfg.Port)
	}
}

func TestManagerReloadUpdatesConfigFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	next := DefaultConfig()
	next.Port = 18888
	next.DefaultModel = "reload-model"
	next.Providers = []ProviderConfig{DefaultUseAIProvider()}
	data, err := json.Marshal(next)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	reloaded, err := mgr.Reload()
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if reloaded.Port != 18888 {
		t.Fatalf("reloaded port = %d, want 18888", reloaded.Port)
	}
	if mgr.Get().DefaultModel != "reload-model" {
		t.Fatalf("manager default model = %q, want reload-model", mgr.Get().DefaultModel)
	}
}

func TestManagerReloadMigratesModelNamespaceProviderBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	next := DefaultConfig()
	next.Providers = []ProviderConfig{{
		ID:       "usecpa",
		Name:     "UseCpa",
		Type:     "openai",
		BaseURL:  "https://cpa.eforge.xyz/v1",
		Enabled:  true,
		Priority: 10,
	}}
	next.Models = []ModelConfig{{
		Name:       "z-ai/glm-5.2",
		ProviderID: "z-ai",
		Provider:   "z-ai",
		Enabled:    true,
	}}
	data, err := json.Marshal(next)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	reloaded, err := mgr.Reload()
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if len(reloaded.Models) != 1 {
		t.Fatalf("models len = %d, want 1", len(reloaded.Models))
	}
	if reloaded.Models[0].ProviderID != "" || reloaded.Models[0].Provider != "" {
		t.Fatalf("model provider binding = %q/%q, want empty automatic routing", reloaded.Models[0].ProviderID, reloaded.Models[0].Provider)
	}
}

func TestNewManagerMigratesModelNamespaceProviderBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := DefaultConfig()
	cfg.Providers = []ProviderConfig{{
		ID:       "usecpa",
		Name:     "UseCpa",
		Type:     "openai",
		BaseURL:  "https://cpa.eforge.xyz/v1",
		Enabled:  true,
		Priority: 10,
	}}
	cfg.Models = []ModelConfig{{
		Name:       "z-ai/glm-5.2",
		ProviderID: "z-ai",
		Provider:   "z-ai",
		Enabled:    true,
	}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	loaded := mgr.Get()
	if len(loaded.Models) != 1 {
		t.Fatalf("models len = %d, want 1", len(loaded.Models))
	}
	if loaded.Models[0].ProviderID != "" || loaded.Models[0].Provider != "" {
		t.Fatalf("model provider binding = %q/%q, want empty automatic routing", loaded.Models[0].ProviderID, loaded.Models[0].Provider)
	}
}

func TestManagerSaveWritesValidConfigAndCleansTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	cfg := mgr.Get()
	cfg.DefaultModel = "saved-model"
	if err := mgr.Save(cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var saved AppConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("saved config is invalid JSON: %v", err)
	}
	if saved.DefaultModel != "saved-model" {
		t.Fatalf("saved default_model = %q, want saved-model", saved.DefaultModel)
	}

	matches, err := filepath.Glob(filepath.Join(dir, ".config-*.tmp"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary config files left behind: %#v", matches)
	}
}
