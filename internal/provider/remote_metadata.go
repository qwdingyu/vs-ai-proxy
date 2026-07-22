package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 远程模型元数据抓取
//
// 在后台异步从 OpenRouter / LiteLLM 等公开源拉取模型参数，用于补齐
// 内置 models.json 未覆盖的模型（如 grok-4.5、kimi-k3 等）。
//
// 设计原则：
//   - 不阻塞启动：抓取失败只打日志，服务继续运行
//   - 本地缓存：避免每次启动都重复拉取
//   - 三源合并：用户 config.json > OpenRouter > LiteLLM > 内置 models.json
// ---------------------------------------------------------------------------

// 缓存文件名
const remoteMetadataCacheFile = "model-metadata-cache.json"

// 缓存有效期：1 小时内不重复拉取
const remoteMetadataCacheTTL = 1 * time.Hour

// 远程源超时
const openRouterTimeout = 15 * time.Second
const liteLLMTimeout = 10 * time.Second

// MetadataFetcher 负责从远程源拉取模型元数据并缓存到本地。
type MetadataFetcher struct {
	configDir string
	client    *http.Client
	mu        sync.Mutex
}

// NewMetadataFetcher 创建元数据抓取器。
// configDir 是缓存文件存放目录，通常为 config.DefaultConfigDir()。
func NewMetadataFetcher(configDir string) *MetadataFetcher {
	return &MetadataFetcher{
		configDir: configDir,
		client: &http.Client{
			Timeout: openRouterTimeout,
			Transport: &http.Transport{
				IdleConnTimeout:     30 * time.Second,
				DisableKeepAlives:   false,
				MaxIdleConnsPerHost: 2,
			},
		},
	}
}

// ---------------------------------------------------------------------------
// 公开接口
// ---------------------------------------------------------------------------

// Fetch 尝试从远程源拉取模型元数据，优先使用本地缓存。
//
// 鲁棒性策略：
//  1. 缓存有效 → 直接返回缓存，不发起网络请求
//  2. 缓存过期 → 后台拉取 OpenRouter；若失败则降级到 LiteLLM
//  3. 远程源全部不可用 → 使用过期缓存作为 fallback（不阻塞服务）
//  4. 缓存不存在且远程源不可用 → 返回 nil，调用方使用内置元数据兜底
//
// 返回的 ModelProfile 可用作 catalog 的元数据补充。
func (f *MetadataFetcher) Fetch(ctx context.Context) []ModelProfile {
	// 1. 尝试加载有效缓存
	if cached := f.tryLoadCache(false); cached != nil {
		return cached
	}

	// 2. 先试 OpenRouter，失败则试 LiteLLM
	profiles := f.tryFetchOpenRouter(ctx)
	if profiles == nil {
		profiles = f.tryFetchLiteLLM(ctx)
	}

	// 3. 远程拉取成功 → 保存缓存后返回
	if profiles != nil {
		f.trySaveCache(profiles)
		return profiles
	}

	// 4. 远程源全部不可用 → 使用过期缓存作为 fallback
	if expired := f.tryLoadCache(true); expired != nil {
		return expired
	}

	return nil
}

// ---------------------------------------------------------------------------
// 缓存管理
// ---------------------------------------------------------------------------

func (f *MetadataFetcher) cachePath() string {
	if f.configDir == "" {
		return remoteMetadataCacheFile
	}
	return filepath.Join(f.configDir, remoteMetadataCacheFile)
}

type cachedMetadata struct {
	FetchedAt time.Time      `json:"fetched_at"`
	Profiles  []ModelProfile `json:"profiles"`
}

func (f *MetadataFetcher) tryLoadCache(allowExpired bool) []ModelProfile {
	path := f.cachePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var cached cachedMetadata
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil
	}

	if time.Since(cached.FetchedAt) > remoteMetadataCacheTTL {
		if allowExpired {
			return cached.Profiles // 缓存过期但仍可用作网络故障时的 fallback
		}
		return nil
	}
	return cached.Profiles
}

func (f *MetadataFetcher) trySaveCache(profiles []ModelProfile) {
	f.mu.Lock()
	defer f.mu.Unlock()

	cached := cachedMetadata{
		FetchedAt: time.Now(),
		Profiles:  profiles,
	}
	data, err := json.MarshalIndent(cached, "", "  ")
	if err != nil {
		return
	}

	path := f.cachePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	// 原子写入：先写临时文件再 rename
	tmp, err := os.CreateTemp(dir, ".metadata-*.tmp")
	if err != nil {
		return
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
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return
	}
	cleanup = false
}

// ---------------------------------------------------------------------------
// OpenRouter 源
// ---------------------------------------------------------------------------

type openRouterResponse struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID                 string                  `json:"id"`
	ContextLength      int                     `json:"context_length"`
	Architecture       openRouterArchitecture  `json:"architecture"`
	SupportedParams    []string                `json:"supported_parameters"`
	TopProvider        openRouterTopProvider   `json:"top_provider"`
}

type openRouterArchitecture struct {
	Modality string `json:"modality"`
}

type openRouterTopProvider struct {
	MaxCompletionTokens *int `json:"max_completion_tokens"`
}

func (f *MetadataFetcher) tryFetchOpenRouter(ctx context.Context) []ModelProfile {
	reqCtx, cancel := context.WithTimeout(ctx, openRouterTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", "https://openrouter.ai/api/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB 上限
	if err != nil {
		return nil
	}

	var data openRouterResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil
	}

	profiles := make([]ModelProfile, 0, len(data.Data))
	for _, m := range data.Data {
		providerID := modelMetadataProvider(m.ID)
		if providerID == "" {
			continue
		}

		profile := ModelProfile{
			Model:         m.ID,
			Provider:      providerID,
			ContextLength: positiveIntPtr(m.ContextLength),
			Enabled:       true,
			MatchPriority: 900, // 低于内置 metadata（1000），让内置优先
		}

		// max_output_tokens / max_completion_tokens
		if m.TopProvider.MaxCompletionTokens != nil && *m.TopProvider.MaxCompletionTokens > 0 {
			profile.MaxOutputTokens = positiveIntPtr(*m.TopProvider.MaxCompletionTokens)
		}

		// 视觉能力：从 modality 判断
		modality := strings.ToLower(m.Architecture.Modality)
		if strings.Contains(modality, "image") {
			profile.SupportsVision = boolPtr(true)
		}

		// 工具调用和推理能力：从 supported_parameters 判断
		for _, param := range m.SupportedParams {
			switch strings.ToLower(strings.TrimSpace(param)) {
			case "tools", "tool_choice":
				profile.SupportsTools = boolPtr(true)
			case "reasoning", "reasoning_effort", "include_reasoning":
				profile.SupportsReasoning = boolPtr(true)
			}
		}

		profiles = append(profiles, profile)
	}

	return profiles
}

// ---------------------------------------------------------------------------
// LiteLLM 源（GitHub 开源模型目录，作为 OpenRouter 的 fallback）
// ---------------------------------------------------------------------------

type litellmCatalog map[string]litellmModel

type litellmModel struct {
	MaxInputTokens          *int  `json:"max_input_tokens"`
	MaxOutputTokens         *int  `json:"max_output_tokens"`
	SupportsFunctionCalling *bool `json:"supports_function_calling"`
	SupportsVision          *bool `json:"supports_vision"`
	SupportsReasoning       *bool `json:"supports_reasoning"`
	SupportsResponseSchema  *bool `json:"supports_response_schema"`
	SupportsToolChoice      *bool `json:"supports_tool_choice"`
	Mode                    string `json:"mode"`
	LiteLLMProvider         string `json:"litellm_provider"`
}

func (f *MetadataFetcher) tryFetchLiteLLM(ctx context.Context) []ModelProfile {
	reqCtx, cancel := context.WithTimeout(ctx, liteLLMTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET",
		"https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json", nil)
	if err != nil {
		return nil
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB 上限
	if err != nil {
		return nil
	}

	var catalog litellmCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil
	}

	profiles := make([]ModelProfile, 0, len(catalog)/2) // 预分配，过滤后约一半
	for modelID, m := range catalog {
		// 只处理 chat 模式
		if !strings.EqualFold(m.Mode, "chat") {
			continue
		}

		providerID := modelMetadataProvider(modelID)
		if providerID == "" {
			continue
		}

		profile := ModelProfile{
			Model:         modelID,
			Provider:      providerID,
			Enabled:       true,
			MatchPriority: 800, // 低于 OpenRouter（900），让 OpenRouter 优先
		}

		if m.MaxInputTokens != nil && *m.MaxInputTokens > 0 {
			profile.ContextLength = positiveIntPtr(*m.MaxInputTokens)
		}
		if m.MaxOutputTokens != nil && *m.MaxOutputTokens > 0 {
			profile.MaxOutputTokens = positiveIntPtr(*m.MaxOutputTokens)
		}
		if m.SupportsFunctionCalling != nil && *m.SupportsFunctionCalling {
			profile.SupportsTools = boolPtr(true)
		} else if m.SupportsToolChoice != nil && *m.SupportsToolChoice {
			profile.SupportsTools = boolPtr(true)
		}
		if m.SupportsVision != nil && *m.SupportsVision {
			profile.SupportsVision = boolPtr(true)
		}
		if m.SupportsReasoning != nil && *m.SupportsReasoning {
			profile.SupportsReasoning = boolPtr(true)
		}

		profiles = append(profiles, profile)
	}

	return profiles
}

// ---------------------------------------------------------------------------
// 辅助函数 — positiveIntPtr 在 model_catalog.go 中定义，此处不重复声明
// ---------------------------------------------------------------------------