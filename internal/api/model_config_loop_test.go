package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
)

// ---------------------------------------------------------------------------
// Web 模型表单 → 磁盘 → /api/tags 的完整闭环
//
// 用户在管理页「模型精调」填写的上下文长度与最大输出，必须：
//   1. PUT /admin/api/models 接受并持久化到 config.json
//   2. GET /admin/api/models 原样读回（编辑时能回填）
//   3. 流入代理的 /api/tags 与 /api/show，被 VS Copilot BYOK 发现
//
// 这三段任一断裂，用户都会看到"填了但没生效"。
// ---------------------------------------------------------------------------

// 保存后磁盘上必须真实存在这两个字段（而不是只在内存里）。
func TestModelConfigLoop_PersistsContextAndOutputToDisk(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	// 先建立 provider，否则 models[].provider_id 校验会（正确地）拒绝。
	if err := apiSrv.configMgr.Save(&config.AppConfig{
		Port:         11434,
		DefaultModel: "my-model",
		Providers: []config.ProviderConfig{{
			ID: "p1", Name: "p1", Type: "openai", APIKey: "k",
			BaseURL: "https://example.invalid/v1", Enabled: true,
			Transport: config.TransportConfig{ChatPath: "chat/completions", ModelsPath: "models"},
		}},
		Models: []config.ModelConfig{},
	}); err != nil {
		t.Fatalf("准备 provider 失败: %v", err)
	}

	payload := []map[string]any{{
		"name":              "my-model",
		"provider_id":       "p1",
		"provider":          "p1",
		"context_length":    200000,
		"max_output_tokens": 8192,
		"enabled":           true,
	}}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/models", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	apiSrv.engine.ServeHTTP(rec, req)

	// provider p1 不存在，校验可能拒绝；若拒绝则跳过持久化断言但仍需检查错误可读性
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/models 失败: %d %s", rec.Code, rec.Body.String())
	}

	// 1) 磁盘持久化
	raw, err := os.ReadFile(apiSrv.configMgr.ConfigPath())
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	var disk config.AppConfig
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("解析磁盘配置失败: %v", err)
	}
	var found *config.ModelConfig
	for i := range disk.Models {
		if disk.Models[i].Name == "my-model" {
			found = &disk.Models[i]
		}
	}
	if found == nil {
		t.Fatalf("磁盘上未找到保存的模型；models=%+v", disk.Models)
	}
	if found.ContextLength == nil || *found.ContextLength != 200000 {
		t.Errorf("磁盘 context_length = %v, want 200000", found.ContextLength)
	}
	if found.MaxOutputTokens == nil || *found.MaxOutputTokens != 8192 {
		t.Errorf("磁盘 max_output_tokens = %v, want 8192", found.MaxOutputTokens)
	}

	// 2) 读回原样（编辑回填）
	rec2 := httptest.NewRecorder()
	apiSrv.engine.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /api/models 失败: %d", rec2.Code)
	}
	var listed struct {
		Models []config.ModelConfig `json:"models"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &listed); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	var got *config.ModelConfig
	for i := range listed.Models {
		if listed.Models[i].Name == "my-model" {
			got = &listed.Models[i]
		}
	}
	if got == nil {
		t.Fatal("GET /api/models 未返回保存的模型")
	}
	if got.ContextLength == nil || *got.ContextLength != 200000 {
		t.Errorf("读回 context_length = %v, want 200000（编辑回填会丢值）", got.ContextLength)
	}
	if got.MaxOutputTokens == nil || *got.MaxOutputTokens != 8192 {
		t.Errorf("读回 max_output_tokens = %v, want 8192", got.MaxOutputTokens)
	}
}

// 保存接口必须原样接受这两个字段，不得静默丢弃。
func TestModelConfigLoop_SaveAcceptsBothFields(t *testing.T) {
	apiSrv, _ := newAPITestHarness(t)

	payload := []map[string]any{{
		"name": "m1", "context_length": 123456, "max_output_tokens": 4321, "enabled": true,
	}}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/models", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	cfg := apiSrv.configMgr.Get()
	if len(cfg.Models) == 0 {
		t.Fatal("保存后模型列表为空")
	}
	m := cfg.Models[0]
	if m.ContextLength == nil || *m.ContextLength != 123456 {
		t.Errorf("context_length = %v, want 123456（被静默丢弃）", m.ContextLength)
	}
	if m.MaxOutputTokens == nil || *m.MaxOutputTokens != 4321 {
		t.Errorf("max_output_tokens = %v, want 4321（被静默丢弃）", m.MaxOutputTokens)
	}
}
