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
	catalogModelProvider  map[string]*ProviderEntry
	catalogModelUpstream  map[string]string
	catalogUpstream       map[string][]*ProviderEntry
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
		catalogModelProvider:  make(map[string]*ProviderEntry),
		catalogModelUpstream:  make(map[string]string),
		catalogUpstream:       make(map[string][]*ProviderEntry),
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

	if candidates := r.resolveProviderHintCandidatesLocked(model); len(candidates) > 0 {
		return candidates
	}

	resolved := r.resolveModelLocked(model)
	candidates := r.resolveCandidatesLocked(resolved)
	if len(candidates) > 0 || strings.Contains(stripTagSuffix(strings.TrimSpace(model)), "@") {
		return candidates
	}

	// Visual Studio Copilot 适配：
	// VS 的 BYOM/Copilot UI 可能把 /api/tags 中用于展示的 name 字段
	// 原样回传到 /v1/chat/completions，例如
	// "DEEPSEEK - deepseek-v4-flash:latest"。这不是上游真实模型名，
	// 必须先剥离 provider 展示前缀，再按真实模型名解析候选 provider。
	if displayModel := displayNameModelSuffix(model); displayModel != "" && !strings.EqualFold(displayModel, resolved) {
		displayResolved := r.resolveModelLocked(displayModel)
		displayCandidates := r.resolveCandidatesLocked(displayResolved)
		if len(displayCandidates) > 0 {
			return displayCandidates
		}
		return r.fallbackCandidatesLocked(displayResolved)
	}

	return r.fallbackCandidatesLocked(resolved)
}

// ResolveModel 返回请求模型经 tag/provider hint/catalog 映射后的代理内部模型名。
func (r *Registry) ResolveModel(model string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveModelLocked(model)
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
	if resolved := r.resolveProviderHintModelLocked(clean); resolved != "" {
		return resolved
	}
	// Visual Studio Copilot 适配：
	// ResolveModel 用于诊断头和日志。这里也要把 VS 回传的展示名还原，
	// 否则日志会显示 upstream 仍是 "PROVIDER - model"，排障会被误导。
	if displayModel := displayNameModelSuffix(clean); displayModel != "" && !strings.EqualFold(displayModel, clean) {
		if resolved := r.resolveModelLocked(displayModel); resolved != displayModel {
			return resolved
		}
		if _, ok := r.modelToProvider[displayModel]; ok {
			return displayModel
		}
		if _, ok := r.upstreamToProviders[displayModel]; ok {
			return displayModel
		}
		return displayModel
	}
	return clean
}

func (r *Registry) resolveProviderHintCandidatesLocked(model string) []Candidate {
	clean := stripTagSuffix(strings.TrimSpace(model))
	entry, resolved, ok := r.resolveProviderHintLocked(clean)
	if !ok || entry == nil || !entry.Provider.IsEnabled() {
		return nil
	}

	upstream := r.modelToUpstream[resolved]
	if upstream == "" {
		upstream = resolved
	}
	return []Candidate{{
		Provider:   entry,
		UpstreamID: upstream,
		ModelID:    upstream,
		Priority:   entry.Priority,
	}}
}

func (r *Registry) resolveProviderHintModelLocked(clean string) string {
	_, resolved, ok := r.resolveProviderHintLocked(clean)
	if !ok {
		return ""
	}
	return resolved
}

func (r *Registry) resolveProviderHintLocked(clean string) (*ProviderEntry, string, bool) {
	slash := strings.Index(clean, "/")
	if slash <= 0 || slash >= len(clean)-1 {
		return nil, "", false
	}

	providerHint := clean[:slash]
	entry := r.entryByNameLocked(providerHint)
	if entry == nil {
		return nil, "", false
	}

	if owner := r.modelToProvider[clean]; owner != nil && sameProvider(owner, entry) {
		return entry, clean, true
	}

	bare := clean[slash+1:]
	if owner := r.modelToProvider[bare]; owner != nil && sameProvider(owner, entry) {
		return entry, bare, true
	}

	for model, owner := range r.modelToProvider {
		if owner == nil || !sameProvider(owner, entry) {
			continue
		}
		if strings.EqualFold(modelSuffix(model), bare) {
			return entry, model, true
		}
	}
	return nil, "", false
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

func (r *Registry) fallbackCandidatesLocked(model string) []Candidate {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}

	candidates := []Candidate{}
	for _, entry := range r.orderedEntriesLocked() {
		if entry == nil || entry.Provider == nil || !entry.Provider.IsEnabled() {
			continue
		}
		candidates = append(candidates, Candidate{
			Provider:   entry,
			UpstreamID: model,
			ModelID:    model,
			Priority:   entry.Priority,
		})
	}
	return candidates
}

// UpdateModelMappings 更新模型映射
func (r *Registry) UpdateModelMappings(modelToProvider map[string]*ProviderEntry, modelToUpstream map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.catalogModelProvider = cloneProviderMap(modelToProvider)
	r.catalogModelUpstream = cloneStringMap(modelToUpstream)
	r.catalogUpstream = nil
	r.rebuildModelMappingsLocked()
	r.modelsLastRefresh = time.Now()
}

// UpdateModelMappingsWithUpstream 更新模型映射及上游到 provider 列表映射
func (r *Registry) UpdateModelMappingsWithUpstream(modelToProvider map[string]*ProviderEntry, modelToUpstream map[string]string, upstreamToProviders map[string][]*ProviderEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.catalogModelProvider = cloneProviderMap(modelToProvider)
	r.catalogModelUpstream = cloneStringMap(modelToUpstream)
	r.catalogUpstream = cloneUpstreamMap(upstreamToProviders)
	r.rebuildModelMappingsLocked()
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

// ProviderNames 返回当前已注册且启用的 provider 名称。
func (r *Registry) ProviderNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.entries))
	for _, entry := range r.orderedEntriesLocked() {
		if entry.Provider != nil && entry.Provider.IsEnabled() {
			out = append(out, entry.Provider.Name())
		}
	}
	return out
}

// ModelsLastRefresh 返回模型映射最近一次刷新时间。
func (r *Registry) ModelsLastRefresh() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.modelsLastRefresh
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
	r.mergeCatalogMappingsLocked()
}

func (r *Registry) mergeCatalogMappingsLocked() {
	for model, entry := range r.catalogModelProvider {
		if strings.TrimSpace(model) == "" || entry == nil {
			continue
		}
		if _, exists := r.modelToProvider[model]; !exists {
			r.modelToProvider[model] = entry
			r.modelToUpstream[model] = r.catalogModelUpstream[model]
		}
	}

	for upstream, providers := range r.catalogUpstream {
		if strings.TrimSpace(upstream) == "" {
			continue
		}
		for _, entry := range providers {
			if entry == nil {
				continue
			}
			r.upstreamToProviders[upstream] = appendUniqueProvider(r.upstreamToProviders[upstream], entry)
		}
	}
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

func (r *Registry) entryByNameLocked(name string) *ProviderEntry {
	for key, entry := range r.entries {
		if strings.EqualFold(key, name) {
			return entry
		}
	}
	return nil
}

func sameProvider(a, b *ProviderEntry) bool {
	if a == nil || b == nil || a.Provider == nil || b.Provider == nil {
		return false
	}
	return strings.EqualFold(a.Provider.Name(), b.Provider.Name())
}

func modelSuffix(model string) string {
	at := strings.Index(model, "@")
	if at > 0 {
		model = model[:at]
	}
	slash := strings.LastIndex(model, "/")
	if slash > 0 && slash < len(model)-1 {
		return model[slash+1:]
	}
	return model
}

func displayNameModelSuffix(model string) string {
	// Visual Studio Copilot 适配：
	// /api/tags 的 name 为 "PROVIDER - display-model:latest"，VS 可能把它
	// 当作 chat model 回传。这里只识别这种展示格式，不影响普通
	// model@provider:latest 或 provider/model 的标准路由形式。
	clean := stripTagSuffix(strings.TrimSpace(model))
	sep := " - "
	idx := strings.LastIndex(clean, sep)
	if idx < 0 || idx+len(sep) >= len(clean) {
		return ""
	}
	suffix := strings.TrimSpace(clean[idx+len(sep):])
	if suffix == "" {
		return ""
	}
	return suffix
}

func appendUniqueProvider(entries []*ProviderEntry, entry *ProviderEntry) []*ProviderEntry {
	for _, existing := range entries {
		if existing.Provider.Name() == entry.Provider.Name() {
			return entries
		}
	}
	return append(entries, entry)
}

func cloneProviderMap(src map[string]*ProviderEntry) map[string]*ProviderEntry {
	out := make(map[string]*ProviderEntry, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func cloneStringMap(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func cloneUpstreamMap(src map[string][]*ProviderEntry) map[string][]*ProviderEntry {
	out := make(map[string][]*ProviderEntry, len(src))
	for key, value := range src {
		out[key] = append([]*ProviderEntry(nil), value...)
	}
	return out
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
