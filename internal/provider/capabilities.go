package provider

import (
	"fmt"
	"sort"
	"strings"
)

// ApiFormat 标识上游接口风格。
type ApiFormat string

const (
	ApiFormatOpenAi ApiFormat = "openai"
	ApiFormatOllama ApiFormat = "ollama"
)

// ProviderCategory 标识提供商的类别。
type ProviderCategory string

const (
	ProviderCategoryDirect     ProviderCategory = "direct"
	ProviderCategoryMultiModel ProviderCategory = "multi-model"
)

// ProviderCapabilities 描述某个 provider 的能力边界。
type ProviderCapabilities struct {
	Category                ProviderCategory
	ApiFormat               ApiFormat
	SupportsReasoningEffort bool
	SupportsTopK            bool
	ChatPath                string
	ModelsPath              string
	// OutputTokenParam 是 /chat/completions 请求里的输出预算字段名。
	//
	// 大多数 OpenAI-compatible 上游接受 max_tokens；但少数“兼容接口”会把
	// max_tokens 和 max_completion_tokens 做成不同语义。这里把差异收敛到
	// provider 能力层，避免在 proxy handler 里为单个模型散落特判。
	OutputTokenParam string
	DefaultBaseUrl   string
	EnvPrefix        string
}

// CompatibilityProfile 是提供商兼容档案，用于管理页和诊断摘要。
//
// 它只描述“代理将按什么兼容规则处理该 provider”，不等同于上游真实 SLA，
// 也不代表某个模型一定支持工具调用或视觉能力；模型级能力仍以 ModelProfile 为准。
type CompatibilityProfile struct {
	Capability              string           `json:"capability"`
	Category                ProviderCategory `json:"category"`
	ApiFormat               ApiFormat        `json:"api_format"`
	ChatPath                string           `json:"chat_path"`
	ModelsPath              string           `json:"models_path"`
	OutputTokenParam        string           `json:"output_token_param"`
	SupportsReasoningEffort bool             `json:"supports_reasoning_effort"`
	SupportsTopK            bool             `json:"supports_top_k"`
	DefaultBaseURL          string           `json:"default_base_url"`
	EnvPrefix               string           `json:"env_prefix"`
}

// IsKnownProvider 检查是否已注册。
func IsKnownProvider(name string) bool {
	_, ok := providerCapabilities[strings.ToLower(name)]
	return ok
}

// KnownProviders 返回所有已注册 provider 名称。
func KnownProviders() []string {
	names := make([]string, 0, len(providerCapabilities))
	for name := range providerCapabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GetCapabilities 获取已知 provider 的能力配置，未知 provider 返回零值。
func GetCapabilities(name string) ProviderCapabilities {
	if c, ok := providerCapabilities[strings.ToLower(name)]; ok {
		return c
	}
	return ProviderCapabilities{}
}

// CapabilityNameOf 返回 provider 实例对应的能力注册名。
// provider.Name() 是路由实例名（例如 useai2），能力名才决定内置参数、路径和模型 profile。
func CapabilityNameOf(prov Provider) string {
	if prov == nil {
		return ""
	}
	if p, ok := prov.(*OpenAIProvider); ok {
		return p.capabilityName()
	}
	if p, ok := prov.(*OllamaProvider); ok && strings.TrimSpace(p.CapabilityName) != "" {
		return p.CapabilityName
	}
	return prov.Name()
}

// InferCapabilityName 根据 provider 实例配置推断能力注册表名称。
//
// provider 实例 ID 可以是 useai-paid、sensenova、openai-team-a；能力名则决定：
// - 上游 API path 规则，例如 /v1/chat/completions 还是 /chat/completions
// - provider 专属 header，例如 OpenRouter referer/title
// - 参数治理，例如 reasoning_effort/top_k 是否允许透传
//
// 未识别的 OpenAI-compatible provider 返回空字符串，调用方会使用标准 OpenAI fallback path。
func InferCapabilityName(id, name, baseURL, providerType string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	name = strings.ToLower(strings.TrimSpace(name))
	if IsKnownProvider(id) {
		return id
	}
	if IsKnownProvider(name) {
		return name
	}
	if strings.HasPrefix(id, "useai") || strings.Contains(strings.ToLower(baseURL), "api.eforge.xyz") {
		return "useai"
	}
	if strings.Contains(strings.ToLower(baseURL), "api.xiaomimimo.com") {
		return "xiaomimimo"
	}
	switch strings.ToLower(strings.TrimSpace(providerType)) {
	case string(ApiFormatOllama):
		return "ollama"
	default:
		return ""
	}
}

// MustGetCapabilities 获取已知 provider 的能力配置，未知 provider 会 panic。
func MustGetCapabilities(name string) ProviderCapabilities {
	if c, ok := providerCapabilities[strings.ToLower(name)]; ok {
		return c
	}
	panic(fmt.Sprintf("unknown provider: %q", name))
}

// CompatibilityProfileFor 生成用于展示的 provider 兼容档案。
//
// 这里有意复用 providerCapabilities / InferCapabilityName，而不是在
// Web 或 API 层重新判断 provider 类型。这样新增 zhipu、kimi 这类 provider 时，
// 只需要补能力表，路径治理、参数治理和管理页说明会共用同一份事实来源。
func CompatibilityProfileFor(id, name, baseURL, providerType string) CompatibilityProfile {
	capability := InferCapabilityName(id, name, baseURL, providerType)
	caps := GetCapabilities(capability)

	apiFormat := caps.ApiFormat
	if apiFormat == "" {
		switch strings.ToLower(strings.TrimSpace(providerType)) {
		case string(ApiFormatOllama):
			apiFormat = ApiFormatOllama
		default:
			apiFormat = ApiFormatOpenAi
		}
	}

	category := caps.Category
	if category == "" {
		if apiFormat == ApiFormatOllama {
			category = ProviderCategoryMultiModel
		} else {
			category = ProviderCategoryDirect
		}
	}

	chatPath := strings.TrimSpace(caps.ChatPath)
	modelsPath := strings.TrimSpace(caps.ModelsPath)
	if chatPath == "" || modelsPath == "" {
		if apiFormat == ApiFormatOllama {
			if chatPath == "" {
				chatPath = "api/chat"
			}
			if modelsPath == "" {
				modelsPath = "api/tags"
			}
		} else {
			if chatPath == "" {
				chatPath = "v1/chat/completions"
			}
			if modelsPath == "" {
				modelsPath = "v1/models"
			}
		}
	}

	outputTokenParam := normalizeOutputTokenParam(caps.OutputTokenParam)

	defaultBaseURL := strings.TrimSpace(caps.DefaultBaseUrl)
	if defaultBaseURL == "" {
		// 未注册的 OpenAI-compatible provider 没有“官方默认地址”。
		// 此时优先展示用户配置的 Base URL，避免误导用户以为它会走 OpenAI 官方地址。
		if configuredBaseURL := strings.TrimSpace(baseURL); configuredBaseURL != "" {
			defaultBaseURL = configuredBaseURL
		}
		switch apiFormat {
		case ApiFormatOllama:
			if defaultBaseURL == "" {
				defaultBaseURL = "https://ollama.com"
			}
		default:
			if defaultBaseURL == "" {
				defaultBaseURL = "custom"
			}
		}
	}

	if capability == "" {
		capability = "custom"
	}
	envPrefix := strings.TrimSpace(caps.EnvPrefix)
	if envPrefix == "" && IsKnownProvider(capability) {
		envPrefix = strings.ToUpper(strings.TrimSpace(capability))
	}

	return CompatibilityProfile{
		Capability:              capability,
		Category:                category,
		ApiFormat:               apiFormat,
		ChatPath:                chatPath,
		ModelsPath:              modelsPath,
		OutputTokenParam:        outputTokenParam,
		SupportsReasoningEffort: caps.SupportsReasoningEffort,
		SupportsTopK:            caps.SupportsTopK,
		DefaultBaseURL:          defaultBaseURL,
		EnvPrefix:               envPrefix,
	}
}

// OutputTokenParamFor 返回 OpenAI-compatible chat completions 的输出预算字段名。
// 未注册 provider 默认使用行业最常见的 max_tokens，保持既有行为不变。
func OutputTokenParamFor(name string) string {
	caps := GetCapabilities(name)
	return normalizeOutputTokenParam(caps.OutputTokenParam)
}

func normalizeOutputTokenParam(value string) string {
	switch strings.TrimSpace(value) {
	case "max_completion_tokens":
		return "max_completion_tokens"
	case "max_output_tokens":
		return "max_output_tokens"
	default:
		return "max_tokens"
	}
}

var providerCapabilities = map[string]ProviderCapabilities{
	"useai": {
		Category:                ProviderCategoryMultiModel,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: false,
		SupportsTopK:            false,
		ChatPath:                "chat/completions",
		ModelsPath:              "models",
		DefaultBaseUrl:          "https://api.eforge.xyz/v1",
		EnvPrefix:               "USEAI",
	},
	"deepseek": {
		Category:                ProviderCategoryDirect,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: true,
		SupportsTopK:            false,
		ChatPath:                "v1/chat/completions",
		ModelsPath:              "v1/models",
		DefaultBaseUrl:          "https://api.deepseek.com",
		EnvPrefix:               "DEEPSEEK",
	},
	"openai": {
		Category:                ProviderCategoryDirect,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: true,
		SupportsTopK:            false,
		ChatPath:                "v1/chat/completions",
		ModelsPath:              "v1/models",
		DefaultBaseUrl:          "https://api.openai.com",
		EnvPrefix:               "OPENAI",
	},
	"zhipu": {
		Category:                ProviderCategoryDirect,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: false,
		SupportsTopK:            false,
		ChatPath:                "chat/completions",
		ModelsPath:              "models",
		DefaultBaseUrl:          "https://open.bigmodel.cn/api/paas/v4",
		EnvPrefix:               "ZHIPU",
	},
	"moonshot": {
		Category:                ProviderCategoryDirect,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: false,
		SupportsTopK:            false,
		ChatPath:                "v1/chat/completions",
		ModelsPath:              "v1/models",
		DefaultBaseUrl:          "https://api.moonshot.ai",
		EnvPrefix:               "MOONSHOT",
	},
	// Kimi coding endpoint 的默认 base URL 已经带 /coding/v1，因此 ChatPath/ModelsPath
	// 必须是不带 v1 前缀的相对路径。否则管理页测试、/v1/models 代理和聊天请求
	// 都会被错误拼成 /coding/v1/v1/...。
	"kimi": {
		Category:                ProviderCategoryDirect,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: false,
		SupportsTopK:            false,
		ChatPath:                "chat/completions",
		ModelsPath:              "models",
		DefaultBaseUrl:          "https://api.kimi.com/coding/v1",
		EnvPrefix:               "KIMI",
	},
	// MiMo 虽然暴露 OpenAI-compatible chat/completions，但输出预算字段
	// 与多数网关不同：小预算下 max_tokens 会先消耗在 reasoning_content，
	// 可能导致空 content 或 length 终态；实测应使用 max_completion_tokens。
	"xiaomimimo": {
		Category:                ProviderCategoryDirect,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: false,
		SupportsTopK:            false,
		ChatPath:                "v1/chat/completions",
		ModelsPath:              "v1/models",
		OutputTokenParam:        "max_completion_tokens",
		DefaultBaseUrl:          "https://api.xiaomimimo.com/v1",
		EnvPrefix:               "XIAOMIMIMO",
	},
	"google": {
		Category:                ProviderCategoryDirect,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: true,
		SupportsTopK:            false,
		ChatPath:                "v1beta/openai/chat/completions",
		ModelsPath:              "v1beta/openai/models",
		DefaultBaseUrl:          "https://generativelanguage.googleapis.com",
		EnvPrefix:               "GOOGLE",
	},
	"cerebras": {
		Category:                ProviderCategoryDirect,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: false,
		SupportsTopK:            false,
		ChatPath:                "v1/chat/completions",
		ModelsPath:              "v1/models",
		DefaultBaseUrl:          "https://api.cerebras.ai",
		EnvPrefix:               "CEREBRAS",
	},
	"nvidia": {
		Category:                ProviderCategoryMultiModel,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: false,
		SupportsTopK:            true,
		ChatPath:                "v1/chat/completions",
		ModelsPath:              "v1/models",
		DefaultBaseUrl:          "https://integrate.api.nvidia.com",
		EnvPrefix:               "NVIDIA",
	},
	"openrouter": {
		Category:                ProviderCategoryMultiModel,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: false,
		SupportsTopK:            true,
		ChatPath:                "v1/chat/completions",
		ModelsPath:              "v1/models",
		DefaultBaseUrl:          "https://openrouter.ai/api",
		EnvPrefix:               "OPENROUTER",
	},
	"groq": {
		Category:                ProviderCategoryMultiModel,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: false,
		SupportsTopK:            true,
		ChatPath:                "v1/chat/completions",
		ModelsPath:              "v1/models",
		DefaultBaseUrl:          "https://api.groq.com/openai",
		EnvPrefix:               "GROQ",
	},
	"zenmux": {
		Category:                ProviderCategoryMultiModel,
		ApiFormat:               ApiFormatOpenAi,
		SupportsReasoningEffort: false,
		SupportsTopK:            false,
		ChatPath:                "v1/chat/completions",
		ModelsPath:              "v1/models",
		DefaultBaseUrl:          "https://zenmux.ai/api",
		EnvPrefix:               "ZENMUX",
	},
	"ollama": {
		Category:                ProviderCategoryMultiModel,
		ApiFormat:               ApiFormatOllama,
		SupportsReasoningEffort: false,
		SupportsTopK:            false,
		ChatPath:                "api/chat",
		ModelsPath:              "api/tags",
		DefaultBaseUrl:          "https://ollama.com",
		EnvPrefix:               "OLLAMA",
	},
	"ollamacloud": {
		Category:                ProviderCategoryMultiModel,
		ApiFormat:               ApiFormatOllama,
		SupportsReasoningEffort: false,
		SupportsTopK:            false,
		ChatPath:                "api/chat",
		ModelsPath:              "api/tags",
		DefaultBaseUrl:          "https://ollama.com",
		EnvPrefix:               "OLLAMACLOUD",
	},
}

// ResolveApiFormat 根据 provider 能力判断上游接口格式，类型判断只作兜底。
func ResolveApiFormat(prov Provider) ApiFormat {
	if prov != nil {
		name := CapabilityNameOf(prov)
		if IsKnownProvider(name) {
			return GetCapabilities(name).ApiFormat
		}
	}
	switch prov.(type) {
	case *OllamaProvider:
		return ApiFormatOllama
	case *OpenAIProvider:
		return ApiFormatOpenAi
	default:
		return ApiFormatOpenAi
	}
}
