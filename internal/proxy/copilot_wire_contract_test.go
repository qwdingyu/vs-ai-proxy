package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// -----------------------------------------------------------------------------
// VS Copilot 线级契约：上游 HTTP 请求体里到底有没有 max_tokens
//
// 前面所有测试都只断言 ChatRequest 结构体。这里换成真实 OpenAIProvider +
// httptest 上游，直接抓取**真实 HTTP 字节**，闭合「结构体 ≠ 线上字节」的缺口，
// 并同时覆盖 Copilot 唯一使用的两种入口与流式模式：
//   · Ollama 模式      POST /api/chat            （VS Copilot BYOM 的主路径）
//   · OpenAI 兼容模式  POST /v1/chat/completions
//
// 回归背景：context_length / max_output_tokens 曾被当作 max_tokens 的生成器，
// 而 Copilot 请求总带 tools（跳过 applyGlobalDefaults），导致上游收到
// max_tokens=131072/384000 而被拒 → 「管理页能测、Copilot 不能用」。
// -----------------------------------------------------------------------------

type wireCapture struct {
	mu     sync.Mutex
	bodies []string
}

func (c *wireCapture) add(b string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, b)
}

func (c *wireCapture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.bodies))
	copy(out, c.bodies)
	return out
}

// newCopilotWireServer 搭建「真实 OpenAIProvider → 捕获字节的 httptest 上游」。
func newCopilotWireServer(t *testing.T, ctxLen, maxOut *int) (*http.ServeMux, *wireCapture) {
	t.Helper()

	capture := &wireCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.add(string(body))

		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.1"}]}`))
			return
		}
		// 返回合法的 OpenAI SSE，保证流式链路能走完。
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"index\":0}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	prov := provider.NewOpenAIProviderWithTransport(
		"p1", "useai", "test-key", upstream.URL,
		"chat/completions", "models", true, 30*time.Second,
	)

	srv := newTestServer(prov)
	srv.config.Models = []config.ModelConfig{{
		Name: "glm-5.1", ProviderID: "p1", Provider: "p1",
		ContextLength: ctxLen, MaxOutputTokens: maxOut, Enabled: true,
	}}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux, capture
}

func TestCopilotWireContract_NoInventedMaxTokens(t *testing.T) {
	// 复刻线上真实形态：能力上限很大，且 max_tokens 缺省为空。
	ctxLen, maxOut := 1048576, 131072

	cases := []struct {
		name      string
		path      string
		withTools bool
	}{
		{name: "ollama_chat_stream_tools", path: "/api/chat", withTools: true},
		{name: "openai_chat_stream_tools", path: "/v1/chat/completions", withTools: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux, capture := newCopilotWireServer(t, &ctxLen, &maxOut)

			tools := ""
			if tc.withTools {
				tools = `,"tools":[{"type":"function","function":{"name":"f","description":"d",` +
					`"parameters":{"type":"object","properties":{}}}}]`
			}
			body := `{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}],"stream":true` + tools + `}`

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}

			bodies := capture.all()
			var upstreamChat string
			for _, b := range bodies {
				if !strings.Contains(b, `"model"`) {
					continue
				}
				upstreamChat = b
			}
			if upstreamChat == "" {
				t.Fatalf("未捕获到上游 chat 请求，捕获内容=%v", bodies)
			}
			t.Logf("上游真实 HTTP 字节: %s", upstreamChat)

			if strings.Contains(upstreamChat, `"max_tokens"`) {
				t.Fatalf("上游不得收到 max_tokens，实际字节=%s", upstreamChat)
			}
			// 上下文窗口与输出上限都不能泄漏成上游参数。
			if strings.Contains(upstreamChat, `"num_ctx"`) {
				t.Fatalf("上游不得收到 num_ctx，实际字节=%s", upstreamChat)
			}
			// 必须保留模型名与工具定义，否则 Copilot 无法工作。
			if !strings.Contains(upstreamChat, `"glm-5.1"`) {
				t.Fatalf("上游模型名丢失: %s", upstreamChat)
			}
			if tc.withTools && !strings.Contains(upstreamChat, `"tools"`) {
				t.Fatalf("上游工具定义丢失: %s", upstreamChat)
			}
		})
	}
}

// 客户端显式声明输出上限时，必须原样送达上游。
// 注意两种入口的协议形态不同，必须各自使用正确写法，否则测的是无效输入：
//
//	· Ollama /api/chat          → options.num_predict 嵌套
//	· OpenAI /v1/chat/completions → 顶层 max_tokens
func TestCopilotWireContract_ClientMaxTokensPreserved(t *testing.T) {
	ctxLen, maxOut := 1048576, 131072

	cases := []struct {
		name    string
		path    string
		options string
	}{
		{
			name:    "ollama_options_num_predict",
			path:    "/api/chat",
			options: `,"options":{"num_predict":4096}`,
		},
		{
			name:    "openai_top_level_max_tokens",
			path:    "/v1/chat/completions",
			options: `,"max_tokens":4096`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux, capture := newCopilotWireServer(t, &ctxLen, &maxOut)

			body := `{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}],"stream":true` +
				tc.options +
				`,"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{}}}}]}`
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}

			var upstreamChat string
			for _, b := range capture.all() {
				if strings.Contains(b, `"model"`) {
					upstreamChat = b
				}
			}
			if upstreamChat == "" {
				t.Fatalf("未捕获到上游 chat 请求: %v", capture.all())
			}
			t.Logf("上游真实 HTTP 字节: %s", upstreamChat)

			var parsed struct {
				MaxTokens *int `json:"max_tokens"`
			}
			if err := json.Unmarshal([]byte(upstreamChat), &parsed); err != nil {
				t.Fatalf("解析上游请求失败: %v", err)
			}
			if parsed.MaxTokens == nil || *parsed.MaxTokens != 4096 {
				t.Fatalf("max_tokens = %v, 期望客户端声明值 4096；上游字节=%s", parsed.MaxTokens, upstreamChat)
			}
		})
	}
}

// Ollama 协议一致性：生成参数必须嵌套在 options 内。
//
// 立方断言"顶层同名字段不被当作生成参数"是有意为之，用来防止后人出于"兼容性"
// 把顶层 max_output_tokens 也接进来 —— 那会重演"能力上限被当成生成长度"的故障
// （上游收到超限 max_tokens 而拒绝，VS Copilot 完全不可用）。
func TestCopilotWireContract_OllamaTopLevelGenerationParamsIgnored(t *testing.T) {
	ctxLen, maxOut := 1048576, 131072
	mux, capture := newCopilotWireServer(t, &ctxLen, &maxOut)

	// 错误的协议形态：把 OpenAI 风格字段放在 Ollama 顶层。
	body := `{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}],"stream":true,` +
		`"max_tokens":4096,"max_output_tokens":131072,` +
		`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{}}}}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)

	var upstreamChat string
	for _, b := range capture.all() {
		if strings.Contains(b, `"model"`) {
			upstreamChat = b
		}
	}
	if upstreamChat == "" {
		t.Fatalf("未捕获到上游 chat 请求: %v", capture.all())
	}
	t.Logf("上游真实 HTTP 字节: %s", upstreamChat)

	if strings.Contains(upstreamChat, `"max_tokens"`) {
		t.Fatalf("顶层 max_tokens/max_output_tokens 不得成为上游生成参数（会重演超限故障）: %s", upstreamChat)
	}
}
