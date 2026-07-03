package provider

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelProfile 描述模型能力与执行参数。
type ModelProfile struct {
	Model                string   `json:"model"`
	Provider             string   `json:"provider"`
	ContextLength        *int     `json:"context_length"`
	MaxOutputTokens      *int     `json:"max_output_tokens"`
	SupportsTools        *bool    `json:"supports_tools"`
	SupportsVision       *bool    `json:"supports_vision"`
	Family               string   `json:"family"`
	Temperature          *float64 `json:"temperature"`
	TopP                 *float64 `json:"top_p"`
	MaxTokens            *int     `json:"max_tokens"`
	ReasoningEffort      string   `json:"reasoning_effort"`
	TimeoutSeconds       *int     `json:"timeout_seconds"`
	OverrideClientParams bool     `json:"override_client_params"`
	SupportsReasoning    *bool    `json:"supports_reasoning"`
	MatchPriority        int      `json:"match_priority"`
	Enabled              bool     `json:"enabled"`
}

// ModelSelection 模型选择配置。
type ModelSelection struct {
	Provider string         `json:"provider"`
	Models   []ModelProfile `json:"models"`
}

// CatalogEntry 是 catalog 对外的模型条目。
type CatalogEntry struct {
	Model         string
	Provider      string
	UpstreamModel string
	Priority      int
	Profile       ModelProfile
	Enabled       bool
	Configured    bool
}

// ModelCatalog 负责生成 Visual Studio 可见模型、provider-qualified alias、upstream 映射、failover candidates、定时刷新。
type ModelCatalog struct {
	mu           sync.RWMutex
	registry     *Registry
	configs      []ModelSelection
	entries      map[string]CatalogEntry
	upstreamMap  map[string][]CatalogEntry
	refreshEvery time.Duration
	lastRefresh  time.Time
}

// NewModelCatalog 创建 model catalog。
func NewModelCatalog(registry *Registry, configDir string, refreshEvery time.Duration) *ModelCatalog {
	if refreshEvery <= 0 {
		refreshEvery = 5 * time.Minute
	}

	c := &ModelCatalog{
		registry:     registry,
		entries:      make(map[string]CatalogEntry),
		upstreamMap:  make(map[string][]CatalogEntry),
		refreshEvery: refreshEvery,
	}
	c.loadEmbeddedModelSelections()
	c.loadModelSelections(configDir)
	c.rebuildLocked()
	return c
}

// AllEntries 返回当前 catalog 的模型列表。
func (c *ModelCatalog) AllEntries() []CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]CatalogEntry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return strings.Compare(out[i].Model, out[j].Model) < 0
	})
	return out
}

// UpstreamEntries 返回某个 upstream model 对应的 catalog entries，用于 failover 排序。
func (c *ModelCatalog) UpstreamEntries(upstream string) []CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := strings.TrimSpace(upstream)
	if key == "" {
		return nil
	}
	out := append([]CatalogEntry(nil), c.upstreamMap[key]...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return strings.Compare(out[i].Model, out[j].Model) < 0
	})
	return out
}

// LastRefresh 返回 catalog 最近一次重建时间。
func (c *ModelCatalog) LastRefresh() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastRefresh
}

// Profile 返回模型的 profile；找不到返回零值。
func (c *ModelCatalog) Profile(model, provider string) (ModelProfile, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	model = strings.TrimSpace(model)
	provider = strings.TrimSpace(provider)
	var best CatalogEntry
	hasBest := false
	for _, entry := range c.entries {
		if !entry.Enabled || !entry.Configured || entry.Provider != provider {
			continue
		}
		if !strings.Contains(strings.ToLower(model), strings.ToLower(entry.Model)) {
			continue
		}
		if !hasBest || len(entry.Model) > len(best.Model) {
			best = entry
			hasBest = true
		}
	}
	if hasBest {
		return best.Profile, true
	}

	key := catalogKey(model, provider)
	if e, ok := c.entries[key]; ok && e.Enabled {
		return e.Profile, true
	}
	return ModelProfile{}, false
}

// ProfileAny 返回跨 provider 的最佳模型 profile。
// 管理端新增模型时 provider 可能是自定义聚合商，无法与内置 provider 名完全对应；
// 这里只用模型名做元数据补齐，不参与真实请求路由。
func (c *ModelCatalog) ProfileAny(model string) (ModelProfile, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	model = StripModelTag(strings.TrimSpace(model))
	if model == "" {
		return ModelProfile{}, false
	}

	bestScore := -1
	var best ModelProfile
	for _, entry := range c.entries {
		if !entry.Enabled || !entry.Configured {
			continue
		}
		score := profileNameMatchScore(model, entry.Model)
		if score < 0 {
			continue
		}
		if score > bestScore || (score == bestScore && entry.Priority < best.MatchPriority) {
			bestScore = score
			best = entry.Profile
		}
	}
	if bestScore < 0 {
		return ModelProfile{}, false
	}
	return best, true
}

// Rebuild 重建 catalog，通常在 provider 发现结果或配置变更后调用。
func (c *ModelCatalog) Rebuild() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rebuildLocked()
}

// RefreshIfNeeded 按间隔刷新 catalog。
func (c *ModelCatalog) RefreshIfNeeded() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if time.Since(c.lastRefresh) < c.refreshEvery {
		return
	}

	// 当前 catalog 不直接执行网络发现，只做重建；发现由 registry 异步完成。
	c.rebuildLocked()
	c.lastRefresh = time.Now()
}

func (c *ModelCatalog) loadModelSelections(configDir string) {
	if configDir == "" {
		return
	}

	base := filepath.Join(configDir, "model-selection")
	c.loadModelSelectionsFromFS(os.DirFS(base), ".")
}

func (c *ModelCatalog) loadEmbeddedModelSelections() {
	c.loadModelSelectionsFromFS(defaultModelSelectionFS, "model-selection")
}

func (c *ModelCatalog) loadModelSelectionsFromFS(fsys fs.FS, dir string) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := fs.ReadFile(fsys, filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var ms ModelSelection
		if err := json.Unmarshal(data, &ms); err != nil {
			continue
		}
		c.configs = append(c.configs, ms)
	}
}

func (c *ModelCatalog) rebuildLocked() {
	entries := make(map[string]CatalogEntry)
	upstreamMap := make(map[string][]CatalogEntry)

	for _, sel := range c.configs {
		if !c.providerActiveLocked(sel.Provider) {
			continue
		}
		for _, profile := range sel.Models {
			key := catalogKey(profile.Model, sel.Provider)
			if strings.TrimSpace(profile.Provider) == "" {
				profile.Provider = sel.Provider
			}
			entry := CatalogEntry{
				Model:         profile.Model,
				Provider:      sel.Provider,
				UpstreamModel: profile.Model,
				Priority:      profile.MatchPriority,
				Profile:       profile,
				Enabled:       profile.Enabled,
				Configured:    true,
			}
			if !entry.Enabled {
				continue
			}

			entries[key] = entry
			upstreamMap[profile.Model] = appendUniqueCatalogEntry(upstreamMap[profile.Model], entry)
		}
	}

	// 把 registry 中已发现的模型也纳入 catalog。
	for _, discovered := range c.discoveredEntries() {
		key := catalogKey(discovered.Model, discovered.Provider)
		if _, exists := entries[key]; exists {
			continue
		}
		entries[key] = discovered
		upstreamMap[discovered.UpstreamModel] = appendUniqueCatalogEntry(upstreamMap[discovered.UpstreamModel], discovered)
	}

	c.entries = entries
	c.upstreamMap = upstreamMap
	c.lastRefresh = time.Now()
	c.syncRegistryMappings(entries)
}

func (c *ModelCatalog) providerActiveLocked(providerName string) bool {
	if c.registry == nil {
		return true
	}
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return false
	}

	c.registry.mu.RLock()
	defer c.registry.mu.RUnlock()
	entry := c.registry.entryByNameLocked(providerName)
	return entry != nil && entry.Provider != nil && entry.Provider.IsEnabled()
}

func catalogKey(model, provider string) string {
	return strings.TrimSpace(model) + "@" + strings.TrimSpace(provider)
}

func profileNameMatchScore(requested, candidate string) int {
	requested = strings.ToLower(StripModelTag(strings.TrimSpace(requested)))
	candidate = strings.ToLower(StripModelTag(strings.TrimSpace(candidate)))
	if requested == "" || candidate == "" {
		return -1
	}
	if requested == candidate {
		return 10_000 + len(candidate)
	}

	requestedBase := strings.ToLower(ModelBasename(requested))
	candidateBase := strings.ToLower(ModelBasename(candidate))
	if requestedBase != "" && requestedBase == candidateBase {
		return 9_000 + len(candidateBase)
	}
	if strings.Contains(requested, candidate) {
		return 7_000 + len(candidate)
	}
	if strings.Contains(candidate, requested) {
		return 6_000 + len(requested)
	}
	if requestedBase != "" && strings.Contains(candidateBase, requestedBase) {
		return 5_000 + len(requestedBase)
	}
	return -1
}

func resolveProviderName(registry *Registry, model string) string {
	candidates := registry.ResolveCandidates(model)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].Provider.Provider.Name()
}

func (c *ModelCatalog) discoveredEntries() []CatalogEntry {
	if c.registry == nil {
		return nil
	}

	c.registry.mu.RLock()
	defer c.registry.mu.RUnlock()

	out := []CatalogEntry{}
	seenBare := map[string]struct{}{}
	for _, providerEntry := range c.registry.orderedEntriesLocked() {
		if providerEntry == nil || providerEntry.Provider == nil || !providerEntry.Provider.IsEnabled() {
			continue
		}

		providerName := providerEntry.Provider.Name()
		for _, model := range providerEntry.Models {
			upstream := strings.TrimSpace(model)
			if upstream == "" {
				continue
			}

			if _, ok := seenBare[strings.ToLower(upstream)]; !ok {
				out = append(out, catalogEntryFromDiscovery(upstream, providerName, upstream, providerEntry.Priority))
				seenBare[strings.ToLower(upstream)] = struct{}{}
			}

			qualified := upstream + "@" + providerName
			out = append(out, catalogEntryFromDiscovery(qualified, providerName, upstream, providerEntry.Priority))
		}
	}
	return out
}

func catalogEntryFromDiscovery(model, providerName, upstream string, priority int) CatalogEntry {
	return CatalogEntry{
		Model:         model,
		Provider:      providerName,
		UpstreamModel: upstream,
		Priority:      priority,
		Profile: ModelProfile{
			Model:    model,
			Provider: providerName,
			Enabled:  true,
		},
		Enabled:    true,
		Configured: false,
	}
}

func (c *ModelCatalog) syncRegistryMappings(entries map[string]CatalogEntry) {
	if c.registry == nil {
		return
	}

	ordered := make([]CatalogEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Enabled {
			ordered = append(ordered, entry)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return strings.Compare(ordered[i].Model, ordered[j].Model) < 0
	})

	providers := make(map[string]*ProviderEntry)
	c.registry.mu.RLock()
	for name, entry := range c.registry.entries {
		providers[name] = entry
	}
	c.registry.mu.RUnlock()

	modelToProvider := make(map[string]*ProviderEntry)
	modelToUpstream := make(map[string]string)
	upstreamToProviders := make(map[string][]*ProviderEntry)
	for _, entry := range ordered {
		providerEntry := providers[entry.Provider]
		if providerEntry == nil || providerEntry.Provider == nil || !providerEntry.Provider.IsEnabled() {
			continue
		}

		upstream := strings.TrimSpace(entry.UpstreamModel)
		if upstream == "" {
			upstream = entry.Model
		}

		if _, exists := modelToProvider[entry.Model]; !exists {
			modelToProvider[entry.Model] = providerEntry
			modelToUpstream[entry.Model] = upstream
		}

		if !strings.Contains(entry.Model, "@") {
			qualified := entry.Model + "@" + entry.Provider
			modelToProvider[qualified] = providerEntry
			modelToUpstream[qualified] = upstream
		}

		upstreamToProviders[upstream] = appendUniqueProvider(upstreamToProviders[upstream], providerEntry)
	}

	c.registry.UpdateModelMappingsWithUpstream(modelToProvider, modelToUpstream, upstreamToProviders)
}

func appendUniqueCatalogEntry(entries []CatalogEntry, entry CatalogEntry) []CatalogEntry {
	for _, e := range entries {
		if e.Model == entry.Model && e.Provider == entry.Provider {
			return entries
		}
	}
	return append(entries, entry)
}

func (p *ModelProfile) UnmarshalJSON(data []byte) error {
	type rawExecution struct {
		ContextLength        *int     `json:"context_length"`
		MaxOutputTokens      *int     `json:"max_output_tokens"`
		SupportsTools        *bool    `json:"supports_tools"`
		SupportsVision       *bool    `json:"supports_vision"`
		Family               string   `json:"family"`
		Temperature          *float64 `json:"temperature"`
		TopP                 *float64 `json:"top_p"`
		MaxTokens            *int     `json:"max_tokens"`
		ReasoningEffort      string   `json:"reasoning_effort"`
		TimeoutSeconds       *int     `json:"timeout_seconds"`
		OverrideClientParams bool     `json:"override_client_params"`
		SupportsReasoning    *bool    `json:"supports_reasoning"`
	}
	type rawProfile struct {
		Match         string        `json:"match"`
		Model         string        `json:"model"`
		ID            string        `json:"id"`
		Provider      string        `json:"provider"`
		Priority      *int          `json:"priority"`
		MatchPriority *int          `json:"match_priority"`
		Enabled       *bool         `json:"enabled"`
		Execution     *rawExecution `json:"execution"`
		rawExecution
	}

	var raw rawProfile
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	model := coalesceModelID(raw.Match, raw.Model, raw.ID)
	priority := 0
	if raw.MatchPriority != nil {
		priority = *raw.MatchPriority
	} else if raw.Priority != nil {
		priority = *raw.Priority
	}
	enabled := true
	if raw.Enabled != nil {
		enabled = *raw.Enabled
	}

	exec := raw.rawExecution
	if raw.Execution != nil {
		exec = *raw.Execution
	}

	*p = ModelProfile{
		Model:                model,
		Provider:             raw.Provider,
		ContextLength:        exec.ContextLength,
		MaxOutputTokens:      exec.MaxOutputTokens,
		SupportsTools:        exec.SupportsTools,
		SupportsVision:       exec.SupportsVision,
		Family:               exec.Family,
		Temperature:          exec.Temperature,
		TopP:                 exec.TopP,
		MaxTokens:            exec.MaxTokens,
		ReasoningEffort:      exec.ReasoningEffort,
		TimeoutSeconds:       exec.TimeoutSeconds,
		OverrideClientParams: exec.OverrideClientParams,
		SupportsReasoning:    exec.SupportsReasoning,
		MatchPriority:        priority,
		Enabled:              enabled,
	}
	return nil
}

func coalesceModelID(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
