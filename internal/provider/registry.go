package provider

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProviderEntry 提供商条目
type ProviderEntry struct {
	Provider Provider
	Models   []string
	Priority int
}

// Candidate 故障转移候选
type Candidate struct {
	Provider   *ProviderEntry
	UpstreamID string
	ModelID    string
	Priority   int
}

// Registry 管理 provider 与模型映射，并提供故障转移候选解析
type Registry struct {
	mu                    sync.RWMutex
	entries               map[string]*ProviderEntry
	order                 []string
	modelToProvider       map[string]*ProviderEntry
	modelToUpstream       map[string]string
	upstreamToProviders   map[string][]*ProviderEntry
	defaultModel          string
	modelsRefreshInterval time.Duration
	modelsLastRefresh     time.Time
}

// NewRegistry 创建 registry
func NewRegistry(defaultModel string, refreshInterval time.Duration) *Registry {
	if refreshInterval <= 0 {
		refreshInterval = 5 * time.Minute
	}
	return &Registry{
		entries:               make(map[string]*ProviderEntry),
		order:                 []string{},
		modelToProvider:       make(map[string]*ProviderEntry),
		modelToUpstream:       make(map[string]string),
		upstreamToProviders:   make(map[string][]*ProviderEntry),
		defaultModel:          defaultModel,
		modelsRefreshInterval: refreshInterval,
	}
}

// Add 添加 provider 并异步刷新模型
func (r *Registry) Add(entry *ProviderEntry) {
	if entry == nil {
		return
	}

	name := entry.Provider.Name()
	if entry.Models == nil {
		entry.Models = []string{}
	}

	r.mu.Lock()
	if _, exists := r.entries[name]; !exists {
		r.order = append(r.order, name)
	}
	r.entries[name] = entry
	r.rebuildModelMappingsLocked()
	r.mu.Unlock()

	go r.refreshModels(entry)
}

// ResolveCandidates 返回某个模型的可用 provider 候选（按优先级排序）
func (r *Registry) ResolveCandidates(model string) []Candidate {
	r.mu.RLock()
	defer r.mu.RUnlock()

	resolved := r.resolveModelLocked(model)
	return r.resolveCandidatesLocked(resolved)
}

func (r *Registry) resolveModelLocked(model string) string {
	clean := stripTagSuffix(strings.TrimSpace(model))
	if clean == "" {
		clean = r.defaultModel
	}
	if _, ok := r.modelToProvider[clean]; ok {
		return clean
	}
	if _, ok := r.upstreamToProviders[clean]; ok {
		return clean
	}
	return clean
}

func (r *Registry) resolveCandidatesLocked(model string) []Candidate {
	candidates := []Candidate{}
	entry, hasDirect := r.modelToProvider[model]
	upstream := r.modelToUpstream[model]
	if upstream == "" {
		upstream = model
	}

	if hasDirect && strings.Contains(model, "@") {
		return []Candidate{{
			Provider:   entry,
			UpstreamID: upstream,
			ModelID:    upstream,
			Priority:   entry.Priority,
		}}
	}

	if providers, ok := r.upstreamToProviders[upstream]; ok {
		for _, entry := range providers {
			candidates = append(candidates, Candidate{
				Provider:   entry,
				UpstreamID: upstream,
				ModelID:    upstream,
				Priority:   entry.Priority,
			})
		}
		return candidates
	}

	if hasDirect {
		return []Candidate{{
			Provider:   entry,
			UpstreamID: upstream,
			ModelID:    upstream,
			Priority:   entry.Priority,
		}}
	}

	for _, entry := range r.orderedEntriesLocked() {
		if !entry.Provider.IsEnabled() {
			continue
		}
		for _, m := range entry.Models {
			upstream := strings.TrimSpace(m)
			if strings.EqualFold(stripTagSuffix(upstream), model) {
				candidates = append(candidates, Candidate{
					Provider:   entry,
					UpstreamID: upstream,
					ModelID:    upstream,
					Priority:   entry.Priority,
				})
			}
		}
	}
	return candidates
}

// UpdateModelMappings 更新模型映射
func (r *Registry) UpdateModelMappings(modelToProvider map[string]*ProviderEntry, modelToUpstream map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.modelToProvider = modelToProvider
	r.modelToUpstream = modelToUpstream
	r.rebuildUpstreamProvidersLocked()
	r.modelsLastRefresh = time.Now()
}

// UpdateModelMappingsWithUpstream 更新模型映射及上游到 provider 列表映射
func (r *Registry) UpdateModelMappingsWithUpstream(modelToProvider map[string]*ProviderEntry, modelToUpstream map[string]string, upstreamToProviders map[string][]*ProviderEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.modelToProvider = modelToProvider
	r.modelToUpstream = modelToUpstream
	r.upstreamToProviders = upstreamToProviders
	r.modelsLastRefresh = time.Now()
}

// SetModels 更新指定 provider 的模型列表，并重建模型、别名与故障转移映射。
func (r *Registry) SetModels(providerName string, models []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.entries[providerName]
	if !ok {
		return
	}

	entry.Models = normalizeModels(models)
	r.rebuildModelMappingsLocked()
	r.modelsLastRefresh = time.Now()
}

// NeedRefresh 判断是否需要刷新模型
func (r *Registry) NeedRefresh() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.modelsRefreshInterval <= 0 {
		return false
	}
	return time.Since(r.modelsLastRefresh) >= r.modelsRefreshInterval
}

// AllModels 返回所有已发现模型
func (r *Registry) AllModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := make(map[string]struct{}, len(r.modelToProvider))
	out := make([]string, 0, len(r.modelToProvider))
	for m := range r.modelToProvider {
		if _, ok := seen[m]; !ok {
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// DefaultModel 返回默认模型
func (r *Registry) DefaultModel() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultModel
}

// SetDefaultModel 设置默认模型
func (r *Registry) SetDefaultModel(model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultModel = model
}

// EnsureDefaultModelPresent 确保默认模型在可用列表中
func (r *Registry) EnsureDefaultModelPresent(entry *ProviderEntry) {
	if entry == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.modelToProvider[r.defaultModel]; ok {
		return
	}

	if !entry.Provider.IsEnabled() {
		return
	}

	r.modelToProvider[r.defaultModel] = entry
	r.modelToUpstream[r.defaultModel] = r.defaultModel
	r.rebuildUpstreamProvidersLocked()
}

// RefreshIfNeeded 按间隔刷新所有 provider
func (r *Registry) RefreshIfNeeded() {
	if !r.NeedRefresh() {
		return
	}

	for _, entry := range r.entries {
		if entry.Provider.IsEnabled() {
			go r.refreshModels(entry)
		}
	}
}

func (r *Registry) refreshModels(entry *ProviderEntry) {
	if entry == nil || !entry.Provider.IsEnabled() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	models, err := entry.Provider.ListModels(ctx)
	if err != nil {
		return
	}

	r.SetModels(entry.Provider.Name(), models)
}

func (r *Registry) rebuildModelMappingsLocked() {
	modelToProvider := make(map[string]*ProviderEntry)
	modelToUpstream := make(map[string]string)
	upstreamToProviders := make(map[string][]*ProviderEntry)

	for _, entry := range r.orderedEntriesLocked() {
		if !entry.Provider.IsEnabled() {
			continue
		}
		for _, model := range entry.Models {
			upstream := strings.TrimSpace(model)
			if upstream == "" {
				continue
			}

			qualified := upstream + "@" + entry.Provider.Name()
			modelToProvider[qualified] = entry
			modelToUpstream[qualified] = upstream

			if _, ok := modelToProvider[upstream]; !ok {
				modelToProvider[upstream] = entry
				modelToUpstream[upstream] = upstream
			}

			upstreamToProviders[upstream] = appendUniqueProvider(upstreamToProviders[upstream], entry)
		}
	}

	r.modelToProvider = modelToProvider
	r.modelToUpstream = modelToUpstream
	r.upstreamToProviders = upstreamToProviders
}

func (r *Registry) rebuildUpstreamProvidersLocked() {
	upstreamToProviders := make(map[string][]*ProviderEntry)
	for model, entry := range r.modelToProvider {
		upstream := r.modelToUpstream[model]
		if upstream == "" {
			upstream = model
		}
		upstreamToProviders[upstream] = appendUniqueProvider(upstreamToProviders[upstream], entry)
	}

	for upstream, providers := range upstreamToProviders {
		sort.SliceStable(providers, func(i, j int) bool {
			if providers[i].Priority != providers[j].Priority {
				return providers[i].Priority < providers[j].Priority
			}
			return r.providerOrderLocked(providers[i]) < r.providerOrderLocked(providers[j])
		})
		upstreamToProviders[upstream] = providers
	}
	r.upstreamToProviders = upstreamToProviders
}

func (r *Registry) orderedEntriesLocked() []*ProviderEntry {
	entries := make([]*ProviderEntry, 0, len(r.entries))
	for _, name := range r.order {
		if entry, ok := r.entries[name]; ok {
			entries = append(entries, entry)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Priority != entries[j].Priority {
			return entries[i].Priority < entries[j].Priority
		}
		return r.providerOrderLocked(entries[i]) < r.providerOrderLocked(entries[j])
	})
	return entries
}

func (r *Registry) providerOrderLocked(entry *ProviderEntry) int {
	if entry == nil || entry.Provider == nil {
		return len(r.order)
	}
	name := entry.Provider.Name()
	for i, orderedName := range r.order {
		if orderedName == name {
			return i
		}
	}
	return len(r.order)
}

func appendUniqueProvider(entries []*ProviderEntry, entry *ProviderEntry) []*ProviderEntry {
	for _, existing := range entries {
		if existing.Provider.Name() == entry.Provider.Name() {
			return entries
		}
	}
	return append(entries, entry)
}

func normalizeModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	return out
}

func stripTagSuffix(model string) string {
	colon := strings.LastIndex(model, ":")
	if colon <= 0 {
		return model
	}
	if strings.Contains(model[colon+1:], "/") {
		return model
	}
	return model[:colon]
}
