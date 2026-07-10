package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestLoggingMiddlewareCapturesToolDiagnosticsWithoutArguments(t *testing.T) {
	st := store.New(10)
	server := &Server{store: st, logger: log.New(nil, log.LevelError, false)}
	handler := server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setProxyDiagnosticHeader(w, "X-Proxy-Request-Tools", "declared: git,powershell")
		setProxyDiagnosticHeader(w, "X-Proxy-Response-Tools", "returned: powershell")
		setProxyFallbackMode(w, "stream-to-nonstream")
		setProxyToolNormalization(w, "dsml")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	logs := st.GetLogs(1)
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	logEntry := logs[0]
	if logEntry.RequestTools != "declared: git,powershell" || logEntry.ResponseTools != "returned: powershell" {
		t.Fatalf("tool diagnostics missing: %#v", logEntry)
	}
	if logEntry.FallbackMode != "stream-to-nonstream" || logEntry.Normalization != "dsml" {
		t.Fatalf("recovery diagnostics missing: %#v", logEntry)
	}
	if strings.Contains(logEntry.RequestTools+logEntry.ResponseTools, "Get-ChildItem") {
		t.Fatalf("tool diagnostics must not include command arguments: %#v", logEntry)
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

func TestServerConfigDirFollowsConfigManagerPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	mgr, err := config.NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	server := NewServer(mgr.Get(), mgr, store.New(10), log.New(nil, log.LevelError, false))

	if got, want := server.configDir(), filepath.Dir(path); got != want {
		t.Fatalf("configDir() = %q, want %q", got, want)
	}
}

func TestModelTimeoutSecondsUsesSafeDefaultBudget(t *testing.T) {
	if got := modelTimeoutSeconds(&config.AppConfig{}, "gpt-test", "gpt-test", "useai", provider.ModelProfile{}, false); got != 60 {
		t.Fatalf("default timeout = %d, want 60 seconds", got)
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

func TestParseOpenAIStreamPayloadConvertsLegacyFunctionCall(t *testing.T) {
	chunk, err := parseOpenAIStreamPayload(`{"choices":[{"delta":{"function_call":{"name":"powershell","arguments":"{\"command\":\"pwd\"}"}},"finish_reason":null}]}`)
	if err != nil {
		t.Fatalf("parseOpenAIStreamPayload returned error: %v", err)
	}
	if len(chunk.ToolCalls) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(chunk.ToolCalls))
	}
	call, _ := chunk.ToolCalls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "powershell" {
		t.Fatalf("function call not converted: %#v", chunk.ToolCalls)
	}
}

func TestParseOpenAIStreamPayloadConvertsLegacyGitFunctionCall(t *testing.T) {
	chunk, err := parseOpenAIStreamPayload(`{"choices":[{"delta":{"function_call":{"name":"git","arguments":"{\"args\":[\"status\",\"--short\"],\"cwd\":\"C:\\\\repo\"}"}},"finish_reason":null}]}`)
	if err != nil {
		t.Fatalf("parseOpenAIStreamPayload returned error: %v", err)
	}
	if len(chunk.ToolCalls) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(chunk.ToolCalls))
	}
	call, _ := chunk.ToolCalls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "git" || fn["arguments"] != `{"args":["status","--short"],"cwd":"C:\\repo"}` {
		t.Fatalf("legacy git function call not converted: %#v", chunk.ToolCalls)
	}
}

func TestStreamOpenAIToOllamaPreservesToolCallArgumentChunks(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()

	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "create_file"}}},
	}
	err := server.streamOpenAIToOllama(rec, req, prov, chatReq, rec)
	if err != nil {
		t.Fatalf("streamOpenAIToOllama returned error: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("tool call chunks missing from Ollama stream: %s", body)
	}
	if !strings.Contains(body, `"arguments":"{\"path\":"`) || !strings.Contains(body, `"arguments":"\"a.txt\"}"`) {
		t.Fatalf("tool call argument chunks were not preserved: %s", body)
	}
	if !strings.Contains(body, `"done_reason":"tool_calls"`) {
		t.Fatalf("tool_calls finish reason missing: %s", body)
	}
}

func TestStreamOpenAIToOllamaDropsContinuationAfterBlockedUndeclaredTool(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Remove-Item\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "grep_search"}}},
	}

	if err := server.streamOpenAIToOllama(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("streamOpenAIToOllama returned error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Proxy blocked undeclared tool calls: powershell") {
		t.Fatalf("undeclared tool block notice missing: %s", body)
	}
	if strings.Contains(body, "Remove-Item") || strings.Contains(body, "<empty>") || strings.Contains(body, `"tool_calls":[`) {
		t.Fatalf("blocked tool continuation leaked to Ollama client: %s", body)
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

func TestStreamOpenAINormalizesBlankFinishReasonForVisualStudio(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":""}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	err := server.streamOpenAI(rec, req, prov, &provider.ChatRequest{Model: "gpt-test"}, rec)
	if err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}

	body := rec.Body.String()
	if strings.Contains(body, `"finish_reason":""`) {
		t.Fatalf("blank finish_reason leaked to Visual Studio stream: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason was not normalized to stop: %s", body)
	}
}

func TestStreamOpenAIPreservesDeclaredToolCallContinuationChunks(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_grep","type":"function","function":{"name":"grep_search","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"needle\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "grep_search"}}},
	}

	if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Proxy blocked undeclared tool calls") || strings.Contains(body, "<empty>") {
		t.Fatalf("declared stream tool continuation was incorrectly blocked: %s", body)
	}
	if !strings.Contains(body, `"name":"grep_search"`) || !strings.Contains(body, `"arguments":"\"needle\"}"`) {
		t.Fatalf("declared stream tool chunks were not preserved: %s", body)
	}
}

func TestStreamOpenAIDropsContinuationAfterBlockedUndeclaredTool(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ps","type":"function","function":{"name":"powershell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Remove-Item\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "grep_search"}}},
	}

	if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Proxy blocked undeclared tool calls: powershell") {
		t.Fatalf("undeclared tool block notice missing: %s", body)
	}
	if strings.Contains(body, "Remove-Item") || strings.Contains(body, "<empty>") {
		t.Fatalf("blocked tool continuation leaked to client: %s", body)
	}
	if strings.Contains(body, `"finish_reason":"tool_calls"`) || !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason should be stop after all stream tool calls were blocked: %s", body)
	}
}

func TestStreamOpenAIConvertsSuccessfulDSMLStreamToToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"<｜DSML｜tool_calls> <｜DSML｜invoke name=\"get_file\">"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":" <｜DSML｜parameter name=\"filename\" string=\"true\">a.cs</｜DSML｜parameter> </｜DSML｜invoke> </｜DSML｜tool_calls>"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "step-router-v1",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "get_file"}}},
	}

	err := server.streamOpenAI(rec, req, prov, chatReq, rec)
	if err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<｜DSML｜") || !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"name":"get_file"`) {
		t.Fatalf("DSML stream was not normalized: %s", body)
	}
	if got := rec.Header().Get("X-Proxy-Tool-Call-Normalization"); got != "dsml" {
		t.Fatalf("normalization header = %q, want dsml", got)
	}
}

func TestStreamOpenAIProbePreservesOrdinarySSE(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "get_file"}}},
	}

	if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	if rec.Body.String() != stream {
		t.Fatalf("ordinary SSE changed during DSML probe:\n got: %q\nwant: %q", rec.Body.String(), stream)
	}
	if got := rec.Header().Get("X-Proxy-Tool-Call-Normalization"); got != "" {
		t.Fatalf("ordinary stream should not report DSML normalization: %q", got)
	}
}

func TestStreamOpenAILeavesUndeclaredDSMLAsText(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"<｜DSML｜tool_calls><｜DSML｜invoke name=\"delete_file\"></｜DSML｜invoke></｜DSML｜tool_calls>"},"finish_reason":null}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	server := &Server{}
	prov := &fakeStreamProvider{name: "openai", body: stream}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	chatReq := &provider.ChatRequest{
		Model: "gpt-test",
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "get_file"}}},
	}

	if err := server.streamOpenAI(rec, req, prov, chatReq, rec); err != nil {
		t.Fatalf("streamOpenAI returned error: %v", err)
	}
	if rec.Body.String() != stream {
		t.Fatalf("undeclared DSML should remain original text: got %q want %q", rec.Body.String(), stream)
	}
	if got := rec.Header().Get("X-Proxy-Tool-Call-Normalization"); got != "" {
		t.Fatalf("rejected DSML must not report normalization: %q", got)
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
