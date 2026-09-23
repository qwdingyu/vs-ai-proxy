package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// -----------------------------------------------------------------------------
// 管理页测试链路 ≡ 真实代理链路
//
// 2026-09 的 max_tokens 事故之所以拖了很久，是因为管理页 testChat 直接构造裸
// ChatRequest，绕过了 transformRequest / applyExecutionDefaults / applyProfileDefaults，
// 于是"管理页能测、VS Copilot 不能用"成为长期假阳性。
//
// 本测试断言两条链路对同一份配置产出**完全一致**的参数。任何一方将来改动而
// 另一方没跟上，这里就会失败——这是防止再次分叉的机制保障。
// -----------------------------------------------------------------------------

type paramSnapshot struct {
	Model           string
	MaxTokens       *int
	Temperature     *float64
	TopP            *float64
	TopK            *int
	ReasoningEffort string
	Stream          bool
	MessageCount    int
}

func snapshotParams(req *provider.ChatRequest) paramSnapshot {
	s := paramSnapshot{
		Model:           req.Model,
		MaxTokens:       req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		TopK:            req.TopK,
		ReasoningEffort: req.ReasoningEffort,
		Stream:          req.Stream,
		MessageCount:    len(req.Messages),
	}
	return s
}

func derefInt(v *int) string {
	if v == nil {
		return "nil"
	}
	return string(rune('0'+*v%10)) + "…"
}

func sameParams(a, b paramSnapshot) bool {
	eqInt := func(x, y *int) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return *x == *y
	}
	eqF := func(x, y *float64) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return *x == *y
	}
	return a.Model == b.Model &&
		eqInt(a.MaxTokens, b.MaxTokens) &&
		eqF(a.Temperature, b.Temperature) &&
		eqF(a.TopP, b.TopP) &&
		eqInt(a.TopK, b.TopK) &&
		a.ReasoningEffort == b.ReasoningEffort &&
		a.Stream == b.Stream &&
		a.MessageCount == b.MessageCount
}

func TestManagementTestPathMatchesProxyPath(t *testing.T) {
	cases := []struct {
		name string
		// 线上真实形态：能力上限很大，且未配置 max_tokens。
		ctxLen   *int
		maxOut   *int
		modelCfg string
	}{
		{
			name:   "custom_model_with_large_caps",
			ctxLen: intPtr(1048576), maxOut: intPtr(131072),
			modelCfg: "my-custom-model",
		},
		{
			name:     "custom_model_context_only",
			ctxLen:   intPtr(200000),
			modelCfg: "my-custom-model",
		},
		{
			// 目录里有合法 max_tokens 推荐值的模型：两条链路都应应用该缺省。
			name:     "catalog_model_deepseek_v4_pro",
			modelCfg: "deepseek-v4-pro",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{
				name: "p1", enabled: true, models: []string{tc.modelCfg},
				chatResp: &fakeChatResponse{Model: tc.modelCfg, Content: "ok"},
			}
			srv := newTestServer(prov)
			srv.config.Models = []config.ModelConfig{{
				Name: tc.modelCfg, ProviderID: "p1", Provider: "p1",
				ContextLength: tc.ctxLen, MaxOutputTokens: tc.maxOut, Enabled: true,
			}}

			// --- 链路 A：真实代理 /api/chat ---
			mux := http.NewServeMux()
			srv.RegisterRoutes(mux)
			body := `{"model":"` + tc.modelCfg + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
			rec := httptest.NewRecorder()
			httpReq := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
			httpReq.Header.Set("Content-Type", "application/json")
			mux.ServeHTTP(rec, httpReq)
			if prov.lastReq == nil {
				t.Fatalf("真实链路未调用上游: status=%d body=%s", rec.Code, rec.Body.String())
			}
			realParams := snapshotParams(prov.lastReq)

			// --- 链路 B：管理页准备函数 ---
			adminReq := &provider.ChatRequest{
				Model:    tc.modelCfg,
				Messages: []provider.Message{{Role: "user", Content: "hi"}},
				Stream:   false,
			}
			srv.PrepareManagementTestRequest(srv.config, adminReq, tc.modelCfg, prov)
			adminParams := snapshotParams(adminReq)

			t.Logf("真实链路 : max_tokens=%s temp=%v top_p=%v top_k=%v reasoning=%q",
				derefInt(realParams.MaxTokens), realParams.Temperature, realParams.TopP, realParams.TopK, realParams.ReasoningEffort)
			t.Logf("管理页   : max_tokens=%s temp=%v top_p=%v top_k=%v reasoning=%q",
				derefInt(adminParams.MaxTokens), adminParams.Temperature, adminParams.TopP, adminParams.TopK, adminParams.ReasoningEffort)

			if !sameParams(realParams, adminParams) {
				t.Fatalf("管理页链路与真实链路参数不一致（假阳性复发）:\n 真实=%+v\n 管理页=%+v",
					realParams, adminParams)
			}
		})
	}
}

// 普通聊天（未声明 tools）走 applyGlobalDefaults 的保守兜底 max_tokens=4096，
// 这是有意行为；关键是它绝不能是"能力上限"派生的 131072。
func TestManagementTestPathPlainChatUsesGlobalDefault(t *testing.T) {
	ctxLen, maxOut := 1048576, 131072
	prov := &fakeProvider{name: "p1", enabled: true, models: []string{"my-custom-model"},
		chatResp: &fakeChatResponse{Model: "my-custom-model", Content: "ok"}}
	srv := newTestServer(prov)
	srv.config.Models = []config.ModelConfig{{
		Name: "my-custom-model", ProviderID: "p1", Provider: "p1",
		ContextLength: &ctxLen, MaxOutputTokens: &maxOut, Enabled: true,
	}}

	req := &provider.ChatRequest{
		Model:    "my-custom-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Stream:   false,
	}
	srv.PrepareManagementTestRequest(srv.config, req, "my-custom-model", prov)

	if req.MaxTokens == nil {
		t.Fatalf("普通聊天应有保守兜底 max_tokens")
	}
	if *req.MaxTokens == maxOut || *req.MaxTokens == ctxLen {
		t.Fatalf("max_tokens=%d 由能力上限派生，属回归", *req.MaxTokens)
	}
	if *req.MaxTokens != 4096 {
		t.Fatalf("普通聊天兜底应为 4096，实际 %d", *req.MaxTokens)
	}
}

// 声明 tools 时（VS Copilot 的真实形态）跳过全局兜底，两条链路都不得生成 max_tokens。
func TestManagementTestPathWithToolsMatchesProxyPath(t *testing.T) {
	ctxLen, maxOut := 1048576, 131072
	prov := &fakeProvider{name: "p1", enabled: true, models: []string{"my-custom-model"},
		chatResp: &fakeChatResponse{Model: "my-custom-model", Content: "ok"}}
	srv := newTestServer(prov)
	srv.config.Models = []config.ModelConfig{{
		Name: "my-custom-model", ProviderID: "p1", Provider: "p1",
		ContextLength: &ctxLen, MaxOutputTokens: &maxOut, Enabled: true,
	}}

	tools := []provider.Tool{{
		Type:     "function",
		Function: provider.ToolFunc{Name: "f", Description: "d"},
	}}

	// --- 真实链路 ---
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	body := `{"model":"my-custom-model","messages":[{"role":"user","content":"hi"}],"stream":false,` +
		`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{}}}}]}`
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, httpReq)
	if prov.lastReq == nil {
		t.Fatalf("真实链路未调用上游: status=%d body=%s", rec.Code, rec.Body.String())
	}
	realParams := snapshotParams(prov.lastReq)

	// --- 管理页链路（同样声明 tools）---
	adminReq := &provider.ChatRequest{
		Model:    "my-custom-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Stream:   false,
		Tools:    tools,
	}
	srv.PrepareManagementTestRequest(srv.config, adminReq, "my-custom-model", prov)
	adminParams := snapshotParams(adminReq)

	t.Logf("真实链路(tools): max_tokens=%s", derefInt(realParams.MaxTokens))
	t.Logf("管理页(tools)  : max_tokens=%s", derefInt(adminParams.MaxTokens))

	if realParams.MaxTokens != nil {
		t.Fatalf("真实链路不得生成 max_tokens，实际 = %d", *realParams.MaxTokens)
	}
	if adminParams.MaxTokens != nil {
		t.Fatalf("管理页链路不得生成 max_tokens，实际 = %d", *adminParams.MaxTokens)
	}
	if !sameParams(realParams, adminParams) {
		t.Fatalf("tools 形态下两条链路参数不一致:\n 真实=%+v\n 管理页=%+v", realParams, adminParams)
	}
}
