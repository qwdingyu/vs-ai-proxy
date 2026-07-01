package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ProviderConfig 表示一个 AI 提供商的配置
// 优先级字段当前仅在管理 UI 中展示，尚未用于代理路由选型。
type ProviderConfig struct {
	Name     string `json:"name"`      // 提供商名称，作为内部唯一标识和展示名称
	APIKey   string `json:"api_key"`   // API 密钥，OpenAI 兼容提供商按 Bearer Token 使用
	BaseURL  string `json:"base_url"`  // API 基础地址，末尾斜杠会被统一 TrimRight
	Type     string `json:"type"`      // 提供商协议类型，可选值为 openai / ollama / custom
	Enabled  bool   `json:"enabled"`   // 是否启用，禁用后不会参与模型发现和请求转发
	Priority int    `json:"priority"`  // 优先级，数字越小越优先，当前仅 UI 展示用
}

// ModelConfig 表示模型配置
// 用于在管理界面展示和按模型名称注入默认请求参数。
type ModelConfig struct {
	Name              string  `json:"name"`                // 模型名称，管理界面展示用
	Provider          string  `json:"provider"`            // 所属提供商名称，用于和 provider 配置对照
	ContextLength     *int    `json:"context_length"`      // 上下文长度，仅 UI 展示，当前不参与请求校验
	MaxOutputTokens   *int    `json:"max_output_tokens"`   // 最大输出 token，仅 UI 展示
	SupportsTools     *bool   `json:"supports_tools"`      // 是否支持工具调用，仅 UI 展示
	SupportsVision    *bool   `json:"supports_vision"`     // 是否支持视觉，仅 UI 展示
	Temperature       *float64 `json:"temperature"`       // 默认温度，请求未显式设置时作为 fallback
	TopP              *float64 `json:"top_p"`             // 默认 top_p，请求未显式设置时作为 fallback
	MaxTokens         *int    `json:"max_tokens"`          // 默认 max_tokens，请求未显式设置时作为 fallback
	ReasoningEffort   string  `json:"reasoning_effort"`    // 推理强度，仅 UI 展示，当前未透传给上游
	TimeoutSeconds    *int    `json:"timeout_seconds"`     // 超时秒数，仅 UI 展示，当前未单独使用
	Enabled           bool    `json:"enabled"`             // 是否启用，禁用后仍会展示，但不会主动参与路由
}

// AppConfig 是应用主配置
type AppConfig struct {
	Port        int             `json:"port"`         // 代理端口，供 Visual Studio / Ollama 客户端访问
	DefaultModel string         `json:"default_model"` // 默认模型，请求未提供 model 时回退使用
	Providers   []ProviderConfig `json:"providers"`  // 提供商列表，启动时按此注册到代理服务
	Models      []ModelConfig    `json:"models"`     // 模型配置，用于前端展示和默认参数兜底
}

// DefaultConfig 返回默认配置
func DefaultConfig() *AppConfig {
	return &AppConfig{
		Port:        11434,
		DefaultModel: "deepseek-v4-pro",
		Providers: []ProviderConfig{
			{
				Name:     "deepseek",
				BaseURL:  "https://api.deepseek.com",
				Type:     "openai",
				Enabled:  false,
				Priority: 1,
			},
			{
				Name:     "ollama",
				BaseURL:  "http://localhost:11434",
				Type:     "ollama",
				Enabled:  true,
				Priority: 2,
			},
		},
		Models: []ModelConfig{
			{
				Name:           "deepseek-v4-pro",
				Provider:       "deepseek",
				ContextLength:  intPtr(1000000),
				MaxOutputTokens: intPtr(384000),
				SupportsTools:  boolPtr(true),
				Enabled:        true,
			},
			{
				Name:           "llama-3.3-70b",
				Provider:       "ollama",
				ContextLength:  intPtr(128000),
				MaxOutputTokens: intPtr(16384),
				SupportsTools:  boolPtr(true),
				Enabled:        true,
			},
		},
	}
}

func intPtr(i int) *int {
	return &i
}

func boolPtr(b bool) *bool {
	return &b
}

// Manager 管理应用配置的加载和保存
type Manager struct {
	configPath string
	config     *AppConfig
}

// NewManager 创建配置管理器
func NewManager(configPath string) (*Manager, error) {
	if configPath == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("获取配置目录失败: %w", err)
		}
		configPath = filepath.Join(configDir, "vs-ai-proxy", "config.json")
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

	m.config = cfg
	return m, nil
}

// Get 返回当前配置
func (m *Manager) Get() *AppConfig {
	return m.config
}

// Save 保存配置
func (m *Manager) Save(cfg *AppConfig) error {
	m.config = cfg
	return m.save(cfg)
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

	if err := os.WriteFile(m.configPath, data, 0644); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	return nil
}
