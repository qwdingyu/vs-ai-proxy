package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// -----------------------------------------------------------------------------
// 输出上限语义：context_length / max_output_tokens 只「钳制」，绝不「生成」max_tokens
//
// 回归背景（真实线上配置 73 个模型中 51 个命中）：
//   用户通过 Web 表单为模型填写 context_length 与 max_output_tokens 后，这两个字段
//   是「能力声明」而非「本次想生成的 token 数」。旧实现把它们当成 max_tokens 的来源：
//     · 只填 context_length        → 生成 max_tokens = context_length（如 200000）
//     · 只填 max_output_tokens      → 生成 max_tokens = max_output_tokens（如 131072）
//   VS Copilot 的请求总带 tools，会跳过 applyGlobalDefaults，于是 max_tokens 保持为空，
//   上述生成逻辑必然触发 → 上游判定超限并拒绝 → 「管理页能测、Copilot 不能用」。
//
// 正确语义：两者都是上限；客户端未声明 max_tokens 时不得下发该字段。
// 真正代表「缺省生成长度」的是 profile.MaxTokens，由上方 override 策略单独应用。
// -----------------------------------------------------------------------------

// 只配置 context_length、客户端未声明 max_tokens：不得生成 max_tokens。
func TestApplyProfileDefaultsContextLengthAloneDoesNotInventMaxTokens(t *testing.T) {
	server := &Server{}
	contextLength := 200000
	req := &provider.ChatRequest{Model: "my-custom-model"}

	server.applyProfileDefaults(req, provider.ModelProfile{
		ContextLength: &contextLength,
	}, &stubProvider{name: "openai"})

	if req.MaxTokens != nil {
		t.Fatalf("只配置 context_length 时不得生成 max_tokens，实际 = %d", *req.MaxTokens)
	}
	// 上下文窗口本身仍必须下发（Ollama 走 num_ctx）。
	if req.ContextLength == nil || *req.ContextLength != contextLength {
		t.Fatalf("context_length = %v, 期望 %d", req.ContextLength, contextLength)
	}
}

// 只配置 max_output_tokens、客户端未声明 max_tokens：同样不得生成 max_tokens。
func TestApplyProfileDefaultsMaxOutputTokensAloneDoesNotInventMaxTokens(t *testing.T) {
	server := &Server{}
	maxOutput := 131072
	req := &provider.ChatRequest{Model: "glm-5.1"}

	server.applyProfileDefaults(req, provider.ModelProfile{
		MaxOutputTokens: &maxOutput,
	}, &stubProvider{name: "openai"})

	if req.MaxTokens != nil {
		t.Fatalf("只配置 max_output_tokens 时不得生成 max_tokens，实际 = %d", *req.MaxTokens)
	}
}

// 两者同时配置且都很大时，仍不得生成 max_tokens。
func TestApplyProfileDefaultsBothCapsDoNotInventMaxTokens(t *testing.T) {
	server := &Server{}
	contextLength := 1000000
	maxOutput := 384000
	req := &provider.ChatRequest{Model: "deepseek/deepseek-v4-pro"}

	server.applyProfileDefaults(req, provider.ModelProfile{
		ContextLength:   &contextLength,
		MaxOutputTokens: &maxOutput,
	}, &stubProvider{name: "openai"})

	if req.MaxTokens != nil {
		t.Fatalf("不得由能力上限生成 max_tokens，实际 = %d", *req.MaxTokens)
	}
}

// profile.MaxTokens 才是「缺省生成长度」，必须仍然生效（不要过度删除）。
func TestApplyProfileDefaultsStillAppliesProfileMaxTokensDefault(t *testing.T) {
	server := &Server{}
	profileMaxTokens := 8192
	contextLength := 1048576
	maxOutput := 384000
	req := &provider.ChatRequest{Model: "deepseek-v4-pro"}

	server.applyProfileDefaults(req, provider.ModelProfile{
		MaxTokens:       &profileMaxTokens,
		ContextLength:   &contextLength,
		MaxOutputTokens: &maxOutput,
	}, &stubProvider{name: "openai"})

	if req.MaxTokens == nil || *req.MaxTokens != profileMaxTokens {
		t.Fatalf("max_tokens = %v, 期望目录推荐缺省 %d", req.MaxTokens, profileMaxTokens)
	}
}

// 客户端显式声明的 max_tokens 超过上限时必须向下钳制。
func TestApplyProfileDefaultsClampsClientMaxTokensToContext(t *testing.T) {
	server := &Server{}
	contextLength := 2048
	clientMax := 100000
	req := &provider.ChatRequest{Model: "my-custom-model", MaxTokens: &clientMax}

	server.applyProfileDefaults(req, provider.ModelProfile{
		ContextLength: &contextLength,
	}, &stubProvider{name: "openai"})

	if req.MaxTokens == nil || *req.MaxTokens != contextLength {
		t.Fatalf("max_tokens = %v, 期望钳制为 %d", req.MaxTokens, contextLength)
	}
}

// 客户端声明的 max_tokens 未超上限时不得被改动（保持客户端意图）。
func TestApplyProfileDefaultsKeepsClientMaxTokensBelowCap(t *testing.T) {
	server := &Server{}
	contextLength := 200000
	clientMax := 4096
	req := &provider.ChatRequest{Model: "my-custom-model", MaxTokens: &clientMax}

	server.applyProfileDefaults(req, provider.ModelProfile{
		ContextLength: &contextLength,
	}, &stubProvider{name: "openai"})

	if req.MaxTokens == nil || *req.MaxTokens != clientMax {
		t.Fatalf("max_tokens = %v, 期望保持客户端值 %d", req.MaxTokens, clientMax)
	}
}

// -----------------------------------------------------------------------------
// 端到端：走真实 /v1/chat/completions 路由，覆盖 Copilot 的带 tools 请求。
// -----------------------------------------------------------------------------

// 复刻线上真实配置形态：模型已填 context_length 与大额 max_output_tokens，
// 且目录未提供 max_tokens 缺省（如 glm-5.1：MaxTokens=nil, MaxOutputTokens=131072）。
func TestE2ECopilotToolsRequestDoesNotInventMaxTokensFromCaps(t *testing.T) {
	cases := []struct {
		name        string
		contextLen  *int
		maxOutput   *int
		clientMax   string
		wantMax     int
		wantPresent bool
	}{
		{
			// 关键回归：只有上下文长度。
			name: "context_only_without_client_max", contextLen: intPtr(1000000),
			clientMax: "", wantPresent: false,
		},
		{
			// 关键回归：只有大额输出上限（线上 51/73 模型的实际形态）。
			name: "large_max_output_without_client_max", maxOutput: intPtr(131072),
			clientMax: "", wantPresent: false,
		},
		{
			// 关键回归：两者都很大，且没有目录 max_tokens 兜底。
			name: "both_caps_without_client_max", contextLen: intPtr(1000000), maxOutput: intPtr(384000),
			clientMax: "", wantPresent: false,
		},
		{
			// 客户端声明后必须原样保留。
			name: "caps_with_client_max", contextLen: intPtr(1000000), maxOutput: intPtr(384000),
			clientMax: `,"max_tokens":4096`, wantMax: 4096, wantPresent: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{
				name:     "p1",
				enabled:  true,
				models:   []string{"my-custom-model"},
				chatResp: &fakeChatResponse{Model: "my-custom-model", Content: "ok"},
			}
			srv := newTestServer(prov)
			srv.config.Models = []config.ModelConfig{{
				Name: "my-custom-model", ProviderID: "p1", Provider: "p1",
				ContextLength: tc.contextLen, MaxOutputTokens: tc.maxOutput, Enabled: true,
			}}

			mux := http.NewServeMux()
			srv.RegisterRoutes(mux)

			body := `{"model":"my-custom-model","messages":[{"role":"user","content":"hi"}],"stream":false,` +
				`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{}}}}]` +
				tc.clientMax + `}`
			rec := httptest.NewRecorder()
			httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			httpReq.Header.Set("Content-Type", "application/json")
			mux.ServeHTTP(rec, httpReq)

			if prov.lastReq == nil {
				t.Fatalf("上游未被调用: status=%d body=%s", rec.Code, rec.Body.String())
			}
			raw, err := json.Marshal(prov.lastReq)
			if err != nil {
				t.Fatalf("序列化上游请求失败: %v", err)
			}
			if tc.wantPresent {
				if prov.lastReq.MaxTokens == nil || *prov.lastReq.MaxTokens != tc.wantMax {
					t.Fatalf("max_tokens = %v, 期望 %d；上游请求=%s", prov.lastReq.MaxTokens, tc.wantMax, raw)
				}
				return
			}
			if prov.lastReq.MaxTokens != nil {
				t.Fatalf("上游不得收到 max_tokens，实际 = %d；上游请求=%s", *prov.lastReq.MaxTokens, raw)
			}
			// 线级校验：MaxTokens 为 nil 时依赖 `json:"max_tokens,omitempty"`，
			// 必须确认真实序列化结果里确实没有该键，而不是仅仅指针为空。
			if strings.Contains(string(raw), `"max_tokens"`) {
				t.Fatalf("上游 JSON 不得包含 max_tokens 键: %s", raw)
			}
		})
	}
}
