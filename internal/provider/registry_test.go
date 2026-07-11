package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

type fakeProvider struct {
	name    string
	enabled bool
	models  []string
}

func (p *fakeProvider) Name() string {
	return p.name
}

func (p *fakeProvider) Chat(context.Context, *ChatRequest) (*ChatResponse, error) {
	return nil, errors.New("not implemented")
}

func (p *fakeProvider) ChatStream(context.Context, *ChatRequest) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (p *fakeProvider) ListModels(context.Context) ([]string, error) {
	return p.models, nil
}

func (p *fakeProvider) IsEnabled() bool {
	return p.enabled
}

func TestRegistryBuildsModelAliasesAndPriorityCandidates(t *testing.T) {
	registry := NewRegistry("shared", time.Minute)
	slow := &fakeProvider{
		name:    "slow",
		enabled: true,
		models:  []string{"shared", "slow-only"},
	}
	fast := &fakeProvider{
		name:    "fast",
		enabled: true,
		models:  []string{"shared", "fast-only"},
	}

	registry.Add(&ProviderEntry{Provider: slow, Models: slow.models, Priority: 2})
	registry.Add(&ProviderEntry{Provider: fast, Models: fast.models, Priority: 1})
	registry.SetModels("slow", slow.models)
	registry.SetModels("fast", fast.models)

	models := registry.AllModels()
	assertContains(t, models, "shared")
	assertContains(t, models, "shared@fast")
	assertContains(t, models, "shared@slow")

	candidates := registry.ResolveCandidates("shared")
	if len(candidates) != 2 {
		t.Fatalf("expected two candidates, got %#v", candidates)
	}
	if candidates[0].Provider.Provider.Name() != "fast" {
		t.Fatalf("expected fast provider first, got %#v", candidates)
	}
	if candidates[1].Provider.Provider.Name() != "slow" {
		t.Fatalf("expected slow provider second, got %#v", candidates)
	}

	qualified := registry.ResolveCandidates("shared@slow")
	if len(qualified) != 1 || qualified[0].Provider.Provider.Name() != "slow" {
		t.Fatalf("expected qualified alias to pin slow provider, got %#v", qualified)
	}

	tagged := registry.ResolveCandidates("shared@fast:latest")
	if len(tagged) != 1 || tagged[0].Provider.Provider.Name() != "fast" {
		t.Fatalf("expected tagged alias to pin fast provider, got %#v", tagged)
	}
}

func TestRegistryRanksSamePriorityCandidatesByHealth(t *testing.T) {
	registry := NewRegistry("shared", time.Minute)
	slow := &fakeProvider{name: "slow", enabled: true, models: []string{"shared"}}
	fast := &fakeProvider{name: "fast", enabled: true, models: []string{"shared"}}

	registry.Add(&ProviderEntry{Provider: slow, Models: slow.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: fast, Models: fast.models, Priority: 1})
	registry.SetModels("slow", slow.models)
	registry.SetModels("fast", fast.models)
	registry.RecordCandidateSuccess("slow", 900*time.Millisecond)
	registry.RecordCandidateSuccess("fast", 120*time.Millisecond)

	candidates := registry.ResolveCandidates("shared")
	if len(candidates) != 2 {
		t.Fatalf("candidates len = %d, want 2", len(candidates))
	}
	if candidates[0].Provider.Provider.Name() != "fast" {
		t.Fatalf("expected faster healthy provider first, got %#v", candidates)
	}
}

func TestRegistryKeepsManualPriorityBeforeHealth(t *testing.T) {
	registry := NewRegistry("shared", time.Minute)
	primary := &fakeProvider{name: "primary", enabled: true, models: []string{"shared"}}
	secondary := &fakeProvider{name: "secondary", enabled: true, models: []string{"shared"}}

	registry.Add(&ProviderEntry{Provider: primary, Models: primary.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: secondary, Models: secondary.models, Priority: 2})
	registry.SetModels("primary", primary.models)
	registry.SetModels("secondary", secondary.models)
	registry.RecordCandidateSuccess("secondary", 10*time.Millisecond)

	candidates := registry.ResolveCandidates("shared")
	if len(candidates) != 2 {
		t.Fatalf("candidates len = %d, want 2", len(candidates))
	}
	if candidates[0].Provider.Provider.Name() != "primary" {
		t.Fatalf("manual priority should win before health, got %#v", candidates)
	}
}

func TestRegistrySkipsCoolingCandidateWhenAlternativeExists(t *testing.T) {
	registry := NewRegistry("shared", time.Minute)
	flaky := &fakeProvider{name: "flaky", enabled: true, models: []string{"shared"}}
	healthy := &fakeProvider{name: "healthy", enabled: true, models: []string{"shared"}}

	registry.Add(&ProviderEntry{Provider: flaky, Models: flaky.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: healthy, Models: healthy.models, Priority: 1})
	registry.SetModels("flaky", flaky.models)
	registry.SetModels("healthy", healthy.models)
	registry.RecordCandidateFailure("flaky", fmt.Errorf("API 错误 503"))

	candidates := registry.ResolveCandidates("shared")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].Provider.Provider.Name() != "healthy" {
		t.Fatalf("expected cooling provider skipped, got %#v", candidates)
	}
}

func TestRegistryCoolsDownAnyUpstreamServerError(t *testing.T) {
	registry := NewRegistry("shared", time.Minute)
	flaky := &fakeProvider{name: "flaky", enabled: true, models: []string{"shared"}}
	healthy := &fakeProvider{name: "healthy", enabled: true, models: []string{"shared"}}

	registry.Add(&ProviderEntry{Provider: flaky, Models: flaky.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: healthy, Models: healthy.models, Priority: 1})
	registry.SetModels("flaky", flaky.models)
	registry.SetModels("healthy", healthy.models)
	registry.RecordCandidateFailure("flaky", fmt.Errorf("API 错误 502"))

	candidates := registry.ResolveCandidates("shared")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].Provider.Provider.Name() != "healthy" {
		t.Fatalf("expected 5xx provider skipped, got %#v", candidates)
	}
}

func TestRegistryUsesRetryAfterForRateLimitCooldown(t *testing.T) {
	registry := NewRegistry("shared", time.Minute)
	limited := &fakeProvider{name: "limited", enabled: true, models: []string{"shared"}}
	healthy := &fakeProvider{name: "healthy", enabled: true, models: []string{"shared"}}

	registry.Add(&ProviderEntry{Provider: limited, Models: limited.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: healthy, Models: healthy.models, Priority: 1})
	registry.SetModels("limited", limited.models)
	registry.SetModels("healthy", healthy.models)
	registry.RecordCandidateFailure("limited", fmt.Errorf("API 错误 429: rate limited; retry_after_seconds=120"))

	health := registry.ProviderHealthSnapshot()["limited"]
	remaining := time.Until(health.CooldownUntil)
	if remaining < 110*time.Second || remaining > 130*time.Second {
		t.Fatalf("cooldown remaining = %s, want about 120s", remaining)
	}
	candidates := registry.ResolveCandidates("shared")
	if len(candidates) != 1 || candidates[0].Provider.Provider.Name() != "healthy" {
		t.Fatalf("rate-limited provider should be skipped while healthy alternative exists, got %#v", candidates)
	}
}

func TestRegistryReturnsCoolingCandidatesWhenAllAreCooling(t *testing.T) {
	registry := NewRegistry("shared", time.Minute)
	left := &fakeProvider{name: "left", enabled: true, models: []string{"shared"}}
	right := &fakeProvider{name: "right", enabled: true, models: []string{"shared"}}

	registry.Add(&ProviderEntry{Provider: left, Models: left.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: right, Models: right.models, Priority: 1})
	registry.SetModels("left", left.models)
	registry.SetModels("right", right.models)
	registry.RecordCandidateFailure("left", fmt.Errorf("API 错误 503"))
	registry.RecordCandidateFailure("right", fmt.Errorf("API 错误 503"))

	candidates := registry.ResolveCandidates("shared")
	if len(candidates) != 2 {
		t.Fatalf("all cooling candidates should remain as last resort, got %#v", candidates)
	}
}

func TestRegistryResolvesProviderModelHint(t *testing.T) {
	registry := NewRegistry("shared", time.Minute)
	slow := &fakeProvider{
		name:    "slow",
		enabled: true,
		models:  []string{"shared"},
	}
	fast := &fakeProvider{
		name:    "fast",
		enabled: true,
		models:  []string{"shared"},
	}

	registry.Add(&ProviderEntry{Provider: slow, Models: slow.models, Priority: 2})
	registry.Add(&ProviderEntry{Provider: fast, Models: fast.models, Priority: 1})
	registry.SetModels("slow", slow.models)
	registry.SetModels("fast", fast.models)

	candidates := registry.ResolveCandidates("slow/shared")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}
	if candidates[0].Provider.Provider.Name() != "slow" {
		t.Fatalf("provider = %q, want slow", candidates[0].Provider.Provider.Name())
	}
	if candidates[0].ModelID != "shared" {
		t.Fatalf("model id = %q, want shared", candidates[0].ModelID)
	}
}

func TestRegistryResolvesProviderModelHintByUpstreamSuffix(t *testing.T) {
	registry := NewRegistry("qwen/qwen3-coder", time.Minute)
	nvidia := &fakeProvider{
		name:    "nvidia",
		enabled: true,
		models:  []string{"qwen/qwen3-coder"},
	}

	registry.Add(&ProviderEntry{Provider: nvidia, Models: nvidia.models, Priority: 1})
	registry.SetModels("nvidia", nvidia.models)

	candidates := registry.ResolveCandidates("nvidia/qwen3-coder")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}
	if candidates[0].Provider.Provider.Name() != "nvidia" {
		t.Fatalf("provider = %q, want nvidia", candidates[0].Provider.Provider.Name())
	}
	if candidates[0].ModelID != "qwen/qwen3-coder" {
		t.Fatalf("model id = %q, want qwen/qwen3-coder", candidates[0].ModelID)
	}
}

func TestRegistryResolvesVisualStudioDisplayModelName(t *testing.T) {
	registry := NewRegistry("deepseek-v4-flash", time.Minute)
	usecpa := &fakeProvider{
		name:    "usecpa",
		enabled: true,
		models:  []string{"deepseek-v4-flash"},
	}

	registry.Add(&ProviderEntry{Provider: usecpa, Models: usecpa.models, Priority: 1, Aliases: []string{"DEEPSEEK"}})
	registry.SetModels("usecpa", usecpa.models)

	candidates := registry.ResolveCandidates("DEEPSEEK - deepseek-v4-flash:latest")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].Provider.Provider.Name() != "usecpa" {
		t.Fatalf("provider = %q, want usecpa", candidates[0].Provider.Provider.Name())
	}
	if candidates[0].ModelID != "deepseek-v4-flash" {
		t.Fatalf("model id = %q, want deepseek-v4-flash", candidates[0].ModelID)
	}
	if got := registry.ResolveModel("DEEPSEEK - deepseek-v4-flash:latest"); got != "deepseek-v4-flash" {
		t.Fatalf("ResolveModel() = %q, want deepseek-v4-flash", got)
	}
}

func TestRegistryDisplayNamePrefersExactProviderModelBeforeBasename(t *testing.T) {
	registry := NewRegistry("deepseek-v4-flash", time.Minute)
	deepseek := &fakeProvider{
		name:    "deepseek",
		enabled: true,
		models:  []string{"deepseek/deepseek-v4-flash", "deepseek-v4-flash"},
	}

	registry.Add(&ProviderEntry{Provider: deepseek, Models: deepseek.models, Priority: 1, Aliases: []string{"deepseek"}})
	registry.SetModels("deepseek", deepseek.models)

	candidates := registry.ResolveCandidates("deepseek - deepseek-v4-flash")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].UpstreamID != "deepseek-v4-flash" {
		t.Fatalf("upstream = %q, want deepseek-v4-flash", candidates[0].UpstreamID)
	}
}

func TestRegistryProviderHintPrefersExactBareModelBeforeNamespacedModel(t *testing.T) {
	registry := NewRegistry("deepseek-v4-flash", time.Minute)
	deepseek := &fakeProvider{
		name:    "deepseek",
		enabled: true,
		models:  []string{"deepseek/deepseek-v4-flash", "deepseek-v4-flash"},
	}

	registry.Add(&ProviderEntry{Provider: deepseek, Models: deepseek.models, Priority: 1})
	registry.SetModels("deepseek", deepseek.models)

	candidates := registry.ResolveCandidates("deepseek/deepseek-v4-flash")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].UpstreamID != "deepseek-v4-flash" {
		t.Fatalf("upstream = %q, want exact bare deepseek-v4-flash", candidates[0].UpstreamID)
	}
}

func TestRegistryCatalogMappingPrefersExactUpstreamBeforeBasename(t *testing.T) {
	registry := NewRegistry("deepseek-v4-flash", time.Minute)
	deepseek := &fakeProvider{name: "deepseek", enabled: true, models: []string{"deepseek/deepseek-v4-flash", "deepseek-v4-flash"}}

	registry.Add(&ProviderEntry{Provider: deepseek, Models: deepseek.models, Priority: 1})
	registry.SetModels("deepseek", deepseek.models)
	entry := registry.entries["deepseek"]
	registry.UpdateModelMappingsWithUpstream(
		map[string]*ProviderEntry{
			"deepseek/deepseek-v4-flash": entry,
			"deepseek-v4-flash":          entry,
		},
		map[string]string{
			"deepseek/deepseek-v4-flash": "deepseek/deepseek-v4-flash",
			"deepseek-v4-flash":          "deepseek-v4-flash",
		},
		map[string][]*ProviderEntry{
			"deepseek/deepseek-v4-flash": {entry},
			"deepseek-v4-flash":          {entry},
		},
	)

	candidates := registry.ResolveCandidates("deepseek - deepseek-v4-flash")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].UpstreamID != "deepseek-v4-flash" {
		t.Fatalf("upstream = %q, want exact upstream deepseek-v4-flash", candidates[0].UpstreamID)
	}
}

func TestRegistryResolvesVisualStudioDisplayBasenameToNamespacedUpstream(t *testing.T) {
	registry := NewRegistry("z-ai/glm-5.2", time.Minute)
	usecpa := &fakeProvider{
		name:    "usecpa",
		enabled: true,
		models:  []string{"z-ai/glm-5.2"},
	}

	registry.Add(&ProviderEntry{Provider: usecpa, Models: usecpa.models, Priority: 1})
	registry.SetModels("usecpa", usecpa.models)

	for _, requested := range []string{"glm-5.2", "USECPA - glm-5.2:latest"} {
		candidates := registry.ResolveCandidates(requested)
		if len(candidates) != 1 {
			t.Fatalf("%s candidates len = %d, want 1: %#v", requested, len(candidates), candidates)
		}
		if candidates[0].Provider.Provider.Name() != "usecpa" {
			t.Fatalf("%s provider = %q, want usecpa", requested, candidates[0].Provider.Provider.Name())
		}
		if candidates[0].ModelID != "z-ai/glm-5.2" {
			t.Fatalf("%s model id = %q, want z-ai/glm-5.2", requested, candidates[0].ModelID)
		}
		if got := registry.ResolveModel(requested); got != "z-ai/glm-5.2" {
			t.Fatalf("%s ResolveModel() = %q, want z-ai/glm-5.2", requested, got)
		}
	}
}

func TestRegistryDisplayNamePrefixPinsProviderWhenModelShared(t *testing.T) {
	registry := NewRegistry("step-3.7-flash", time.Minute)
	useai := &fakeProvider{name: "useai", enabled: true, models: []string{"step-3.7-flash"}}
	usecpa := &fakeProvider{name: "usecpa", enabled: true, models: []string{"step-3.7-flash"}}

	registry.Add(&ProviderEntry{Provider: useai, Models: useai.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: usecpa, Models: usecpa.models, Priority: 2, Aliases: []string{"UseCpa", "UseCpa Paid"}})
	registry.SetModels("useai", useai.models)
	registry.SetModels("usecpa", usecpa.models)

	for _, requested := range []string{"UseCpa - step-3.7-flash:latest", "UseCpa Paid - step-3.7-flash:latest"} {
		candidates := registry.ResolveCandidates(requested)
		if len(candidates) != 1 {
			t.Fatalf("%s candidates len = %d, want 1: %#v", requested, len(candidates), candidates)
		}
		if candidates[0].Provider.Provider.Name() != "usecpa" {
			t.Fatalf("%s display provider prefix should pin usecpa, got %q", requested, candidates[0].Provider.Provider.Name())
		}
		if candidates[0].ModelID != "step-3.7-flash" {
			t.Fatalf("%s model id = %q, want step-3.7-flash", requested, candidates[0].ModelID)
		}
	}
}

func TestRegistryMergeModelsKeepsConfiguredAndDiscoveredModels(t *testing.T) {
	registry := NewRegistry("", time.Minute)
	useai := &fakeProvider{name: "useai", enabled: true, models: []string{"gpt-5.5"}}

	registry.Add(&ProviderEntry{Provider: useai, Models: []string{"deepseek-v4-flash"}, Priority: 1, Aliases: []string{"UseAI"}})
	registry.MergeModels("useai", []string{"gpt-5.5", "step-3.7-flash"})

	for _, requested := range []string{"UseAI - deepseek-v4-flash", "UseAI - gpt-5.5", "UseAI - step-3.7-flash"} {
		candidates := registry.ResolveCandidates(requested)
		if len(candidates) != 1 {
			t.Fatalf("%s candidates len = %d, want 1: %#v", requested, len(candidates), candidates)
		}
		if candidates[0].Provider.Provider.Name() != "useai" {
			t.Fatalf("%s provider = %q, want useai", requested, candidates[0].Provider.Provider.Name())
		}
	}
}

func TestRegistryDisplayNamePrefixFallsBackOnlyToPinnedProvider(t *testing.T) {
	registry := NewRegistry("step-3.7-flash", time.Minute)
	useai := &fakeProvider{name: "useai", enabled: true, models: []string{"step-3.7-flash"}}
	usecpa := &fakeProvider{name: "usecpa", enabled: true, models: []string{"other-model"}}

	registry.Add(&ProviderEntry{Provider: useai, Models: useai.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: usecpa, Models: usecpa.models, Priority: 2, Aliases: []string{"UseCpa"}})
	registry.SetModels("useai", useai.models)
	registry.SetModels("usecpa", usecpa.models)

	candidates := registry.ResolveCandidates("UseCpa - step-3.7-flash:latest")
	if len(candidates) != 1 {
		t.Fatalf("display provider prefix should still pin one provider, got %#v", candidates)
	}
	if candidates[0].Provider.Provider.Name() != "usecpa" {
		t.Fatalf("display provider prefix must not fallback to useai, got %q", candidates[0].Provider.Provider.Name())
	}
	if candidates[0].UpstreamID != "step-3.7-flash" {
		t.Fatalf("upstream = %q, want step-3.7-flash", candidates[0].UpstreamID)
	}
}

func TestRegistryUnknownDisplayNamePrefixDoesNotFallbackToOtherProvider(t *testing.T) {
	registry := NewRegistry("deepseek-v4-flash", time.Minute)
	deepseek := &fakeProvider{name: "deepseek", enabled: true, models: []string{"deepseek-v4-flash"}}

	registry.Add(&ProviderEntry{Provider: deepseek, Models: deepseek.models, Priority: 1})
	registry.SetModels("deepseek", deepseek.models)

	candidates := registry.ResolveCandidates("UnknownProvider - deepseek-v4-flash")
	if len(candidates) != 0 {
		t.Fatalf("unknown display provider must not fallback to deepseek: %#v", candidates)
	}
}

func TestRegistryDoesNotStripRealColonModelSuffix(t *testing.T) {
	registry := NewRegistry("qwen3-coder:480b", time.Minute)
	ollamaCloud := &fakeProvider{
		name:    "ollama-cloud",
		enabled: true,
		models:  []string{"qwen3-coder:480b"},
	}

	registry.Add(&ProviderEntry{Provider: ollamaCloud, Models: ollamaCloud.models, Priority: 1})
	registry.SetModels("ollama-cloud", ollamaCloud.models)

	candidates := registry.ResolveCandidates("qwen3-coder:480b")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].ModelID != "qwen3-coder:480b" {
		t.Fatalf("model id = %q, want qwen3-coder:480b", candidates[0].ModelID)
	}
}

func TestRegistryRejectsAmbiguousNamespacedBasename(t *testing.T) {
	registry := NewRegistry("z-ai/glm-5.2", time.Minute)
	usecpa := &fakeProvider{name: "usecpa", enabled: true, models: []string{"z-ai/glm-5.2"}}
	other := &fakeProvider{name: "other", enabled: true, models: []string{"other/glm-5.2"}}

	registry.Add(&ProviderEntry{Provider: usecpa, Models: usecpa.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: other, Models: other.models, Priority: 2})
	registry.SetModels("usecpa", usecpa.models)
	registry.SetModels("other", other.models)

	if candidates := registry.ResolveCandidates("glm-5.2"); len(candidates) != 0 {
		t.Fatalf("ambiguous basename should not route automatically: %#v", candidates)
	}

	qualified := registry.ResolveCandidates("z-ai/glm-5.2@usecpa")
	if len(qualified) != 1 || qualified[0].Provider.Provider.Name() != "usecpa" {
		t.Fatalf("qualified model should still route to usecpa: %#v", qualified)
	}
}

func TestRegistryPrefersDirectOfficialGLMOverNamespacedBasename(t *testing.T) {
	registry := NewRegistry("glm-5.2", time.Minute)
	zhipu := &fakeProvider{name: "zhipu", enabled: true, models: []string{"glm-5.2"}}
	usecpa := &fakeProvider{name: "usecpa", enabled: true, models: []string{"z-ai/glm-5.2"}}

	registry.Add(&ProviderEntry{Provider: zhipu, Models: zhipu.models, Priority: 1})
	registry.Add(&ProviderEntry{Provider: usecpa, Models: usecpa.models, Priority: 2})
	registry.SetModels("zhipu", zhipu.models)
	registry.SetModels("usecpa", usecpa.models)

	candidates := registry.ResolveCandidates("glm-5.2")
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].Provider.Provider.Name() != "zhipu" {
		t.Fatalf("provider = %q, want zhipu", candidates[0].Provider.Provider.Name())
	}
	if candidates[0].ModelID != "glm-5.2" {
		t.Fatalf("model id = %q, want glm-5.2", candidates[0].ModelID)
	}

	namespaced := registry.ResolveCandidates("z-ai/glm-5.2")
	if len(namespaced) != 1 || namespaced[0].Provider.Provider.Name() != "usecpa" || namespaced[0].ModelID != "z-ai/glm-5.2" {
		t.Fatalf("namespaced model should route to usecpa upstream z-ai/glm-5.2: %#v", namespaced)
	}
}

func assertContains(t *testing.T, values []string, want string) {
	t.Helper()
	for _, value := range values {
		if value == want {
			return
		}
	}
	t.Fatalf("expected %q in %#v", want, values)
}
