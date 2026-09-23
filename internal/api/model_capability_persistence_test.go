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

// -----------------------------------------------------------------------------
// 模型能力落盘契约（VS Copilot BYOM 的元数据来源）
//
// 背景：saveAndApplyConfig 会在保存前调用 enrichModelDefaults 补齐能力值并做安全检查。
// 这是有意设计（f318102 "Preserve authoritative model"），但此前没有任何测试锁定：
//   · 凭空删掉补齐逻辑不会有测试失败；
//   · f318102 专门加入的 max_output_tokens <= context_length 钳制同样零覆盖。
// 本文件把这两条都锁住，避免后续被"优化掉"而悄悄破坏 Copilot 元数据。
// -----------------------------------------------------------------------------

// 保存时会从 catalog 补齐缺失的能力值，并写盘。
func TestModelCapabilityPersistence_FillsFromCatalog(t *testing.T) {
	svc, _ := newAPITestHarness(t)
	seedProvider(t, svc, "p1")

	// 只填名字，能力字段留空；deepseek-v4-pro 在目录中存在。
	putModels(t, svc, `[{"name":"deepseek-v4-pro","provider_id":"p1","provider":"p1","enabled":true}]`)

	m := readDiskModel(t, svc, "deepseek-v4-pro")
	if m.ContextLength == nil || *m.ContextLength <= 0 {
		t.Fatalf("保存后 context_length 应被 catalog 补齐，实际 = %v", m.ContextLength)
	}
	if m.MaxOutputTokens == nil || *m.MaxOutputTokens <= 0 {
		t.Fatalf("保存后 max_output_tokens 应被 catalog 补齐，实际 = %v", m.MaxOutputTokens)
	}
	t.Logf("补齐结果: context_length=%d max_output_tokens=%d", *m.ContextLength, *m.MaxOutputTokens)
}

// f318102 不变式：max_output_tokens 绝不能大于 context_length。
func TestModelCapabilityPersistence_OutputClampedToContext(t *testing.T) {
	svc, _ := newAPITestHarness(t)
	seedProvider(t, svc, "p1")

	// 刻意提交一个自相矛盾的组合：输出上限 500000 > 上下文 8192。
	putModels(t, svc, `[{"name":"my-custom-model","provider_id":"p1","provider":"p1",`+
		`"context_length":8192,"max_output_tokens":500000,"enabled":true}]`)

	m := readDiskModel(t, svc, "my-custom-model")
	if m.ContextLength == nil || m.MaxOutputTokens == nil {
		t.Fatal("能力字段不应为空")
	}
	if *m.MaxOutputTokens > *m.ContextLength {
		t.Fatalf("不变式被破坏: max_output_tokens=%d > context_length=%d",
			*m.MaxOutputTokens, *m.ContextLength)
	}
	if *m.MaxOutputTokens != *m.ContextLength {
		t.Fatalf("应被钳制到 context_length=%d，实际 %d", *m.ContextLength, *m.MaxOutputTokens)
	}
}

// 用户显式填写的合法值必须原样保留（补缺省不得覆盖用户意图）。
func TestModelCapabilityPersistence_PreservesExplicitValues(t *testing.T) {
	svc, _ := newAPITestHarness(t)
	seedProvider(t, svc, "p1")

	putModels(t, svc, `[{"name":"my-custom-model","provider_id":"p1","provider":"p1",`+
		`"context_length":200000,"max_output_tokens":8192,"enabled":true}]`)

	m := readDiskModel(t, svc, "my-custom-model")
	if *m.ContextLength != 200000 || *m.MaxOutputTokens != 8192 {
		t.Fatalf("用户显式值被改动: ctx=%d maxOut=%d", *m.ContextLength, *m.MaxOutputTokens)
	}
}

// 空值必须被安全兜底填满，不能留 nil（否则下游会拿到 0）。
func TestModelCapabilityPersistence_EmptyValuesGetSafeFallback(t *testing.T) {
	svc, _ := newAPITestHarness(t)
	seedProvider(t, svc, "p1")

	// 前端对空输入框会发送 0，这里显式模拟该形态。
	putModels(t, svc, `[{"name":"totally-unknown-model","provider_id":"p1","provider":"p1",`+
		`"context_length":0,"max_output_tokens":0,"enabled":true}]`)

	m := readDiskModel(t, svc, "totally-unknown-model")
	if m.ContextLength == nil || *m.ContextLength <= 0 {
		t.Fatalf("context_length 必须被安全兜底，实际 = %v", m.ContextLength)
	}
	if m.MaxOutputTokens == nil || *m.MaxOutputTokens <= 0 {
		t.Fatalf("max_output_tokens 必须被安全兜底，实际 = %v", m.MaxOutputTokens)
	}
}

// -----------------------------------------------------------------------------
// 测试辅助
// -----------------------------------------------------------------------------

func seedProvider(t *testing.T, svc *Server, id string) {
	t.Helper()
	if err := svc.configMgr.Save(&config.AppConfig{
		Port:         11434,
		DefaultModel: "default-model",
		Providers: []config.ProviderConfig{{
			ID: id, Name: id, Type: "openai", APIKey: "k",
			BaseURL: "https://example.invalid/v1", Enabled: true,
			Transport: config.TransportConfig{ChatPath: "chat/completions", ModelsPath: "models"},
		}},
		Models: []config.ModelConfig{},
	}); err != nil {
		t.Fatalf("准备 provider 失败: %v", err)
	}
}

func putModels(t *testing.T, svc *Server, payload string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/models", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	svc.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/models 失败: %d %s", rec.Code, rec.Body.String())
	}
}

func readDiskModel(t *testing.T, svc *Server, name string) config.ModelConfig {
	t.Helper()
	raw, err := os.ReadFile(svc.configMgr.ConfigPath())
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	var disk config.AppConfig
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	for _, m := range disk.Models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("磁盘上未找到模型 %q", name)
	return config.ModelConfig{}
}
