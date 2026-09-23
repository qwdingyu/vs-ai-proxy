package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
)

// -----------------------------------------------------------------------------
// 管理页「测试对话」必须走真实参数处理链路
//
// 回归背景：testChat 过去直接构造裸 ChatRequest，绕过 transformRequest /
// applyExecutionDefaults / applyProfileDefaults。于是"管理页能测、VS Copilot 不能用"
// 成为长期假阳性（2026-09 max_tokens 事故即被其掩盖）。
//
// 本测试用真实上游抓取管理页实际发出的请求体，断言链路确实生效：
//   · 改动前：请求体里根本没有 max_tokens（裸请求）；
//   · 改动后：普通聊天应带上 applyGlobalDefaults 的保守兜底 4096，
//     且绝不能是能力上限派生的 131072。
// -----------------------------------------------------------------------------

func TestManagementTestChatAppliesRealRequestPipeline(t *testing.T) {
	var (
		mu         sync.Mutex
		chatBodies []string
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"my-custom-model"}]}`))
		default:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, _ := json.Marshal(body)
			mu.Lock()
			chatBodies = append(chatBodies, string(raw))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		}
	}))
	defer upstream.Close()

	apiSrv, _ := newAPITestHarness(t)

	// 保存 provider 与模型配置（复刻线上形态：能力上限很大，max_tokens 未配置）。
	providerCfg := config.ProviderConfig{
		ID: "p1", Name: "p1", Type: "openai", APIKey: "k",
		BaseURL: upstream.URL, Enabled: true,
		Transport: config.TransportConfig{ChatPath: "chat/completions", ModelsPath: "models"},
	}
	ctxLen, maxOut := 1048576, 131072
	if err := apiSrv.configMgr.Save(&config.AppConfig{
		Port: 11434, DefaultModel: "my-custom-model",
		Providers: []config.ProviderConfig{providerCfg},
		Models: []config.ModelConfig{{
			Name: "my-custom-model", ProviderID: "p1", Provider: "p1",
			ContextLength: &ctxLen, MaxOutputTokens: &maxOut, Enabled: true,
		}},
	}); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/test/chat", mustJSONBody(t, map[string]any{
		"provider": providerCfg,
		"message":  "hello",
		"model":    "my-custom-model",
	}))
	apiSrv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("测试对话失败: status=%d body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	bodies := append([]string(nil), chatBodies...)
	mu.Unlock()
	if len(bodies) == 0 {
		t.Fatalf("上游未收到对话请求；响应=%s", rec.Body.String())
	}
	body := bodies[len(bodies)-1]
	t.Logf("管理页实际发出的上游请求体: %s", body)

	var parsed struct {
		MaxTokens *int `json:"max_tokens"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("解析上游请求失败: %v", err)
	}

	// 证明链路生效：裸请求时代这里会是 nil。
	if parsed.MaxTokens == nil {
		t.Fatalf("管理页未应用真实链路兜底（max_tokens 缺失）——链路可能又被绕过: %s", body)
	}
	// 证明没有重演事故：绝不能是能力上限派生的值。
	if *parsed.MaxTokens == maxOut || *parsed.MaxTokens == ctxLen {
		t.Fatalf("max_tokens=%d 由能力上限派生，属事故回归: %s", *parsed.MaxTokens, body)
	}
	if *parsed.MaxTokens != 4096 {
		t.Fatalf("普通聊天兜底应为 4096，实际 %d", *parsed.MaxTokens)
	}
}
