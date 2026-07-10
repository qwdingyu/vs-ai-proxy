package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

// -----------------------------------------------------------------------------
// 通用 fake provider
// -----------------------------------------------------------------------------

type fakeChatResponse struct {
	Model     string
	Content   string
	Reasoning string
}

type fakeProvider struct {
	name          string
	enabled       bool
	models        []string
	chatResp      *fakeChatResponse
	streamBody    string
	chatErr       error
	streamErr     error
	lastReq       *provider.ChatRequest
	hadDeadline   bool
	streamReadErr error
	streamCalls   int
	rawBody       []byte
	rawErr        error
	rawCalls      int
	chatCalls     int
}

func newFakeProvider(name string, enabled bool, models []string, resp *fakeChatResponse, streamBody string) *fakeProvider {
	return &fakeProvider{
		name:       name,
		enabled:    enabled,
		models:     models,
		chatResp:   resp,
		streamBody: streamBody,
	}
}

func (p *fakeProvider) Name() string {
	return p.name
}

func (p *fakeProvider) IsEnabled() bool {
	return p.enabled
}

func (p *fakeProvider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	p.chatCalls++
	p.lastReq = cloneChatRequest(req)
	_, p.hadDeadline = ctx.Deadline()
	if p.chatErr != nil {
		return nil, p.chatErr
	}
	if p.chatResp == nil {
		return nil, errors.New("not implemented")
	}
	return &provider.ChatResponse{
		Model: p.chatResp.Model,
		Choices: []provider.Choice{{
			Message: provider.Message{
				Role:      "assistant",
				Content:   p.chatResp.Content,
				Reasoning: p.chatResp.Reasoning,
			},
			FinishReason: "stop",
		}},
	}, nil
}

func (p *fakeProvider) ChatStream(ctx context.Context, _ *provider.ChatRequest) (io.ReadCloser, error) {
	_, p.hadDeadline = ctx.Deadline()
	p.streamCalls++
	if p.streamErr != nil {
		return nil, p.streamErr
	}
	if p.streamBody == "" {
		return nil, errors.New("not implemented")
	}
	if p.streamReadErr != nil {
		return &errAfterEOFReader{
			reader: strings.NewReader(p.streamBody),
			err:    p.streamReadErr,
		}, nil
	}
	return io.NopCloser(strings.NewReader(p.streamBody)), nil
}

func (p *fakeProvider) ChatRaw(ctx context.Context, req *provider.ChatRequest) ([]byte, error) {
	p.rawCalls++
	p.lastReq = cloneChatRequest(req)
	_, p.hadDeadline = ctx.Deadline()
	if p.rawErr != nil {
		return nil, p.rawErr
	}
	if len(p.rawBody) > 0 {
		return append([]byte(nil), p.rawBody...), nil
	}
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if provider.ResolveApiFormat(p) == provider.ApiFormatOpenAi {
		return json.Marshal(resp)
	}
	return json.Marshal(buildOllamaChatResponse(req.Model, resp))
}

func (p *fakeProvider) ListModels(_ context.Context) ([]string, error) {
	return p.models, nil
}

type errAfterEOFReader struct {
	reader *strings.Reader
	err    error
}

func (r *errAfterEOFReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		return n, r.err
	}
	return n, err
}

func (r *errAfterEOFReader) Close() error {
	return nil
}

// -----------------------------------------------------------------------------
// 辅助函数：构建 server 和本地 mux
// -----------------------------------------------------------------------------

func newTestServer(providers ...provider.Provider) *Server {
	cfg := &config.AppConfig{
		Port:         8080,
		DefaultModel: "default-model",
		Providers:    []config.ProviderConfig{},
		Models:       []config.ModelConfig{},
	}
	registry := provider.NewRegistry(cfg.DefaultModel, 0)

	for _, p := range providers {
		models, _ := p.ListModels(context.Background())
		entry := &provider.ProviderEntry{
			Provider: p,
			Models:   models,
			Priority: 0,
		}
		registry.Add(entry)
	}

	return &Server{
		config:   cfg,
		registry: registry,
		proxyKey: "test-secret",
		logger:   log.New(nil, log.LevelError, false),
		store:    store.New(10),
	}
}

func newOpenServer(providers ...provider.Provider) *Server {
	s := newTestServer(providers...)
	s.proxyKey = ""
	return s
}

func withMux(server *Server, register func(mux *http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	register(mux)
	return mux
}

func containsAnyString(values []any, want string) bool {
	for _, value := range values {
		if s, ok := value.(string); ok && s == want {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// 1. 模型 catalog：/v1/models 与 /api/tags
// -----------------------------------------------------------------------------

func TestCatalogEndpointsReturnModels(t *testing.T) {
	provA := newFakeProvider("provider-a", true, []string{"model-a", "shared"}, &fakeChatResponse{Model: "model-a", Content: "hi"}, "")
	provB := newFakeProvider("provider-b", true, []string{"model-b", "shared"}, nil, "")

	server := newOpenServer(provA, provB)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/models", server.handleListModels)
		mux.HandleFunc("/api/tags", server.handleOllamaTags)
	})

	tests := []struct {
		name string
		path string
		want []string
	}{
		{
			name: "openai models",
			path: "/v1/models",
			want: []string{"model-a", "model-b", "shared", "shared@provider-a", "shared@provider-b"},
		},
		{
			name: "ollama tags",
			path: "/api/tags",
			want: []string{"model-a@provider-a", "model-b@provider-b", "shared@provider-a", "shared@provider-b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			var items []map[string]any
			if tt.path == "/v1/models" {
				data, _ := body["data"].([]any)
				for _, it := range data {
					m, _ := it.(map[string]any)
					items = append(items, m)
				}
			} else {
				models, _ := body["models"].([]any)
				for _, it := range models {
					m, _ := it.(map[string]any)
					items = append(items, m)
				}
			}

			got := make([]string, 0, len(items))
			for _, it := range items {
				if id, ok := it["id"].(string); ok && id != "" {
					got = append(got, id)
				} else if model, ok := it["model"].(string); ok && model != "" {
					got = append(got, model)
				} else if name, ok := it["name"].(string); ok && name != "" {
					got = append(got, name)
				}
			}

			for _, want := range tt.want {
				found := false
				for _, g := range got {
					if g == want {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("missing model %q in %#v", want, got)
				}
			}
		})
	}
}

func TestCatalogEndpointsRebuildAfterRegistryModelDiscovery(t *testing.T) {
	prov := newFakeProvider("sensenova", true, []string{"deepseek-v4-flash"}, &fakeChatResponse{Model: "deepseek-v4-flash", Content: "ok"}, "")
	cfg := &config.AppConfig{DefaultModel: "deepseek-v4-flash"}
	registry := provider.NewRegistry(cfg.DefaultModel, time.Minute)
	catalog := provider.NewModelCatalog(registry, "", time.Minute)

	registry.Add(&provider.ProviderEntry{
		Provider: prov,
		Models:   []string{"deepseek-v4-flash"},
		Priority: 0,
	})

	server := &Server{
		config:   cfg,
		registry: registry,
		catalog:  catalog,
		logger:   log.New(nil, log.LevelError, false),
		store:    store.New(10),
	}
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/models", server.handleListModels)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "deepseek-v4-flash") {
		t.Fatalf("/v1/models should include model discovered after catalog creation: %s", rec.Body.String())
	}
}

func TestOpenAIModelsExposeIdentityMetadata(t *testing.T) {
	prov := newFakeProvider("usecpa", true, []string{"z-ai/glm-5.2"}, &fakeChatResponse{Model: "z-ai/glm-5.2", Content: "ok"}, "")
	server := newOpenServer(prov)
	supportsTools := true
	supportsVision := false
	server.config.Providers = []config.ProviderConfig{{
		ID:          "usecpa",
		Name:        "UseCpa",
		DisplayName: "UseCpa Paid",
		Type:        "openai",
		Enabled:     true,
	}}
	server.config.Models = []config.ModelConfig{{
		Name:           "z-ai/glm-5.2",
		ProviderID:     "usecpa",
		Provider:       "usecpa",
		SupportsTools:  &supportsTools,
		SupportsVision: &supportsVision,
		Enabled:        true,
	}}
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/models", server.handleListModels)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Fatalf("models missing: %s", rec.Body.String())
	}
	item, _ := data[0].(map[string]any)
	if item["display_name"] != "UseCpa Paid - glm-5.2" {
		t.Fatalf("display_name = %v, want UseCpa Paid - glm-5.2", item["display_name"])
	}
	if item["upstream_model"] != "z-ai/glm-5.2" {
		t.Fatalf("upstream_model = %v, want z-ai/glm-5.2", item["upstream_model"])
	}
	if item["canonical"] != "z-ai/glm-5.2@usecpa" {
		t.Fatalf("canonical = %v, want z-ai/glm-5.2@usecpa", item["canonical"])
	}
	if item["supports_tools"] != true || item["supports_tool_calls"] != true {
		t.Fatalf("tool support metadata missing: %#v", item)
	}
	capabilities, _ := item["capabilities"].([]any)
	if !containsAnyString(capabilities, "tools") {
		t.Fatalf("capabilities = %#v, want tools", capabilities)
	}
	modelInfo, _ := item["model_info"].(map[string]any)
	if modelInfo["supports_tools"] != true || modelInfo["supports_tool_calls"] != true {
		t.Fatalf("model_info tool support metadata missing: %#v", modelInfo)
	}
}

func TestOpenAIChatForwardsToolRequestFields(t *testing.T) {
	prov := newFakeProvider("usecpa", true, []string{"z-ai/glm-5.2"}, &fakeChatResponse{Model: "z-ai/glm-5.2", Content: "ok"}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"z-ai/glm-5.2",
		"messages":[{"role":"user","content":"create a file"}],
		"tools":[{"type":"function","strict":true,"function":{"name":"create_file","description":"Create file","parameters":{"type":"object"}}}],
		"tool_choice":"auto",
		"parallel_tool_calls":true
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if prov.lastReq == nil || len(prov.lastReq.Tools) != 1 {
		t.Fatalf("tools were not forwarded: %#v", prov.lastReq)
	}
	if _, ok := prov.lastReq.Extra["tool_choice"]; !ok {
		t.Fatalf("tool_choice was not preserved: %#v", prov.lastReq.Extra)
	}
	if _, ok := prov.lastReq.Extra["parallel_tool_calls"]; !ok {
		t.Fatalf("parallel_tool_calls was not preserved: %#v", prov.lastReq.Extra)
	}
}

func TestOpenAIChatConvertsNonStreamSSEBodyToJSON(t *testing.T) {
	prov := newFakeProvider("useai", true, []string{"gpt-5.5"}, nil, "")
	prov.rawBody = []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"!"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"hi"}],
		"stream":false
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("non-stream response leaked SSE body: %s", rec.Body.String())
	}
	var body provider.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response should be JSON chat response: %v; body=%s", err, rec.Body.String())
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "Hello!" {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
}

func TestOpenAIChatConvertsDSMLTextToToolCalls(t *testing.T) {
	prov := newFakeProvider("useai", true, []string{"step-router-v1"}, &fakeChatResponse{Model: "step-router-v1", Content: dsmlAdvisorSample}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"step-router-v1",
		"messages":[{"role":"user","content":"review files"}],
		"tools":[{"type":"function","function":{"name":"get_file","description":"Read file","parameters":{"type":"object"}}}],
		"stream":false
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body provider.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response should be JSON: %v; body=%s", err, rec.Body.String())
	}
	if len(body.Choices) != 1 || body.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish reason/tool choice missing: %s", rec.Body.String())
	}
	if len(body.Choices[0].Message.ToolCalls) != 4 || body.Choices[0].Message.ToolCalls[0].Function.Name != "get_file" {
		t.Fatalf("tool calls missing: %s", rec.Body.String())
	}
}

func TestOpenAIChatFallsBackToStreamWhenNonStreamUpstreamFails(t *testing.T) {
	prov := newFakeProvider("useai", true, []string{"gpt-5.5"}, nil, strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":" from stream"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))
	prov.rawErr = errors.New(`API 错误 503: {"error":{"message":"Service temporarily unavailable"}}`)
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"hi"}],
		"stream":false
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body provider.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response should be JSON chat response: %v; body=%s", err, rec.Body.String())
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "Hello from stream" {
		t.Fatalf("unexpected fallback response: %s", rec.Body.String())
	}
	if prov.rawCalls != 1 || prov.streamCalls != 1 {
		t.Fatalf("rawCalls=%d streamCalls=%d, want one raw failure and one stream fallback", prov.rawCalls, prov.streamCalls)
	}
	if got := rec.Header().Get("X-Proxy-Fallback-Mode"); got != "nonstream-to-stream" {
		t.Fatalf("fallback header = %q, want nonstream-to-stream", got)
	}
}

func TestOpenAIChatDoesNotFallbackToStreamForClientError(t *testing.T) {
	prov := newFakeProvider("useai", true, []string{"gpt-5.5"}, nil, `data: [DONE]`+"\n")
	prov.rawErr = errors.New(`API 错误 400: {"error":{"message":"invalid tools"}}`)
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"hi"}],
		"stream":false
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if prov.streamCalls != 0 {
		t.Fatalf("streamCalls=%d, client errors must not trigger alternate-mode requests", prov.streamCalls)
	}
}

func TestOpenAIStreamFallsBackToNonStreamWhenStreamUpstreamFails(t *testing.T) {
	prov := newFakeProvider("useai2", true, []string{"gpt-5.5"}, &fakeChatResponse{Model: "gpt-5.5", Content: "Hello non-stream"}, "")
	prov.streamErr = errors.New(`API 错误 503: {"error":{"message":"Service temporarily unavailable"}}`)
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"hi"}],
		"stream":true
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data:") || !strings.Contains(body, "Hello non-stream") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected SSE fallback body, got: %s", body)
	}
	if prov.streamCalls != 1 || prov.chatCalls != 1 {
		t.Fatalf("streamCalls=%d chatCalls=%d, want one stream failure and one non-stream fallback", prov.streamCalls, prov.chatCalls)
	}
	if got := rec.Header().Get("X-Proxy-Fallback-Mode"); got != "stream-to-nonstream" {
		t.Fatalf("fallback header = %q, want stream-to-nonstream", got)
	}
}

func TestOpenAIStreamFallbackConvertsDSMLToToolCalls(t *testing.T) {
	prov := newFakeProvider("useai", true, []string{"step-router-v1"}, &fakeChatResponse{Model: "step-router-v1", Content: dsmlAdvisorSample}, "")
	prov.streamErr = errors.New(`API 错误 503: {"error":{"message":"Service temporarily unavailable"}}`)
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"step-router-v1",
		"messages":[{"role":"user","content":"review files"}],
		"tools":[{"type":"function","function":{"name":"get_file","description":"Read file","parameters":{"type":"object"}}}],
		"stream":true
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"name":"get_file"`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected SSE tool call fallback body, got: %s", body)
	}
}

func TestOllamaChatConvertsDSMLTextToToolCalls(t *testing.T) {
	prov := newFakeProvider("useai", true, []string{"step-router-v1"}, &fakeChatResponse{Model: "step-router-v1", Content: dsmlAdvisorSample}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/chat", server.handleOllamaChat)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{
		"model":"step-router-v1",
		"messages":[{"role":"user","content":"review files"}],
		"tools":[{"type":"function","function":{"name":"get_file","description":"Read file","parameters":{"type":"object"}}}],
		"stream":false
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"name":"get_file"`) || !strings.Contains(body, `"done_reason":"tool_calls"`) {
		t.Fatalf("expected Ollama tool call body, got: %s", body)
	}
}

func TestOllamaChatForwardsToolSchemaExtensionsToOpenAIProvider(t *testing.T) {
	prov := newFakeProvider("usecpa", true, []string{"z-ai/glm-5.2"}, &fakeChatResponse{Model: "z-ai/glm-5.2", Content: "ok"}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/chat", server.handleOllamaChat)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{
		"model":"z-ai/glm-5.2",
		"messages":[{"role":"user","content":"create a file"}],
		"tools":[{"type":"function","strict":true,"function":{"name":"create_file","description":"Create file","parameters":{"type":"object"},"x-provider":"keep"}}],
		"stream":false
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if prov.lastReq == nil || len(prov.lastReq.Tools) != 1 {
		t.Fatalf("tools were not forwarded: %#v", prov.lastReq)
	}
	if _, ok := prov.lastReq.Tools[0].Extra["strict"]; !ok {
		t.Fatalf("tool strict extension was not preserved: %#v", prov.lastReq.Tools[0])
	}
	if _, ok := prov.lastReq.Tools[0].Function.Extra["x-provider"]; !ok {
		t.Fatalf("function extension was not preserved: %#v", prov.lastReq.Tools[0].Function)
	}
}

func TestOllamaTagsExposeQualifiedAliasesAndCapabilities(t *testing.T) {
	provA := newFakeProvider("provider-a", true, []string{"shared"}, &fakeChatResponse{Model: "shared", Content: "hi"}, "")
	provB := newFakeProvider("provider-b", true, []string{"shared"}, nil, "")
	server := newOpenServer(provA, provB)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/tags", server.handleOllamaTags)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	models, _ := body["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models len = %d, want provider-specific entries only: %s", len(models), rec.Body.String())
	}

	for _, raw := range models {
		item, _ := raw.(map[string]any)
		model, _ := item["model"].(string)
		if model != "shared@provider-a" && model != "shared@provider-b" {
			t.Fatalf("unexpected model alias %q", model)
		}
		name, _ := item["name"].(string)
		if strings.HasSuffix(name, ":latest") {
			t.Fatalf("display name should not include :latest: %q", name)
		}
		aliases, _ := item["aliases"].([]any)
		if len(aliases) == 0 {
			t.Fatalf("aliases missing for %q", model)
		}
		if !containsAnyString(aliases, "shared:latest") || !containsAnyString(aliases, model+":latest") {
			t.Fatalf("latest aliases missing for %q: %#v", model, aliases)
		}
		if item["context_length"] == nil || item["max_output_tokens"] == nil {
			t.Fatalf("token limits missing in %#v", item)
		}
		if item["model_info"] == nil || item["capabilities"] == nil {
			t.Fatalf("capability metadata missing in %#v", item)
		}
	}
}

func TestOllamaTagsUseModelConfigLimits(t *testing.T) {
	prov := newFakeProvider("sensenova", true, []string{"deepseek-v4-flash"}, &fakeChatResponse{Model: "deepseek-v4-flash", Content: "hi"}, "")
	server := newOpenServer(prov)
	supportsTools := true
	server.config.Models = []config.ModelConfig{{
		Name:            "deepseek-v4-flash",
		ProviderID:      "sensenova",
		Provider:        "sensenova",
		ContextLength:   intPtr(1048576),
		MaxOutputTokens: intPtr(65536),
		SupportsTools:   &supportsTools,
		Enabled:         true,
	}}
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/tags", server.handleOllamaTags)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	models, _ := body["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models len = %d, want 1: %s", len(models), rec.Body.String())
	}
	item, _ := models[0].(map[string]any)
	if got := int(item["context_length"].(float64)); got != 1048576 {
		t.Fatalf("context_length = %d, want 1048576; body=%s", got, rec.Body.String())
	}
	if got := int(item["max_output_tokens"].(float64)); got != 65536 {
		t.Fatalf("max_output_tokens = %d, want 65536; body=%s", got, rec.Body.String())
	}
}

func TestOllamaTagsUseConfiguredProviderDisplayName(t *testing.T) {
	prov := newFakeProvider("usecpa", true, []string{"z-ai/glm-5.2"}, &fakeChatResponse{Model: "z-ai/glm-5.2", Content: "hi"}, "")
	server := newOpenServer(prov)
	server.config.Providers = []config.ProviderConfig{{
		ID:          "usecpa",
		Name:        "UseCpa",
		DisplayName: "UseCpa Paid",
		Type:        "openai",
		Enabled:     true,
	}}
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/tags", server.handleOllamaTags)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	models, _ := body["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models len = %d, want 1: %s", len(models), rec.Body.String())
	}
	item, _ := models[0].(map[string]any)
	if item["name"] != "UseCpa Paid - glm-5.2" {
		t.Fatalf("name = %v, want UseCpa Paid - glm-5.2", item["name"])
	}
}

func TestOllamaVersionEndpoint(t *testing.T) {
	server := newOpenServer()
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/version", server.handleOllamaVersion)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["version"] == "" {
		t.Fatalf("version should not be empty")
	}

	post := httptest.NewRequest(http.MethodPost, "/api/version", nil)
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d", postRec.Code, http.StatusMethodNotAllowed)
	}
}

// -----------------------------------------------------------------------------
// 2. 流式矩阵：OpenAI SSE vs Ollama NDJSON
// -----------------------------------------------------------------------------

func TestStreamingMatrixReturnsCorrectFormats(t *testing.T) {
	openaiStream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")

	ollamaStream := strings.Join([]string{
		`{"model":"llama","message":{"role":"assistant","content":"hi"},"done":false}`,
		`{"model":"llama","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
		"",
	}, "\n")

	openaiProv := newFakeProvider("openai-provider", true, []string{"gpt-test"}, nil, openaiStream)
	ollamaProv := newFakeProvider("ollama-provider", true, []string{"llama"}, nil, ollamaStream)

	server := newOpenServer(openaiProv, ollamaProv)

	openaiHandler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	openaiHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("openai status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("openai content type = %q, want text/event-stream", ct)
	}
	if !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "data: {") {
		t.Fatalf("openai body prefix = %q, want data: {", rec.Body.String())
	}

	ollamaRec := httptest.NewRecorder()
	if err := server.streamOllamaPassthrough(ollamaRec, req, ollamaProv, &provider.ChatRequest{Model: "llama"}, ollamaRec); err != nil {
		t.Fatalf("streamOllamaPassthrough returned error: %v", err)
	}

	if ct := ollamaRec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("ollama content type = %q, want application/x-ndjson", ct)
	}
	if !strings.HasPrefix(strings.TrimSpace(ollamaRec.Body.String()), `{"model":"llama"`) {
		t.Fatalf("ollama body prefix = %q, want {\"model\":\"llama\"", ollamaRec.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 3. failover：首个 provider 失败后自动切换
// -----------------------------------------------------------------------------

func TestFailoverWhenPrimaryProviderFails(t *testing.T) {
	primary := newFakeProvider("primary", true, []string{"shared"}, &fakeChatResponse{Model: "shared", Content: "primary"}, "")
	primary.chatErr = errors.New("primary unavailable")

	secondary := newFakeProvider("secondary", true, []string{"shared"}, &fakeChatResponse{Model: "shared", Content: "secondary"}, "")

	server := newOpenServer(primary, secondary)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp provider.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Choices[0].Message.Content != "secondary" {
		t.Fatalf("content = %q, want %q", resp.Choices[0].Message.Content, "secondary")
	}
}

func TestChatCompletionsPreservesOpenAIRawResponseFields(t *testing.T) {
	raw := []byte(`{
		"id":"chatcmpl-raw",
		"object":"chat.completion",
		"created":1700000000,
		"model":"gpt-test",
		"provider_trace":{"request_id":"trace-1"},
		"choices":[{"index":0,"message":{"role":"assistant","content":"raw ok","reasoning_content":"kept for cache"},"finish_reason":""}],
		"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}
	}`)
	prov := newFakeProvider("openai", true, []string{"gpt-test"}, nil, "")
	prov.rawBody = raw

	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if prov.rawCalls != 1 {
		t.Fatalf("raw calls = %d, want 1", prov.rawCalls)
	}
	if got := rec.Header().Get("X-Proxy-Requested-Model"); got != "gpt-test" {
		t.Fatalf("requested model header = %q, want gpt-test", got)
	}
	if got := rec.Header().Get("X-Proxy-Provider"); got != "openai" {
		t.Fatalf("provider header = %q, want openai", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["provider_trace"]; !ok {
		t.Fatalf("provider_trace should be preserved in raw response: %s", rec.Body.String())
	}
	choices := body["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop; body=%s", choice["finish_reason"], rec.Body.String())
	}
	message := choice["message"].(map[string]any)
	if message["reasoning_content"] != "kept for cache" {
		t.Fatalf("reasoning_content should remain in raw response: %v", message)
	}
}

func TestOllamaChatReturnsDiagnosticHeaders(t *testing.T) {
	prov := newFakeProvider("deepseek", true, []string{"deepseek-chat"}, &fakeChatResponse{Model: "deepseek-chat", Content: "pong"}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/chat", server.handleOllamaChat)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("X-Proxy-Requested-Model"); got != "deepseek-chat" {
		t.Fatalf("requested model header = %q, want deepseek-chat", got)
	}
	if got := rec.Header().Get("X-Proxy-Resolved-Model"); got != "deepseek-chat" {
		t.Fatalf("resolved model header = %q, want deepseek-chat", got)
	}
	if got := rec.Header().Get("X-Proxy-Provider"); got != "deepseek" {
		t.Fatalf("provider header = %q, want deepseek", got)
	}
	if got := rec.Header().Get("X-Proxy-Upstream-Model"); got != "deepseek-chat" {
		t.Fatalf("upstream model header = %q, want deepseek-chat", got)
	}
}

func TestStreamingDoesNotFailoverAfterBytesWritten(t *testing.T) {
	primaryStream := `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}` + "\n"
	primary := newFakeProvider("primary", true, []string{"shared"}, nil, primaryStream)
	primary.streamReadErr = errors.New("stream interrupted")

	secondaryStream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"secondary"},"finish_reason":null}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	secondary := newFakeProvider("secondary", true, []string{"shared"}, nil, secondaryStream)

	server := newOpenServer(primary, secondary)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"shared","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if primary.streamCalls != 1 {
		t.Fatalf("primary stream calls = %d, want 1", primary.streamCalls)
	}
	if secondary.streamCalls != 0 {
		t.Fatalf("secondary stream calls = %d, want 0 after primary wrote bytes", secondary.streamCalls)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "partial") {
		t.Fatalf("body should contain primary partial stream, got %q", body)
	}
	if strings.Contains(body, "secondary") {
		t.Fatalf("body should not contain secondary stream after partial write, got %q", body)
	}
}

func TestStreamingFailoverBeforeBytesWritten(t *testing.T) {
	primary := newFakeProvider("primary", true, []string{"shared"}, nil, "")
	primary.streamErr = errors.New("stream unavailable")

	secondaryStream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"secondary"},"finish_reason":null}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	secondary := newFakeProvider("secondary", true, []string{"shared"}, nil, secondaryStream)

	server := newOpenServer(primary, secondary)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"shared","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if primary.streamCalls != 1 {
		t.Fatalf("primary stream calls = %d, want 1", primary.streamCalls)
	}
	if secondary.streamCalls != 1 {
		t.Fatalf("secondary stream calls = %d, want 1 before bytes written", secondary.streamCalls)
	}
	if !strings.Contains(rec.Body.String(), "secondary") {
		t.Fatalf("body should contain secondary stream after pre-write failover, got %q", rec.Body.String())
	}
}

func TestStreamingDoesNotFailoverWhenClientGone(t *testing.T) {
	primary := newFakeProvider("primary", true, []string{"shared"}, nil, "")
	primary.streamErr = context.Canceled
	secondaryStream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"secondary"},"finish_reason":null}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	secondary := newFakeProvider("secondary", true, []string{"shared"}, nil, secondaryStream)

	server := newOpenServer(primary, secondary)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"shared","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if primary.streamCalls != 1 {
		t.Fatalf("primary stream calls = %d, want 1", primary.streamCalls)
	}
	if secondary.streamCalls != 0 || secondary.chatCalls != 0 {
		t.Fatalf("secondary calls = stream:%d chat:%d, client_gone must not fail over", secondary.streamCalls, secondary.chatCalls)
	}
}

func TestChatCompletionsAppliesModelTimeout(t *testing.T) {
	timeout := 2
	prov := newFakeProvider("openai", true, []string{"gpt-test"}, &fakeChatResponse{Model: "gpt-test", Content: "pong"}, "")
	server := newOpenServer(prov)
	server.config.Models = []config.ModelConfig{{
		Name:           "gpt-test",
		Provider:       "openai",
		TimeoutSeconds: &timeout,
		Enabled:        true,
	}}

	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !prov.hadDeadline {
		t.Fatalf("provider context should have deadline from timeout_seconds")
	}
}

func TestOllamaChatAppliesModelTimeout(t *testing.T) {
	timeout := 2
	prov := newFakeProvider("ollama", true, []string{"llama"}, &fakeChatResponse{Model: "llama", Content: "pong"}, "")
	server := newOpenServer(prov)
	server.config.Models = []config.ModelConfig{{
		Name:           "llama",
		Provider:       "ollama",
		TimeoutSeconds: &timeout,
		Enabled:        true,
	}}

	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/chat", server.handleOllamaChat)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"llama","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !prov.hadDeadline {
		t.Fatalf("provider context should have deadline from timeout_seconds")
	}
}

// -----------------------------------------------------------------------------
// 4. auth：PROXY_API_KEY 机制
// -----------------------------------------------------------------------------

func TestAuthMiddlewareBlocksUnauthorizedRequests(t *testing.T) {
	server := newOpenServer()
	server.proxyKey = "secret"

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", authHeader: "Basic secret", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", authHeader: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "valid", authHeader: "Bearer secret", wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			handler := server.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestHealthReturnsModelAndProviderState(t *testing.T) {
	oldVersion := buildVersion
	SetBuildVersion("test-version")
	t.Cleanup(func() { buildVersion = oldVersion })

	prov := newFakeProvider("openai", true, []string{"gpt-test"}, &fakeChatResponse{Model: "gpt-test", Content: "ok"}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/health", server.handleHealth)
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %v, want ok", body["status"])
	}
	if body["version"] != "test-version" {
		t.Fatalf("version = %v, want test-version", body["version"])
	}
	if body["model"] != "default-model" {
		t.Fatalf("model = %v, want default-model", body["model"])
	}
	providers, _ := body["providers"].([]any)
	if len(providers) != 1 || providers[0] != "openai" {
		t.Fatalf("providers = %#v, want [openai]", providers)
	}
	models, _ := body["available_models"].([]any)
	if len(models) == 0 {
		t.Fatalf("available_models should not be empty: %s", rec.Body.String())
	}
	if body["models_last_refresh_utc"] == nil {
		t.Fatalf("models_last_refresh_utc missing: %s", rec.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 5. show 响应：/api/show 返回模型能力信息
// -----------------------------------------------------------------------------

func TestOllamaShowReturnsModelCapabilities(t *testing.T) {
	ctxLength := 32000
	maxOutput := 4096
	supportsTools := true
	supportsVision := true
	temp := 0.7
	topP := 0.9
	maxTokens := 2048
	timeout := 30
	effort := "medium"

	cfg := &config.AppConfig{
		DefaultModel: "vision-model",
		Models: []config.ModelConfig{{
			Name:            "vision-model",
			Provider:        "openai",
			ContextLength:   &ctxLength,
			MaxOutputTokens: &maxOutput,
			MaxTokens:       &maxTokens,
			Temperature:     &temp,
			TopP:            &topP,
			ReasoningEffort: effort,
			TimeoutSeconds:  &timeout,
			SupportsTools:   &supportsTools,
			SupportsVision:  &supportsVision,
			Enabled:         true,
		}},
	}
	server := newOpenServer()
	registry := provider.NewRegistry(cfg.DefaultModel, 0)

	server.config = cfg
	server.registry = registry

	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/show", server.handleOllamaShow)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/show?model=vision-model", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body["model"] != "vision-model" {
		t.Fatalf("model = %v, want vision-model", body["model"])
	}
	if body["context_length"] != float64(ctxLength) {
		t.Fatalf("context_length = %v, want %d", body["context_length"], ctxLength)
	}
	if body["max_output_tokens"] != float64(maxOutput) {
		t.Fatalf("max_output_tokens = %v, want %d", body["max_output_tokens"], maxOutput)
	}

	capabilities, _ := body["capabilities"].([]any)
	foundTools := false
	foundVision := false
	for _, c := range capabilities {
		switch v := c.(type) {
		case string:
			if v == "tools" {
				foundTools = true
			}
			if v == "vision" {
				foundVision = true
			}
		}
	}
	if !foundTools || !foundVision {
		t.Fatalf("capabilities = %#v, want tools and vision", capabilities)
	}

	modelInfo, _ := body["model_info"].(map[string]any)
	if modelInfo == nil {
		t.Fatalf("missing model_info")
	}
	if modelInfo["supports_tools"] != true {
		t.Fatalf("supports_tools = %v, want true", modelInfo["supports_tools"])
	}
	if modelInfo["supports_vision"] != true {
		t.Fatalf("supports_vision = %v, want true", modelInfo["supports_vision"])
	}

	params, _ := body["recommended_parameters"].(map[string]any)
	if params == nil {
		t.Fatalf("missing recommended_parameters")
	}
	if params["max_tokens"] != float64(maxTokens) {
		t.Fatalf("recommended max_tokens = %v, want %d", params["max_tokens"], maxTokens)
	}
	if params["temperature"] != temp {
		t.Fatalf("recommended temperature = %v, want %v", params["temperature"], temp)
	}
	if params["top_p"] != topP {
		t.Fatalf("recommended top_p = %v, want %v", params["top_p"], topP)
	}
	if params["reasoning_effort"] != effort {
		t.Fatalf("recommended reasoning_effort = %v, want %v", params["reasoning_effort"], effort)
	}
	if params["timeout_seconds"] != float64(timeout) {
		t.Fatalf("recommended timeout_seconds = %v, want %d", params["timeout_seconds"], timeout)
	}
}

func TestModelDiscoveryDoesNotAdvertiseDisabledTools(t *testing.T) {
	supportsTools := false
	cfg := &config.AppConfig{
		DefaultModel: "plain-model",
		Models: []config.ModelConfig{{
			Name:          "plain-model",
			Provider:      "openai",
			SupportsTools: &supportsTools,
			Enabled:       true,
		}},
	}
	prov := newFakeProvider("openai", true, []string{"plain-model"}, &fakeChatResponse{Model: "plain-model", Content: "ok"}, "")
	server := newOpenServer(prov)
	server.config = cfg
	server.registry = provider.NewRegistry(cfg.DefaultModel, 0)
	server.registry.Add(&provider.ProviderEntry{Provider: prov, Models: []string{"plain-model"}})
	server.catalog = provider.NewModelCatalog(server.registry, "", 0)

	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/models", server.handleListModels)
		mux.HandleFunc("/api/tags", server.handleOllamaTags)
		mux.HandleFunc("/api/show", server.handleOllamaShow)
	})

	openAIRec := httptest.NewRecorder()
	handler.ServeHTTP(openAIRec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if openAIRec.Code != http.StatusOK {
		t.Fatalf("/v1/models status = %d; body=%s", openAIRec.Code, openAIRec.Body.String())
	}
	var openAIModels map[string]any
	if err := json.Unmarshal(openAIRec.Body.Bytes(), &openAIModels); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	data := openAIModels["data"].([]any)
	assertNoToolCapability(t, data[0].(map[string]any))

	tagsRec := httptest.NewRecorder()
	handler.ServeHTTP(tagsRec, httptest.NewRequest(http.MethodGet, "/api/tags", nil))
	if tagsRec.Code != http.StatusOK {
		t.Fatalf("/api/tags status = %d; body=%s", tagsRec.Code, tagsRec.Body.String())
	}
	var tags map[string]any
	if err := json.Unmarshal(tagsRec.Body.Bytes(), &tags); err != nil {
		t.Fatalf("decode /api/tags: %v", err)
	}
	models := tags["models"].([]any)
	assertNoToolCapability(t, models[0].(map[string]any))

	showRec := httptest.NewRecorder()
	handler.ServeHTTP(showRec, httptest.NewRequest(http.MethodGet, "/api/show?model=plain-model", nil))
	if showRec.Code != http.StatusOK {
		t.Fatalf("/api/show status = %d; body=%s", showRec.Code, showRec.Body.String())
	}
	var show map[string]any
	if err := json.Unmarshal(showRec.Body.Bytes(), &show); err != nil {
		t.Fatalf("decode /api/show: %v", err)
	}
	assertNoToolCapability(t, show)
}

func TestModelDiscoveryUserConfigOverridesCatalogToolCapability(t *testing.T) {
	supportsTools := false
	prov := newFakeProvider("deepseek", true, []string{"deepseek-v4-pro"}, &fakeChatResponse{Model: "deepseek-v4-pro", Content: "ok"}, "")
	server := newOpenServer(prov)
	server.config.Models = []config.ModelConfig{{
		Name:          "deepseek-v4-pro",
		Provider:      "deepseek",
		SupportsTools: &supportsTools,
		Enabled:       true,
	}}
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/models", server.handleListModels)
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	data := body["data"].([]any)
	assertNoToolCapability(t, data[0].(map[string]any))
}

func assertNoToolCapability(t *testing.T, item map[string]any) {
	t.Helper()
	if item["supports_tools"] != false || item["supports_tool_calls"] != false {
		t.Fatalf("tool support should be false: %#v", item)
	}
	if containsAnyString(item["capabilities"].([]any), "tools") {
		t.Fatalf("capabilities should not include tools: %#v", item["capabilities"])
	}
}

func TestOllamaShowAcceptsPostBody(t *testing.T) {
	cfg := &config.AppConfig{
		DefaultModel: "post-model",
		Models: []config.ModelConfig{{
			Name:    "post-model",
			Enabled: true,
		}},
	}
	server := newOpenServer()
	server.config = cfg
	server.registry = provider.NewRegistry(cfg.DefaultModel, 0)

	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/show", server.handleOllamaShow)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/show", strings.NewReader(`{"model":"post-model"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["model"] != "post-model" {
		t.Fatalf("model = %v, want post-model", body["model"])
	}
}

// -----------------------------------------------------------------------------
// 额外覆盖：/v1/chat/completions 非流式 + auth
// -----------------------------------------------------------------------------

func TestChatCompletionsNonStreamingWithAuth(t *testing.T) {
	prov := newFakeProvider("openai", true, []string{"gpt-test"}, &fakeChatResponse{Model: "gpt-test", Content: "pong"}, "")
	server := newTestServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp provider.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Choices[0].Message.Content != "pong" {
		t.Fatalf("content = %q, want %q", resp.Choices[0].Message.Content, "pong")
	}
}

func TestChatCompletionsUnknownModelReturnsBadRequest(t *testing.T) {
	server := newOpenServer()
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseed-v4-flash","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := rec.Header().Get("X-Proxy-Candidate-Count"); got != "0" {
		t.Fatalf("candidate count = %q, want 0", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal diagnostic error: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "model_not_routable" {
		t.Fatalf("error.code = %v, want model_not_routable; body=%s", errObj["code"], rec.Body.String())
	}
}

func TestChatCompletionsRebuildsCatalogBeforeResolvingCandidates(t *testing.T) {
	prov := newFakeProvider("useai", true, nil, &fakeChatResponse{Model: "deepseek-v4-flash", Content: "pong"}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("X-Proxy-Candidate-Count"); got != "1" {
		t.Fatalf("candidate count = %q, want 1", got)
	}
	if prov.lastReq == nil || prov.lastReq.Model != "deepseek-v4-flash" {
		t.Fatalf("provider request model = %#v, want deepseek-v4-flash", prov.lastReq)
	}
}

func TestChatCompletionsAcceptsVisualStudioDisplayModelName(t *testing.T) {
	prov := newFakeProvider("usecpa", true, []string{"deepseek-v4-flash"}, &fakeChatResponse{Model: "deepseek-v4-flash", Content: "pong"}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"DEEPSEEK - deepseek-v4-flash:latest","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("X-Proxy-Resolved-Model"); got != "deepseek-v4-flash" {
		t.Fatalf("resolved model = %q, want deepseek-v4-flash", got)
	}
	if got := rec.Header().Get("X-Proxy-Upstream-Model"); got != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash", got)
	}
	if prov.lastReq == nil || prov.lastReq.Model != "deepseek-v4-flash" {
		t.Fatalf("provider request model = %#v, want deepseek-v4-flash", prov.lastReq)
	}
}

func TestChatCompletionsMapsVisualStudioBasenameToNamespacedUpstream(t *testing.T) {
	prov := newFakeProvider("usecpa", true, []string{"z-ai/glm-5.2"}, &fakeChatResponse{Model: "z-ai/glm-5.2", Content: "pong"}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("X-Proxy-Resolved-Model"); got != "z-ai/glm-5.2" {
		t.Fatalf("resolved model = %q, want z-ai/glm-5.2", got)
	}
	if got := rec.Header().Get("X-Proxy-Upstream-Model"); got != "z-ai/glm-5.2" {
		t.Fatalf("upstream model = %q, want z-ai/glm-5.2", got)
	}
	if prov.lastReq == nil || prov.lastReq.Model != "z-ai/glm-5.2" {
		t.Fatalf("provider request model = %#v, want z-ai/glm-5.2", prov.lastReq)
	}
}

func TestChatCompletionsRejectsAmbiguousVisualStudioBasename(t *testing.T) {
	provA := newFakeProvider("usecpa", true, []string{"z-ai/glm-5.2"}, &fakeChatResponse{Model: "z-ai/glm-5.2", Content: "pong"}, "")
	provB := newFakeProvider("other", true, []string{"other/glm-5.2"}, &fakeChatResponse{Model: "other/glm-5.2", Content: "pong"}, "")
	server := newOpenServer(provA, provB)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "model_alias_ambiguous" {
		t.Fatalf("error.code = %v, want model_alias_ambiguous; body=%s", errObj["code"], rec.Body.String())
	}
	if provA.lastReq != nil || provB.lastReq != nil {
		t.Fatalf("ambiguous basename should not call providers: a=%#v b=%#v", provA.lastReq, provB.lastReq)
	}
}

func TestBuildRegistrySeedsConfiguredProviderModelsBeforeDiscovery(t *testing.T) {
	cfg := &config.AppConfig{
		DefaultModel: "z-ai/glm-5.2",
		Providers: []config.ProviderConfig{{
			ID:       "usecpa",
			Name:     "UseCpa",
			Type:     "openai",
			Enabled:  true,
			Priority: 1,
		}},
		Models: []config.ModelConfig{{
			Name:       "z-ai/glm-5.2",
			ProviderID: "usecpa",
			Enabled:    true,
		}},
	}

	registry := (&Server{logger: log.New(nil, log.LevelError, false)}).buildRegistry(cfg)
	candidates := registry.ResolveCandidates("glm-5.2")

	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].Provider.Provider.Name() != "usecpa" {
		t.Fatalf("provider = %q, want usecpa", candidates[0].Provider.Provider.Name())
	}
	if candidates[0].UpstreamID != "z-ai/glm-5.2" {
		t.Fatalf("upstream = %q, want z-ai/glm-5.2", candidates[0].UpstreamID)
	}
}

func TestChatCompletionsProviderFailureReturnsDiagnosticAttempts(t *testing.T) {
	prov := newFakeProvider("useai", true, []string{"deepseek-v4-flash"}, nil, "")
	prov.chatErr = errors.New("请求失败: dial tcp: connect: connection refused")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				CandidateCount int `json:"candidate_count"`
				Attempts       []struct {
					Provider string `json:"provider"`
					Upstream string `json:"upstream_model"`
					Category string `json:"category"`
				} `json:"attempts"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal diagnostic error: %v", err)
	}
	if body.Error.Code != "network_error" {
		t.Fatalf("error.code = %q, want network_error; body=%s", body.Error.Code, rec.Body.String())
	}
	if body.Error.Details.CandidateCount != 1 {
		t.Fatalf("candidate_count = %d, want 1", body.Error.Details.CandidateCount)
	}
	if len(body.Error.Details.Attempts) != 1 {
		t.Fatalf("attempts len = %d, want 1; body=%s", len(body.Error.Details.Attempts), rec.Body.String())
	}
	attempt := body.Error.Details.Attempts[0]
	if attempt.Provider != "useai" || attempt.Upstream != "deepseek-v4-flash" || attempt.Category != "network_error" {
		t.Fatalf("attempt = %#v, want useai/deepseek-v4-flash/network_error", attempt)
	}
}

func TestChatCompletionsProviderFailureIsRecordedInRequestLog(t *testing.T) {
	prov := newFakeProvider("useai", true, []string{"deepseek-v4-flash"}, nil, "")
	prov.chatErr = errors.New("请求失败: dial tcp: connect: connection refused")
	server := newOpenServer(prov)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	logs := server.store.GetLogs(1)
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	got := logs[0]
	if got.ErrorCode != "network_error" {
		t.Fatalf("log error_code = %q, want network_error; log=%#v", got.ErrorCode, got)
	}
	if got.ErrorMessage == "" {
		t.Fatalf("log error_message is empty: %#v", got)
	}
	if got.ErrorHint == "" {
		t.Fatalf("log error_hint is empty: %#v", got)
	}
}

func TestChatCompletionsSkipsCoolingProviderOnNextRequest(t *testing.T) {
	primary := newFakeProvider("primary", true, []string{"shared"}, nil, "")
	primary.chatErr = errors.New("API 错误 503")
	secondary := newFakeProvider("secondary", true, []string{"shared"}, &fakeChatResponse{
		Model:   "shared",
		Content: "ok",
	}, "")
	server := newOpenServer(primary, secondary)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"shared","messages":[{"role":"user","content":"ping"}]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want %d; body=%s", i+1, rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	if primary.chatCalls != 1 {
		t.Fatalf("primary chat calls = %d, want 1 after cooldown skip", primary.chatCalls)
	}
	if secondary.chatCalls != 2 {
		t.Fatalf("secondary chat calls = %d, want 2", secondary.chatCalls)
	}
}

// -----------------------------------------------------------------------------
// 额外覆盖：Ollama 流式直通格式校验
// -----------------------------------------------------------------------------

func TestOllamaStreamPassthroughPreservesNDJSON(t *testing.T) {
	stream := strings.Join([]string{
		`{"model":"llama","message":{"role":"assistant","content":"a"},"done":false}`,
		`{"model":"llama","message":{"role":"assistant","content":"a"},"done":true,"done_reason":"stop"}`,
		"",
	}, "\n")

	prov := newFakeProvider("ollama", true, []string{"llama"}, nil, stream)
	server := newOpenServer(prov)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()
	if err := server.streamOllamaPassthrough(rec, req, prov, &provider.ChatRequest{Model: "llama"}, rec); err != nil {
		t.Fatalf("streamOllamaPassthrough returned error: %v", err)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type = %q, want application/x-ndjson", ct)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"done":false`) {
		t.Fatalf("expected streaming chunk, got %q", body)
	}
	if !strings.Contains(body, `"done":true`) {
		t.Fatalf("expected final chunk, got %q", body)
	}
}

func TestOllamaChatPreservesMessageAndOptionExtensions(t *testing.T) {
	prov := newFakeProvider("ollama", true, []string{"llama"}, &fakeChatResponse{Model: "llama", Content: "ok"}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/chat", server.handleOllamaChat)
	})

	body := `{
		"model":"llama",
		"messages":[{
			"role":"user",
			"content":"hi",
			"cache_control":{"type":"ephemeral"}
		}],
		"options":{
			"temperature":0.2,
			"num_keep":24,
			"custom_option":{"enabled":true}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if prov.lastReq == nil {
		t.Fatalf("provider did not receive chat request")
	}
	if len(prov.lastReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(prov.lastReq.Messages))
	}
	if _, ok := prov.lastReq.Messages[0].Extra["cache_control"]; !ok {
		t.Fatalf("cache_control message extension was not preserved: %#v", prov.lastReq.Messages[0].Extra)
	}
	if _, ok := prov.lastReq.OptionsExtra["num_keep"]; !ok {
		t.Fatalf("num_keep option extension was not preserved: %#v", prov.lastReq.OptionsExtra)
	}
	if _, ok := prov.lastReq.OptionsExtra["custom_option"]; !ok {
		t.Fatalf("custom_option was not preserved: %#v", prov.lastReq.OptionsExtra)
	}
}

func TestOllamaChatConvertsImagesToOpenAIMultimodalContent(t *testing.T) {
	prov := newFakeProvider("openai", true, []string{"vision-model"}, &fakeChatResponse{Model: "vision-model", Content: "ok"}, "")
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/chat", server.handleOllamaChat)
	})

	body := `{
		"model":"vision-model",
		"messages":[{
			"role":"user",
			"content":"Describe this",
			"images":["AA==","data:image/jpeg;base64,BB=="]
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if prov.lastReq == nil || len(prov.lastReq.Messages) != 1 {
		t.Fatalf("provider did not receive expected chat request")
	}

	var content []map[string]any
	if err := json.Unmarshal(prov.lastReq.Messages[0].ContentRaw, &content); err != nil {
		t.Fatalf("decode content raw: %v", err)
	}
	if len(content) != 3 {
		t.Fatalf("content parts len = %d, want 3: %#v", len(content), content)
	}
	if content[0]["type"] != "text" || content[0]["text"] != "Describe this" {
		t.Fatalf("text part = %#v", content[0])
	}
	firstImage := content[1]["image_url"].(map[string]any)
	if firstImage["url"] != "data:image/png;base64,AA==" {
		t.Fatalf("first image url = %#v", firstImage["url"])
	}
	secondImage := content[2]["image_url"].(map[string]any)
	if secondImage["url"] != "data:image/jpeg;base64,BB==" {
		t.Fatalf("second image url = %#v", secondImage["url"])
	}
}

func TestOllamaChatReturnsReasoningAsThinking(t *testing.T) {
	prov := newFakeProvider(
		"openai",
		true,
		[]string{"reasoner"},
		&fakeChatResponse{Model: "reasoner", Reasoning: "chain of thought"},
		"",
	)
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/chat", server.handleOllamaChat)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"reasoner","messages":[{"role":"user","content":"think"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	message, _ := body["message"].(map[string]any)
	if message["thinking"] != "chain of thought" {
		t.Fatalf("thinking = %v, want chain of thought", message["thinking"])
	}
	if message["reasoning_content"] != "chain of thought" {
		t.Fatalf("reasoning_content = %v, want chain of thought", message["reasoning_content"])
	}
	if message["content"] != "chain of thought" {
		t.Fatalf("empty content should fall back to reasoning, got %v", message["content"])
	}
}

func TestOllamaChatNativeProviderPreservesRawResponseFields(t *testing.T) {
	prov := newFakeProvider("ollama", true, []string{"llama"}, nil, "")
	prov.rawBody = []byte(`{
		"model":"llama",
		"created_at":"2026-07-02T00:00:00Z",
		"message":{"role":"assistant","content":"","thinking":"reasoned"},
		"done":true,
		"total_duration":123,
		"eval_duration":456,
		"prompt_eval_count":7,
		"eval_count":8
	}`)
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/chat", server.handleOllamaChat)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"llama","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if prov.rawCalls != 1 {
		t.Fatalf("raw calls = %d, want 1", prov.rawCalls)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["total_duration"] != float64(123) || body["eval_duration"] != float64(456) {
		t.Fatalf("native timing fields not preserved: %#v", body)
	}
	message := body["message"].(map[string]any)
	if message["thinking"] != "reasoned" {
		t.Fatalf("thinking = %#v, want reasoned", message["thinking"])
	}
	if message["content"] != "reasoned" {
		t.Fatalf("content should be filled from thinking, got %#v", message["content"])
	}
}
