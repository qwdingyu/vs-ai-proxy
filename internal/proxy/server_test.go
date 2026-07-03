package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

func TestAuthMiddlewareRequiresBearerToken(t *testing.T) {
	server := &Server{proxyKey: "secret"}
	handler := server.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", authHeader: "Basic secret", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", authHeader: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "valid token", authHeader: "Bearer secret", wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestAuthMiddlewareAllowsOpenProxyWithoutKey(t *testing.T) {
	server := &Server{}
	handler := server.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestLoggingMiddlewareCapturesProviderAndModelHeaders(t *testing.T) {
	st := store.New(10)
	server := &Server{store: st, logger: log.New(nil, log.LevelError, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Proxy-Provider", "UseAI")
		w.Header().Set("X-Proxy-Requested-Model", "useai-model")
		w.Header().Set("X-Proxy-Upstream-Model", "upstream-model")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	logs := st.GetLogs(1)
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	if logs[0].Provider != "UseAI" {
		t.Fatalf("provider = %q, want UseAI", logs[0].Provider)
	}
	if logs[0].Model != "useai-model" {
		t.Fatalf("model = %q, want useai-model", logs[0].Model)
	}
	if logs[0].Upstream != "upstream-model" {
		t.Fatalf("upstream = %q, want upstream-model", logs[0].Upstream)
	}
}

func TestBuildRegistryUsesConfiguredProviderIDs(t *testing.T) {
	server := testRegistryServer()
	registry := server.buildRegistry(&config.AppConfig{
		DefaultModel: "model-a",
		Providers: []config.ProviderConfig{{
			ID:      "useai-paid",
			Name:    "UseAI Paid",
			BaseURL: "https://api.eforge.xyz/v1",
			Type:    "openai",
			Enabled: true,
		}},
	})

	if !containsString(registry.ProviderNames(), "useai-paid") {
		t.Fatalf("providers = %#v, want useai-paid", registry.ProviderNames())
	}
	if containsString(registry.ProviderNames(), "UseAI Paid") {
		t.Fatalf("providers = %#v, registry should use stable provider id, not display name", registry.ProviderNames())
	}
}

func TestBuildRegistryDoesNotRegisterProviderFromEnvironment(t *testing.T) {
	t.Setenv("PROVIDER_DEEPSEEK_API_KEY", "env-key")
	t.Setenv("PROVIDER_OLLAMA_BASE_URL", "http://127.0.0.1:11434")
	t.Setenv("DEEPSEEK_API_KEY", "legacy-key")

	server := testRegistryServer()
	registry := server.buildRegistry(&config.AppConfig{DefaultModel: "model-a"})

	if got := registry.ProviderNames(); len(got) != 0 {
		t.Fatalf("providers = %#v, want none because provider env discovery is intentionally disabled", got)
	}
}

func TestStreamOpenAIToOllamaWritesNDJSON(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()

	err := server.streamOpenAIToOllama(
		rec,
		req,
		prov,
		&provider.ChatRequest{Model: "gpt-test"},
		rec,
	)
	if err != nil {
		t.Fatalf("streamOpenAIToOllama returned error: %v", err)
	}

	if contentType := rec.Header().Get("Content-Type"); contentType != "application/x-ndjson" {
		t.Fatalf("content type = %q, want application/x-ndjson", contentType)
	}
	body := rec.Body.String()
	if strings.Contains(body, "data:") {
		t.Fatalf("expected NDJSON without SSE data prefix, got %q", body)
	}
	if !strings.Contains(body, `"done":false`) || !strings.Contains(body, `"done":true`) {
		t.Fatalf("expected streaming and final chunks, got %q", body)
	}
}

func TestStreamOllamaPassthroughWritesNDJSON(t *testing.T) {
	stream := `{"model":"llama","message":{"role":"assistant","content":"hi"},"done":false}` + "\n"
	server := &Server{}
	prov := &fakeStreamProvider{name: "ollama", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()

	err := server.streamOllamaPassthrough(
		rec,
		req,
		prov,
		&provider.ChatRequest{Model: "llama"},
		rec,
	)
	if err != nil {
		t.Fatalf("streamOllamaPassthrough returned error: %v", err)
	}

	if contentType := rec.Header().Get("Content-Type"); contentType != "application/x-ndjson" {
		t.Fatalf("content type = %q, want application/x-ndjson", contentType)
	}
	if rec.Body.String() != stream {
		t.Fatalf("body = %q, want %q", rec.Body.String(), stream)
	}
}

func TestStreamOpenAIHandlesLargeSSELine(t *testing.T) {
	largeContent := strings.Repeat("x", 80*1024)
	stream := `data: {"choices":[{"delta":{"content":"` + largeContent + `"},"finish_reason":null}]}` + "\n" +
		`data: [DONE]` + "\n"
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	err := server.streamOpenAI(rec, req, prov, &provider.ChatRequest{Model: "gpt-test"}, rec)
	if err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), largeContent) {
		t.Fatalf("large content was not preserved")
	}
}

func TestStreamOllamaPassthroughHandlesLargeNDJSONLine(t *testing.T) {
	largeContent := strings.Repeat("x", 80*1024)
	stream := `{"model":"llama","message":{"role":"assistant","content":"` + largeContent + `"},"done":false}` + "\n"
	server := &Server{}
	prov := &fakeStreamProvider{name: "ollama", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()

	err := server.streamOllamaPassthrough(rec, req, prov, &provider.ChatRequest{Model: "llama"}, rec)
	if err != nil {
		t.Fatalf("streamOllamaPassthrough returned error: %v", err)
	}
	if rec.Body.String() != stream {
		t.Fatalf("large NDJSON line was not preserved")
	}
}

type fakeStreamProvider struct {
	name string
	body string
}

func (p *fakeStreamProvider) Name() string {
	return p.name
}

func (p *fakeStreamProvider) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("not implemented")
}

func (p *fakeStreamProvider) ChatStream(context.Context, *provider.ChatRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(p.body)), nil
}

func (p *fakeStreamProvider) ListModels(context.Context) ([]string, error) {
	return []string{}, nil
}

func (p *fakeStreamProvider) IsEnabled() bool {
	return true
}

func testRegistryServer() *Server {
	return &Server{logger: log.New(nil, log.LevelError, false)}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
