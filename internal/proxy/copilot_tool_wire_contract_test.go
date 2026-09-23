package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// -----------------------------------------------------------------------------
// VS Copilot 流式工具调用线级契约
//
// Copilot 的全部价值依赖「流式 + 工具调用」，而管理页测试（testChat）只做一次
// 非流式/流式兜底的内容对话，完全不覆盖工具调用。这里用真实 OpenAIProvider +
// httptest 上游，断言 OpenAI SSE 的工具调用增量被正确转成 Ollama NDJSON。
// -----------------------------------------------------------------------------

func TestCopilotToolWireContract_OllamaStreamToolCalls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.1"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sse := strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_file","arguments":"{\"path\":"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
			``,
		}, "\n\n")
		_, _ = w.Write([]byte(sse))
	}))
	defer upstream.Close()

	prov := provider.NewOpenAIProviderWithTransport("p1", "useai", "k", upstream.URL,
		"chat/completions", "models", true, 30*time.Second)
	srv := newTestServer(prov)
	// 复刻线上形态：能力上限很大，max_tokens 缺省为空。
	ctxLen, maxOut := 1048576, 131072
	srv.config.Models = []config.ModelConfig{{
		Name: "glm-5.1", ProviderID: "p1", Provider: "p1",
		ContextLength: &ctxLen, MaxOutputTokens: &maxOut, Enabled: true,
	}}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	body := `{"model":"glm-5.1","messages":[{"role":"user","content":"read a.go"}],"stream":true,` +
		`"tools":[{"type":"function","function":{"name":"get_file","description":"d",` +
		`"parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	t.Logf("Ollama 流式响应:\n%s", out)

	// 工具调用必须完整可见且可执行。
	if !strings.Contains(out, `"tool_calls"`) {
		t.Fatalf("缺少 tool_calls —— Copilot 无法调用工具")
	}
	if !strings.Contains(out, `"get_file"`) {
		t.Fatalf("tool_calls 缺少函数名 get_file")
	}
	if !strings.Contains(out, `a.go`) {
		t.Fatalf("tool_calls 参数增量未被聚合: %s", out)
	}
	if !strings.Contains(out, `"done":true`) {
		t.Fatalf("缺少终止帧 done:true —— Copilot 会挂起")
	}
	// 分片增量不应泄漏到最终可见内容里造成重复文本。
	if strings.Contains(out, `"content":"{\"path\"`) {
		t.Fatalf("参数增量泄漏进了 content: %s", out)
	}
}
