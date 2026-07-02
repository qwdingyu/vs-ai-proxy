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

func TestBuildRegistryDiscoversProviderFromEnvironment(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	t.Setenv("PROVIDER_DEEPSEEK_API_KEY", "env-key")
	t.Setenv("PROVIDER_DEEPSEEK_BASE_URL", upstream.URL)

	server := testRegistryServer()
	registry := server.buildRegistry(&config.AppConfig{
		DefaultModel: "deepseek-v4-pro",
		Providers: []config.ProviderConfig{{
			Name:    "deepseek",
			BaseURL: "https://api.deepseek.com",
			Type:    "openai",
			Enabled: false,
		}},
	})

	if !containsString(registry.ProviderNames(), "deepseek") {
		t.Fatalf("providers = %#v, want env-discovered deepseek", registry.ProviderNames())
	}
}

func TestBuildRegistryDiscoversLegacyDeepSeekEnvironment(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	t.Setenv("DEEPSEEK_API_KEY", "legacy-key")
	t.Setenv("DEEPSEEK_BASE_URL", upstream.URL)

	server := testRegistryServer()
	registry := server.buildRegistry(&config.AppConfig{DefaultModel: "deepseek-v4-pro"})

	if !containsString(registry.ProviderNames(), "deepseek") {
		t.Fatalf("providers = %#v, want legacy deepseek", registry.ProviderNames())
	}
}

func TestBuildRegistryDiscoversLocalOllamaFromEnvironment(t *testing.T) {
	t.Setenv("PROVIDER_OLLAMA_BASE_URL", "http://127.0.0.1:11434")

	server := testRegistryServer()
	registry := server.buildRegistry(&config.AppConfig{DefaultModel: "llama"})

	if !containsString(registry.ProviderNames(), "ollama") {
		t.Fatalf("providers = %#v, want local ollama", registry.ProviderNames())
	}
}

func TestBuildRegistryDoesNotDiscoverRemoteOllamaWithoutKey(t *testing.T) {
	t.Setenv("PROVIDER_OLLAMA_BASE_URL", "https://ollama.com")

	server := testRegistryServer()
	registry := server.buildRegistry(&config.AppConfig{DefaultModel: "llama"})

	if containsString(registry.ProviderNames(), "ollama") {
		t.Fatalf("providers = %#v, remote ollama without key should not be registered", registry.ProviderNames())
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
