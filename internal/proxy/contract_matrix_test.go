package proxy

// ---------------------------------------------------------------------------
// 协议契约矩阵（Contract Matrix）—— 发布门槛测试资产
//
// 背景：v0.2.76 的 anthropic 流式事故（docs/45、docs/46）证明，"管理测试页可用"
// 不能代表"VS Copilot 可用"：VS 固定 stream=true，而历史测试大量走非流式分支或
// 直接调用内部函数，跳过了完整代理管线。本文件把真实客户端契约固化为可自动
// 执行的矩阵，作为 make release-gate 的组成部分，任何新 provider 类型或协议
// 分支改动都必须先过这里。
//
// 矩阵维度（v1.1）：
//   上游类型   {openai, anthropic}      （ollama 上游见 integration_test.go 的专项用例）
//   请求模式   {stream, non-stream}
//   响应形态   {纯文本, 工具调用(分片/start块内联), 扩展思考(thinking_delta),
//               200 错误体, 上游截断, 上游 error 事件, 终态缺省}
//   客户端入口 {/v1/chat/completions, /v1/messages, /api/chat}
//
// v1.1 增补说明：下面三条用例对应两个**真实发生过的漏检**——
//   - thinking_delta 的实际字段名是 "thinking"，此前结构体缺失该字段，
//     矩阵与单测又都用了错误的 "text"，导致思考内容被静默丢弃；
//   - 部分网关把完整工具参数放在 content_block_start 的 input 里，
//     此前该 input 被主动清空，工具参数退化为 {}。
// 这两类缺陷在 v1.0 矩阵下全绿，因此必须显式覆盖。
//
// 关键断言设计：
//   1. 增量性：流式必须边收边吐。用通道门控（upstream 写一半后阻塞等待下游
//      确认）做确定性证明——若实现"聚合后一次性写出"，下游永远收不到首帧，
//      测试超时失败。不依赖 sleep，不在 CI 上抖动。
//   2. 终态独立性：上游以无换行的 message_stop 结尾且不关闭连接（真实网关
//      行为），下游必须能完成。同样是通道门控：上游 handler 不返回，下游
//      却必须能拿到完整响应。
//   3. 失败关闭：上游错误（非 200、error 事件、截断、200 错误体）不得伪装
//      成功下发给客户端。
// ---------------------------------------------------------------------------

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

const matrixModel = "matrix-model"

// gateOnce 是通道门控：upstream handler 写出关键帧后阻塞等待，
// 测试在收到下游对应内容后释放。t.Cleanup 兜底保证即使测试失败也释放，
// 避免 httptest.Server.Close 永久阻塞。
type gateOnce struct {
	once sync.Once
	ch   chan struct{}
}

func newGate() *gateOnce { return &gateOnce{ch: make(chan struct{})} }

func (g *gateOnce) wait() { <-g.ch }

func (g *gateOnce) release() { g.once.Do(func() { close(g.ch) }) }

// newMatrixUpstream 构造假上游。路径分发：
//   - /v1/models            → 模型发现列表（供 registry 解析候选）
//   - chatPath（/v1/chat/completions 或 /v1/messages）→ serve 决定协议响应
func newMatrixUpstream(t *testing.T, chatPath string, serve func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+strings.TrimPrefix(chatPath, "/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"` + matrixModel + `"}]}`))
			return
		}
		serve(w, r)
	}))
}

// newMatrixServer 构建挂齐三个客户端入口的完整代理 Server。
func newMatrixServer(t *testing.T, providerType, chatPath string, upstreamURL string) http.Handler {
	t.Helper()
	prov := provider.NewOpenAIProviderWithTransport(
		"matrix-"+providerType, "openai", "sk-test", upstreamURL,
		chatPath, "v1/models", true, 5*time.Second,
	)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{{
		ID: "matrix-" + providerType, Name: "matrix-" + providerType, Type: providerType,
		BaseURL: upstreamURL, APIKey: "sk-test", Enabled: true,
		Transport: config.TransportConfig{ChatPath: chatPath, ModelsPath: "v1/models"},
	}}
	return server.loggingMiddleware(withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
		mux.HandleFunc("/api/chat", server.handleOllamaChat)
	}))
}

// openStream 发起真实 HTTP 流式请求并逐行转发到 channel，EOF 后关闭 channel。
// gated 用例必须在 upstream 阻塞期间就能读到下游首帧，因此不能用
// httptest.ResponseRecorder（它会缓冲全部响应）。
func openStream(t *testing.T, ts *httptest.Server, path, body string) (*http.Response, <-chan string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	lines := make(chan string, 128)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	return resp, lines
}

// collectLines 在超时内读完 channel 直到 [DONE] 或 EOF，返回完整 body。
func collectLines(t *testing.T, lines <-chan string) string {
	t.Helper()
	var sb strings.Builder
	drainLines(t, lines, &sb)
	return sb.String()
}

// drainLines 持续读取并累积到 sb，直到 [DONE] 或 EOF。
// 注意：门控用例中门控阶段已消费的行也在同一个 sb 里，因此最终 body 完整。
func drainLines(t *testing.T, lines <-chan string, sb *strings.Builder) {
	t.Helper()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return
			}
			sb.WriteString(line)
			sb.WriteString("\n")
			if strings.TrimSpace(line) == "data: [DONE]" {
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("读取下游流超时（可能被聚合实现阻塞）: %s", sb.String())
		}
	}
}

// scanUntil 逐行读取并累积到 sb，遇到包含 want 的行返回。
// 与 drainLines 共用同一个 sb，保证最终断言看到完整 body。
func scanUntil(t *testing.T, lines <-chan string, want string, sb *strings.Builder) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("流在等待 %q 前结束，已收到:\n%s", want, sb.String())
			}
			sb.WriteString(line)
			sb.WriteString("\n")
			if strings.Contains(line, want) {
				return
			}
		case <-deadline:
			t.Fatalf("10s 内未在下游收到 %q：流式实现疑似聚合后一次性写出，违反增量契约。已收到:\n%s", want, sb.String())
		}
	}
}

// assertOpenAIStreamContract 校验 VS Copilot 依赖的 OpenAI SSE 完整契约。
func assertOpenAIStreamContract(t *testing.T, body string) {
	t.Helper()
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("下游缺少 [DONE] 终态，Visual Studio 会挂起等待:\n%s", body)
	}
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("下游缺少 OpenAI chunk 结构:\n%s", body)
	}
	for _, leaked := range []string{"content_block_delta", "message_start", "content_block_start"} {
		if strings.Contains(body, leaked) {
			t.Errorf("Anthropic SSE 事件 %q 泄漏到下游:\n%s", leaked, body)
		}
	}
}

// ---------------------------------------------------------------------------
// 单元 1：/v1/chat/completions × openai provider
// ---------------------------------------------------------------------------

// 上游写完首帧后阻塞；下游必须在此期间就能收到该帧 → 证明边收边吐。
func TestMatrix_ChatCompletions_OpenAI_Stream_TextIncremental(t *testing.T) {
	gate := newGate()
	upstream := newMatrixUpstream(t, "v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunk := func(content, finish string) string {
			return "data: {\"id\":\"chatcmpl-matrix\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"" + content + "\"},\"finish_reason\":" + finish + "}]}\n\n"
		}
		_, _ = w.Write([]byte(chunk("Hel", "null")))
		flusher.Flush()
		gate.wait()
		_, _ = w.Write([]byte(chunk("lo", "\"stop\"")))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "openai", "v1/chat/completions", upstream.URL)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, lines := openStream(t, ts, "/v1/chat/completions",
		`{"model":"`+matrixModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// 门控断言：upstream 尚未写完（阻塞在 gate），下游已收到 "Hel"。
	var sb strings.Builder
	t.Cleanup(gate.release)
	scanUntil(t, lines, "Hel", &sb)
	gate.release()

	drainLines(t, lines, &sb)
	assertOpenAIStreamContract(t, sb.String())
	if !strings.Contains(sb.String(), `"finish_reason":"stop"`) {
		t.Errorf("下游缺少 finish_reason stop:\n%s", sb.String())
	}
}

// 工具调用流式契约：分片保序透传，由客户端（VS Copilot）自行聚合——
// 这是标准 OpenAI 流式语义；代理不得吞掉或改写分片序列。
func TestMatrix_ChatCompletions_OpenAI_Stream_ToolCallSplitArguments(t *testing.T) {
	chunk := func(delta, finish string) string {
		return "data: {\"id\":\"chatcmpl-matrix\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{" + delta + "},\"finish_reason\":" + finish + "}]}\n\n"
	}
	stream := chunk(
		`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":"}}]`, "null") +
		chunk(
			`"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\",\"content\":\"x\"}"}}]`, "null") +
		chunk("", "\"tool_calls\"") +
		"data: [DONE]\n\n"
	upstream := newMatrixUpstream(t, "v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "openai", "v1/chat/completions", upstream.URL)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, lines := openStream(t, ts, "/v1/chat/completions",
		`{"model":"`+matrixModel+`","stream":true,"messages":[{"role":"user","content":"create"}],"tools":[{"type":"function","function":{"name":"create_file"}}]}`)
	body := collectLines(t, lines)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	assertOpenAIStreamContract(t, body)
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("下游缺少 finish_reason tool_calls:\n%s", body)
	}
	// 分片保序透传由三段证据共同锁定：工具 id、函数名、第二段参数内容
	// 均原样出现在下游（客户端按标准 OpenAI 语义自行聚合 arguments）。
	for _, fragment := range []string{`"id":"call_1"`, `"name":"create_file"`, `a.go`} {
		if !strings.Contains(body, fragment) {
			t.Errorf("工具调用分片特征 %q 被吞掉或改写（客户端聚合将失败）:\n%s", fragment, body)
		}
	}
}

func TestMatrix_ChatCompletions_OpenAI_NonStream_TextContract(t *testing.T) {
	upstream := newMatrixUpstream(t, "v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "openai", "v1/chat/completions", upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+matrixModel+`","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion"`) || !strings.Contains(body, `"content":"ok"`) {
		t.Errorf("非流式响应契约不完整:\n%s", body)
	}
}

// 上游 200 但 body 是 error 对象：必须失败关闭并带诊断，不得下发伪成功。
func TestMatrix_ChatCompletions_OpenAI_NonStream_Upstream200ErrorObjectFailsClosed(t *testing.T) {
	upstream := newMatrixUpstream(t, "v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":{"message":"insufficient_quota","type":"insufficient_quota"}}`))
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "openai", "v1/chat/completions", upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+matrixModel+`","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("上游 200 错误体被伪装成成功下发:\n%s", rec.Body.String())
	}
}

// 上游流在 finish/[DONE] 之前 EOF：下游不得出现 [DONE]（截断必须可识别）。
func TestMatrix_ChatCompletions_OpenAI_Stream_TruncatedUpstreamFailsClosed(t *testing.T) {
	upstream := newMatrixUpstream(t, "v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 只写一个内容 chunk 即返回（EOF），无 finish_reason、无 [DONE]。
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"))
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "openai", "v1/chat/completions", upstream.URL)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, lines := openStream(t, ts, "/v1/chat/completions",
		`{"model":"`+matrixModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	_ = resp
	body := collectLines(t, lines)
	if strings.Contains(body, "[DONE]") {
		t.Errorf("上游截断时下游不得出现 [DONE]（会把截断伪装成完整响应）:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// 单元 2：/v1/chat/completions × anthropic provider（v0.2.76 事故单元）
// ---------------------------------------------------------------------------

// 上游以无换行的 message_stop 结尾且保持连接不关闭：代理必须基于应用层
// 终态完成响应，而不是等待 EOF。门控保证确定性：upstream handler 不返回，
// 下游却必须拿到完整 OpenAI SSE。
func TestMatrix_ChatCompletions_Anthropic_Stream_TerminalWithoutNewlineOrEOF(t *testing.T) {
	gate := newGate()
	upstream := newMatrixUpstream(t, "v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request","message":"missing anthropic-version"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"" + matrixModel + "\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}") // 故意无换行
		flusher.Flush()
		gate.wait() // 不返回连接：模拟真实网关复用连接、不关 body 的行为
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "anthropic", "v1/messages", upstream.URL)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, lines := openStream(t, ts, "/v1/chat/completions",
		`{"model":"`+matrixModel+`","stream":true,"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	t.Cleanup(gate.release)
	var sb strings.Builder
	scanUntil(t, lines, "Hel", &sb)
	gate.release()

	drainLines(t, lines, &sb)
	body := sb.String()
	assertOpenAIStreamContract(t, body)
	// 文本是增量下发的（"Hel" + "lo"），不能在原始 body 里直接找 "Hello"；
	// 按 delta 累加后再比对。增量性本身由 handler_anthropic_incremental_test.go 保证。
	if got := assembleStreamContent(t, body); got != "Hello" {
		t.Errorf("下游累加文本 = %q, want %q:\n%s", got, "Hello", body)
	}
}

// assembleStreamContent 把 OpenAI SSE body 里所有 delta.content 按顺序拼接，
// 用于在"增量下发"语义下校验内容完整性。
func assembleStreamContent(t *testing.T, body string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		sb.WriteString(chunk.Choices[0].Delta.Content)
	}
	return sb.String()
}

func TestMatrix_ChatCompletions_Anthropic_Stream_ToolUseInputJSONDelta(t *testing.T) {
	var capturedHeader http.Header
	upstream := newMatrixUpstream(t, "v1/messages", func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"create_file\",\"input\":{}}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"a.go\\\"}\"}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "anthropic", "v1/messages", upstream.URL)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, lines := openStream(t, ts, "/v1/chat/completions",
		`{"model":"`+matrixModel+`","stream":true,"messages":[{"role":"user","content":"create"}],"tools":[{"type":"function","function":{"name":"create_file"}}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := collectLines(t, lines)
	assertOpenAIStreamContract(t, body)
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("Anthropic tool_use 未映射为 finish_reason tool_calls:\n%s", body)
	}
	if !strings.Contains(body, `"arguments":"{\"path\":\"a.go\"}"`) {
		t.Errorf("input_json_delta 分片未聚合为合法 JSON:\n%s", body)
	}
	if capturedHeader.Get("anthropic-version") == "" {
		t.Errorf("上游请求缺少 anthropic-version 头（OpenAI 体打到 Anthropic 端点的标志）")
	}
}

// 上游 error 事件：必须失败关闭，真实原因进入诊断，不得下发伪成功。
func TestMatrix_ChatCompletions_Anthropic_Stream_UpstreamErrorEventFailsClosed(t *testing.T) {
	upstream := newMatrixUpstream(t, "v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_3\",\"role\":\"assistant\"}}\n\n")
		write("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "anthropic", "v1/messages", upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+matrixModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("上游 error 事件被伪装成成功下发:\n%s", rec.Body.String())
	}
}

func TestMatrix_ChatCompletions_Anthropic_NonStream_TextContract(t *testing.T) {
	upstream := newMatrixUpstream(t, "v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_9","type":"message","role":"assistant","model":"` + matrixModel + `","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "anthropic", "v1/messages", upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+matrixModel+`","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"ok"`) || !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("anthropic 非流式转换契约不完整:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// 单元 3：其他客户端入口（跨客户端契约）
// ---------------------------------------------------------------------------

// Anthropic 原生客户端（Claude Code 等）走 /v1/messages：上游是 OpenAI 型，
// 下游必须收到 Anthropic event-based SSE。
func TestMatrix_MessagesClient_OpenAIProvider_StreamAnthropicSSE(t *testing.T) {
	upstream := newMatrixUpstream(t, "v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("data: {\"choices\":[{\"delta\":{\"content\":\"hi there\"},\"finish_reason\":null}]}\n\n")
		write("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		write("data: [DONE]\n\n")
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "openai", "v1/chat/completions", upstream.URL)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, lines := openStream(t, ts, "/v1/messages",
		`{"model":"`+matrixModel+`","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := collectLines(t, lines)
	for _, want := range []string{"message_start", "content_block_delta", "message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("/v1/messages 下游缺少 Anthropic 事件 %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("OpenAI chunk 结构泄漏到 /v1/messages 客户端:\n%s", body)
	}
}

// Ollama 客户端走 /api/chat：上游是 OpenAI 型，下游必须收到 NDJSON。
func TestMatrix_OllamaClient_OpenAIProvider_StreamNDJSON(t *testing.T) {
	upstream := newMatrixUpstream(t, "v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("data: {\"choices\":[{\"delta\":{\"content\":\"ollama hi\"},\"finish_reason\":null}]}\n\n")
		write("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		write("data: [DONE]\n\n")
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "openai", "v1/chat/completions", upstream.URL)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, lines := openStream(t, ts, "/api/chat",
		`{"model":"`+matrixModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := collectLines(t, lines)
	if !strings.Contains(body, `"done":true`) {
		t.Errorf("/api/chat 下游缺少 Ollama 终态 done:true:\n%s", body)
	}
	if !strings.Contains(body, "ollama hi") {
		t.Errorf("/api/chat 下游缺少内容:\n%s", body)
	}
	if strings.Contains(body, "data: ") {
		t.Errorf("/api/chat 下游不得出现 SSE data: 前缀（应为 NDJSON）:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// 以下三条是补齐的矩阵缺口：都是被测出过的真实缺陷形态，
// 原先不在矩阵里，导致"矩阵全绿"也无法拦截。
// ---------------------------------------------------------------------------

// thinking_delta 的线格式字段是 "thinking" 而不是 "text"。
// 此前 anthropicStreamDelta 缺 Thinking 字段，思考内容被静默丢弃，
// 而矩阵与旧测试都用错字段，双双漏检。
func TestMatrix_ChatCompletions_Anthropic_Stream_ThinkingDeltaContract(t *testing.T) {
	upstream := newMatrixUpstream(t, "v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_think\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
		// 真实线格式：字段名是 thinking
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"STEP-ONE\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"-STEP-TWO\"}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"ANSWER\"}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "anthropic", "v1/messages", upstream.URL)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, lines := openStream(t, ts, "/v1/chat/completions",
		`{"model":"`+matrixModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := collectLines(t, lines)
	assertOpenAIStreamContract(t, body)

	var reasoning, content string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		reasoning += chunk.Choices[0].Delta.ReasoningContent
		content += chunk.Choices[0].Delta.Content
	}
	if reasoning != "STEP-ONE-STEP-TWO" {
		t.Errorf("thinking_delta 未完整映射到 reasoning_content: got %q, want %q\n%s",
			reasoning, "STEP-ONE-STEP-TWO", body)
	}
	if content != "ANSWER" {
		t.Errorf("正文内容不正确: got %q, want %q", content, "ANSWER")
	}
}

// 部分网关把完整工具参数直接放在 content_block_start 的 input 里、
// 不发 input_json_delta。此前该 input 被主动清空，工具参数会退化成 {}，
// 工具静默失效（VS 收到调用却没有可执行参数）。
func TestMatrix_ChatCompletions_Anthropic_Stream_ToolInputInStartBlock(t *testing.T) {
	upstream := newMatrixUpstream(t, "v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_toolstart\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n")
		// 完整参数就在 start 块里，没有任何 input_json_delta
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_s\",\"name\":\"create_file\",\"input\":{\"path\":\"b.go\",\"content\":\"hi\"}}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "anthropic", "v1/messages", upstream.URL)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, lines := openStream(t, ts, "/v1/chat/completions",
		`{"model":"`+matrixModel+`","stream":true,"messages":[{"role":"user","content":"create"}],"tools":[{"type":"function","function":{"name":"create_file"}}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := collectLines(t, lines)
	assertOpenAIStreamContract(t, body)

	var args string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		for _, call := range chunk.Choices[0].Delta.ToolCalls {
			args += call.Function.Arguments
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("start 块里的工具参数丢失或非法: %q\n%s", args, body)
	}
	if parsed["path"] != "b.go" || parsed["content"] != "hi" {
		t.Errorf("start 块工具参数不完整: %v\n%s", parsed, body)
	}
}

// anthropic 上游返回 200 + 错误体：必须失败关闭，不得当成正常消息下发。
func TestMatrix_ChatCompletions_Anthropic_NonStream_Upstream200ErrorObjectFailsClosed(t *testing.T) {
	upstream := newMatrixUpstream(t, "v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "anthropic", "v1/messages", upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+matrixModel+`","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("上游 200 错误体被伪装成成功下发:\n%s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 主 OpenAI 路径的终态独立性（矩阵 v1.1 补齐的缺口）
//
// 覆盖的是最繁忙的链路：OpenAI 兼容 provider × stream。此前矩阵只测了
// anthropic 上游的终态无换行场景，主 OpenAI 路径"上游发完 [DONE] 但保持连接
// 不关闭"这一**真实网关行为**完全没有断言。
//
// 断言方式：上游写出 [DONE] 后阻塞（gate.wait），测试必须在下游看到 [DONE]
// 之后才放行。若实现依赖传输层 EOF 才结束，下游永远拿不到 [DONE]，测试超时失败。
// ---------------------------------------------------------------------------

func TestMatrix_ChatCompletions_OpenAI_Stream_TerminalWithoutUpstreamClose(t *testing.T) {
	gate := newGate()
	upstream := newMatrixUpstream(t, "v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"" + matrixModel + "\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
		write("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"" + matrixModel + "\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
		write("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"" + matrixModel + "\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		write("data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 真实网关：发完终态后复用连接、不关闭 body
		gate.wait()
	})
	defer upstream.Close()

	handler := newMatrixServer(t, "openai", "v1/chat/completions", upstream.URL)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, lines := openStream(t, ts, "/v1/chat/completions",
		`{"model":"`+matrixModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// upstream 仍在 gate 上阻塞，下游却必须先拿到 [DONE]。
	t.Cleanup(gate.release)
	var sb strings.Builder
	scanUntil(t, lines, "[DONE]", &sb)
	gate.release()
	drainLines(t, lines, &sb)

	body := sb.String()
	assertOpenAIStreamContract(t, body)
	if !strings.Contains(body, "Hello") {
		t.Errorf("下游缺少正文内容:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("下游缺少 finish_reason:\n%s", body)
	}
}
