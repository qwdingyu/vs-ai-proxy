package provider

import (
	"context"
	"errors"
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

	registry.Add(&ProviderEntry{Provider: usecpa, Models: usecpa.models, Priority: 1})
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

func assertContains(t *testing.T, values []string, want string) {
	t.Helper()
	for _, value := range values {
		if value == want {
			return
		}
	}
	t.Fatalf("expected %q in %#v", want, values)
}
