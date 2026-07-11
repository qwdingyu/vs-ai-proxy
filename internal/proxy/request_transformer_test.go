package proxy

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

func TestApplyExecutionDefaultsInjectsReasoningEffortForSupportedProvider(t *testing.T) {
	server := &Server{
		config: &config.AppConfig{
			Models: []config.ModelConfig{{
				Name:            "deepseek-v4-pro",
				Provider:        "deepseek",
				ReasoningEffort: "high",
				TopP:            floatPtr(0.9),
				Enabled:         true,
			}},
		},
		reasoningCache: newReasoningCache(),
	}
	req := &provider.ChatRequest{Model: "deepseek-v4-pro"}

	server.applyExecutionDefaults(server.config, req, "deepseek-v4-pro", &stubProvider{name: "deepseek"})

	if req.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want high", req.ReasoningEffort)
	}
	if req.TopP != nil {
		t.Fatalf("top_p should not be injected for native reasoner, got %v", *req.TopP)
	}
}

func TestApplyExecutionDefaultsStripsUnsupportedTopK(t *testing.T) {
	server := &Server{
		config:         &config.AppConfig{},
		reasoningCache: newReasoningCache(),
	}
	topK := 42
	req := &provider.ChatRequest{Model: "moonshot-v1", TopK: &topK}

	server.applyExecutionDefaults(server.config, req, "moonshot-v1", &stubProvider{name: "moonshot"})

	if req.TopK != nil {
		t.Fatalf("top_k should be stripped for unsupported provider")
	}
}

func TestApplyExecutionDefaultsOverrideClientParams(t *testing.T) {
	server := &Server{
		config: &config.AppConfig{
			Models: []config.ModelConfig{{
				Name:                 "kimi-k2.6",
				Provider:             "openrouter",
				Temperature:          floatPtr(1),
				MaxTokens:            intPtr(8192),
				OverrideClientParams: true,
				Enabled:              true,
			}},
		},
		reasoningCache: newReasoningCache(),
	}
	clientTemp := 0.2
	clientMax := 128
	req := &provider.ChatRequest{
		Model:       "kimi-k2.6",
		Temperature: &clientTemp,
		MaxTokens:   &clientMax,
	}

	server.applyExecutionDefaults(server.config, req, "kimi-k2.6", &stubProvider{name: "openrouter"})

	if req.Temperature == nil || *req.Temperature != 1 {
		t.Fatalf("temperature = %v, want 1", req.Temperature)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %v, want 8192", req.MaxTokens)
	}
}

func TestApplyExecutionDefaultsMatchesVisualStudioDisplayAndLatestAlias(t *testing.T) {
	server := &Server{
		config: &config.AppConfig{
			Models: []config.ModelConfig{{
				Name:            "z-ai/glm-5.2",
				ProviderID:      "usecpa",
				MaxTokens:       intPtr(131072),
				ContextLength:   intPtr(1000000),
				MaxOutputTokens: intPtr(131072),
				Enabled:         true,
			}},
		},
		reasoningCache: newReasoningCache(),
	}
	req := &provider.ChatRequest{Model: "glm-5.2"}

	server.applyExecutionDefaults(
		server.config,
		req,
		"USECPA - glm-5.2:latest",
		&stubProvider{name: "usecpa"},
	)

	if req.MaxTokens == nil || *req.MaxTokens != 131072 {
		t.Fatalf("max_tokens = %v, want 131072", req.MaxTokens)
	}
}

func TestApplyProfileDefaultsOverrideClientParams(t *testing.T) {
	server := &Server{}
	clientTemp := 0.2
	clientMax := 128
	profileTemp := 1.0
	profileMax := 4096
	profileCtx := 2048
	req := &provider.ChatRequest{
		Model:       "deepseek-v4-pro",
		Temperature: &clientTemp,
		MaxTokens:   &clientMax,
	}

	server.applyProfileDefaults(req, provider.ModelProfile{
		Temperature:          &profileTemp,
		MaxTokens:            &profileMax,
		ContextLength:        &profileCtx,
		ReasoningEffort:      "high",
		OverrideClientParams: true,
	}, &stubProvider{name: "deepseek"})

	if req.Temperature == nil || *req.Temperature != profileTemp {
		t.Fatalf("temperature = %v, want %v", req.Temperature, profileTemp)
	}
	if req.MaxTokens == nil || *req.MaxTokens != profileCtx {
		t.Fatalf("max_tokens = %v, want context cap %d", req.MaxTokens, profileCtx)
	}
	if req.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want high", req.ReasoningEffort)
	}
}

func TestApplyProfileDefaultsStripsUnsupportedReasoning(t *testing.T) {
	server := &Server{}
	req := &provider.ChatRequest{Model: "moonshot-v1"}

	server.applyProfileDefaults(req, provider.ModelProfile{
		ReasoningEffort: "high",
	}, &stubProvider{name: "moonshot"})

	if req.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want empty", req.ReasoningEffort)
	}
}

func TestInjectCachedReasoningAndDropPlaceholder(t *testing.T) {
	server := &Server{
		config:         &config.AppConfig{},
		reasoningCache: newReasoningCache(),
	}
	server.reasoningCache.Set("assistant:0", "cached reasoning")

	req := &provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "assistant", Content: ""},
			{Role: "assistant", Content: "real"},
		},
	}

	server.injectCachedReasoning(req)

	if len(req.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Reasoning != "cached reasoning" {
		t.Fatalf("reasoning = %q, want cached reasoning", req.Messages[0].Reasoning)
	}
}

func TestInjectCachedReasoningByToolCallKey(t *testing.T) {
	server := &Server{
		config:         &config.AppConfig{},
		reasoningCache: newReasoningCache(),
	}
	server.reasoningCache.Set("toolcall:call_1", "tool reasoning")

	req := &provider.ChatRequest{
		Messages: []provider.Message{{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: provider.FunctionCall{
					Name:      "demo",
					Arguments: "{}",
				},
			}},
		}},
	}

	server.injectCachedReasoning(req)

	if req.Messages[0].Reasoning != "tool reasoning" {
		t.Fatalf("reasoning = %q, want tool reasoning", req.Messages[0].Reasoning)
	}
}

func TestTransformRequestDoesNotInjectCachedReasoningForUseAIGateway(t *testing.T) {
	server := &Server{config: &config.AppConfig{}, reasoningCache: newReasoningCache()}
	server.reasoningCache.Set("assistant:0", "cached reasoning")
	req := &provider.ChatRequest{Model: "deepseek-v4-flash", Messages: []provider.Message{{Role: "assistant"}}}
	prov := provider.NewOpenAIProviderWithCapability("useai", "useai", "sk-test", "https://api.eforge.xyz/v1", true, 0)

	server.transformRequest(server.config, req, "UseAI - deepseek-v4-flash", prov)

	if req.Messages[0].Reasoning != "" {
		t.Fatalf("useai gateway must not receive cached reasoning_content, got %q", req.Messages[0].Reasoning)
	}
}

func TestTransformRequestInjectsCachedReasoningForDirectReasoningProvider(t *testing.T) {
	server := &Server{config: &config.AppConfig{}, reasoningCache: newReasoningCache()}
	server.reasoningCache.Set("assistant:0", "cached reasoning")
	req := &provider.ChatRequest{Model: "deepseek-v4-flash", Messages: []provider.Message{{Role: "assistant"}}}
	prov := provider.NewOpenAIProviderWithCapability("deepseek", "deepseek", "sk-test", "https://api.deepseek.com", true, 0)

	server.transformRequest(server.config, req, "deepseek - deepseek-v4-flash", prov)

	if req.Messages[0].Reasoning != "cached reasoning" {
		t.Fatalf("direct reasoning provider should keep cached reasoning_content, got %q", req.Messages[0].Reasoning)
	}
}

func TestTransformRequestAvoidsBodyGrowthFromCachedReasoningOnUseAI(t *testing.T) {
	server := &Server{config: &config.AppConfig{}, reasoningCache: newReasoningCache()}
	server.reasoningCache.Set("assistant:0", strings.Repeat("reasoning ", 10_000))
	req := &provider.ChatRequest{Model: "deepseek-v4-flash", Messages: []provider.Message{{Role: "assistant"}}}
	prov := provider.NewOpenAIProviderWithCapability("useai", "useai", "sk-test", "https://api.eforge.xyz/v1", true, 0)

	before, err := provider.OpenAIChatCompletionsRequestBytes(req)
	if err != nil {
		t.Fatalf("before bytes: %v", err)
	}
	server.transformRequest(server.config, req, "UseAI - deepseek-v4-flash", prov)
	after, err := provider.OpenAIChatCompletionsRequestBytes(req)
	if err != nil {
		t.Fatalf("after bytes: %v", err)
	}

	if after-before > 1024 {
		t.Fatalf("useai request grew by %d bytes after transform; cached reasoning should not be injected", after-before)
	}
}

func TestCloneChatRequestDeepCopiesLegacyFunctionCall(t *testing.T) {
	req := &provider.ChatRequest{
		Messages: []provider.Message{{
			Role: "assistant",
			FunctionCall: &provider.FunctionCall{
				Name:      "powershell",
				Arguments: `{"command":"pwd"}`,
				Extra: map[string]json.RawMessage{
					"provider_state": []byte(`{"chunk":1}`),
				},
			},
		}},
	}

	clone := cloneChatRequest(req)
	req.Messages[0].FunctionCall.Name = "mutated"
	req.Messages[0].FunctionCall.Extra["provider_state"] = []byte(`{"chunk":2}`)

	if clone.Messages[0].FunctionCall == nil || clone.Messages[0].FunctionCall.Name != "powershell" {
		t.Fatalf("function_call was not deep-copied: %#v", clone.Messages[0].FunctionCall)
	}
	if string(clone.Messages[0].FunctionCall.Extra["provider_state"]) != `{"chunk":1}` {
		t.Fatalf("function_call extra was not deep-copied: %s", clone.Messages[0].FunctionCall.Extra["provider_state"])
	}
}

type stubProvider struct {
	name string
}

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	panic("not implemented")
}

func (p *stubProvider) ChatStream(context.Context, *provider.ChatRequest) (io.ReadCloser, error) {
	panic("not implemented")
}

func (p *stubProvider) ListModels(context.Context) ([]string, error) {
	panic("not implemented")
}

func (p *stubProvider) IsEnabled() bool { return true }

func floatPtr(v float64) *float64 { return &v }
