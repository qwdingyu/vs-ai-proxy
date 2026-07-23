package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

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
			ID:      "openai-main",
			Name:    "openai",
			Type:    "openai",
			APIKey:  "sk-test",
			BaseURL: "https://example.invalid",
			Enabled: true,
		}},
		Models: []config.ModelConfig{{
			Name:       "model-x",
			ProviderID: "openai-main",
			Provider:   "openai-main",
			Enabled:    true,
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
	if gotCfg.Providers[0].ID != config.UseAIProviderID {
		t.Fatalf("first provider id = %q, want built-in UseAI first", gotCfg.Providers[0].ID)
	}
	if _, ok := findProviderConfig(gotCfg.Providers, "openai-main"); !ok {
		t.Fatalf("saved providers = %#v, want openai-main", gotCfg.Providers)
	}

	cfg, registry, _ := proxySrv.SnapshotComponents()
	if cfg.DefaultModel != "model-x" {
		t.Fatalf("proxy snapshot default model = %q, want %q", cfg.DefaultModel, "model-x")
	}
	if !containsString(registry.ProviderNames(), "openai-main") {
		t.Fatalf("registry providers = %#v, want openai-main", registry.ProviderNames())
	}
}

func TestConfigSaveHotUpdatesDefenseMode(t *testing.T) {
	apiSrv, proxySrv := newAPITestHarness(t)
	calls := 0
	var userAgent string
	var requestedWith string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			calls++
			userAgent = r.Header.Get("User-Agent")
			requestedWith = r.Header.Get("X-Requested-With")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"Service temporarily unavailable"}}`))
	}))
	defer upstream.Close()

	disabled := false
	payload := config.AppConfig{
		Port:         11434,
		DefaultModel: "gpt-5.5",
		Defense:      config.DefenseConfig{Enabled: &disabled},
		Providers: []config.ProviderConfig{{
			ID:       "useai2",
			Name:     "UseAI2",
			Type:     "openai",
			APIKey:   "sk-test",
			BaseURL:  upstream.URL + "/v1",
			Enabled:  true,
			Priority: 1,
		}},
		Models: []config.ModelConfig{{
			Name:       "gpt-5.5",
			ProviderID: "useai2",
			Provider:   "useai2",
			Enabled:    true,
		}},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config", mustJSONBody(t, payload))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/config status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	chatRec := httptest.NewRecorder()
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`))
	chatReq.Header.Set("Content-Type", "application/json")
	proxySrv.Handler().ServeHTTP(chatRec, chatReq)

	if chatRec.Code != http.StatusBadGateway {
		t.Fatalf("chat status = %d, want %d; body=%s", chatRec.Code, http.StatusBadGateway, chatRec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("calls = %d, disabled defense must not retry through hot-updated proxy", calls)
	}
	if strings.Contains(userAgent, "vs-ai-proxy") || requestedWith != "" {
		t.Fatalf("defensive headers should be disabled after config save, user-agent=%q x-requested-with=%q", userAgent, requestedWith)
	}
}

func TestManagementAPIResponsesAreNotCached(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
}

func TestProviderHealthEndpointReturnsRuntimeSnapshot(t *testing.T) {
	apiSrv, proxySrv := newAPITestHarness(t)
	_, registry, _ := proxySrv.SnapshotComponents()
	registry.RecordCandidateSuccess("useai", 125*time.Millisecond)
	registry.RecordCandidateFailure("useai", nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/providers/health", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/providers/health status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got []struct {
		Provider            string  `json:"provider"`
		Successes           int     `json:"successes"`
		Failures            int     `json:"failures"`
		ConsecutiveFailures int     `json:"consecutive_failures"`
		SuccessRate         float64 `json:"success_rate"`
		LatencyMs           float64 `json:"latency_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal provider health: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("health len = %d, want 1: %s", len(got), rec.Body.String())
	}
	item := got[0]
	if item.Provider != "useai" {
		t.Fatalf("provider = %q, want useai", item.Provider)
	}
	if item.Successes != 1 || item.Failures != 1 || item.ConsecutiveFailures != 1 {
		t.Fatalf("health counters = %#v, want one success and one consecutive failure", item)
	}
	if item.SuccessRate != 0.5 {
		t.Fatalf("success_rate = %v, want 0.5", item.SuccessRate)
	}
	if item.LatencyMs != 125 {
		t.Fatalf("latency_ms = %v, want 125", item.LatencyMs)
	}
}

func TestProviderHealthEndpointIncludesRegisteredProvidersWithoutTraffic(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	payload := config.AppConfig{
		Port:         11434,
		DefaultModel: "model-x",
		Providers: []config.ProviderConfig{{
			ID:       "idle-provider",
			Name:     "Idle Provider",
			Type:     "openai",
			BaseURL:  "https://example.invalid/v1",
			Enabled:  true,
			Priority: 1,
		}},
		Models: []config.ModelConfig{{
			Name:       "model-x",
			ProviderID: "idle-provider",
			Enabled:    true,
		}},
	}
	saveRec := httptest.NewRecorder()
	saveReq := httptest.NewRequest(http.MethodPut, "/api/config", mustJSONBody(t, payload))
	apiSrv.engine.ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("PUT /api/config status = %d, want %d; body=%s", saveRec.Code, http.StatusOK, saveRec.Body.String())
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/providers/health", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/providers/health status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"provider":"idle-provider"`)) {
		t.Fatalf("provider health should include idle registered provider: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"successes":0`)) {
		t.Fatalf("idle provider should have zero successes: %s", rec.Body.String())
	}
}

func TestVersionEndpointReturnsBuildVersion(t *testing.T) {
	proxy.SetBuildVersion("dev")
	t.Cleanup(func() { proxy.SetBuildVersion("dev") })
	proxy.SetBuildVersion("v9.9.9-test")
	apiSrv, _ := newAPITestHarness(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/version status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"version":"v9.9.9-test"`)) {
		t.Fatalf("version response = %s", rec.Body.String())
	}
}

func TestRuntimeStatusEndpointReturnsUpdateArtifacts(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/status", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/runtime/status status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got struct {
		Version         string `json:"version"`
		ExecutablePath  string `json:"executable_path"`
		PID             int    `json:"pid"`
		GOOS            string `json:"goos"`
		GOARCH          string `json:"goarch"`
		ListenHost      string `json:"listen_host"`
		ListenPort      int    `json:"listen_port"`
		AdminURL        string `json:"admin_url"`
		ProviderCount   int    `json:"provider_count"`
		ModelCount      int    `json:"model_count"`
		UpdateArtifacts struct {
			StagedBinary struct {
				Path string `json:"path"`
			} `json:"staged_binary"`
			SelfUpdateScript struct {
				Path string `json:"path"`
			} `json:"self_update_script"`
			SelfUpdateLog struct {
				Path string `json:"path"`
			} `json:"self_update_log"`
			BackupCount int `json:"backup_count"`
		} `json:"update_artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal runtime status: %v", err)
	}
	if got.Version == "" || got.ExecutablePath == "" {
		t.Fatalf("runtime status missing version/path: %#v", got)
	}
	if got.PID <= 0 || got.GOOS == "" || got.GOARCH == "" {
		t.Fatalf("runtime status missing process/platform fields: %#v", got)
	}
	if got.ListenHost == "" || got.ListenPort <= 0 || !strings.Contains(got.AdminURL, "/admin") {
		t.Fatalf("runtime status missing listen/admin fields: %#v", got)
	}
	if got.ProviderCount <= 0 {
		t.Fatalf("provider_count = %d, want positive default provider count", got.ProviderCount)
	}
	if !strings.HasSuffix(got.UpdateArtifacts.StagedBinary.Path, ".new") {
		t.Fatalf("staged path = %q, want .new suffix", got.UpdateArtifacts.StagedBinary.Path)
	}
	if !strings.HasSuffix(got.UpdateArtifacts.SelfUpdateScript.Path, "vs-ai-proxy-self-update.ps1") {
		t.Fatalf("script path = %q, want self-update ps1", got.UpdateArtifacts.SelfUpdateScript.Path)
	}
	if !strings.HasSuffix(got.UpdateArtifacts.SelfUpdateLog.Path, "vs-ai-proxy-self-update.log") {
		t.Fatalf("log path = %q, want self-update log", got.UpdateArtifacts.SelfUpdateLog.Path)
	}
}

func TestAdminURLHostUsesOpenableLoopbackForWildcardBinds(t *testing.T) {
	tests := []struct {
		listenHost string
		want       string
	}{
		{listenHost: "", want: "127.0.0.1"},
		{listenHost: "0.0.0.0", want: "127.0.0.1"},
		{listenHost: "::", want: "127.0.0.1"},
		{listenHost: "::1", want: "[::1]"},
		{listenHost: "2001:db8::1", want: "[2001:db8::1]"},
		{listenHost: "127.0.0.1", want: "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.listenHost, func(t *testing.T) {
			if got := adminURLHost(tt.listenHost); got != tt.want {
				t.Fatalf("adminURLHost(%q) = %q, want %q", tt.listenHost, got, tt.want)
			}
		})
	}
}

func TestDiagnosticsSummaryEndpointReturnsCopyableSummary(t *testing.T) {
	apiSrv, proxySrv := newAPITestHarness(t)
	_, registry, _ := proxySrv.SnapshotComponents()
	registry.RecordCandidateFailure("useai", errors.New("API 错误 503"))
	apiSrv.store.AddLog(store.RequestLog{
		Method:            "POST",
		Path:              "/v1/chat/completions",
		Provider:          "useai",
		Model:             "UseAI - gpt-5.5",
		Upstream:          "gpt-5.5",
		StatusCode:        499,
		ErrorCode:         "client_deadline_reached",
		ErrorReason:       "客户端等待上限",
		ErrorAction:       "查上游首 token",
		DiagnosticSummary: "VS/Copilot 接近等待上限后取消",
		CancelReason:      "client_deadline_reached",
		StreamState:       "waiting_response_headers",
		IsSuccess:         false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/diagnostics/summary", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/diagnostics/summary status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got struct {
		CopySummary      string                   `json:"copy_summary"`
		LatestFailure    store.RequestLog         `json:"latest_failure"`
		RecentStability  []store.StabilitySummary `json:"recent_stability"`
		ProblemProviders []providerHealthResponse `json:"problem_providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal diagnostics summary: %v", err)
	}
	if !strings.Contains(got.CopySummary, "client_deadline_reached") || !strings.Contains(got.CopySummary, "查上游首 token") {
		t.Fatalf("copy_summary missing actionable failure details: %q", got.CopySummary)
	}
	if got.LatestFailure.ErrorCode != "client_deadline_reached" {
		t.Fatalf("latest failure = %#v", got.LatestFailure)
	}
	if len(got.RecentStability) != 1 || got.RecentStability[0].Failures != 1 || got.RecentStability[0].TopCancelReasons[0].Key != "client_deadline_reached" {
		t.Fatalf("recent stability = %#v, want one failing useai summary with cancel reason", got.RecentStability)
	}
	if len(got.ProblemProviders) == 0 || got.ProblemProviders[0].Provider != "useai" {
		t.Fatalf("problem providers = %#v, want useai", got.ProblemProviders)
	}
}

func TestStatisticsEndpointExposesTokenCoverageAndModelUsage(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)
	apiSrv.store.AddLog(store.RequestLog{
		Provider: "zhipu", Model: "glm", Upstream: "glm-5.1", StatusCode: 200, IsSuccess: true,
		Usage: &store.TokenUsage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25, CachedTokens: 4, ReasoningTokens: 2, Source: "upstream"},
	})

	rec := httptest.NewRecorder()
	apiSrv.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/statistics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/statistics status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var stats store.Statistics
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal statistics: %v", err)
	}
	if stats.TokenUsageRequests != 1 || stats.UsageReportedCount != 1 || stats.TotalTokens != 25 {
		t.Fatalf("token statistics = %#v, want coverage 1/1 and total 25", stats)
	}
	if len(stats.ModelUsage) != 1 || stats.ModelUsage[0].Provider != "zhipu" || stats.ModelUsage[0].Upstream != "glm-5.1" {
		t.Fatalf("model usage = %#v, want zhipu/glm-5.1", stats.ModelUsage)
	}
}

func TestDiagnosticsSummaryUsesLatestFailureAcrossRetainedLogs(t *testing.T) {
	apiSrv, _ := newAPITestHarnessWithStoreMax(t, 100)
	apiSrv.store.AddLog(store.RequestLog{
		Method:      "POST",
		Path:        "/v1/chat/completions",
		Provider:    "useai",
		Model:       "gpt-5.5",
		StatusCode:  502,
		RequestID:   "retained-failure",
		ErrorCode:   "upstream_server_error",
		ErrorReason: "上游服务异常",
		IsSuccess:   false,
	})
	for i := 0; i < 60; i++ {
		apiSrv.store.AddLog(store.RequestLog{Method: "GET", Path: "/health", StatusCode: 200, IsSuccess: true})
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/diagnostics/summary", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/diagnostics/summary status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got struct {
		LatestFailure store.RequestLog `json:"latest_failure"`
		CopySummary   string           `json:"copy_summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal diagnostics summary: %v", err)
	}
	if got.LatestFailure.RequestID != "retained-failure" {
		t.Fatalf("latest_failure.request_id = %q, want retained-failure", got.LatestFailure.RequestID)
	}
	if !strings.Contains(got.CopySummary, "retained-failure") {
		t.Fatalf("copy_summary = %q, want retained-failure", got.CopySummary)
	}
}

func TestDiagnosticsSummaryCopyTextIncludesLatestFailureAndProblemProvider(t *testing.T) {
	apiSrv, proxySrv := newAPITestHarness(t)
	_, registry, _ := proxySrv.SnapshotComponents()
	registry.RecordCandidateFailure("useai", errors.New("API 错误 503"))
	apiSrv.store.AddLog(store.RequestLog{
		RequestID:         "req-copy-1",
		Provider:          "useai",
		Model:             "gpt-5.5",
		Upstream:          "gpt-5.5",
		StatusCode:        499,
		ErrorCode:         "client_deadline_reached",
		ErrorReason:       "客户端等待上限",
		ErrorAction:       "查上游首 token",
		DiagnosticSummary: "VS/Copilot 接近等待上限后取消",
		IsSuccess:         false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/diagnostics/summary", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/diagnostics/summary status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got struct {
		CopySummary string `json:"copy_summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal diagnostics summary: %v", err)
	}
	for _, want := range []string{"req-copy-1", "client_deadline_reached", "查上游首 token", "异常 provider", "近期稳定性", "近期最新失败"} {
		if !strings.Contains(got.CopySummary, want) {
			t.Fatalf("copy_summary = %q, want contains %q", got.CopySummary, want)
		}
	}
}

func TestLogsEndpointSupportsFiltering(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)
	apiSrv.store.AddLog(store.RequestLog{Method: "POST", Path: "/v1/chat/completions", Provider: "useai", Model: "gpt-5.5", StatusCode: 499, ErrorCode: "client_deadline_reached", ErrorReason: "客户端等待上限", RequestID: "req-1", DiagnosticSummary: "VS/Copilot 接近等待上限后取消"})
	apiSrv.store.AddLog(store.RequestLog{Method: "POST", Path: "/v1/chat/completions", Provider: "deepseek", Model: "deepseek-v4-flash", StatusCode: 502, ErrorCode: "upstream_server_error", ErrorReason: "上游服务异常", RequestID: "req-2", DiagnosticSummary: "上游返回 5xx"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs?page=1&page_size=20&provider=deepseek&status_code=502&q=5xx&request_id=req-2&error_reason="+url.QueryEscape("上游服务"), nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var result store.LogPageResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal logs result: %v", err)
	}
	if got, want := len(result.Logs), 1; got != want {
		t.Fatalf("logs len = %d, want %d", got, want)
	}
	if result.Logs[0].Provider != "deepseek" || result.Logs[0].RequestID != "req-2" {
		t.Fatalf("filtered log = %#v, want deepseek req-2", result.Logs[0])
	}
}

func TestLogsEndpointSupportsLegacyLimit(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)
	apiSrv.store.AddLog(store.RequestLog{Method: "GET", Path: "/health", Provider: "useai", StatusCode: 200, IsSuccess: true})
	apiSrv.store.AddLog(store.RequestLog{Method: "POST", Path: "/v1/chat/completions", Provider: "deepseek", StatusCode: 502, IsSuccess: false})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs?limit=1", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs limit status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var result struct {
		Logs []store.RequestLog `json:"logs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal logs result: %v", err)
	}
	if got, want := len(result.Logs), 1; got != want {
		t.Fatalf("logs len = %d, want %d", got, want)
	}
	if result.Logs[0].Provider != "deepseek" {
		t.Fatalf("newest log = %#v, want deepseek", result.Logs[0])
	}
}

func TestLogsEndpointClampsPageSizeAndLegacyLimit(t *testing.T) {
	apiSrv, _ := newAPITestHarnessWithStoreMax(t, 500)
	for i := 0; i < 250; i++ {
		apiSrv.store.AddLog(store.RequestLog{Method: "GET", Path: "/health", Provider: "useai", StatusCode: 200, IsSuccess: true})
	}

	pageRec := httptest.NewRecorder()
	pageReq := httptest.NewRequest(http.MethodGet, "/api/logs?page=1&page_size=1000", nil)
	apiSrv.engine.ServeHTTP(pageRec, pageReq)

	if pageRec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs page status = %d, want %d; body=%s", pageRec.Code, http.StatusOK, pageRec.Body.String())
	}
	var pageResult store.LogPageResult
	if err := json.Unmarshal(pageRec.Body.Bytes(), &pageResult); err != nil {
		t.Fatalf("unmarshal page result: %v", err)
	}
	if got, want := pageResult.Size, 200; got != want {
		t.Fatalf("page size = %d, want %d", got, want)
	}
	if got, want := len(pageResult.Logs), 200; got != want {
		t.Fatalf("page logs len = %d, want %d", got, want)
	}

	limitRec := httptest.NewRecorder()
	limitReq := httptest.NewRequest(http.MethodGet, "/api/logs?limit=1000", nil)
	apiSrv.engine.ServeHTTP(limitRec, limitReq)

	if limitRec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs limit status = %d, want %d; body=%s", limitRec.Code, http.StatusOK, limitRec.Body.String())
	}
	var limitResult struct {
		Logs []store.RequestLog `json:"logs"`
	}
	if err := json.Unmarshal(limitRec.Body.Bytes(), &limitResult); err != nil {
		t.Fatalf("unmarshal limit result: %v", err)
	}
	if got, want := len(limitResult.Logs), 200; got != want {
		t.Fatalf("limit logs len = %d, want %d", got, want)
	}
}

func TestClearLogsPersistsEmptySnapshot(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)
	path := filepath.Join(t.TempDir(), "logs.json")
	apiSrv.SetStorePath(path)
	apiSrv.store.AddLog(store.RequestLog{Method: "POST", Path: "/v1/chat/completions", Provider: "useai", StatusCode: 502, IsSuccess: false})
	if err := apiSrv.store.PersistToFile(path); err != nil {
		t.Fatalf("PersistToFile() before clear error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/logs", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/logs status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	loaded := store.New(10)
	if err := loaded.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile() after clear error = %v", err)
	}
	if got := len(loaded.GetLogs(10)); got != 0 {
		t.Fatalf("persisted logs len = %d, want 0", got)
	}
}

func TestClearLogsReportsPersistFailure(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)
	badPath := t.TempDir()
	apiSrv.SetStorePath(badPath)
	apiSrv.store.AddLog(store.RequestLog{Method: "POST", Path: "/v1/chat/completions", Provider: "useai", StatusCode: 502, IsSuccess: false})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/logs", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE /api/logs status = %d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if got := len(apiSrv.store.GetLogs(10)); got != 0 {
		t.Fatalf("in-memory logs len = %d, want 0", got)
	}
	if _, err := os.Stat(badPath); err != nil {
		t.Fatalf("badPath should still exist as directory: %v", err)
	}
}

func TestAdminManagementAPIRoutesWorkAndAreNotCached(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/config", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/config status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
}

func TestAdminManagementAPIRequiresBearerTokenWhenConfigured(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "admin-secret")
	apiSrv, _ := newAPITestHarness(t)

	unauthorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/config", nil)
	apiSrv.engine.ServeHTTP(unauthorized, req)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	authorized := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/config", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	apiSrv.engine.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d; body=%s", authorized.Code, http.StatusOK, authorized.Body.String())
	}
}

func TestAdminRouteRequiresLoginWhenConfigured(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "admin-secret")
	apiSrv, _ := newAPITestHarnessWithStaticFS(t, fstest.MapFS{
		"index.html": {Data: []byte("admin-app")},
	})

	unauthorizedPage := httptest.NewRecorder()
	pageReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	apiSrv.engine.ServeHTTP(unauthorizedPage, pageReq)
	if unauthorizedPage.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized admin page status = %d, want %d", unauthorizedPage.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(unauthorizedPage.Body.String(), "ADMIN_API_KEY") {
		t.Fatalf("unauthorized admin page should render login form: %s", unauthorizedPage.Body.String())
	}

	badLogin := httptest.NewRecorder()
	badReq := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader("token=wrong"))
	badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	apiSrv.engine.ServeHTTP(badLogin, badReq)
	if badLogin.Code != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d, want %d", badLogin.Code, http.StatusUnauthorized)
	}

	login := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader("token=admin-secret"))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	apiSrv.engine.ServeHTTP(login, loginReq)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", login.Code, http.StatusSeeOther)
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("login did not set session cookie")
	}

	authorizedPage := httptest.NewRecorder()
	authorizedReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	authorizedReq.AddCookie(cookies[0])
	apiSrv.engine.ServeHTTP(authorizedPage, authorizedReq)
	if authorizedPage.Code != http.StatusOK {
		t.Fatalf("authorized admin page status = %d, want %d; body=%s", authorizedPage.Code, http.StatusOK, authorizedPage.Body.String())
	}
	if authorizedPage.Body.String() != "admin-app" {
		t.Fatalf("authorized admin page body = %q, want admin-app", authorizedPage.Body.String())
	}

	authorizedAPI := httptest.NewRecorder()
	apiReq := httptest.NewRequest(http.MethodGet, "/admin/api/config", nil)
	apiReq.AddCookie(cookies[0])
	apiSrv.engine.ServeHTTP(authorizedAPI, apiReq)
	if authorizedAPI.Code != http.StatusOK {
		t.Fatalf("authorized admin api status = %d, want %d; body=%s", authorizedAPI.Code, http.StatusOK, authorizedAPI.Body.String())
	}

	logout := httptest.NewRecorder()
	logoutReq := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	logoutReq.AddCookie(cookies[0])
	apiSrv.engine.ServeHTTP(logout, logoutReq)
	if logout.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want %d", logout.Code, http.StatusSeeOther)
	}
	cleared := false
	for _, cookie := range logout.Result().Cookies() {
		if cookie.Name == adminSessionCookieName && cookie.MaxAge < 0 && cookie.Path == "/admin" {
			cleared = true
			break
		}
	}
	if !cleared {
		t.Fatalf("logout did not clear %s cookie: %v", adminSessionCookieName, logout.Result().Cookies())
	}
}

func TestAdminStaticFAQImagesAreServed(t *testing.T) {
	apiSrv, _ := newAPITestHarnessWithStaticFS(t, fstest.MapFS{
		"index.html":                    {Data: []byte("admin-app")},
		"assets/images/qrcode_qq.png":   {Data: []byte("png-qq")},
		"assets/images/vs-config-1.png": {Data: []byte("png-1")},
		"assets/images/vs-config-2.png": {Data: []byte("png-2")},
		"assets/images/vs-config-3.png": {Data: []byte("png-3")},
	})

	for _, path := range []string{
		"/assets/images/qrcode_qq.png",
		"/admin/assets/images/qrcode_qq.png",
		"/admin/assets/images/vs-config-1.png",
		"/admin/assets/images/vs-config-2.png",
		"/admin/assets/images/vs-config-3.png",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		apiSrv.engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want %d; body=%s", path, rec.Code, http.StatusOK, rec.Body.String())
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "image/png") {
			t.Fatalf("GET %s Content-Type = %q, want image/png", path, rec.Header().Get("Content-Type"))
		}
	}
}

func TestAdminI18nAssetsAreServed(t *testing.T) {
	apiSrv, _ := newAPITestHarnessWithStaticFS(t, fstest.MapFS{
		"index.html":    {Data: []byte("admin-app")},
		"i18n/index.js": {Data: []byte("window.i18nRuntime = true;")},
		"i18n/zh.js":    {Data: []byte("window.zhCatalog = true;")},
		"i18n/en.js":    {Data: []byte("window.enCatalog = true;")},
	})

	tests := []struct {
		path string
		body string
	}{
		{path: "/admin/i18n/index.js", body: "window.i18nRuntime = true;"},
		{path: "/admin/i18n/zh.js", body: "window.zhCatalog = true;"},
		{path: "/admin/i18n/en.js", body: "window.enCatalog = true;"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			apiSrv.engine.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d; body=%s", test.path, rec.Code, http.StatusOK, rec.Body.String())
			}
			if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "javascript") {
				t.Fatalf("GET %s Content-Type = %q, want JavaScript", test.path, contentType)
			}
			if rec.Body.String() != test.body {
				t.Fatalf("GET %s body = %q, want %q", test.path, rec.Body.String(), test.body)
			}
		})
	}
}

func TestAdminManagementAPIFallsBackToProxyAPIKey(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "")
	t.Setenv("PROXY_API_KEY", "proxy-secret")
	apiSrv, _ := newAPITestHarness(t)

	unauthorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/config", nil)
	apiSrv.engine.ServeHTTP(unauthorized, req)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	authorized := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/config", nil)
	req.Header.Set("Authorization", "Bearer proxy-secret")
	apiSrv.engine.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d; body=%s", authorized.Code, http.StatusOK, authorized.Body.String())
	}
}

func TestConfigSaveRejectsDuplicateProviderIDs(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	payload := config.AppConfig{
		Port:         11434,
		DefaultModel: "model-x",
		Providers: []config.ProviderConfig{
			{ID: "dup", Name: "A", Type: "openai", BaseURL: "https://a.invalid", Enabled: true},
			{ID: "dup", Name: "B", Type: "openai", BaseURL: "https://b.invalid", Enabled: true},
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config", mustJSONBody(t, payload))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate provider config status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestConfigValidateClearsModelNamespaceProviderBinding(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	payload := config.AppConfig{
		Port: 12345,
		Providers: []config.ProviderConfig{{
			ID:      "usecpa",
			Name:    "UseCPA",
			Type:    "openai",
			BaseURL: "https://example.invalid/v1",
			Enabled: true,
		}},
		Models: []config.ModelConfig{{
			Name:       "z-ai/glm-5.2",
			ProviderID: "z-ai",
			Provider:   "z-ai",
			Enabled:    true,
		}},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/validate", mustJSONBody(t, payload))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/config/validate status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"valid":true`)) {
		t.Fatalf("model namespace provider binding should be treated as automatic routing: %s", rec.Body.String())
	}
}

func TestConfigValidateReportsUnknownModelProvider(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	payload := config.AppConfig{
		Port: 12345,
		Providers: []config.ProviderConfig{{
			ID:      "usecpa",
			Name:    "UseCPA",
			Type:    "openai",
			BaseURL: "https://example.invalid/v1",
			Enabled: true,
		}},
		Models: []config.ModelConfig{{
			Name:       "z-ai/glm-5.2",
			ProviderID: "missing-provider",
			Provider:   "missing-provider",
			Enabled:    true,
		}},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/validate", mustJSONBody(t, payload))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/config/validate status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"valid":false`)) {
		t.Fatalf("validation should be invalid: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"model_provider_not_found"`)) {
		t.Fatalf("validation should explain missing provider: %s", rec.Body.String())
	}
}

func TestConfigSaveRejectsInvalidModelProvider(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	payload := config.AppConfig{
		Port: 12345,
		Providers: []config.ProviderConfig{{
			ID:      "usecpa",
			Name:    "UseCPA",
			Type:    "openai",
			BaseURL: "https://example.invalid/v1",
			Enabled: true,
		}},
		Models: []config.ModelConfig{{
			Name:       "z-ai/glm-5.2",
			ProviderID: "z-ai/glm-5.2",
			Provider:   "z-ai/glm-5.2",
			Enabled:    true,
		}},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config", mustJSONBody(t, payload))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid model provider status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"model_provider_not_found"`)) {
		t.Fatalf("response should include structured validation issue: %s", rec.Body.String())
	}
}

func TestConfigSaveMigratesUserPayloadWithModelNamespaceProviderBinding(t *testing.T) {
	apiSrv, proxySrv := newAPITestHarness(t)

	payload := config.AppConfig{
		Port:         12345,
		DefaultModel: "deepseek-v4-flash",
		Providers: []config.ProviderConfig{
			{ID: "useai", Name: "UseAI", DisplayName: "UseAI", APIKey: "sk-test", BaseURL: "https://api.eforge.xyz/v1", Type: "openai", Enabled: true, Priority: 0},
			{ID: "deepseek", Name: "deepseek", DisplayName: "deepseek", APIKey: "sk-test", BaseURL: "https://api.deepseek.com", Type: "openai", Enabled: true, Priority: 1},
			{ID: "ollama", Name: "ollama", DisplayName: "ollama", BaseURL: "http://localhost:11434", Type: "ollama", Enabled: true, Priority: 2},
			{ID: "usecpa", Name: "UseCpa", DisplayName: "UseCpa", APIKey: "api123", BaseURL: "https://cpa.eforge.xyz/v1", Type: "openai", Enabled: true, Priority: 10},
		},
		Models: []config.ModelConfig{
			{Name: "deepseek/deepseek-v4-pro", ProviderID: "deepseek", Provider: "deepseek", Enabled: true},
			{Name: "llama-3.3-70b", ProviderID: "ollama", Provider: "ollama", Enabled: true},
			{Name: "deepseek/deepseek-v4-flash", ProviderID: "deepseek", Provider: "deepseek", Enabled: true},
			{Name: "z-ai/glm-5.2", ProviderID: "z-ai", Provider: "z-ai", Enabled: true},
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config", mustJSONBody(t, payload))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/config status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	cfg, registry, _ := proxySrv.SnapshotComponents()
	found := false
	for _, model := range cfg.Models {
		if model.Name != "z-ai/glm-5.2" {
			continue
		}
		found = true
		if model.ProviderID != "" || model.Provider != "" {
			t.Fatalf("z-ai/glm-5.2 provider binding = %q/%q, want empty automatic routing", model.ProviderID, model.Provider)
		}
	}
	if !found {
		t.Fatalf("saved config missing z-ai/glm-5.2: %#v", cfg.Models)
	}
	candidates := registry.ResolveCandidates("z-ai/glm-5.2")
	if len(candidates) == 0 {
		t.Fatalf("z-ai/glm-5.2 should resolve through automatic provider fallback")
	}
}

func TestConfigValidateUsesBuiltInProviderNormalization(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	payload := config.AppConfig{
		Port:      12345,
		Providers: []config.ProviderConfig{},
		Models: []config.ModelConfig{{
			Name:       "deepseek-v4-flash",
			ProviderID: config.UseAIProviderID,
			Provider:   config.UseAIProviderID,
			Enabled:    true,
		}},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/validate", mustJSONBody(t, payload))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/config/validate status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"valid":true`)) {
		t.Fatalf("built-in UseAI normalization should make provider reference valid: %s", rec.Body.String())
	}
}

func TestProviderEndpointsCRUDAndHotUpdate(t *testing.T) {
	apiSrv, proxySrv := newAPITestHarness(t)

	addReqBody := config.ProviderConfig{
		ID:      "openai-paid",
		Name:    "OpenAI Paid",
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
	if !bytes.Contains(listRec.Body.Bytes(), []byte(`"id":"openai-paid"`)) {
		t.Fatalf("provider list missing openai-paid: %s", listRec.Body.String())
	}
	if !bytes.Contains(listRec.Body.Bytes(), []byte(`"compatibility_profile"`)) ||
		!bytes.Contains(listRec.Body.Bytes(), []byte(`"api_format":"openai"`)) ||
		!bytes.Contains(listRec.Body.Bytes(), []byte(`"output_token_param":"max_tokens"`)) {
		t.Fatalf("provider list should expose compatibility profile: %s", listRec.Body.String())
	}

	updBody := config.ProviderConfig{
		ID:      "openai-paid",
		Name:    "OpenAI Paid",
		Type:    "openai",
		APIKey:  "sk-updated",
		BaseURL: "https://example.invalid",
		Enabled: false,
	}
	updRec := httptest.NewRecorder()
	updReq := httptest.NewRequest(http.MethodPut, "/api/providers/openai-paid", mustJSONBody(t, updBody))
	apiSrv.engine.ServeHTTP(updRec, updReq)
	if updRec.Code != http.StatusOK {
		t.Fatalf("PUT /api/providers/openai-paid status = %d, want %d; body=%s", updRec.Code, http.StatusOK, updRec.Body.String())
	}

	cfg, registry, _ := proxySrv.SnapshotComponents()
	openAI, ok := findProviderConfig(cfg.Providers, "openai-paid")
	if !ok {
		t.Fatalf("openai-paid provider missing after update: %#v", cfg.Providers)
	}
	if openAI.APIKey != "sk-updated" {
		t.Fatalf("updated provider key = %q, want sk-updated", openAI.APIKey)
	}
	if containsString(registry.ProviderNames(), "openai-paid") {
		t.Fatalf("disabled provider should not be routed by registry")
	}

	delRec := httptest.NewRecorder()
	delReq := httptest.NewRequest(http.MethodDelete, "/api/providers/openai-paid", nil)
	apiSrv.engine.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/providers/openai-paid status = %d, want %d; body=%s", delRec.Code, http.StatusOK, delRec.Body.String())
	}
}

func TestProviderEndpointsRejectInvalidProvider(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	tests := []struct {
		name string
		body config.ProviderConfig
	}{
		{name: "empty id and name", body: config.ProviderConfig{Type: "openai", BaseURL: "https://example.invalid"}},
		{name: "empty base url", body: config.ProviderConfig{ID: "bad", Name: "Bad", Type: "openai"}},
		{name: "bad type", body: config.ProviderConfig{ID: "bad", Name: "Bad", Type: "bad", BaseURL: "https://example.invalid"}},
		{name: "useai renamed id", body: config.ProviderConfig{ID: "useai-paid", Name: "UseAI", Type: "openai", BaseURL: "https://api.eforge.xyz/v1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/providers", mustJSONBody(t, tt.body))
			apiSrv.engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func TestProviderEndpointsRejectDuplicateIDs(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	body := config.ProviderConfig{
		ID:      "openai-paid",
		Name:    "OpenAI Paid",
		Type:    "openai",
		APIKey:  "sk-test",
		BaseURL: "https://example.invalid",
		Enabled: true,
	}
	firstRec := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/api/providers", mustJSONBody(t, body))
	apiSrv.engine.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, want %d; body=%s", firstRec.Code, http.StatusOK, firstRec.Body.String())
	}

	secondRec := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/api/providers", mustJSONBody(t, body))
	apiSrv.engine.ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusConflict {
		t.Fatalf("duplicate POST status = %d, want %d; body=%s", secondRec.Code, http.StatusConflict, secondRec.Body.String())
	}
}

func TestProviderProbeOpenAICorrectsBaseURL(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
	}))
	defer upstream.Close()

	apiSrv, _ := newAPITestHarness(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers/probe", mustJSONBody(t, map[string]any{
		"provider": config.ProviderConfig{
			ID:      "probe",
			Name:    "Probe",
			Type:    "openai",
			BaseURL: upstream.URL + "/v1/chat/completions",
			Enabled: true,
		},
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/providers/probe status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"reachable":true`)) {
		t.Fatalf("probe should be reachable: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"corrected_base_url":"`+upstream.URL+`/v1"`)) {
		t.Fatalf("probe should suggest /v1 base URL: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"models_count":2`)) {
		t.Fatalf("probe should count models: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"models":["model-a","model-b"]`)) {
		t.Fatalf("probe should include full models for Web import: %s", rec.Body.String())
	}
}

func TestProviderProbeOllamaCorrectsBaseURL(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3"},{"name":"qwen3"}]}`))
	}))
	defer upstream.Close()

	apiSrv, _ := newAPITestHarness(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers/probe", mustJSONBody(t, map[string]any{
		"provider": config.ProviderConfig{
			ID:      "ollama-local",
			Name:    "Ollama",
			Type:    "ollama",
			BaseURL: upstream.URL + "/api/tags",
			Enabled: true,
		},
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/providers/probe status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"reachable":true`)) {
		t.Fatalf("probe should be reachable: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"corrected_base_url":"`+upstream.URL+`"`)) {
		t.Fatalf("probe should suggest root Ollama base URL: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"models_count":2`)) {
		t.Fatalf("probe should count models: %s", rec.Body.String())
	}
}

func TestProviderProbeReportsFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	apiSrv, _ := newAPITestHarness(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers/probe", mustJSONBody(t, map[string]any{
		"provider": config.ProviderConfig{
			ID:      "bad",
			Name:    "Bad",
			Type:    "openai",
			BaseURL: upstream.URL,
			Enabled: true,
		},
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/providers/probe status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"reachable":false`)) {
		t.Fatalf("probe should fail: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"attempts"`)) {
		t.Fatalf("probe should include attempts: %s", rec.Body.String())
	}
}

func TestProviderUpdateRejectsIDCollision(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	for _, body := range []config.ProviderConfig{
		{ID: "provider-a", Name: "Provider A", Type: "openai", BaseURL: "https://a.invalid", Enabled: true},
		{ID: "provider-b", Name: "Provider B", Type: "openai", BaseURL: "https://b.invalid", Enabled: true},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/providers", mustJSONBody(t, body))
		apiSrv.engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s status = %d, want %d; body=%s", body.ID, rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/providers/provider-b", mustJSONBody(t, config.ProviderConfig{
		ID:      "provider-a",
		Name:    "Provider B renamed",
		Type:    "openai",
		BaseURL: "https://b.invalid",
		Enabled: true,
	}))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("colliding PUT status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestProviderUpdateRejectsBreakingModelBinding(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	providerBody := config.ProviderConfig{
		ID:      "provider-a",
		Name:    "Provider A",
		Type:    "openai",
		BaseURL: "https://a.invalid",
		Enabled: true,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers", mustJSONBody(t, providerBody))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/providers status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	models := []config.ModelConfig{{Name: "model-a", ProviderID: "provider-a", Provider: "provider-a", Enabled: true}}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/models", mustJSONBody(t, models))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/models status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/providers/provider-a", mustJSONBody(t, config.ProviderConfig{
		ID:      "provider-renamed",
		Name:    "Provider Renamed",
		Type:    "openai",
		BaseURL: "https://a.invalid",
		Enabled: true,
	}))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("renaming bound provider status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"model_provider_not_found"`)) {
		t.Fatalf("response should explain broken model binding: %s", rec.Body.String())
	}
}

func TestProviderUpdateAllowsUnrelatedInvalidModelBinding(t *testing.T) {
	apiSrv, proxySrv := newAPITestHarness(t)
	dirtyCfg := config.AppConfig{
		Port:         11434,
		DefaultModel: "z-ai/glm-5.2",
		Providers: []config.ProviderConfig{
			{ID: "usecpa", Name: "UseCpa", Type: "openai", BaseURL: "https://cpa.eforge.xyz/v1", Enabled: true},
		},
		Models: []config.ModelConfig{
			{Name: "deepseek-v4-flash", ProviderID: "deepseek-v4-flash", Provider: "deepseek-v4-flash", Enabled: true},
			{Name: "z-ai/glm-5.2", ProviderID: "usecpa", Provider: "usecpa", Enabled: true},
		},
	}
	if err := apiSrv.configMgr.Save(&dirtyCfg); err != nil {
		t.Fatalf("Save dirty config: %v", err)
	}
	proxySrv.Reconfigure(apiSrv.configMgr.Get())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/providers/usecpa", mustJSONBody(t, config.ProviderConfig{
		ID:      "usecpa",
		Name:    "UseCpa",
		Type:    "openai",
		APIKey:  "sk-updated",
		BaseURL: "https://cpa.eforge.xyz/v1",
		Enabled: true,
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT unrelated dirty provider status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	cfg, _, _ := proxySrv.SnapshotComponents()
	updated, ok := findProviderConfig(cfg.Providers, "usecpa")
	if !ok {
		t.Fatalf("usecpa provider missing after update: %#v", cfg.Providers)
	}
	if updated.APIKey != "sk-updated" {
		t.Fatalf("updated api key = %q, want sk-updated", updated.APIKey)
	}
}

func TestProviderUpdateRejectsBuiltInUseAIIDChange(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/providers/useai", mustJSONBody(t, config.ProviderConfig{
		ID:      "useai-renamed",
		Name:    "UseAI Renamed",
		Type:    "openai",
		BaseURL: "https://api.eforge.xyz/v1",
		Enabled: true,
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("UseAI id change status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestDeleteProviderRejectsBreakingModelBinding(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	providerBody := config.ProviderConfig{
		ID:      "provider-a",
		Name:    "Provider A",
		Type:    "openai",
		BaseURL: "https://a.invalid",
		Enabled: true,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers", mustJSONBody(t, providerBody))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/providers status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	models := []config.ModelConfig{{Name: "model-a", ProviderID: "provider-a", Provider: "provider-a", Enabled: true}}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/models", mustJSONBody(t, models))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/models status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/providers/provider-a", nil)
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE bound provider status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"model_provider_not_found"`)) {
		t.Fatalf("response should explain broken model binding: %s", rec.Body.String())
	}
}

func TestDeleteProviderRejectsBuiltInUseAI(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/providers/useai", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE built-in UseAI status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("UseAI")) {
		t.Fatalf("DELETE built-in UseAI response should explain UseAI is protected: %s", rec.Body.String())
	}
}

func TestModelEndpointsRoundTrip(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	providerRec := httptest.NewRecorder()
	providerReq := httptest.NewRequest(http.MethodPost, "/api/providers", mustJSONBody(t, config.ProviderConfig{
		ID:      "openai-paid",
		Name:    "OpenAI Paid",
		Type:    "openai",
		BaseURL: "https://example.invalid",
		Enabled: true,
	}))
	apiSrv.engine.ServeHTTP(providerRec, providerReq)
	if providerRec.Code != http.StatusOK {
		t.Fatalf("POST /api/providers status = %d, want %d; body=%s", providerRec.Code, http.StatusOK, providerRec.Body.String())
	}

	models := []config.ModelConfig{{
		Name:            "model-a",
		Provider:        "OpenAI Paid",
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
	if !bytes.Contains(listRec.Body.Bytes(), []byte(`"provider_id":"openai-paid"`)) {
		t.Fatalf("model list missing normalized provider_id: %s", listRec.Body.String())
	}
}

func TestModelSaveEnrichesMissingMetadata(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)
	addProviderRec := httptest.NewRecorder()
	addProviderReq := httptest.NewRequest(http.MethodPost, "/api/providers", mustJSONBody(t, config.ProviderConfig{
		ID:      "deepseek",
		Name:    "deepseek",
		Type:    "openai",
		APIKey:  "sk-test",
		BaseURL: "https://api.deepseek.com",
		Enabled: true,
	}))
	apiSrv.engine.ServeHTTP(addProviderRec, addProviderReq)
	if addProviderRec.Code != http.StatusOK {
		t.Fatalf("POST /api/providers status = %d, want %d; body=%s", addProviderRec.Code, http.StatusOK, addProviderRec.Body.String())
	}

	models := []config.ModelConfig{{
		Name:       "deepseek/deepseek-v4-flash",
		ProviderID: "deepseek",
		Provider:   "deepseek",
		Enabled:    true,
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/models", mustJSONBody(t, models))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/models status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	cfgRec := httptest.NewRecorder()
	cfgReq := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	apiSrv.engine.ServeHTTP(cfgRec, cfgReq)
	if cfgRec.Code != http.StatusOK {
		t.Fatalf("GET /api/config status = %d, want %d", cfgRec.Code, http.StatusOK)
	}

	var got config.AppConfig
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("models len = %d, want 1", len(got.Models))
	}
	model := got.Models[0]
	if model.ContextLength == nil || *model.ContextLength <= 0 {
		t.Fatalf("context_length = %v, want positive metadata value", model.ContextLength)
	}
	if model.MaxOutputTokens == nil || *model.MaxOutputTokens <= 0 {
		t.Fatalf("max_output_tokens = %v, want positive metadata value", model.MaxOutputTokens)
	}
	if model.SupportsTools == nil || !*model.SupportsTools {
		t.Fatalf("supports_tools = %v, want true", model.SupportsTools)
	}
	if model.SupportsVision == nil || *model.SupportsVision {
		t.Fatalf("supports_vision = %v, want false", model.SupportsVision)
	}
}

func TestModelSaveEnrichesAPISwitchMetadataSeed(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	models := []config.ModelConfig{{
		Name:       "z-ai/glm-5.2",
		ProviderID: config.UseAIProviderID,
		Provider:   config.UseAIProviderID,
		Enabled:    true,
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/models", mustJSONBody(t, models))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/models status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	cfgRec := httptest.NewRecorder()
	cfgReq := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	apiSrv.engine.ServeHTTP(cfgRec, cfgReq)
	if cfgRec.Code != http.StatusOK {
		t.Fatalf("GET /api/config status = %d, want %d", cfgRec.Code, http.StatusOK)
	}

	var got config.AppConfig
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("models len = %d, want 1", len(got.Models))
	}
	model := got.Models[0]
	if model.ContextLength == nil || *model.ContextLength != 1000000 {
		t.Fatalf("context_length = %v, want 1000000", model.ContextLength)
	}
	if model.MaxOutputTokens == nil || *model.MaxOutputTokens != 131072 {
		t.Fatalf("max_output_tokens = %v, want 131072", model.MaxOutputTokens)
	}
	if model.SupportsTools == nil || !*model.SupportsTools {
		t.Fatalf("supports_tools = %v, want true", model.SupportsTools)
	}
	if model.SupportsVision == nil || *model.SupportsVision {
		t.Fatalf("supports_vision = %v, want false", model.SupportsVision)
	}
}

func TestModelSavePreservesExplicitContextAndInheritsUnsetDefaults(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)
	explicitContext := 222222
	models := []config.ModelConfig{{
		Name:          "deepseek-v4-flash",
		ProviderID:    config.UseAIProviderID,
		Provider:      config.UseAIProviderID,
		ContextLength: &explicitContext,
		Enabled:       true,
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/models", mustJSONBody(t, models))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/models status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	configRec := httptest.NewRecorder()
	configReq := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	apiSrv.engine.ServeHTTP(configRec, configReq)
	if configRec.Code != http.StatusOK {
		t.Fatalf("GET /api/config status = %d, want %d", configRec.Code, http.StatusOK)
	}

	var got config.AppConfig
	if err := json.Unmarshal(configRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("models len = %d, want 1", len(got.Models))
	}
	model := got.Models[0]
	if model.ContextLength == nil || *model.ContextLength != explicitContext {
		t.Fatalf("context_length = %v, want explicit %d", model.ContextLength, explicitContext)
	}
	if model.MaxOutputTokens == nil || *model.MaxOutputTokens != 131072 {
		t.Fatalf("max_output_tokens = %v, want inherited 131072", model.MaxOutputTokens)
	}
	if model.SupportsTools == nil || !*model.SupportsTools {
		t.Fatalf("supports_tools = %v, want inherited true", model.SupportsTools)
	}
}

func TestModelSaveClampsOutputToExplicitContext(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)
	contextLength := 1000
	maxOutput := 4096
	models := []config.ModelConfig{{
		Name:            "unknown-model",
		ProviderID:      config.UseAIProviderID,
		Provider:        config.UseAIProviderID,
		ContextLength:   &contextLength,
		MaxOutputTokens: &maxOutput,
		Enabled:         true,
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/models", mustJSONBody(t, models))
	apiSrv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/models status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	configRec := httptest.NewRecorder()
	apiSrv.engine.ServeHTTP(configRec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got config.AppConfig
	if err := json.Unmarshal(configRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if len(got.Models) != 1 || got.Models[0].MaxOutputTokens == nil || *got.Models[0].MaxOutputTokens != contextLength {
		t.Fatalf("saved limits = %#v, want max_output_tokens=%d", got.Models, contextLength)
	}
}

func TestModelMetadataEndpointReturnsCatalogDefaults(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/models/metadata?name=deepseek/deepseek-v4-flash&provider_id=deepseek", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models/metadata status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"found":true`)) {
		t.Fatalf("metadata should be found: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"context_length":`)) {
		t.Fatalf("metadata should include context_length: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"max_output_tokens":`)) {
		t.Fatalf("metadata should include max_output_tokens: %s", rec.Body.String())
	}
}

func TestModelMetadataEndpointReturnsAPISwitchMetadataSeed(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/models/metadata?name=z-ai/glm-5.2&provider_id=useai", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models/metadata status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"found":true`)) {
		t.Fatalf("metadata should be found: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"model":"z-ai/glm-5.2"`)) {
		t.Fatalf("metadata should use z-ai alias: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"context_length":1000000`)) {
		t.Fatalf("metadata should include context_length: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"max_output_tokens":131072`)) {
		t.Fatalf("metadata should include max_output_tokens: %s", rec.Body.String())
	}
}

func TestModelSearchReturnsMetadataCatalogMatches(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/models/search?q=glm-5.2", nil)
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models/search status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"model":"z-ai/glm-5.2"`)) {
		t.Fatalf("search response should include z-ai/glm-5.2: %s", rec.Body.String())
	}
}

func TestModelSaveRejectsInvalidProviderID(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	models := []config.ModelConfig{{
		Name:       "z-ai/glm-5.2",
		ProviderID: "z-ai/glm-5.2",
		Provider:   "z-ai/glm-5.2",
		Enabled:    true,
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/models", mustJSONBody(t, models))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT /api/models status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"model_provider_not_found"`)) {
		t.Fatalf("response should include model_provider_not_found: %s", rec.Body.String())
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
	if !bytes.Contains(chatRec.Body.Bytes(), []byte(`"provider_id":"openai"`)) || !bytes.Contains(chatRec.Body.Bytes(), []byte(`"model":"model-a"`)) {
		t.Fatalf("test chat should include request context: %s", chatRec.Body.String())
	}
}

func TestManagementTestEndpointsUseZhipuVersionedAPIBaseURL(t *testing.T) {
	seen := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path]++
		switch r.URL.Path {
		case "/api/paas/v4/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-4.7"}]}`))
		case "/api/paas/v4/chat/completions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode chat request: %v", err)
			}
			if body["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"non-stream unavailable"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	apiSrv, _ := newAPITestHarness(t)
	payload := config.ProviderConfig{
		ID:      "zhipu",
		Name:    "zhipu",
		Type:    "openai",
		APIKey:  "test-key",
		BaseURL: upstream.URL + "/api/paas/v4/",
		Enabled: true,
	}

	connRec := httptest.NewRecorder()
	connReq := httptest.NewRequest(http.MethodPost, "/api/test/connection", mustJSONBody(t, map[string]any{
		"provider": payload,
	}))
	apiSrv.engine.ServeHTTP(connRec, connReq)
	if connRec.Code != http.StatusOK || !bytes.Contains(connRec.Body.Bytes(), []byte(`"success":true`)) {
		t.Fatalf("zhipu connection test failed: status=%d body=%s", connRec.Code, connRec.Body.String())
	}

	chatRec := httptest.NewRecorder()
	chatReq := httptest.NewRequest(http.MethodPost, "/api/test/chat", mustJSONBody(t, map[string]any{
		"provider": payload,
		"message":  "hello",
		"model":    "glm-4.7",
	}))
	apiSrv.engine.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK || !bytes.Contains(chatRec.Body.Bytes(), []byte(`"success":true`)) {
		t.Fatalf("zhipu chat test failed: status=%d body=%s", chatRec.Code, chatRec.Body.String())
	}
	if !bytes.Contains(chatRec.Body.Bytes(), []byte(`"fallback_mode":"stream"`)) || !bytes.Contains(chatRec.Body.Bytes(), []byte(`"content":"ok"`)) {
		t.Fatalf("zhipu stream fallback details missing: %s", chatRec.Body.String())
	}
	if seen["/api/paas/v4/models"] != 1 || seen["/api/paas/v4/chat/completions"] != 2 {
		t.Fatalf("unexpected zhipu endpoint calls: %#v", seen)
	}
	if seen["/api/paas/v4/v1/models"] != 0 || seen["/api/paas/v4/v1/chat/completions"] != 0 {
		t.Fatalf("v1 must not be inserted after the zhipu v4 API root: %#v", seen)
	}
}

func TestManagementTestChatHandlesEmptyChoices(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"chatcmpl-empty","object":"chat.completion","created":1,"model":"model-a","choices":[]}`))
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

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/test/chat", mustJSONBody(t, map[string]any{
		"provider": payload,
		"message":  "hello",
		"model":    "model-a",
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"success":false`)) {
		t.Fatalf("empty choices should be a failed test result, got: %s", rec.Body.String())
	}
}

func TestManagementTestChatRejectsEmptyModel(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	apiSrv, _ := newAPITestHarness(t)
	providerCfg := config.ProviderConfig{ID: "zhipu", Name: "zhipu", Type: "openai", BaseURL: upstream.URL, Enabled: true}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/test/chat", mustJSONBody(t, map[string]any{
		"provider": providerCfg,
		"message":  "hello",
		"model":    "",
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"success":false`)) {
		t.Fatalf("empty model should fail: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestManagementTestChatRejectsModelBoundToDifferentProvider(t *testing.T) {
	chatCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.2"}]}`))
		case "/v1/chat/completions":
			chatCalls++
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	apiSrv, _ := newAPITestHarness(t)
	cfg := apiSrv.configMgr.Get()
	zhipuProvider := config.ProviderConfig{ID: "zhipu", Name: "zhipu", Type: "openai", BaseURL: upstream.URL, Enabled: true}
	cfg.Providers = []config.ProviderConfig{zhipuProvider}
	cfg.Models = []config.ModelConfig{{Name: "z-ai/glm-5.2", ProviderID: "usecpa", Provider: "usecpa", Enabled: true}}
	if err := apiSrv.configMgr.Save(cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/test/chat", mustJSONBody(t, map[string]any{
		"provider": zhipuProvider,
		"message":  "hello",
		"model":    "z-ai/glm-5.2",
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"success":false`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`未绑定到当前提供商`)) {
		t.Fatalf("wrong provider model should fail with friendly error: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"provider_id":"zhipu"`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`"model":"z-ai/glm-5.2"`)) {
		t.Fatalf("wrong provider response should include request context: %s", rec.Body.String())
	}
	if chatCalls != 0 {
		t.Fatalf("chat calls = %d, want 0", chatCalls)
	}
}

func TestManagementTestChatAllowsLiveProviderModelDespiteStaleBinding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-v4-flash"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	apiSrv, _ := newAPITestHarness(t)
	usecpaProvider := config.ProviderConfig{ID: "usecpa", Name: "UseCpa", Type: "openai", BaseURL: upstream.URL, Enabled: true}
	cfg := apiSrv.configMgr.Get()
	cfg.Providers = []config.ProviderConfig{usecpaProvider}
	cfg.Models = []config.ModelConfig{{Name: "deepseek-v4-flash", ProviderID: "deepseek", Provider: "deepseek", Enabled: true}}
	if err := apiSrv.configMgr.Save(cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/test/chat", mustJSONBody(t, map[string]any{
		"provider": usecpaProvider,
		"message":  "hello",
		"model":    "deepseek-v4-flash",
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"success":true`)) {
		t.Fatalf("live provider model should not be blocked by stale config binding: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestManagementTestChatFallsBackToStreamWhenNonStreamFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.5"}]}`))
		case "/v1/chat/completions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode chat request: %v", err)
			}
			if body["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(strings.Join([]string{
					`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
					`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
					`data: {"choices":[{"delta":{"content":"!"},"finish_reason":null}]}`,
					`data: [DONE]`,
					``,
				}, "\n")))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream error: do request failed","code":"do_request_failed"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	apiSrv, _ := newAPITestHarness(t)
	providerCfg := config.ProviderConfig{ID: "useai", Name: "UseAI", Type: "openai", BaseURL: upstream.URL + "/v1", Enabled: true}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/test/chat", mustJSONBody(t, map[string]any{
		"provider": providerCfg,
		"message":  "hi",
		"model":    "gpt-5.5",
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"success":true`)) {
		t.Fatalf("stream fallback should succeed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"fallback_mode":"stream"`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`"content":"Hello!"`)) {
		t.Fatalf("stream fallback details missing: %s", rec.Body.String())
	}
}

func newAPITestHarness(t *testing.T) (*Server, *proxy.Server) {
	return newAPITestHarnessWithStaticFSAndStoreMax(t, nil, 50)
}

func newAPITestHarnessWithStaticFS(t *testing.T, staticFS fs.FS) (*Server, *proxy.Server) {
	return newAPITestHarnessWithStaticFSAndStoreMax(t, staticFS, 50)
}

func newAPITestHarnessWithStoreMax(t *testing.T, maxLogs int) (*Server, *proxy.Server) {
	return newAPITestHarnessWithStaticFSAndStoreMax(t, nil, maxLogs)
}

func newAPITestHarnessWithStaticFSAndStoreMax(t *testing.T, staticFS fs.FS, maxLogs int) (*Server, *proxy.Server) {
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
	st := store.New(maxLogs)
	proxySrv := proxy.NewServer(cfg, mgr, st, logger)
	apiSrv := NewServer(cfg.Port, mgr, proxySrv, st, logger, staticFS)
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
		if config.ProviderKey(value) == name || value.Name == name {
			return value, true
		}
	}
	return config.ProviderConfig{}, false
}
