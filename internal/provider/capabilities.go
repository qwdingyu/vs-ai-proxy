package provider

import (
	"fmt"
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
	return names
}

// GetCapabilities 获取已知 provider 的能力配置，未知 provider 返回零值。
func GetCapabilities(name string) ProviderCapabilities {
	if c, ok := providerCapabilities[strings.ToLower(name)]; ok {
		return c
	}
	return ProviderCapabilities{}
}

// MustGetCapabilities 获取已知 provider 的能力配置，未知 provider 会 panic。
func MustGetCapabilities(name string) ProviderCapabilities {
	if c, ok := providerCapabilities[strings.ToLower(name)]; ok {
		return c
	}
	panic(fmt.Sprintf("unknown provider: %q", name))
}

var providerCapabilities = map[string]ProviderCapabilities{
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

// ResolveApiFormat 根据 provider 实例类型判断上游接口格式：OllamaProvider 返回 Ollama 格式，其余回退为 OpenAI 兼容格式。
func ResolveApiFormat(prov Provider) ApiFormat {
	switch prov.(type) {
	case *OllamaProvider:
		return ApiFormatOllama
	case *OpenAIProvider:
		return ApiFormatOpenAi
	default:
		return ApiFormatOpenAi
	}
}
