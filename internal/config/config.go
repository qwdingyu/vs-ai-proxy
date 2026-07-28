package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

	// DefaultClientTimeoutBudgetSeconds 是客户端超时预算的默认值（秒）。
	// 防御模式开启时，代理会把更长的模型 timeout_seconds 裁剪到该值，
	// 避免 VS/Copilot 在 ~100 秒客户端超时后才收到上游响应。
	// 防御模式关闭时，该值不生效，由模型 timeout_seconds 直接决定。
	DefaultClientTimeoutBudgetSeconds = 90
	// MinClientTimeoutBudgetSeconds 和 MaxClientTimeoutBudgetSeconds 是
	// 防御模式开启时 client_timeout_budget_seconds 的合法范围。
	// 用户设置的值超出此范围会被钳位。
	MinClientTimeoutBudgetSeconds = 15
	MaxClientTimeoutBudgetSeconds = 95
)

// ProviderConfig 表示一个 AI 提供商的配置。
// Priority 数字越小越优先，会参与同模型多 provider 候选排序。
type ProviderConfig struct {
	ID          string          `json:"id"`                  // provider 实例 ID，作为路由、日志、model@provider_id 的稳定标识
	Name        string          `json:"name"`                // 提供商名称，兼容旧配置；未设置 display_name 时也用于展示
	DisplayName string          `json:"display_name"`        // UI 展示名称
	APIKey      string          `json:"api_key"`             // API 密钥，OpenAI 兼容提供商按 Bearer Token 使用
	BaseURL     string          `json:"base_url"`            // API 基础地址，末尾斜杠会被统一 TrimRight
	Type        string          `json:"type"`                // 提供商协议类型，可选值为 openai / ollama / custom
	Transport   TransportConfig `json:"transport,omitempty"` // 上游传输事实：base_url 后追加的资源相对路径
	Enabled     bool            `json:"enabled"`             // 是否启用，禁用后不会参与模型发现和请求转发
	Priority    int             `json:"priority"`            // 优先级，数字越小越优先
}

// TransportConfig 描述 provider 实例实际使用的 HTTP 资源路径。
// base_url 是版本化 API 根；ChatPath/ModelsPath 是相对于 base_url 的资源路径。
type TransportConfig struct {
	ChatPath   string `json:"chat_path,omitempty"`
	ModelsPath string `json:"models_path,omitempty"`
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

// currentConfigVersion 是当前配置格式的版本号。
// 升级配置格式时递增此常量，并在 NormalizeForRuntime 中加入对应版本的迁移逻辑。
const currentConfigVersion = 2

// CurrentConfigVersion 是当前配置格式版本号，供外部（如启动日志）引用。
const CurrentConfigVersion = currentConfigVersion

// AppConfig 是应用主配置
type AppConfig struct {
	ConfigVersion int              `json:"config_version"` // 配置格式版本号，用于自动迁移；缺失或为 0 视为 v1
	Port          int              `json:"port"`           // 代理端口，供 Visual Studio / Ollama 客户端访问
	DefaultModel  string           `json:"default_model"`  // 默认模型，请求未提供 model 时回退使用
	Defense       DefenseConfig    `json:"defense"`        // 上游网关防御策略，默认开启以兼容 new-api/sub2api 等网关抖动
	Providers     []ProviderConfig `json:"providers"`      // 提供商列表，启动时按此注册到代理服务
	Models        []ModelConfig    `json:"models"`         // 模型配置，用于前端展示和默认参数兜底
}

// DefenseConfig 控制代理侧对 OpenAI-compatible 上游的防御行为。
// Enabled 用指针是为了区分"旧配置没写该字段"和"用户明确关闭"：旧配置升级时默认开启。
// 注意：Enabled 同时控制 client_timeout_budget_seconds 是否生效——
// 关闭后超时预算不裁剪，模型 timeout_seconds 直接透传到底层。
type DefenseConfig struct {
	Enabled                    *bool `json:"enabled"`                       // 是否启用短重试、稳定 User-Agent、限流冷却、协议兜底和超时预算裁剪
	ClientTimeoutBudgetSeconds *int  `json:"client_timeout_budget_seconds"` // 客户端超时预算（秒），仅在防御模式开启时生效；关闭时该值被忽略，模型 timeout_seconds 直接透传
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
		Defense:      DefenseConfig{Enabled: boolPtr(true), ClientTimeoutBudgetSeconds: intPtr(DefaultClientTimeoutBudgetSeconds)},
		Providers: []ProviderConfig{
			DefaultUseAIProvider(),
			{
				ID:       "deepseek",
				Name:     "deepseek",
				BaseURL:  "https://api.deepseek.com/v1",
				Type:     "openai",
				Enabled:  false,
				Priority: 1,
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
			if providerHasTransport(p) {
				useAI.Transport = p.Transport
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
	// useAI 是 DefaultUseAIProvider() 的副本，Transport 字段未设置；
	// 必须归一化以固化 chat_path / models_path，否则每次启动都会产生差异并重复写回。
	useAI = NormalizeProvider(useAI)
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
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	p.Type = strings.ToLower(strings.TrimSpace(p.Type))
	p.Transport = normalizeProviderTransport(p)
	return p
}

func normalizeProviderTransport(p ProviderConfig) TransportConfig {
	transport := TransportConfig{
		ChatPath:   normalizeResourcePath(p.Transport.ChatPath),
		ModelsPath: normalizeResourcePath(p.Transport.ModelsPath),
	}
	if transport.ChatPath != "" && transport.ModelsPath != "" {
		return transport
	}

	providerType := strings.ToLower(strings.TrimSpace(p.Type))
	if providerType == "ollama" {
		if transport.ChatPath == "" {
			transport.ChatPath = "api/chat"
		}
		if transport.ModelsPath == "" {
			transport.ModelsPath = "api/tags"
		}
		return transport
	}

	if transport.ChatPath == "" {
		transport.ChatPath = defaultOpenAIChatPathForBaseURL(p.BaseURL)
	}
	if transport.ModelsPath == "" {
		transport.ModelsPath = defaultOpenAIModelsPathForBaseURL(p.BaseURL)
	}
	return transport
}

func normalizeResourcePath(path string) string {
	path = strings.Trim(strings.TrimSpace(path), "/")
	return path
}

func defaultOpenAIChatPathForBaseURL(baseURL string) string {
	if shouldPreserveLegacyV1Path(baseURL) {
		return "v1/chat/completions"
	}
	return "chat/completions"
}

func defaultOpenAIModelsPathForBaseURL(baseURL string) string {
	if shouldPreserveLegacyV1Path(baseURL) {
		return "v1/models"
	}
	return "models"
}

func shouldPreserveLegacyV1Path(baseURL string) bool {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return false
	}
	path := baseURLPath(baseURL)
	return path == "" || path == "/"
}

func baseURLPath(baseURL string) string {
	schemeSep := strings.Index(baseURL, "://")
	if schemeSep >= 0 {
		rest := baseURL[schemeSep+3:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return ""
		}
		path := rest[slash:]
		if q := strings.IndexAny(path, "?#"); q >= 0 {
			path = path[:q]
		}
		return strings.TrimRight(path, "/")
	}
	slash := strings.Index(baseURL, "/")
	if slash < 0 {
		return ""
	}
	return strings.TrimRight(baseURL[slash:], "/")
}

func providerHasTransport(p ProviderConfig) bool {
	return strings.TrimSpace(p.Transport.ChatPath) != "" || strings.TrimSpace(p.Transport.ModelsPath) != ""
}

func NormalizeModel(m ModelConfig) ModelConfig {
	// provider 是旧字段；provider_id 是新字段。保存/热更新时把旧值迁移为稳定 ID。
	if strings.TrimSpace(m.ProviderID) == "" {
		m.ProviderID = normalizeID(m.Provider)
	}
	return m
}

// NormalizeForRuntime 对配置进行运行时归一化，确保各字段合法且与旧配置兼容。
//
// 核心职责：
//   - config_version 缺失时自动补全并执行版本迁移；
//   - defense.enabled 默认为 true（向后兼容旧配置）；
//   - 防御开启时，client_timeout_budget_seconds 钳位到 [MinClientTimeoutBudgetSeconds, MaxClientTimeoutBudgetSeconds]；
//   - 防御关闭时，跳过预算钳位，让运行时直接透传模型 timeout_seconds；
//   - 确保 UseAI 内置 provider 始终在列表第一位；
//   - 将模型的 provider 旧字段迁移到 provider_id 稳定标识。
//
// 返回值表示是否执行了跨版本配置迁移（v1 → v2）。
func NormalizeForRuntime(cfg *AppConfig) bool {
	if cfg == nil {
		return false
	}

	migrated := normalizeConfigVersion(cfg)

	if cfg.Defense.Enabled == nil {
		cfg.Defense.Enabled = boolPtr(true)
	}
	if cfg.Defense.ClientTimeoutBudgetSeconds == nil {
		cfg.Defense.ClientTimeoutBudgetSeconds = intPtr(DefaultClientTimeoutBudgetSeconds)
	} else {
		budget := *cfg.Defense.ClientTimeoutBudgetSeconds
		if budget <= 0 {
			budget = DefaultClientTimeoutBudgetSeconds
		}
		// 防御关闭时，不强制钳位预算范围，让运行时直接透传模型 timeout_seconds。
		// 用户主动关闭防御意味着期望完全控制超时策略，不再需要安全阀保护。
		if cfg.Defense.Enabled == nil || *cfg.Defense.Enabled {
			if budget < MinClientTimeoutBudgetSeconds {
				budget = MinClientTimeoutBudgetSeconds
			}
			if budget > MaxClientTimeoutBudgetSeconds {
				budget = MaxClientTimeoutBudgetSeconds
			}
		}
		cfg.Defense.ClientTimeoutBudgetSeconds = intPtr(budget)
	}
	EnsureBuiltInProviders(cfg)
	NormalizeModelProviderBindings(cfg.Models, cfg.Providers)
	// CloneAppConfig 会把 nil Models 转为空切片，导致 DeepEqual 误判；
	// 统一归一化为非 nil 空切片，消除 JSON null vs [] 的歧义。
	if cfg.Models == nil {
		cfg.Models = []ModelConfig{}
	}
	return migrated
}

// normalizeConfigVersion 将旧格式配置迁移到当前版本。
// 返回值表示是否执行了迁移（可用于日志提示）。
func normalizeConfigVersion(cfg *AppConfig) bool {
	if cfg.ConfigVersion >= currentConfigVersion {
		return false
	}

	// v1 → v2: provider 旧字段迁移为稳定 provider_id
	// 旧配置（v1）中模型使用 provider 字段按名称绑定，v2 改为 provider_id 按 ID 绑定。
	// 归一化函数 NormalizeModel / EnsureBuiltInProviders 已经处理单条迁移，
	// 这里只需要标记版本号，具体迁移在后续逻辑中完成。
	cfg.ConfigVersion = currentConfigVersion
	return true
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
	saveMu     sync.Mutex
	configPath string
	config     *AppConfig
	migrated   bool // 最近一次 NewManager 或 Reload 是否执行了配置归一化
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
		// 配置文件不存在，生成默认配置，归一化后保存
		cfg = DefaultConfig()
		NormalizeForRuntime(cfg)
		if err := m.save(cfg); err != nil {
			return nil, fmt.Errorf("保存默认配置失败: %w", err)
		}
		applyEnvOverrides(cfg)
		m.config = CloneAppConfig(cfg)
		return m, nil
	}

	// 加载已有配置后，比对归一化结果与磁盘内容。
	// 归一化会固化 transport path、provider ID、defense 默认值等派生字段；
	// 这些变更只存在于内存会导致下次重启 / Reload 重复计算，写回磁盘可一劳永逸。
	loaded := CloneAppConfig(cfg)
	NormalizeForRuntime(cfg)
	// 比对必须在 applyEnvOverrides 之前：PORT 环境变量只在运行时生效，不能写回磁盘。
	if !reflect.DeepEqual(loaded, cfg) {
		m.migrated = true
		if err := m.save(cfg); err != nil {
			// 写回失败不阻塞启动：归一化结果已存在于内存，下次 Save / 热加载会再次写入。
			// 调用方可通过 Migrated() 感知迁移是否发生，自行决定是否记录日志。
		}
	}
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

// Migrated 返回最近一次 NewManager 加载时是否执行了配置归一化写回。
// 调用方可据此向用户提示“配置已自动升级”，或记录审计日志。
func (m *Manager) Migrated() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.migrated
}

// Save 保存配置
func (m *Manager) Save(cfg *AppConfig) error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

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
// 加载后执行归一化，若结果与磁盘不同则自动写回，行为与 NewManager 一致。
func (m *Manager) Reload() (*AppConfig, error) {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

	cfg, err := m.load()
	if err != nil {
		return nil, err
	}

	m.migrated = false
	loaded := CloneAppConfig(cfg)
	NormalizeForRuntime(cfg)
	// 比对必须在 applyEnvOverrides 之前：PORT 环境变量只在运行时生效，不能写回磁盘。
	if !reflect.DeepEqual(loaded, cfg) {
		m.migrated = true
		if err := m.save(cfg); err != nil {
			// 写回失败不阻塞热加载：归一化结果已存在于内存。
		}
	}
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
	if cfg.Defense.ClientTimeoutBudgetSeconds != nil {
		v := *cfg.Defense.ClientTimeoutBudgetSeconds
		out.Defense.ClientTimeoutBudgetSeconds = &v
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
