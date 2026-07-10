package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	UseAIProviderID       = "useai"
	UseAIProviderName     = "UseAI"
	UseAIProviderBaseURL  = "https://api.eforge.xyz/v1"
	UseAIProviderType     = "openai"
	UseAIProviderPriority = 0
)

// ProviderConfig 表示一个 AI 提供商的配置。
// Priority 数字越小越优先，会参与同模型多 provider 候选排序。
type ProviderConfig struct {
	ID          string `json:"id"`           // provider 实例 ID，作为路由、日志、model@provider_id 的稳定标识
	Name        string `json:"name"`         // 提供商名称，兼容旧配置；未设置 display_name 时也用于展示
	DisplayName string `json:"display_name"` // UI 展示名称
	APIKey      string `json:"api_key"`      // API 密钥，OpenAI 兼容提供商按 Bearer Token 使用
	BaseURL     string `json:"base_url"`     // API 基础地址，末尾斜杠会被统一 TrimRight
	Type        string `json:"type"`         // 提供商协议类型，可选值为 openai / ollama / custom
	Enabled     bool   `json:"enabled"`      // 是否启用，禁用后不会参与模型发现和请求转发
	Priority    int    `json:"priority"`     // 优先级，数字越小越优先
}

// ModelConfig 表示模型配置
// 用于在管理界面展示和按模型名称注入默认请求参数。
type ModelConfig struct {
	Name                 string   `json:"name"`                   // 模型名称，管理界面展示用
	ProviderID           string   `json:"provider_id"`            // 可选 provider 实例 ID；为空表示按 provider priority 自动选择
	Provider             string   `json:"provider"`               // 所属提供商名称，用于和 provider 配置对照
	ContextLength        *int     `json:"context_length"`         // 上下文长度，仅 UI 展示，当前不参与请求校验
	MaxOutputTokens      *int     `json:"max_output_tokens"`      // 最大输出 token，仅 UI 展示
	SupportsTools        *bool    `json:"supports_tools"`         // 是否支持工具调用，仅 UI 展示
	SupportsVision       *bool    `json:"supports_vision"`        // 是否支持视觉，仅 UI 展示
	Temperature          *float64 `json:"temperature"`            // 默认温度，请求未显式设置时作为 fallback
	TopP                 *float64 `json:"top_p"`                  // 默认 top_p，请求未显式设置时作为 fallback
	MaxTokens            *int     `json:"max_tokens"`             // 默认 max_tokens，请求未显式设置时作为 fallback
	ReasoningEffort      string   `json:"reasoning_effort"`       // 推理强度，只有 provider 支持时才会透传给上游
	OverrideClientParams bool     `json:"override_client_params"` // 为 true 时，模型默认参数会覆盖客户端同名参数
	TimeoutSeconds       *int     `json:"timeout_seconds"`        // 单模型上游请求超时秒数
	Enabled              bool     `json:"enabled"`                // 是否启用，禁用后仍会展示，但不会主动参与路由
}

// AppConfig 是应用主配置
type AppConfig struct {
	Port         int              `json:"port"`          // 代理端口，供 Visual Studio / Ollama 客户端访问
	DefaultModel string           `json:"default_model"` // 默认模型，请求未提供 model 时回退使用
	Defense      DefenseConfig    `json:"defense"`       // 上游网关防御策略，默认开启以兼容 new-api/sub2api 等网关抖动
	Providers    []ProviderConfig `json:"providers"`     // 提供商列表，启动时按此注册到代理服务
	Models       []ModelConfig    `json:"models"`        // 模型配置，用于前端展示和默认参数兜底
}

// DefenseConfig 控制代理侧对 OpenAI-compatible 上游的防御行为。
// Enabled 用指针是为了区分“旧配置没写该字段”和“用户明确关闭”：旧配置升级时默认开启。
type DefenseConfig struct {
	Enabled *bool `json:"enabled"` // 是否启用短重试、稳定 User-Agent、限流冷却和协议兜底
}

// DefaultConfigDir 返回本项目默认配置目录。
//
// Go 在 macOS 上的 os.UserConfigDir() 会返回 ~/Library/Application Support，
// 但项目文档和用户排障都以 ~/.config/vs-ai-proxy 为准。这里显式采用
// XDG 风格目录，保证 macOS / Linux / 脚本环境的默认持久化位置一致。
func DefaultConfigDir() string {
	if raw := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); raw != "" {
		return filepath.Join(raw, "vs-ai-proxy")
	}
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return filepath.Join(home, ".config", "vs-ai-proxy")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "vs-ai-proxy"
	}
	return filepath.Join(dir, "vs-ai-proxy")
}

// DefaultConfigPath 返回默认 config.json 路径。
func DefaultConfigPath() string {
	return filepath.Join(DefaultConfigDir(), "config.json")
}

// DefaultConfig 返回默认配置
func DefaultConfig() *AppConfig {
	cfg := &AppConfig{
		Port:         12345,
		DefaultModel: "deepseek-v4-pro",
		Defense:      DefenseConfig{Enabled: boolPtr(true)},
		Providers: []ProviderConfig{
			DefaultUseAIProvider(),
			{
				ID:       "deepseek",
				Name:     "deepseek",
				BaseURL:  "https://api.deepseek.com",
				Type:     "openai",
				Enabled:  false,
				Priority: 1,
			},
			{
				ID:       "ollama-local",
				Name:     "ollama",
				BaseURL:  "http://localhost:11434",
				Type:     "ollama",
				Enabled:  true,
				Priority: 2,
			},
		},
		Models: []ModelConfig{
			{
				Name:            "deepseek-v4-pro",
				ProviderID:      "deepseek",
				Provider:        "deepseek",
				ContextLength:   intPtr(1000000),
				MaxOutputTokens: intPtr(384000),
				SupportsTools:   boolPtr(true),
				Enabled:         true,
			},
			{
				Name:            "llama-3.3-70b",
				ProviderID:      "ollama-local",
				Provider:        "ollama",
				ContextLength:   intPtr(128000),
				MaxOutputTokens: intPtr(16384),
				SupportsTools:   boolPtr(true),
				Enabled:         true,
			},
		},
	}
	EnsureBuiltInProviders(cfg)
	return cfg
}

// DefaultUseAIProvider returns the built-in first-party OpenAI-compatible provider.
func DefaultUseAIProvider() ProviderConfig {
	return ProviderConfig{
		ID:          UseAIProviderID,
		Name:        UseAIProviderName,
		DisplayName: UseAIProviderName,
		BaseURL:     UseAIProviderBaseURL,
		Type:        UseAIProviderType,
		Enabled:     true,
		Priority:    UseAIProviderPriority,
	}
}

// EnsureBuiltInProviders keeps first-party providers available even for older config files.
//
// 这里有两个产品约束：
// 1. UseAI 是项目自带的第一方入口，必须始终出现在 provider 列表第一位，方便新用户开箱使用。
// 2. provider 的 api_key/base_url 以 config.json 为唯一事实来源，不再读取 PROVIDER_* 环境变量。
//
// 旧配置可能只有 name 没有 id，或者模型仍使用 provider 字段；因此这里也承担轻量迁移职责。
func EnsureBuiltInProviders(cfg *AppConfig) {
	if cfg == nil {
		return
	}

	useAI := DefaultUseAIProvider()

	out := make([]ProviderConfig, 0, len(cfg.Providers)+1)
	for _, p := range cfg.Providers {
		p = NormalizeProvider(p)
		if strings.EqualFold(ProviderKey(p), UseAIProviderID) ||
			strings.EqualFold(strings.TrimSpace(p.Name), UseAIProviderName) {
			if strings.TrimSpace(p.APIKey) != "" {
				useAI.APIKey = p.APIKey
			}
			if strings.TrimSpace(p.BaseURL) != "" {
				useAI.BaseURL = p.BaseURL
			}
			if strings.TrimSpace(p.DisplayName) != "" {
				useAI.DisplayName = p.DisplayName
			}
			useAI.Enabled = p.Enabled
			if p.Priority != 0 {
				useAI.Priority = p.Priority
			}
			continue
		}
		out = append(out, p)
	}
	cfg.Providers = append([]ProviderConfig{useAI}, dedupeNonUseAIProviders(out)...)
	for i := range cfg.Models {
		cfg.Models[i] = NormalizeModel(cfg.Models[i])
	}
}

func NormalizeProvider(p ProviderConfig) ProviderConfig {
	// ID 是路由、日志、model@provider_id 的稳定标识；name/display_name 允许用户改展示文案。
	p.ID = normalizeID(p.ID)
	if p.ID == "" {
		p.ID = normalizeID(p.Name)
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = p.ID
	}
	if strings.TrimSpace(p.DisplayName) == "" {
		p.DisplayName = p.Name
	}
	return p
}

func NormalizeModel(m ModelConfig) ModelConfig {
	// provider 是旧字段；provider_id 是新字段。保存/热更新时把旧值迁移为稳定 ID。
	if strings.TrimSpace(m.ProviderID) == "" {
		m.ProviderID = normalizeID(m.Provider)
	}
	return m
}

func NormalizeForRuntime(cfg *AppConfig) {
	if cfg == nil {
		return
	}
	if cfg.Defense.Enabled == nil {
		cfg.Defense.Enabled = boolPtr(true)
	}
	EnsureBuiltInProviders(cfg)
	NormalizeModelProviderBindings(cfg.Models, cfg.Providers)
}

func NormalizeModelProviderBindings(models []ModelConfig, providers []ProviderConfig) {
	providerRefs := providerReferenceSet(providers)
	for i := range models {
		model := NormalizeModel(models[i])
		providerID := strings.TrimSpace(model.ProviderID)
		if providerID != "" {
			_, providerExists := providerRefs[strings.ToLower(providerID)]
			if !providerExists && isModelNamespaceProviderBinding(model.Name, providerID) {
				model.ProviderID = ""
				model.Provider = ""
			}
		}
		models[i] = model
	}
}

func providerReferenceSet(providers []ProviderConfig) map[string]struct{} {
	refs := map[string]struct{}{}
	for _, p := range providers {
		p = NormalizeProvider(p)
		for _, value := range []string{ProviderKey(p), p.Name, p.DisplayName} {
			value = strings.TrimSpace(value)
			if value != "" {
				refs[strings.ToLower(value)] = struct{}{}
			}
		}
	}
	return refs
}

func isModelNamespaceProviderBinding(modelName, providerID string) bool {
	modelName = strings.TrimSpace(modelName)
	providerID = strings.TrimSpace(providerID)
	if modelName == "" || providerID == "" {
		return false
	}
	slash := strings.Index(modelName, "/")
	return slash > 0 && strings.EqualFold(modelName[:slash], providerID)
}

func ProviderKey(p ProviderConfig) string {
	p = NormalizeProvider(p)
	return p.ID
}

func dedupeNonUseAIProviders(providers []ProviderConfig) []ProviderConfig {
	out := make([]ProviderConfig, 0, len(providers))
	seen := map[string]struct{}{}
	for _, p := range providers {
		p = NormalizeProvider(p)
		key := strings.ToLower(ProviderKey(p))
		if key == "" || key == UseAIProviderID {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out
}

func normalizeID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "-")
	return value
}

func intPtr(i int) *int {
	return &i
}

func boolPtr(b bool) *bool {
	return &b
}

func applyEnvOverrides(cfg *AppConfig) {
	if cfg == nil {
		return
	}
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		// PROXY_PORT 是旧版双端口配置名，保留读取兼容已有 .env。
		port = strings.TrimSpace(os.Getenv("PROXY_PORT"))
	}
	if port == "" {
		return
	}
	if v, err := strconv.Atoi(port); err == nil && v > 0 {
		cfg.Port = v
	}
}

// Manager 管理应用配置的加载和保存
type Manager struct {
	mu         sync.RWMutex
	configPath string
	config     *AppConfig
}

// NewManager 创建配置管理器
func NewManager(configPath string) (*Manager, error) {
	if configPath == "" {
		configPath = DefaultConfigPath()
	}

	m := &Manager{
		configPath: configPath,
	}

	// 确保配置目录存在
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return nil, fmt.Errorf("创建配置目录失败: %w", err)
	}

	// 尝试加载已有配置
	cfg, err := m.load()
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("加载配置失败: %w", err)
		}
		// 配置文件不存在，使用默认配置并保存
		cfg = DefaultConfig()
		if err := m.save(cfg); err != nil {
			return nil, fmt.Errorf("保存默认配置失败: %w", err)
		}
	}

	NormalizeForRuntime(cfg)
	applyEnvOverrides(cfg)
	m.config = CloneAppConfig(cfg)
	return m, nil
}

// Get 返回当前配置
func (m *Manager) Get() *AppConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return CloneAppConfig(m.config)
}

// ConfigPath 返回当前配置文件路径。
func (m *Manager) ConfigPath() string {
	return m.configPath
}

// Save 保存配置
func (m *Manager) Save(cfg *AppConfig) error {
	NormalizeForRuntime(cfg)
	next := CloneAppConfig(cfg)
	if err := m.save(next); err != nil {
		return err
	}
	m.mu.Lock()
	m.config = next
	m.mu.Unlock()
	return nil
}

// Reload 从磁盘重新加载配置并更新内存快照。
func (m *Manager) Reload() (*AppConfig, error) {
	cfg, err := m.load()
	if err != nil {
		return nil, err
	}
	NormalizeForRuntime(cfg)
	applyEnvOverrides(cfg)
	next := CloneAppConfig(cfg)
	m.mu.Lock()
	m.config = next
	m.mu.Unlock()
	return CloneAppConfig(next), nil
}

// load 从文件加载配置
func (m *Manager) load() (*AppConfig, error) {
	data, err := os.ReadFile(m.configPath)
	if err != nil {
		return nil, err
	}

	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	return &cfg, nil
}

// save 保存配置到文件
func (m *Manager) save(cfg *AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	configDir := filepath.Dir(m.configPath)
	tmp, err := os.CreateTemp(configDir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时配置文件失败: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("设置临时配置文件权限失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步临时配置文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时配置文件失败: %w", err)
	}

	if err := os.Rename(tmpPath, m.configPath); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	cleanup = false

	return nil
}

// CloneAppConfig 深拷贝配置，避免热加载、Web 保存和代理读取之间共享可变切片/指针。
func CloneAppConfig(cfg *AppConfig) *AppConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	if cfg.Defense.Enabled != nil {
		v := *cfg.Defense.Enabled
		out.Defense.Enabled = &v
	}
	out.Providers = append([]ProviderConfig(nil), cfg.Providers...)
	out.Models = make([]ModelConfig, len(cfg.Models))
	for i, model := range cfg.Models {
		out.Models[i] = cloneModelConfig(model)
	}
	return &out
}

func cloneModelConfig(model ModelConfig) ModelConfig {
	out := model
	if model.ContextLength != nil {
		v := *model.ContextLength
		out.ContextLength = &v
	}
	if model.MaxOutputTokens != nil {
		v := *model.MaxOutputTokens
		out.MaxOutputTokens = &v
	}
	if model.SupportsTools != nil {
		v := *model.SupportsTools
		out.SupportsTools = &v
	}
	if model.SupportsVision != nil {
		v := *model.SupportsVision
		out.SupportsVision = &v
	}
	if model.Temperature != nil {
		v := *model.Temperature
		out.Temperature = &v
	}
	if model.TopP != nil {
		v := *model.TopP
		out.TopP = &v
	}
	if model.MaxTokens != nil {
		v := *model.MaxTokens
		out.MaxTokens = &v
	}
	if model.TimeoutSeconds != nil {
		v := *model.TimeoutSeconds
		out.TimeoutSeconds = &v
	}
	return out
}
