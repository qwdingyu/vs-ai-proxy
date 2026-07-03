package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
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
		items = append(items, buildOpenAIModelItem(entry, cfg))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   items,
	})
}

func buildOpenAIModelItem(entry provider.CatalogEntry, cfg *config.AppConfig) map[string]any {
	upstream := strings.TrimSpace(entry.UpstreamModel)
	if upstream == "" {
		upstream = strings.TrimSpace(entry.Model)
	}
	providerName := strings.TrimSpace(entry.Provider)
	identity := provider.NewModelIdentityWithDisplay(upstream, providerName, providerDisplayNameFromConfig(cfg, providerName))

	return map[string]any{
		"id":             entry.Model,
		"object":         "model",
		"created":        1700000000,
		"owned_by":       coalesceString(providerName, "unknown"),
		"display_name":   identity.Display,
		"upstream_model": identity.Upstream,
		"canonical":      identity.Qualified,
		"aliases":        identity.Aliases,
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

	ctxLength := defaultContextLength
	maxOutput := defaultMaxOutputTokens
	supportsTools := true
	supportsVision := false
	family := coalesceString(providerName, "api")
	// 先应用 config.json 中用户维护的模型能力，随后再让内置/发现 profile 覆盖更精确的能力。
	// 这样 SenseNova 这类自定义 OpenAI-compatible provider 也能在 /api/tags 中展示 1M 上下文。
	if modelCfg, ok := findModelConfig(cfg, model, model, providerName); ok {
		if modelCfg.ContextLength != nil && *modelCfg.ContextLength > 0 {
			ctxLength = *modelCfg.ContextLength
		}
		if modelCfg.MaxOutputTokens != nil && *modelCfg.MaxOutputTokens > 0 {
			maxOutput = *modelCfg.MaxOutputTokens
		}
		if modelCfg.SupportsTools != nil {
			supportsTools = *modelCfg.SupportsTools
		}
		if modelCfg.SupportsVision != nil {
			supportsVision = *modelCfg.SupportsVision
		}
	}
	if catalog != nil {
		if profile, ok := catalog.Profile(model, providerName); ok {
			if profile.ContextLength != nil && *profile.ContextLength > 0 {
				ctxLength = *profile.ContextLength
			}
			if profile.MaxOutputTokens != nil && *profile.MaxOutputTokens > 0 {
				maxOutput = *profile.MaxOutputTokens
			}
			if profile.SupportsTools != nil {
				supportsTools = *profile.SupportsTools
			}
			if profile.SupportsVision != nil {
				supportsVision = *profile.SupportsVision
			}
			family = coalesceString(profile.Family, family)
		}
	}

	capabilities := []string{"completion"}
	if supportsTools {
		capabilities = append(capabilities, "tools")
	}
	if supportsVision {
		capabilities = append(capabilities, "vision")
	}

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
			"family":             family,
			"families":           []string{family},
			"parameter_size":     "api",
			"quantization_level": "none",
		},
		"capabilities": capabilities,
		// Visual Studio Copilot 适配：
		// VS 模型发现会读取 token limit、tools、vision 等能力元数据来决定 UI 展示与请求能力。
		// 这些字段不是上游 Ollama 原生必需项，但对 Copilot BYOM 体验很关键。
		"context_length":      ctxLength,
		"max_output_tokens":   maxOutput,
		"input_token_limit":   ctxLength,
		"output_token_limit":  maxOutput,
		"supports_tools":      supportsTools,
		"supports_tool_calls": supportsTools,
		"supports_vision":     supportsVision,
		"supports_images":     supportsVision,
		"model_info": map[string]any{
			"general.architecture":   family,
			"general.basename":       identity.Basename,
			"general.context_length": ctxLength,
			"context_length":         ctxLength,
			"max_output_tokens":      maxOutput,
			"input_token_limit":      ctxLength,
			"output_token_limit":     maxOutput,
			"supports_tools":         supportsTools,
			"supports_tool_calls":    supportsTools,
			"supports_vision":        supportsVision,
			"supports_images":        supportsVision,
		},
	}
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
