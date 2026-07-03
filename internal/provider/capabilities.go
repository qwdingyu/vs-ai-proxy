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
	DefaultBaseUrl          string
	EnvPrefix               string
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
		name := prov.Name()
		if p, ok := prov.(*OpenAIProvider); ok {
			name = p.capabilityName()
		}
		if p, ok := prov.(*OllamaProvider); ok && strings.TrimSpace(p.CapabilityName) != "" {
			name = p.CapabilityName
		}
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
