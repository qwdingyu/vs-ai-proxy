package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestModelCatalogLoadsExecutionConfig(t *testing.T) {
	dir := t.TempDir()
	selectionDir := filepath.Join(dir, "model-selection")
	if err := os.MkdirAll(selectionDir, 0755); err != nil {
		t.Fatalf("mkdir model-selection: %v", err)
	}

	data := []byte(`{
		"provider":"openrouter",
		"models":[{
			"match":"deepseek/deepseek-v4-pro",
			"priority":3,
			"enabled":true,
			"execution":{
				"context_length":1048576,
				"max_output_tokens":384000,
				"supports_tools":true,
				"supports_vision":false,
				"family":"deepseek",
				"temperature":0.2,
				"max_tokens":8192,
				"reasoning_effort":"high",
				"timeout_seconds":180,
				"override_client_params":true,
				"supports_reasoning":true
			}
		}]
	}`)
	if err := os.WriteFile(filepath.Join(selectionDir, "openrouter.json"), data, 0644); err != nil {
		t.Fatalf("write model selection: %v", err)
	}

	registry := NewRegistry("deepseek/deepseek-v4-pro", time.Minute)
	prov := &fakeProvider{
		name:    "openrouter",
		enabled: true,
		models:  []string{"deepseek/deepseek-v4-pro"},
	}
	registry.Add(&ProviderEntry{Provider: prov, Models: prov.models, Priority: 1})
	registry.SetModels("openrouter", prov.models)

	catalog := NewModelCatalog(registry, dir, time.Minute)
	profile, ok := catalog.Profile("deepseek/deepseek-v4-pro", "openrouter")
	if !ok {
		t.Fatalf("expected profile")
	}
	if profile.MatchPriority != 3 {
		t.Fatalf("priority = %d, want 3", profile.MatchPriority)
	}
	if profile.ContextLength == nil || *profile.ContextLength != 1048576 {
		t.Fatalf("context_length = %v, want 1048576", profile.ContextLength)
	}
	if profile.MaxTokens == nil || *profile.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %v, want 8192", profile.MaxTokens)
	}
	if profile.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want high", profile.ReasoningEffort)
	}
	if !profile.OverrideClientParams {
		t.Fatalf("override_client_params = false, want true")
	}
}

func TestModelCatalogLoadsEmbeddedDefaultSelections(t *testing.T) {
	registry := NewRegistry("deepseek-v4-pro", time.Minute)
	prov := &fakeProvider{
		name:    "deepseek",
		enabled: true,
		models:  []string{"deepseek-v4-pro"},
	}
	registry.Add(&ProviderEntry{Provider: prov, Models: prov.models, Priority: 1})

	catalog := NewModelCatalog(registry, "", time.Minute)
	profile, ok := catalog.Profile("deepseek-v4-pro", "deepseek")
	if !ok {
		t.Fatalf("expected embedded default profile")
	}
	if profile.ContextLength == nil || *profile.ContextLength != 1048576 {
		t.Fatalf("context_length = %v, want 1048576", profile.ContextLength)
	}
	if profile.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want high", profile.ReasoningEffort)
	}
}

func TestModelCatalogProfileAnyFindsEmbeddedModelAcrossProviders(t *testing.T) {
	catalog := NewModelCatalog(nil, "", time.Minute)
	profile, ok := catalog.ProfileAny("deepseek-v4-flash")
	if !ok {
		t.Fatalf("expected embedded profile")
	}
	if profile.ContextLength == nil || *profile.ContextLength != 1048576 {
		t.Fatalf("context_length = %v, want 1048576", profile.ContextLength)
	}
	if profile.MaxOutputTokens == nil || *profile.MaxOutputTokens != 131072 {
		t.Fatalf("max_output_tokens = %v, want 131072", profile.MaxOutputTokens)
	}
	if profile.ReasoningEffort != "medium" {
		t.Fatalf("reasoning_effort = %q, want medium", profile.ReasoningEffort)
	}
}

func TestModelCatalogProfileAnyFindsEmbeddedMetadataSeed(t *testing.T) {
	catalog := NewModelCatalog(nil, "", time.Minute)
	profile, ok := catalog.ProfileAny("openai/gpt-4.1")
	if !ok {
		t.Fatalf("expected embedded metadata profile")
	}
	if profile.ContextLength == nil || *profile.ContextLength != 1047576 {
		t.Fatalf("context_length = %v, want 1047576", profile.ContextLength)
	}
	if profile.MaxOutputTokens == nil || *profile.MaxOutputTokens != 32768 {
		t.Fatalf("max_output_tokens = %v, want 32768", profile.MaxOutputTokens)
	}
	if profile.SupportsVision == nil || !*profile.SupportsVision {
		t.Fatalf("supports_vision = %v, want true", profile.SupportsVision)
	}
	if profile.SupportsTools == nil || !*profile.SupportsTools {
		t.Fatalf("supports_tools = %v, want true", profile.SupportsTools)
	}
}

func TestModelCatalogProfileAnySupportsZAIAlias(t *testing.T) {
	catalog := NewModelCatalog(nil, "", time.Minute)
	profile, ok := catalog.ProfileAny("z-ai/glm-5.2")
	if !ok {
		t.Fatalf("expected z-ai alias metadata profile")
	}
	if profile.Model != "z-ai/glm-5.2" {
		t.Fatalf("profile model = %q, want z-ai/glm-5.2", profile.Model)
	}
	if profile.ContextLength == nil || *profile.ContextLength != 1000000 {
		t.Fatalf("context_length = %v, want 1000000", profile.ContextLength)
	}
	if profile.MaxOutputTokens == nil || *profile.MaxOutputTokens != 131072 {
		t.Fatalf("max_output_tokens = %v, want 131072", profile.MaxOutputTokens)
	}
	if profile.SupportsReasoning == nil || !*profile.SupportsReasoning {
		t.Fatalf("supports_reasoning = %v, want true", profile.SupportsReasoning)
	}
}

func TestModelMetadataSeedDoesNotBecomeRoutableCatalogEntries(t *testing.T) {
	registry := NewRegistry("gpt-4.1", time.Minute)
	prov := &fakeProvider{
		name:    "openai",
		enabled: true,
		models:  []string{"real-discovered-model"},
	}
	registry.Add(&ProviderEntry{Provider: prov, Models: prov.models, Priority: 1})
	registry.SetModels("openai", prov.models)

	catalog := NewModelCatalog(registry, "", time.Minute)
	for _, entry := range catalog.AllEntries() {
		if entry.Model == "openai/gpt-4.1" {
			t.Fatalf("metadata seed model should not be exposed as routable catalog entry")
		}
	}
	if _, ok := catalog.ProfileAny("openai/gpt-4.1"); !ok {
		t.Fatalf("metadata seed should still be available for default enrichment")
	}
}

func TestModelCatalogUserSelectionOverridesEmbeddedDefault(t *testing.T) {
	dir := t.TempDir()
	selectionDir := filepath.Join(dir, "model-selection")
	if err := os.MkdirAll(selectionDir, 0755); err != nil {
		t.Fatalf("mkdir model-selection: %v", err)
	}

	data := []byte(`{
		"provider":"deepseek",
		"models":[{
			"match":"deepseek-v4-pro",
			"priority":1,
			"enabled":true,
			"execution":{
				"context_length":12345,
				"max_tokens":321,
				"reasoning_effort":"low"
			}
		}]
	}`)
	if err := os.WriteFile(filepath.Join(selectionDir, "deepseek.json"), data, 0644); err != nil {
		t.Fatalf("write model selection: %v", err)
	}

	registry := NewRegistry("deepseek-v4-pro", time.Minute)
	prov := &fakeProvider{name: "deepseek", enabled: true, models: []string{"deepseek-v4-pro"}}
	registry.Add(&ProviderEntry{Provider: prov, Models: prov.models, Priority: 1})

	catalog := NewModelCatalog(registry, dir, time.Minute)
	profile, ok := catalog.Profile("deepseek-v4-pro", "deepseek")
	if !ok {
		t.Fatalf("expected overridden profile")
	}
	if profile.ContextLength == nil || *profile.ContextLength != 12345 {
		t.Fatalf("context_length = %v, want override 12345", profile.ContextLength)
	}
	if profile.MaxTokens == nil || *profile.MaxTokens != 321 {
		t.Fatalf("max_tokens = %v, want override 321", profile.MaxTokens)
	}
	if profile.ReasoningEffort != "low" {
		t.Fatalf("reasoning_effort = %q, want override low", profile.ReasoningEffort)
	}
}

func TestModelCatalogProfileUsesMostSpecificSubstringMatch(t *testing.T) {
	dir := t.TempDir()
	selectionDir := filepath.Join(dir, "model-selection")
	if err := os.MkdirAll(selectionDir, 0755); err != nil {
		t.Fatalf("mkdir model-selection: %v", err)
	}

	data := []byte(`{
		"provider":"ollama",
		"models":[
			{"match":"nemotron","priority":1,"enabled":true,"execution":{"timeout_seconds":60}},
			{"match":"nemotron-3-super","priority":2,"enabled":true,"execution":{"timeout_seconds":180}}
		]
	}`)
	if err := os.WriteFile(filepath.Join(selectionDir, "ollama.json"), data, 0644); err != nil {
		t.Fatalf("write model selection: %v", err)
	}

	registry := NewRegistry("nemotron-3-super-120b", time.Minute)
	prov := &fakeProvider{
		name:    "ollama",
		enabled: true,
		models:  []string{"nemotron-3-super-120b"},
	}
	registry.Add(&ProviderEntry{Provider: prov, Models: prov.models, Priority: 1})
	registry.SetModels("ollama", prov.models)

	catalog := NewModelCatalog(registry, dir, time.Minute)
	profile, ok := catalog.Profile("nemotron-3-super-120b", "ollama")
	if !ok {
		t.Fatalf("expected profile")
	}
	if profile.TimeoutSeconds == nil || *profile.TimeoutSeconds != 180 {
		t.Fatalf("timeout_seconds = %v, want 180", profile.TimeoutSeconds)
	}
}

func TestModelCatalogDiscoveryDoesNotCreateDoubleQualifiedAliases(t *testing.T) {
	registry := NewRegistry("shared", time.Minute)
	provA := &fakeProvider{
		name:    "provider-a",
		enabled: true,
		models:  []string{"shared"},
	}
	provB := &fakeProvider{
		name:    "provider-b",
		enabled: true,
		models:  []string{"shared"},
	}
	registry.Add(&ProviderEntry{Provider: provA, Models: provA.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: provB, Models: provB.models, Priority: 2})
	registry.SetModels("provider-a", provA.models)
	registry.SetModels("provider-b", provB.models)

	catalog := NewModelCatalog(registry, "", time.Minute)
	entries := catalog.AllEntries()
	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.Model] = true
		if entry.Model == "shared@provider-a@provider-a" || entry.Model == "shared@provider-b@provider-b" {
			t.Fatalf("unexpected double-qualified alias: %s", entry.Model)
		}
	}

	for _, want := range []string{"shared", "shared@provider-a", "shared@provider-b"} {
		if !names[want] {
			t.Fatalf("missing model %q in %#v", want, names)
		}
	}
}

func TestModelCatalogSyncsConfiguredModelsToRegistry(t *testing.T) {
	dir := t.TempDir()
	selectionDir := filepath.Join(dir, "model-selection")
	if err := os.MkdirAll(selectionDir, 0755); err != nil {
		t.Fatalf("mkdir model-selection: %v", err)
	}

	data := []byte(`{
		"provider":"openrouter",
		"models":[{"match":"configured-model","priority":1,"enabled":true}]
	}`)
	if err := os.WriteFile(filepath.Join(selectionDir, "openrouter.json"), data, 0644); err != nil {
		t.Fatalf("write model selection: %v", err)
	}

	registry := NewRegistry("configured-model", time.Minute)
	prov := &fakeProvider{name: "openrouter", enabled: true}
	registry.Add(&ProviderEntry{Provider: prov, Models: nil, Priority: 1})

	NewModelCatalog(registry, dir, time.Minute)

	candidates := registry.ResolveCandidates("configured-model")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}
	if candidates[0].Provider.Provider.Name() != "openrouter" {
		t.Fatalf("provider = %q, want openrouter", candidates[0].Provider.Provider.Name())
	}
	if candidates[0].ModelID != "configured-model" {
		t.Fatalf("model id = %q, want configured-model", candidates[0].ModelID)
	}

	registry.SetModels("openrouter", []string{"fresh-provider-model"})
	candidates = registry.ResolveCandidates("configured-model")
	if len(candidates) != 1 {
		t.Fatalf("candidates after refresh len = %d, want 1", len(candidates))
	}
	if candidates[0].Provider.Provider.Name() != "openrouter" {
		t.Fatalf("provider after refresh = %q, want openrouter", candidates[0].Provider.Provider.Name())
	}
}

func TestFakeProviderStillSatisfiesProvider(t *testing.T) {
	var _ Provider = (*fakeProvider)(nil)
	_, _ = (&fakeProvider{}).ListModels(context.Background())
}
