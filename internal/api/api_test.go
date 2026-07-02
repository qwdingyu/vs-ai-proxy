package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/proxy"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

func TestConfigSaveHotUpdatesProxyRegistry(t *testing.T) {
	apiSrv, proxySrv := newAPITestHarness(t)

	payload := config.AppConfig{
		Port:         11434,
		DefaultModel: "model-x",
		Providers: []config.ProviderConfig{{
			Name:    "openai",
			Type:    "openai",
			APIKey:  "sk-test",
			BaseURL: "https://example.invalid",
			Enabled: true,
		}},
		Models: []config.ModelConfig{{
			Name:     "model-x",
			Provider: "openai",
			Enabled:  true,
		}},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config", mustJSONBody(t, payload))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/config status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	cfgRec := httptest.NewRecorder()
	cfgReq := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	apiSrv.engine.ServeHTTP(cfgRec, cfgReq)
	if cfgRec.Code != http.StatusOK {
		t.Fatalf("GET /api/config status = %d, want %d", cfgRec.Code, http.StatusOK)
	}
	var gotCfg config.AppConfig
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &gotCfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if gotCfg.DefaultModel != "model-x" {
		t.Fatalf("default_model = %q, want %q", gotCfg.DefaultModel, "model-x")
	}

	cfg, registry, _ := proxySrv.SnapshotComponents()
	if cfg.DefaultModel != "model-x" {
		t.Fatalf("proxy snapshot default model = %q, want %q", cfg.DefaultModel, "model-x")
	}
	if !containsString(registry.ProviderNames(), "openai") {
		t.Fatalf("registry providers = %#v, want openai", registry.ProviderNames())
	}
}

func TestProviderEndpointsCRUDAndHotUpdate(t *testing.T) {
	apiSrv, proxySrv := newAPITestHarness(t)

	addReqBody := config.ProviderConfig{
		Name:    "openai",
		Type:    "openai",
		APIKey:  "sk-test",
		BaseURL: "https://example.invalid",
		Enabled: true,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers", mustJSONBody(t, addReqBody))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/providers status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	listRec := httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	apiSrv.engine.ServeHTTP(listRec, listReq)
	if !bytes.Contains(listRec.Body.Bytes(), []byte(`"name":"openai"`)) {
		t.Fatalf("provider list missing openai: %s", listRec.Body.String())
	}

	updBody := config.ProviderConfig{
		Name:    "openai",
		Type:    "openai",
		APIKey:  "sk-updated",
		BaseURL: "https://example.invalid",
		Enabled: false,
	}
	updRec := httptest.NewRecorder()
	updReq := httptest.NewRequest(http.MethodPut, "/api/providers/openai", mustJSONBody(t, updBody))
	apiSrv.engine.ServeHTTP(updRec, updReq)
	if updRec.Code != http.StatusOK {
		t.Fatalf("PUT /api/providers/openai status = %d, want %d; body=%s", updRec.Code, http.StatusOK, updRec.Body.String())
	}

	cfg, registry, _ := proxySrv.SnapshotComponents()
	openAI, ok := findProviderConfig(cfg.Providers, "openai")
	if !ok {
		t.Fatalf("openai provider missing after update: %#v", cfg.Providers)
	}
	if openAI.APIKey != "sk-updated" {
		t.Fatalf("updated provider key = %q, want sk-updated", openAI.APIKey)
	}
	if containsString(registry.ProviderNames(), "openai") {
		t.Fatalf("disabled provider should not be routed by registry")
	}

	delRec := httptest.NewRecorder()
	delReq := httptest.NewRequest(http.MethodDelete, "/api/providers/openai", nil)
	apiSrv.engine.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/providers/openai status = %d, want %d; body=%s", delRec.Code, http.StatusOK, delRec.Body.String())
	}
}

func TestModelEndpointsRoundTrip(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	models := []config.ModelConfig{{
		Name:            "model-a",
		Provider:        "openai",
		ContextLength:   intPtrLocal(8192),
		MaxOutputTokens: intPtrLocal(2048),
		Enabled:         true,
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/models", mustJSONBody(t, models))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/models status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	listRec := httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	apiSrv.engine.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /api/models status = %d, want %d", listRec.Code, http.StatusOK)
	}
	if !bytes.Contains(listRec.Body.Bytes(), []byte(`"name":"model-a"`)) {
		t.Fatalf("model list missing model-a: %s", listRec.Body.String())
	}
}

func TestManagementTestEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	apiSrv, _ := newAPITestHarness(t)
	payload := config.ProviderConfig{
		Name:    "openai",
		Type:    "openai",
		APIKey:  "sk-test",
		BaseURL: upstream.URL,
		Enabled: true,
	}

	connRec := httptest.NewRecorder()
	connReq := httptest.NewRequest(http.MethodPost, "/api/test/connection", mustJSONBody(t, map[string]any{
		"provider": payload,
	}))
	apiSrv.engine.ServeHTTP(connRec, connReq)
	if connRec.Code != http.StatusOK || !bytes.Contains(connRec.Body.Bytes(), []byte(`"success":true`)) {
		t.Fatalf("test connection failed: status=%d body=%s", connRec.Code, connRec.Body.String())
	}

	chatRec := httptest.NewRecorder()
	chatReq := httptest.NewRequest(http.MethodPost, "/api/test/chat", mustJSONBody(t, map[string]any{
		"provider": payload,
		"message":  "hello",
		"model":    "model-a",
	}))
	apiSrv.engine.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK || !bytes.Contains(chatRec.Body.Bytes(), []byte(`"success":true`)) {
		t.Fatalf("test chat failed: status=%d body=%s", chatRec.Code, chatRec.Body.String())
	}
}

func newAPITestHarness(t *testing.T) (*Server, *proxy.Server) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	mgr, err := config.NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	cfg := &config.AppConfig{
		Port:         11434,
		DefaultModel: "default-model",
		Providers:    []config.ProviderConfig{},
		Models:       []config.ModelConfig{},
	}
	if err := mgr.Save(cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	logger := log.New(nil, log.LevelError, false)
	st := store.New(50)
	proxySrv := proxy.NewServer(cfg, mgr, st, logger)
	apiSrv := NewServer(cfg.Port, mgr, proxySrv, st, logger, nil)
	return apiSrv, proxySrv
}

func mustJSONBody(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	return bytes.NewReader(data)
}

func intPtrLocal(v int) *int {
	return &v
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func findProviderConfig(values []config.ProviderConfig, name string) (config.ProviderConfig, bool) {
	for _, value := range values {
		if value.Name == name {
			return value, true
		}
	}
	return config.ProviderConfig{}, false
}
