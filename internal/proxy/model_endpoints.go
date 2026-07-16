package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/converter"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// handleListModels 汇总所有启用 provider 的模型列表，并以 OpenAI /v1/models 格式返回。
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	cfg, _, catalog := s.snapshot()
	// registry 的模型发现是异步完成的；catalog 创建时可能还没有发现结果。
	// 每次模型列表请求前重建一次，确保 Visual Studio 立刻看到最新 provider 模型。
	catalog.Rebuild()
	entries := catalog.AllEntries()

	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		items = append(items, buildOpenAIModelItem(entry, catalog, cfg))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   items,
	})
}

func buildOpenAIModelItem(entry provider.CatalogEntry, catalog *provider.ModelCatalog, cfg *config.AppConfig) map[string]any {
	upstream := strings.TrimSpace(entry.UpstreamModel)
	if upstream == "" {
		upstream = strings.TrimSpace(entry.Model)
	}
	providerName := strings.TrimSpace(entry.Provider)
	identity := provider.NewModelIdentityWithDisplay(upstream, providerName, providerDisplayNameFromConfig(cfg, providerName))
	meta := modelCapabilityMeta(cfg, catalog, upstream, entry.Model, providerName)

	return map[string]any{
		"id":                  entry.Model,
		"object":              "model",
		"created":             1700000000,
		"owned_by":            coalesceString(providerName, "unknown"),
		"display_name":        identity.Display,
		"upstream_model":      identity.Upstream,
		"canonical":           identity.Qualified,
		"aliases":             identity.Aliases,
		"capabilities":        meta.capabilities(),
		"context_length":      meta.contextLength,
		"max_output_tokens":   meta.maxOutputTokens,
		"input_token_limit":   meta.inputTokenLimit,
		"output_token_limit":  meta.maxOutputTokens,
		"supports_tools":      meta.supportsTools,
		"supports_tool_calls": meta.supportsTools,
		"supports_vision":     meta.supportsVision,
		"supports_images":     meta.supportsVision,
		"model_info":          meta.modelInfo(identity.Basename),
	}
}

// handleOllamaTags 汇总所有启用 provider 的模型列表，并以 Ollama /api/tags 格式返回。
func (s *Server) handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	cfg, _, catalog := s.snapshot()
	// /api/tags 是 Ollama/Visual Studio 发现模型能力的入口，必须和 /api/show 使用同一份最新 catalog。
	catalog.Rebuild()
	entries := ollamaVisibleEntries(catalog.AllEntries())

	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		items = append(items, buildOllamaTagModel(entry, catalog, cfg))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"models": items,
	})
}

func (s *Server) handleOllamaVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"version": "0.6.4"})
}

func ollamaVisibleEntries(entries []provider.CatalogEntry) []provider.CatalogEntry {
	out := make([]provider.CatalogEntry, 0, len(entries))
	seen := map[string]struct{}{}

	for _, entry := range entries {
		if !entry.Enabled || strings.TrimSpace(entry.Provider) == "" {
			continue
		}

		upstream := strings.TrimSpace(entry.UpstreamModel)
		if upstream == "" {
			upstream = strings.TrimSpace(entry.Model)
		}
		if upstream == "" {
			continue
		}

		// 对 Ollama 只暴露 provider 绑定后的模型，避免同名模型在多个 provider 上重复且不可区分。
		key := strings.ToLower(upstream + "@" + entry.Provider)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		entry.Model = upstream
		entry.UpstreamModel = upstream
		out = append(out, entry)
	}
	return out
}

func buildOllamaTagModel(entry provider.CatalogEntry, catalog *provider.ModelCatalog, cfg *config.AppConfig) map[string]any {
	model := strings.TrimSpace(entry.UpstreamModel)
	if model == "" {
		model = strings.TrimSpace(entry.Model)
	}
	providerName := strings.TrimSpace(entry.Provider)
	identity := provider.NewModelIdentityWithDisplay(model, providerName, providerDisplayNameFromConfig(cfg, providerName))

	meta := modelCapabilityMeta(cfg, catalog, model, model, providerName)

	// Visual Studio Copilot / BYOM 适配：
	// name 是用户可见名称，不带 Ollama 本地模型习惯的 :latest，降低 VS 用户认知负担；
	// model 是代理 canonical ID，仍用 model@provider 精确路由；
	// aliases 保留 :latest 变体，兼容会回传 tag 的 Ollama-compatible 客户端。
	return map[string]any{
		"name":        identity.Display,
		"model":       identity.Qualified,
		"aliases":     identity.Aliases,
		"modified_at": time.Now().Format(time.RFC3339),
		"size":        3826793677,
		"digest":      "sha256:" + strings.Repeat("0", 64),
		"details": map[string]any{
			"parent_model":       "",
			"format":             "api",
			"family":             meta.family,
			"families":           []string{meta.family},
			"parameter_size":     "api",
			"quantization_level": "none",
		},
		"capabilities": meta.capabilities(),
		// Visual Studio Copilot 适配：
		// VS 模型发现会读取 token limit、tools、vision 等能力元数据来决定 UI 展示与请求能力。
		// 这些字段不是上游 Ollama 原生必需项，但对 Copilot BYOM 体验很关键。
		"context_length":      meta.contextLength,
		"max_output_tokens":   meta.maxOutputTokens,
		"input_token_limit":   meta.inputTokenLimit,
		"output_token_limit":  meta.maxOutputTokens,
		"supports_tools":      meta.supportsTools,
		"supports_tool_calls": meta.supportsTools,
		"supports_vision":     meta.supportsVision,
		"supports_images":     meta.supportsVision,
		"model_info":          meta.modelInfo(identity.Basename),
	}
}

type modelCapabilities struct {
	contextLength   int
	inputTokenLimit int
	maxOutputTokens int
	supportsTools   bool
	supportsVision  bool
	family          string
}

func modelCapabilityMeta(
	cfg *config.AppConfig,
	catalog *provider.ModelCatalog,
	model string,
	alias string,
	providerName string,
) modelCapabilities {
	profile, _ := effectiveModelProfile(cfg, catalog, model, alias, providerName)
	return modelCapabilitiesFromProfile(profile)
}

func effectiveModelProfile(
	cfg *config.AppConfig,
	catalog *provider.ModelCatalog,
	model string,
	alias string,
	providerName string,
) (provider.ModelProfile, bool) {
	var profile provider.ModelProfile
	found := false
	if catalog != nil {
		profile, found = catalog.Profile(model, providerName)
		if !found && !strings.EqualFold(strings.TrimSpace(alias), strings.TrimSpace(model)) {
			profile, found = catalog.Profile(alias, providerName)
		}
	}
	if modelCfg, ok := findModelConfig(cfg, model, alias, providerName); ok {
		profile = mergeModelConfigProfile(profile, modelCfg)
		found = true
	}
	return profile, found
}

func modelCapabilitiesFromProfile(profile provider.ModelProfile) modelCapabilities {
	meta := modelCapabilities{
		contextLength:   defaultContextLength,
		inputTokenLimit: defaultContextLength,
		maxOutputTokens: defaultMaxOutputTokens,
		supportsTools:   true,
		supportsVision:  false,
		family:          "api",
	}

	if profile.ContextLength != nil && *profile.ContextLength > 0 {
		meta.contextLength = *profile.ContextLength
		meta.inputTokenLimit = *profile.ContextLength
	}
	if profile.InputTokenLimit != nil && *profile.InputTokenLimit > 0 {
		meta.inputTokenLimit = *profile.InputTokenLimit
	}
	if profile.MaxOutputTokens != nil && *profile.MaxOutputTokens > 0 {
		meta.maxOutputTokens = *profile.MaxOutputTokens
	}
	if profile.SupportsTools != nil {
		meta.supportsTools = *profile.SupportsTools
	}
	if profile.SupportsVision != nil {
		meta.supportsVision = *profile.SupportsVision
	}
	meta.family = coalesceString(profile.Family, meta.family)
	if meta.inputTokenLimit > meta.contextLength {
		meta.inputTokenLimit = meta.contextLength
	}
	if meta.maxOutputTokens > meta.contextLength {
		meta.maxOutputTokens = meta.contextLength
	}
	return meta
}

func (m modelCapabilities) capabilities() []string {
	capabilities := []string{"completion"}
	if m.supportsTools {
		capabilities = append(capabilities, "tools")
	}
	if m.supportsVision {
		capabilities = append(capabilities, "vision")
	}
	return capabilities
}

func (m modelCapabilities) modelInfo(basename string) map[string]any {
	return converter.BuildOllamaModelInfo(
		basename,
		m.contextLength,
		m.inputTokenLimit,
		m.maxOutputTokens,
		m.family,
		m.supportsTools,
		m.supportsVision,
	)
}

func providerDisplayNameFromConfig(cfg *config.AppConfig, providerName string) string {
	providerName = strings.TrimSpace(providerName)
	if cfg == nil || providerName == "" {
		return providerName
	}
	for _, p := range cfg.Providers {
		p = config.NormalizeProvider(p)
		if strings.EqualFold(config.ProviderKey(p), providerName) || strings.EqualFold(strings.TrimSpace(p.Name), providerName) {
			return strings.TrimSpace(p.DisplayName)
		}
	}
	return providerName
}
