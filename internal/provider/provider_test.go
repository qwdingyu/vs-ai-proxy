package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAIProviderUsesCapabilityPaths(t *testing.T) {
	seen := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path]++
		switch r.URL.Path {
		case "/v1beta/openai/models":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"gemini-test"}]}`))
		case "/v1beta/openai/chat/completions":
			var req ChatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode chat request: %v", err)
			}
			if _, ok := req.Extra["provider_routing"]; !ok {
				t.Fatalf("provider_routing extension field was not forwarded")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"id":"chatcmpl-test",
				"object":"chat.completion",
				"created":1,
				"model":"gemini-test",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
			}`))
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("google", "test-key", upstream.URL, true, time.Second)

	models, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if len(models) != 1 || models[0] != "gemini-test" {
		t.Fatalf("unexpected models: %#v", models)
	}

	resp, err := prov.Chat(context.Background(), &ChatRequest{
		Model:    "gemini-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"provider_routing": []byte(`{"allow_fallbacks":true}`),
		},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Model != "gemini-test" {
		t.Fatalf("unexpected response model: %s", resp.Model)
	}

	if seen["/v1beta/openai/models"] != 1 {
		t.Fatalf("models path was not used, seen=%#v", seen)
	}
	if seen["/v1beta/openai/chat/completions"] != 1 {
		t.Fatalf("chat path was not used, seen=%#v", seen)
	}
}

func TestInferCapabilityNameSupportsProviderInstances(t *testing.T) {
	tests := []struct {
		name         string
		id           string
		providerName string
		baseURL      string
		providerType string
		want         string
	}{
		{name: "known id", id: "openrouter", providerType: "openai", want: "openrouter"},
		{name: "known display name", id: "team-a", providerName: "DeepSeek", providerType: "openai", want: "deepseek"},
		{name: "useai paid id", id: "useai-paid", baseURL: "https://api.eforge.xyz/v1", providerType: "openai", want: "useai"},
		{name: "ollama type", id: "local", providerType: "ollama", want: "ollama"},
		{name: "custom openai compatible", id: "sensenova", baseURL: "https://token.sensenova.cn/v1", providerType: "openai", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InferCapabilityName(tt.id, tt.providerName, tt.baseURL, tt.providerType)
			if got != tt.want {
				t.Fatalf("capability = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUseAIProviderUsesV1BaseURLWithoutDuplicatingPath(t *testing.T) {
	seen := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path]++
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"useai-model"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"id":"chatcmpl-useai",
				"object":"chat.completion",
				"created":1,
				"model":"useai-model",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
			}`))
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("UseAI", "", upstream.URL+"/v1", true, time.Second)

	models, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if len(models) != 1 || models[0] != "useai-model" {
		t.Fatalf("models = %#v, want useai-model", models)
	}

	if _, err := prov.ChatRaw(context.Background(), &ChatRequest{
		Model:    "useai-model",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}
	if seen["/v1/models"] != 1 {
		t.Fatalf("models path count = %d, want 1; seen=%#v", seen["/v1/models"], seen)
	}
	if seen["/v1/chat/completions"] != 1 {
		t.Fatalf("chat path count = %d, want 1; seen=%#v", seen["/v1/chat/completions"], seen)
	}
}

func TestCustomOpenAIProviderUsesV1BaseURLWithoutDuplicatingFallbackPath(t *testing.T) {
	seen := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path]++
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"custom-model"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"id":"chatcmpl-custom",
				"object":"chat.completion",
				"created":1,
				"model":"custom-model",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
			}`))
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	prov := NewOpenAIProviderWithCapability("sensenova", "", "test-key", upstream.URL+"/v1", true, time.Second)

	models, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if len(models) != 1 || models[0] != "custom-model" {
		t.Fatalf("models = %#v, want custom-model", models)
	}

	if _, err := prov.ChatRaw(context.Background(), &ChatRequest{
		Model:    "custom-model",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}
	if seen["/v1/models"] != 1 {
		t.Fatalf("models path count = %d, want 1; seen=%#v", seen["/v1/models"], seen)
	}
	if seen["/v1/chat/completions"] != 1 {
		t.Fatalf("chat path count = %d, want 1; seen=%#v", seen["/v1/chat/completions"], seen)
	}
}

func TestOpenAIProviderOmitsAuthorizationHeaderWhenAPIKeyEmpty(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("UseAI", "", upstream.URL+"/v1", true, time.Second)
	if _, err := prov.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
}

func TestOpenAIProviderAddsOpenRouterHeaders(t *testing.T) {
	t.Setenv("PROVIDER_OPENROUTER_REFERER", "https://example.com")
	t.Setenv("PROVIDER_OPENROUTER_TITLE", "VS AI Proxy")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("HTTP-Referer"); got != "https://example.com" {
			t.Fatalf("HTTP-Referer = %q, want https://example.com", got)
		}
		if got := r.Header.Get("X-Title"); got != "VS AI Proxy" {
			t.Fatalf("X-Title = %q, want VS AI Proxy", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q, want application/json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":1,
			"model":"openrouter-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
		}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("openrouter", "test-key", upstream.URL, true, time.Second)
	if _, err := prov.ChatRaw(context.Background(), &ChatRequest{
		Model:    "openrouter-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("ChatRaw returned error: %v", err)
	}
}

func TestOpenAIProviderAddsOpenRouterFallbackHeaders(t *testing.T) {
	t.Setenv("OPENROUTER_HTTP_REFERER", "https://fallback.example")
	t.Setenv("OPENROUTER_X_TITLE", "Fallback Title")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("HTTP-Referer"); got != "https://fallback.example" {
			t.Fatalf("HTTP-Referer = %q, want fallback", got)
		}
		if got := r.Header.Get("X-Title"); got != "Fallback Title" {
			t.Fatalf("X-Title = %q, want fallback", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("openrouter", "test-key", upstream.URL, true, time.Second)
	models, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if len(models) != 1 || models[0] != "model-a" {
		t.Fatalf("models = %#v, want model-a", models)
	}
}

func TestOpenAIProviderDoesNotAddOpenRouterHeadersForOtherProviders(t *testing.T) {
	t.Setenv("PROVIDER_OPENROUTER_REFERER", "https://example.com")
	t.Setenv("PROVIDER_OPENROUTER_TITLE", "VS AI Proxy")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("HTTP-Referer"); got != "" {
			t.Fatalf("HTTP-Referer = %q, want empty", got)
		}
		if got := r.Header.Get("X-Title"); got != "" {
			t.Fatalf("X-Title = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":1,
			"model":"deepseek-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
		}`))
	}))
	defer upstream.Close()

	prov := NewOpenAIProvider("deepseek", "test-key", upstream.URL, true, time.Second)
	if _, err := prov.Chat(context.Background(), &ChatRequest{
		Model:    "deepseek-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
}

func TestProviderHTTPClientDisablesHTTP2ForStreamingStability(t *testing.T) {
	prov := NewOpenAIProvider("deepseek", "test-key", "https://example.com", true, time.Second)
	transport, ok := prov.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", prov.Client.Transport)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatalf("ForceAttemptHTTP2 = true, want false")
	}
	if !transport.DisableCompression {
		t.Fatalf("DisableCompression = false, want true")
	}
	if prov.Client.Timeout != time.Second {
		t.Fatalf("timeout = %s, want configured timeout", prov.Client.Timeout)
	}
}

func TestOllamaProviderBuildsNativeChatRequest(t *testing.T) {
	temp := 0.3
	topP := 0.8
	topK := 40
	maxTokens := 2048
	prov := NewOllamaProvider("ollama", "http://localhost:11434", true, time.Second)

	req := prov.buildChatRequest(&ChatRequest{
		Model:           "llama",
		Temperature:     &temp,
		TopP:            &topP,
		TopK:            &topK,
		MaxTokens:       &maxTokens,
		ReasoningEffort: "high",
		OptionsExtra: map[string]json.RawMessage{
			"num_keep":      []byte(`24`),
			"custom_option": []byte(`{"enabled":true}`),
			"temperature":   []byte(`2`),
		},
		Messages: []Message{{
			Role:      "assistant",
			Content:   "answer",
			Reasoning: "reason",
			Extra: map[string]json.RawMessage{
				"cache_control": []byte(`{"type":"ephemeral"}`),
			},
		}},
	}, true)

	if req["stream"] != true {
		t.Fatalf("stream = %#v, want true", req["stream"])
	}
	messages := req["messages"].([]map[string]any)
	if messages[0]["reasoning_content"] != "reason" {
		t.Fatalf("reasoning_content = %#v, want reason", messages[0]["reasoning_content"])
	}
	if _, ok := messages[0]["cache_control"]; !ok {
		t.Fatalf("cache_control message extension was not preserved: %#v", messages[0])
	}
	options := req["options"].(map[string]any)
	if options["temperature"] != temp {
		t.Fatalf("temperature = %#v, want %v", options["temperature"], temp)
	}
	if options["top_p"] != topP {
		t.Fatalf("top_p = %#v, want %v", options["top_p"], topP)
	}
	if options["top_k"] != topK {
		t.Fatalf("top_k = %#v, want %v", options["top_k"], topK)
	}
	if options["max_tokens"] != maxTokens {
		t.Fatalf("max_tokens = %#v, want %v", options["max_tokens"], maxTokens)
	}
	if options["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", options["reasoning_effort"])
	}
	if options["num_keep"] != float64(24) {
		t.Fatalf("num_keep = %#v, want 24", options["num_keep"])
	}
	if _, ok := options["custom_option"]; !ok {
		t.Fatalf("custom_option was not preserved: %#v", options)
	}
}

func TestOllamaProviderConvertsOpenAIMultimodalContentToImages(t *testing.T) {
	prov := NewOllamaProvider("ollama", "http://localhost:11434", true, time.Second)
	rawContent := json.RawMessage(`[
		{"type":"text","text":"Describe this"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}
	]`)

	req := prov.buildChatRequest(&ChatRequest{
		Model: "llava",
		Messages: []Message{{
			Role:       "user",
			ContentRaw: rawContent,
		}},
	}, false)

	messages := req["messages"].([]map[string]any)
	if messages[0]["content"] != "Describe this" {
		t.Fatalf("content = %#v, want Describe this", messages[0]["content"])
	}
	images, ok := messages[0]["images"].([]string)
	if !ok || len(images) != 1 {
		t.Fatalf("images = %#v, want one image", messages[0]["images"])
	}
	if images[0] != "data:image/png;base64,AA==" {
		t.Fatalf("image = %q, want data url", images[0])
	}
}
