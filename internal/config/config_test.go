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

func TestDefaultConfigDoesNotIncludeOllamaLocalProvider(t *testing.T) {
	cfg := DefaultConfig()

	for _, p := range cfg.Providers {
		if p.Type == "ollama" || p.ID == "ollama-local" || p.Name == "ollama" {
			t.Fatalf("default config should not include Ollama provider: %#v", p)
		}
	}
	for _, m := range cfg.Models {
		if m.ProviderID == "ollama-local" || m.Provider == "ollama" || m.Name == "llama-3.3-70b" {
			t.Fatalf("default config should not include Ollama model: %#v", m)
		}
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

func TestNormalizeProviderTransportDefaultsForVersionedOpenAIBase(t *testing.T) {
	provider := NormalizeProvider(ProviderConfig{
		ID:      "zp",
		Name:    "智谱2",
		Type:    "openai",
		BaseURL: "https://open.bigmodel.cn/api/paas/v4/",
	})

	if provider.BaseURL != "https://open.bigmodel.cn/api/paas/v4" {
		t.Fatalf("BaseURL = %q, want trimmed versioned root", provider.BaseURL)
	}
	if provider.Transport.ChatPath != "chat/completions" {
		t.Fatalf("chat_path = %q, want resource relative path", provider.Transport.ChatPath)
	}
	if provider.Transport.ModelsPath != "models" {
		t.Fatalf("models_path = %q, want resource relative path", provider.Transport.ModelsPath)
	}
}

func TestNormalizeProviderTransportPreservesLegacyBareHostOpenAIBase(t *testing.T) {
	provider := NormalizeProvider(ProviderConfig{
		ID:      "legacy",
		Type:    "openai",
		BaseURL: "https://api.deepseek.com",
	})

	if provider.Transport.ChatPath != "v1/chat/completions" {
		t.Fatalf("chat_path = %q, want legacy v1 path", provider.Transport.ChatPath)
	}
	if provider.Transport.ModelsPath != "v1/models" {
		t.Fatalf("models_path = %q, want legacy v1 path", provider.Transport.ModelsPath)
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

func TestNormalizeForRuntimeDefaultsDefenseEnabledForOldConfigs(t *testing.T) {
	cfg := &AppConfig{Providers: []ProviderConfig{DefaultUseAIProvider()}}

	NormalizeForRuntime(cfg)

	if cfg.Defense.Enabled == nil || !*cfg.Defense.Enabled {
		t.Fatalf("defense.enabled should default to true for old configs: %#v", cfg.Defense.Enabled)
	}
	if cfg.Defense.ClientTimeoutBudgetSeconds == nil || *cfg.Defense.ClientTimeoutBudgetSeconds != DefaultClientTimeoutBudgetSeconds {
		t.Fatalf("client_timeout_budget_seconds should default to %d: %#v", DefaultClientTimeoutBudgetSeconds, cfg.Defense.ClientTimeoutBudgetSeconds)
	}
}

func TestNormalizeForRuntimePreservesExplicitDefenseDisabled(t *testing.T) {
	disabled := false
	cfg := &AppConfig{Defense: DefenseConfig{Enabled: &disabled}, Providers: []ProviderConfig{DefaultUseAIProvider()}}

	NormalizeForRuntime(cfg)

	if cfg.Defense.Enabled == nil || *cfg.Defense.Enabled {
		t.Fatalf("explicit defense.enabled=false should be preserved: %#v", cfg.Defense.Enabled)
	}
}

func TestNormalizeForRuntimeClampsClientTimeoutBudget(t *testing.T) {
	tooHigh := 300
	cfg := &AppConfig{Defense: DefenseConfig{ClientTimeoutBudgetSeconds: &tooHigh}, Providers: []ProviderConfig{DefaultUseAIProvider()}}

	NormalizeForRuntime(cfg)

	if cfg.Defense.ClientTimeoutBudgetSeconds == nil || *cfg.Defense.ClientTimeoutBudgetSeconds != MaxClientTimeoutBudgetSeconds {
		t.Fatalf("high client timeout budget should clamp to %d: %#v", MaxClientTimeoutBudgetSeconds, cfg.Defense.ClientTimeoutBudgetSeconds)
	}

	tooLow := 1
	cfg = &AppConfig{Defense: DefenseConfig{ClientTimeoutBudgetSeconds: &tooLow}, Providers: []ProviderConfig{DefaultUseAIProvider()}}

	NormalizeForRuntime(cfg)

	if cfg.Defense.ClientTimeoutBudgetSeconds == nil || *cfg.Defense.ClientTimeoutBudgetSeconds != MinClientTimeoutBudgetSeconds {
		t.Fatalf("low client timeout budget should clamp to %d: %#v", MinClientTimeoutBudgetSeconds, cfg.Defense.ClientTimeoutBudgetSeconds)
	}
}

func TestNormalizeForRuntimeSkipsClampWhenDefenseDisabled(t *testing.T) {
	disabled := false
	budget := 300
	cfg := &AppConfig{
		Defense:   DefenseConfig{Enabled: &disabled, ClientTimeoutBudgetSeconds: &budget},
		Providers: []ProviderConfig{DefaultUseAIProvider()},
	}

	NormalizeForRuntime(cfg)

	// 防御关闭时，client_timeout_budget_seconds 不应被钳位到 [15, 95]
	if cfg.Defense.ClientTimeoutBudgetSeconds == nil || *cfg.Defense.ClientTimeoutBudgetSeconds != 300 {
		t.Fatalf("defense disabled: client_timeout_budget_seconds should not be clamped, got %d", *cfg.Defense.ClientTimeoutBudgetSeconds)
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

func TestNormalizeForRuntimeMigratesConfigVersion(t *testing.T) {
	// 模拟 v1 配置（无 config_version 字段）
	cfg := &AppConfig{
		Providers: []ProviderConfig{DefaultUseAIProvider()},
	}
	cfg.ConfigVersion = 0 // JSON 反序列化时缺失字段的自然值

	migrated := NormalizeForRuntime(cfg)

	if !migrated {
		t.Fatalf("NormalizeForRuntime should return true for v1→v2 migration")
	}
	if cfg.ConfigVersion != CurrentConfigVersion {
		t.Fatalf("config_version = %d, want %d after migration", cfg.ConfigVersion, CurrentConfigVersion)
	}
}

func TestNormalizeForRuntimeSkipsMigrationWhenVersionCurrent(t *testing.T) {
	cfg := &AppConfig{
		ConfigVersion: CurrentConfigVersion,
		Providers:     []ProviderConfig{DefaultUseAIProvider()},
	}

	migrated := NormalizeForRuntime(cfg)

	if migrated {
		t.Fatalf("NormalizeForRuntime should return false when version is already current")
	}
	if cfg.ConfigVersion != CurrentConfigVersion {
		t.Fatalf("config_version should remain %d, got %d", CurrentConfigVersion, cfg.ConfigVersion)
	}
}

func TestNewManagerWritesCurrentConfigVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	cfg := mgr.Get()
	if cfg.ConfigVersion != CurrentConfigVersion {
		t.Fatalf("new config_version = %d, want %d", cfg.ConfigVersion, CurrentConfigVersion)
	}

	// 验证写入磁盘的 JSON 包含 config_version
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var saved AppConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("saved config is invalid JSON: %v", err)
	}
	if saved.ConfigVersion != CurrentConfigVersion {
		t.Fatalf("saved config_version = %d, want %d", saved.ConfigVersion, CurrentConfigVersion)
	}
}

func TestManagerReloadMigratesOldConfigVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	// 写一个 v1 格式的配置（无 config_version, 使用 provider 旧字段）
	old := &AppConfig{
		Port:    18888,
		Defense: DefenseConfig{Enabled: boolPtr(true)},
		Providers: []ProviderConfig{
			{ID: "usecpa", Name: "UseCpa", Type: "openai", BaseURL: "https://cpa.example/v1", Enabled: true, Priority: 10},
		},
		Models: []ModelConfig{
			{Name: "gpt-5.5", Provider: "UseCpa", Enabled: true},
		},
	}
	old.ConfigVersion = 0

	data, err := json.Marshal(old)
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
	if reloaded.ConfigVersion != CurrentConfigVersion {
		t.Fatalf("reloaded config_version = %d, want %d", reloaded.ConfigVersion, CurrentConfigVersion)
	}
	if len(reloaded.Models) != 1 {
		t.Fatalf("models len = %d, want 1", len(reloaded.Models))
	}
	if reloaded.Models[0].ProviderID != "usecpa" {
		t.Fatalf("model provider_id = %q, want usecpa (migrated from provider field)", reloaded.Models[0].ProviderID)
	}

	// Reload 归一化后应写回磁盘（与 NewManager 行为一致）
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var diskCfg AppConfig
	if err := json.Unmarshal(disk, &diskCfg); err != nil {
		t.Fatalf("unmarshal disk config: %v", err)
	}
	if diskCfg.ConfigVersion != CurrentConfigVersion {
		t.Fatalf("Reload should write normalized config back to disk; disk config_version = %d, want %d", diskCfg.ConfigVersion, CurrentConfigVersion)
	}
}

func TestNewManagerMigratedReturnsTrueForOldConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// 写一个 v1 配置
	old := &AppConfig{
		Port:    18080,
		Defense: DefenseConfig{Enabled: boolPtr(true)},
		Providers: []ProviderConfig{
			{Name: "custom", Type: "openai", BaseURL: "https://api.example.com/v1", Enabled: true},
		},
	}
	old.ConfigVersion = 0
	data, err := json.Marshal(old)
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

	if !mgr.Migrated() {
		t.Fatalf("Migrated() should be true for old config")
	}

	// 磁盘已写回归一化结果，config_version 应已更新
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var diskCfg AppConfig
	if err := json.Unmarshal(disk, &diskCfg); err != nil {
		t.Fatalf("unmarshal disk config: %v", err)
	}
	if diskCfg.ConfigVersion != CurrentConfigVersion {
		t.Fatalf("disk config_version = %d, want %d", diskCfg.ConfigVersion, CurrentConfigVersion)
	}
}

func TestNewManagerMigratedReturnsFalseForCurrentConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// 写一个已是最新版本的配置
	cfg := DefaultConfig()
	cfg.ConfigVersion = CurrentConfigVersion
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

	if mgr.Migrated() {
		t.Fatalf("Migrated() should be false for current-version config")
	}
}

func TestNewManagerMigratedWritesNormalizedTransportToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// 写一个缺少 transport 的配置
	old := &AppConfig{
		Port: 18080,
		Providers: []ProviderConfig{
			{
				ID:      "zp",
				Name:    "智谱2",
				Type:    "openai",
				BaseURL: "https://open.bigmodel.cn/api/paas/v4/",
				Enabled: true,
			},
		},
	}
	data, err := json.Marshal(old)
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

	if !mgr.Migrated() {
		t.Fatalf("Migrated() should be true: transport paths were added")
	}

	// 磁盘应包含归一化后的 transport
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var diskCfg AppConfig
	if err := json.Unmarshal(disk, &diskCfg); err != nil {
		t.Fatalf("unmarshal disk config: %v", err)
	}
	if len(diskCfg.Providers) == 0 {
		t.Fatalf("providers should not be empty")
	}
	// EnsureBuiltInProviders 会把 UseAI 插到第一位，按 ID 查找自定义 provider
	var p ProviderConfig
	for _, prov := range diskCfg.Providers {
		if prov.ID == "zp" {
			p = prov
			break
		}
	}
	if p.ID == "" {
		t.Fatalf("provider zp not found in saved config: %#v", diskCfg.Providers)
	}
	if p.Transport.ChatPath != "chat/completions" {
		t.Fatalf("transport chat_path = %q, want chat/completions", p.Transport.ChatPath)
	}
	if p.Transport.ModelsPath != "models" {
		t.Fatalf("transport models_path = %q, want models", p.Transport.ModelsPath)
	}
}

func TestNewManagerEnvPortOverrideDoesNotTriggerMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// 写一个当前版本的配置，PORT 不同于环境变量
	cfg := DefaultConfig()
	cfg.ConfigVersion = CurrentConfigVersion
	cfg.Port = 12345
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("PORT", "18080")

	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	// 环境变量 PORT 不应触发迁移写回
	if mgr.Migrated() {
		t.Fatalf("Migrated() should be false: PORT env override must not trigger write-back")
	}

	// 磁盘端口应保持不变
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var diskCfg AppConfig
	if err := json.Unmarshal(disk, &diskCfg); err != nil {
		t.Fatalf("unmarshal disk config: %v", err)
	}
	if diskCfg.Port != 12345 {
		t.Fatalf("disk port = %d, want 12345 (env override must not be persisted)", diskCfg.Port)
	}

	// 但运行时配置应反映环境变量
	runtime := mgr.Get()
	if runtime.Port != 18080 {
		t.Fatalf("runtime port = %d, want 18080 (env should take effect at runtime)", runtime.Port)
	}
}

func TestManagerReloadMigratedReturnsTrueForOldConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	// 写一个旧格式配置并触发 Reload
	old := &AppConfig{
		Port: 18080,
		Providers: []ProviderConfig{
			{Name: "custom", Type: "openai", BaseURL: "https://api.example.com/v1", Enabled: true},
		},
	}
	old.ConfigVersion = 0
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err = mgr.Reload()
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	if !mgr.Migrated() {
		t.Fatalf("Migrated() should be true after Reload of old config")
	}
}

func TestManagerReloadMigratedReturnsFalseForCurrentConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	// 写一个已是当前版本的配置
	cfg := DefaultConfig()
	cfg.ConfigVersion = CurrentConfigVersion
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err = mgr.Reload()
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	if mgr.Migrated() {
		t.Fatalf("Migrated() should be false after Reload of current-version config")
	}
}

func TestManagerReloadMigratedClearsOnSubsequentCurrentConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	// 第一次 Reload：旧配置 → migrated = true
	old := &AppConfig{
		Port: 18080,
		Providers: []ProviderConfig{
			{Name: "custom", Type: "openai", BaseURL: "https://api.example.com/v1", Enabled: true},
		},
	}
	old.ConfigVersion = 0
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err = mgr.Reload()
	if err != nil {
		t.Fatalf("first Reload() error = %v", err)
	}
	if !mgr.Migrated() {
		t.Fatalf("Migrated() should be true after first Reload of old config")
	}

	// 第二次 Reload：已归一化的配置 → migrated = false
	// 第一次 Reload 已写回磁盘，第二次应无变化
	_, err = mgr.Reload()
	if err != nil {
		t.Fatalf("second Reload() error = %v", err)
	}
	if mgr.Migrated() {
		t.Fatalf("Migrated() should be false after second Reload (config already normalized)")
	}
}
