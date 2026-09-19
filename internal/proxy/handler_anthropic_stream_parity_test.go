package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 增量流式的差分不变量（differential invariant）
//
// 增量改造最大的风险不是"完全不工作"，而是"看起来在流、但内容悄悄少了/串了"。
// 因此这里用最强的一致性断言：对**同一份逻辑消息**，
//   A) 走流式路径，把下游所有 delta 按顺序聚合；
//   B) 走非流式路径，直接得到完整响应；
// 两者必须完全等价（文本、思考、工具调用参数、finish_reason）。
//
// 该断言独立于"分片边界"，因此既能覆盖增量实现，也能覆盖将来任何重构。
// ---------------------------------------------------------------------------

// anthropicMessageFixture 是一份逻辑消息的唯一事实源，
// 可分别渲染成流式 SSE 与非流式 JSON，保证两条链路的输入语义相同。
type anthropicMessageFixture struct {
	ID           string
	Model        string
	InitialText  string // content_block_start 自带的初始文本（协议允许）
	TextChunks   []string
	ThinkingInit string
	ThinkChunks  []string
	Tools        []anthropicToolFixture
	StopReason   string
	InputTokens  int
	OutputTokens int
}

type anthropicToolFixture struct {
	Index      int
	ID         string
	Name       string
	ArgChunks  []string
	InitialArg string
}

func (f anthropicMessageFixture) stopReason() string {
	if f.StopReason == "" {
		return "end_turn"
	}
	return f.StopReason
}

// sse 渲染成 Anthropic 事件流。
func (f anthropicMessageFixture) sse() string {
	var b strings.Builder
	write := func(s string) { b.WriteString(s) }

	start, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": f.ID, "type": "message", "role": "assistant", "model": f.Model,
			"usage": map[string]any{"input_tokens": f.InputTokens, "output_tokens": 0},
		},
	})
	write("event: message_start\ndata: " + string(start) + "\n\n")

	index := 0
	if f.InitialText != "" || len(f.TextChunks) > 0 {
		block, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "text", "text": f.InitialText},
		})
		write("event: content_block_start\ndata: " + string(block) + "\n\n")
		for _, chunk := range f.TextChunks {
			delta, _ := json.Marshal(map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "text_delta", "text": chunk},
			})
			write("event: content_block_delta\ndata: " + string(delta) + "\n\n")
		}
		write(`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n")
		index++
	}

	if f.ThinkingInit != "" || len(f.ThinkChunks) > 0 {
		block, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "thinking", "thinking": f.ThinkingInit},
		})
		write("event: content_block_start\ndata: " + string(block) + "\n\n")
		for _, chunk := range f.ThinkChunks {
			delta, _ := json.Marshal(map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": chunk},
			})
			write("event: content_block_delta\ndata: " + string(delta) + "\n\n")
		}
		write(`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}` + "\n\n")
	}

	for _, tool := range f.Tools {
		initial := map[string]any{}
		if tool.InitialArg != "" {
			_ = json.Unmarshal([]byte(tool.InitialArg), &initial)
		}
		block, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": tool.Index,
			"content_block": map[string]any{
				"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": initial,
			},
		})
		write("event: content_block_start\ndata: " + string(block) + "\n\n")
		for _, chunk := range tool.ArgChunks {
			delta, _ := json.Marshal(map[string]any{
				"type": "content_block_delta", "index": tool.Index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": chunk},
			})
			write("event: content_block_delta\ndata: " + string(delta) + "\n\n")
		}
		stop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": tool.Index})
		write("event: content_block_stop\ndata: " + string(stop) + "\n\n")
	}

	msgDelta, _ := json.Marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": f.stopReason()},
		"usage": map[string]any{"output_tokens": f.OutputTokens},
	})
	write("event: message_delta\ndata: " + string(msgDelta) + "\n\n")
	write(`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n")
	return b.String()
}

// jsonResponse 渲染成非流式 Messages API 响应。
func (f anthropicMessageFixture) jsonResponse() string {
	content := []map[string]any{}
	if f.InitialText != "" || len(f.TextChunks) > 0 {
		content = append(content, map[string]any{
			"type": "text", "text": f.InitialText + strings.Join(f.TextChunks, ""),
		})
	}
	if f.ThinkingInit != "" || len(f.ThinkChunks) > 0 {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": f.ThinkingInit + strings.Join(f.ThinkChunks, ""),
		})
	}
	for _, tool := range f.Tools {
		var raw strings.Builder
		raw.WriteString(tool.InitialArg)
		for _, chunk := range tool.ArgChunks {
			raw.WriteString(chunk)
		}
		var parsed any = map[string]any{}
		if s := strings.TrimSpace(raw.String()); s != "" {
			_ = json.Unmarshal([]byte(s), &parsed)
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": parsed,
		})
	}

	body, _ := json.Marshal(map[string]any{
		"id": f.ID, "type": "message", "role": "assistant", "model": f.Model,
		"content":     content,
		"stop_reason": f.stopReason(),
		"usage":       map[string]any{"input_tokens": f.InputTokens, "output_tokens": f.OutputTokens},
	})
	return string(body)
}

// fixtureUpstream 根据请求体里的 stream 字段，分别返回 SSE 或 JSON，
// 两者来自同一个 fixture，确保语义一致。
func fixtureUpstream(t *testing.T, f anthropicMessageFixture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"` + f.Model + `"}]}`))
			return
		}
		body, _ := readAllLimited(r)
		if strings.Contains(body, `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(f.sse()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.jsonResponse()))
	}))
}

func readAllLimited(r *http.Request) (string, error) {
	buf := make([]byte, 1<<20)
	n, err := r.Body.Read(buf)
	return string(buf[:n]), err
}

// aggregatedStream 是下游 SSE 聚合后的等价表示。
type aggregatedStream struct {
	Content      string
	Reasoning    string
	Refusal      string
	FinishReason string
	ToolCalls    []struct {
		ID        string
		Name      string
		Arguments string
	}
	ChunkIDs map[string]bool
	HasDone  bool
}

// runStreamThroughProxy 走完整代理管线（stream=true）并把下游 SSE 聚合回来。
func runStreamThroughProxy(t *testing.T, upstreamURL string) aggregatedStream {
	t.Helper()
	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstreamURL))
	if rec.Code != http.StatusOK {
		t.Fatalf("流式 status=%d body=%s", rec.Code, rec.Body.String())
	}
	return aggregateOpenAIStream(t, rec.Body.String())
}

func aggregateOpenAIStream(t *testing.T, body string) aggregatedStream {
	t.Helper()
	out := aggregatedStream{ChunkIDs: map[string]bool{}}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			out.HasDone = true
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Refusal          string `json:"refusal"`
					ToolCalls        []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.ID != "" {
			out.ChunkIDs[chunk.ID] = true
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		out.Content += choice.Delta.Content
		out.Reasoning += choice.Delta.ReasoningContent
		out.Refusal += choice.Delta.Refusal
		for _, call := range choice.Delta.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, struct {
				ID        string
				Name      string
				Arguments string
			}{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			out.FinishReason = *choice.FinishReason
		}
	}
	return out
}

// runNonStreamThroughProxy 走非流式路径，返回上游给出的完整消息。
func runNonStreamThroughProxy(t *testing.T, upstreamURL string) aggregatedStream {
	t.Helper()
	handler := newAnthropicStreamTestServer(t, upstreamURL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("非流式 status=%d body=%s", rec.Code, rec.Body.String())
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
				Refusal   string `json:"refusal"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("解析非流式响应失败: %v body=%s", err, rec.Body.String())
	}
	if len(parsed.Choices) == 0 {
		t.Fatalf("非流式响应缺少 choices: %s", rec.Body.String())
	}
	out := aggregatedStream{
		Content:      parsed.Choices[0].Message.Content,
		Reasoning:    parsed.Choices[0].Message.Reasoning,
		Refusal:      parsed.Choices[0].Message.Refusal,
		FinishReason: parsed.Choices[0].FinishReason,
	}
	for _, call := range parsed.Choices[0].Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, struct {
			ID        string
			Name      string
			Arguments string
		}{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	return out
}

// assertStreamMatchesNonStream 是核心差分断言。
func assertStreamMatchesNonStream(t *testing.T, f anthropicMessageFixture) {
	t.Helper()
	upstream := fixtureUpstream(t, f)
	defer upstream.Close()

	streamed := runStreamThroughProxy(t, upstream.URL)
	nonStream := runNonStreamThroughProxy(t, upstream.URL)

	if streamed.Content != nonStream.Content {
		t.Errorf("content 不一致:\n 流式聚合 = %q\n 非流式   = %q", streamed.Content, nonStream.Content)
	}
	if streamed.Reasoning != nonStream.Reasoning {
		t.Errorf("reasoning 不一致:\n 流式聚合 = %q\n 非流式   = %q", streamed.Reasoning, nonStream.Reasoning)
	}
	if streamed.FinishReason != nonStream.FinishReason {
		t.Errorf("finish_reason 不一致: 流式=%q 非流式=%q", streamed.FinishReason, nonStream.FinishReason)
	}
	if len(streamed.ToolCalls) != len(nonStream.ToolCalls) {
		t.Fatalf("工具调用数量不一致: 流式=%d 非流式=%d (%+v vs %+v)",
			len(streamed.ToolCalls), len(nonStream.ToolCalls), streamed.ToolCalls, nonStream.ToolCalls)
	}
	for i := range streamed.ToolCalls {
		got, want := streamed.ToolCalls[i], nonStream.ToolCalls[i]
		if got.Name != want.Name || got.ID != want.ID {
			t.Errorf("工具调用 #%d 标识不一致:\n 流式 = %+v\n 非流式 = %+v", i, got, want)
		}
		// 参数必须按**语义**比较，不能比字符串：
		// 流式保留上游原始字节序，非流式会把 JSON 解析成 Go map 再序列化
		// （键被排序），两者键序不同但语义完全等价。
		if !jsonArgumentsEqual(got.Arguments, want.Arguments) {
			t.Errorf("工具调用 #%d 参数语义不一致:\n 流式 = %s\n 非流式 = %s",
				i, got.Arguments, want.Arguments)
		}
		// 参数必须是合法 JSON，否则 VS 无法执行工具。
		var parsed map[string]any
		if err := json.Unmarshal([]byte(got.Arguments), &parsed); err != nil {
			t.Errorf("工具调用 #%d 参数不是合法 JSON: %q", i, got.Arguments)
		}
	}
	if !streamed.HasDone {
		t.Error("流式输出缺少 [DONE] 终止帧")
	}
	if len(streamed.ChunkIDs) > 1 {
		t.Errorf("同一请求的 chunk id 不一致: %v", streamed.ChunkIDs)
	}
	if streamed.Refusal != nonStream.Refusal {
		t.Errorf("refusal 不一致: 流式=%q 非流式=%q", streamed.Refusal, nonStream.Refusal)
	}
}

// 纯文本：最基础形态。
func TestStreamParity_PureText(t *testing.T) {
	assertStreamMatchesNonStream(t, anthropicMessageFixture{
		ID: "msg_pure", Model: "LongCat-2.0",
		TextChunks:  []string{"Hello", ", ", "world", "!"},
		InputTokens: 11, OutputTokens: 4,
	})
}

// content_block_start 自带初始文本 + 后续 delta：初始部分不能在增量路径丢失。
func TestStreamParity_InitialBlockTextNotLost(t *testing.T) {
	assertStreamMatchesNonStream(t, anthropicMessageFixture{
		ID: "msg_init", Model: "LongCat-2.0",
		InitialText: "PREFIX-", TextChunks: []string{"mid", "-SUFFIX"},
		InputTokens: 5, OutputTokens: 3,
	})
}

// 思考块（thinking_delta）必须完整映射到 reasoning_content。
func TestStreamParity_ThinkingContent(t *testing.T) {
	assertStreamMatchesNonStream(t, anthropicMessageFixture{
		ID: "msg_think", Model: "LongCat-2.0",
		TextChunks:   []string{"answer"},
		ThinkingInit: "let me ", ThinkChunks: []string{"think", " about", " it"},
		InputTokens: 7, OutputTokens: 9,
	})
}

// 工具调用：参数分片、非连续 index，都必须聚合出完整合法 JSON。
func TestStreamParity_ToolCallsNonSequentialIndex(t *testing.T) {
	assertStreamMatchesNonStream(t, anthropicMessageFixture{
		ID: "msg_tool", Model: "LongCat-2.0",
		Tools: []anthropicToolFixture{
			{Index: 7, ID: "toolu_a", Name: "get_weather",
				ArgChunks: []string{`{"city":`, `"Paris"`, `,"unit":"c"}`}},
		},
		StopReason:  "tool_use",
		InputTokens: 20, OutputTokens: 15,
	})
}

// 多个工具 + 文本混排 + 非连续 index：最容易出现串参数/丢参数的场景。
func TestStreamParity_MixedTextAndMultipleTools(t *testing.T) {
	assertStreamMatchesNonStream(t, anthropicMessageFixture{
		ID: "msg_mixed", Model: "LongCat-2.0",
		ThinkingInit: "plan: ", ThinkChunks: []string{"call", " two", " tools"},
		TextChunks: []string{"working on it"},
		Tools: []anthropicToolFixture{
			{Index: 3, ID: "toolu_x", Name: "read_file",
				ArgChunks: []string{`{"path":`, `"/tmp/a.txt"}`}},
			{Index: 9, ID: "toolu_y", Name: "write_file",
				ArgChunks: []string{`{"path":"/tmp/b.txt",`, `"content":"hi"}`}},
		},
		StopReason:  "tool_use",
		InputTokens: 33, OutputTokens: 21,
	})
}

// 工具参数完全来自 content_block_start 的 input（没有 input_json_delta）。
func TestStreamParity_ToolInputFromStartBlock(t *testing.T) {
	assertStreamMatchesNonStream(t, anthropicMessageFixture{
		ID: "msg_toolinit", Model: "LongCat-2.0",
		Tools: []anthropicToolFixture{
			{Index: 0, ID: "toolu_z", Name: "ping", InitialArg: `{"n":1}`},
		},
		StopReason:  "tool_use",
		InputTokens: 4, OutputTokens: 2,
	})
}

// 只有思考、没有正文：下游仍需拿到合法的流式契约。
func TestStreamParity_ThinkingOnly(t *testing.T) {
	assertStreamMatchesNonStream(t, anthropicMessageFixture{
		ID: "msg_thinkonly", Model: "LongCat-2.0",
		ThinkingInit: "hmm", ThinkChunks: []string{"..."},
		InputTokens: 3, OutputTokens: 1,
	})
}

// ---------------------------------------------------------------------------
// 增量路径的额外边界
// ---------------------------------------------------------------------------

// 没有任何增量（例如上游只发 message_start 就结束）时，
// 首帧仍必须写出且 id 正确，不能因为"从没触发过增量回调"而完全不写。
func TestStreamParity_ToolOnlyStillWritesPrelude(t *testing.T) {
	upstream := fixtureUpstream(t, anthropicMessageFixture{
		ID: "msg_toolonly", Model: "LongCat-2.0",
		Tools: []anthropicToolFixture{
			{Index: 0, ID: "toolu_q", Name: "noop", ArgChunks: []string{`{}`}},
		},
		StopReason: "tool_use",
	})
	defer upstream.Close()

	streamed := runStreamThroughProxy(t, upstream.URL)
	if len(streamed.ToolCalls) != 1 {
		t.Fatalf("工具调用丢失: %+v", streamed.ToolCalls)
	}
	if !streamed.HasDone {
		t.Error("缺少 [DONE]")
	}
	if len(streamed.ChunkIDs) != 1 {
		t.Errorf("chunk id 应唯一且非空: %v", streamed.ChunkIDs)
	}
	for id := range streamed.ChunkIDs {
		if id != "toolonly" {
			t.Errorf("id = %q, want %q（应为去掉 msg_ 前缀后的值）", id, "toolonly")
		}
	}
}

// 客户端在流中途断开时，上游读取必须尽快停止（回调错误要中止读取），
// 而不是继续把整条上下游流读完。
func TestStreamParity_ClientDisconnectStopsUpstreamRead(t *testing.T) {
	upstreamReadStopped := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(anthropicStreamPrelude(0)))
		if flusher != nil {
			flusher.Flush()
		}
		// 持续发送增量：若代理在客户端断开后仍继续读取，
		// 这个循环会一直跑下去，测试会因此失败。
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < 500; i++ {
			select {
			case <-r.Context().Done():
				close(upstreamReadStopped)
				return
			case <-ticker.C:
			}
			_, _ = w.Write([]byte(anthropicDeltaEvent(0, "text_delta", "x")))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	handler := newAnthropicStreamTestServer(t, upstream.URL)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("请求失败: %v", err)
	}
	reader := bufio.NewReader(resp.Body)
	// 读到第一个内容增量后立刻断开。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, readErr := reader.ReadString('\n')
		if strings.Contains(line, `"content"`) {
			break
		}
		if readErr != nil {
			break
		}
	}
	resp.Body.Close()
	cancel()

	select {
	case <-upstreamReadStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后上游读取未停止：连接与带宽会被白白占用")
	}
}

// jsonArgumentsEqual 按 JSON 语义比较两个工具参数串，
// 忽略键顺序与空白差异；任一侧不是合法 JSON 时退化为字符串比较。
func jsonArgumentsEqual(a, b string) bool {
	var left, right any
	if json.Unmarshal([]byte(a), &left) != nil || json.Unmarshal([]byte(b), &right) != nil {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	return reflect.DeepEqual(left, right)
}

// ---------------------------------------------------------------------------
// 随机化差分 soak
//
// 手写夹具只能覆盖"我能想到的"组合。这里用固定种子的伪随机生成器批量构造
// 文本/思考/多工具/非连续 index/start 块内联参数的组合，逐个跑完
// 流式聚合 vs 非流式 两条链路并要求完全等价。
//
// 固定种子保证结果可复现；失败时会打印种子与夹具，便于最小化定位。
// ---------------------------------------------------------------------------

func TestStreamParity_RandomizedSoak(t *testing.T) {
	const iterations = 150
	rng := rand.New(rand.NewSource(20260919))

	words := []string{"alpha", "beta", "gamma", "delta", "eps", "zeta", " ", "\n", "中文", "x"}
	pickWords := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(words[rng.Intn(len(words))])
		}
		return b.String()
	}

	for iter := 0; iter < iterations; iter++ {
		fixture := anthropicMessageFixture{
			ID:    "msg_soak" + itoa(iter),
			Model: "LongCat-2.0",
		}
		// 随机正文
		if rng.Intn(4) != 0 {
			if rng.Intn(3) == 0 {
				fixture.InitialText = pickWords(1 + rng.Intn(2))
			}
			for i := 0; i < 1+rng.Intn(4); i++ {
				fixture.TextChunks = append(fixture.TextChunks, pickWords(1+rng.Intn(3)))
			}
		}
		// 随机思考
		if rng.Intn(3) == 0 {
			if rng.Intn(2) == 0 {
				fixture.ThinkingInit = pickWords(1 + rng.Intn(2))
			}
			for i := 0; i < 1+rng.Intn(3); i++ {
				fixture.ThinkChunks = append(fixture.ThinkChunks, pickWords(1+rng.Intn(2)))
			}
		}
		// 随机工具。index 故意"非零起点 + 有间隙"，但必须**随出现顺序递增**：
		// Anthropic 协议里 index 就是 content 数组的下标，它不可能出现
		// "先发 index=8 再发 index=2" 这种与出现顺序矛盾的情况。
		// 早期版本在这里生成乱序 index，制造了夹具自身不自洽的假失败：
		// 非流式按数组顺序、流式按 index 排序，两者当然不同。
		toolCount := rng.Intn(4)
		nextIndex := rng.Intn(4) // 允许从非 0 开始
		for i := 0; i < toolCount; i++ {
			idx := nextIndex
			nextIndex += 1 + rng.Intn(3) // 允许间隙
			tool := anthropicToolFixture{
				Index: idx,
				ID:    "toolu_" + itoa(iter) + "_" + itoa(i),
				Name:  "fn_" + itoa(i),
			}
			payload := `{"k` + itoa(i) + `":"` + pickWords(1+rng.Intn(2)) + `"}`
			// 一半用 delta 分片，一半内联在 start 块
			if rng.Intn(2) == 0 {
				tool.InitialArg = payload
			} else {
				// 必须按 **rune 边界**切分，不能按字节：partial_json 是 JSON 字符串，
				// 在真实协议里永远是完整合法的 UTF-8。按字节切会把多字节汉字
				// 劈成两半，json.Marshal 随后把非法字节替换成 U+FFFD，
				// 于是流式链路拿到的是"已被夹具损坏"的输入，制造假失败。
				runes := []rune(payload)
				mid := 1 + rng.Intn(len(runes)-1)
				tool.ArgChunks = []string{string(runes[:mid]), string(runes[mid:])}
			}
			fixture.Tools = append(fixture.Tools, tool)
		}
		if len(fixture.Tools) > 0 {
			fixture.StopReason = "tool_use"
		} else if rng.Intn(2) == 0 {
			fixture.StopReason = "max_tokens"
		}
		fixture.InputTokens = rng.Intn(100)
		fixture.OutputTokens = rng.Intn(100)

		// 至少要有一点内容，否则属于"空响应"的失败关闭路径，另行覆盖。
		if fixture.InitialText == "" && len(fixture.TextChunks) == 0 &&
			fixture.ThinkingInit == "" && len(fixture.ThinkChunks) == 0 && len(fixture.Tools) == 0 {
			fixture.TextChunks = []string{"fallback"}
		}

		upstream := fixtureUpstream(t, fixture)
		streamed := runStreamThroughProxy(t, upstream.URL)
		nonStream := runNonStreamThroughProxy(t, upstream.URL)
		upstream.Close()

		if streamed.Content != nonStream.Content {
			t.Fatalf("iter=%d content 不一致:\n 流式 = %q\n 非流式 = %q\n fixture=%+v",
				iter, streamed.Content, nonStream.Content, fixture)
		}
		if streamed.Reasoning != nonStream.Reasoning {
			t.Fatalf("iter=%d reasoning 不一致:\n 流式 = %q\n 非流式 = %q\n fixture=%+v",
				iter, streamed.Reasoning, nonStream.Reasoning, fixture)
		}
		if streamed.FinishReason != nonStream.FinishReason {
			t.Fatalf("iter=%d finish_reason 不一致: 流式=%q 非流式=%q fixture=%+v",
				iter, streamed.FinishReason, nonStream.FinishReason, fixture)
		}
		if len(streamed.ToolCalls) != len(nonStream.ToolCalls) {
			t.Fatalf("iter=%d 工具数量不一致: 流式=%d 非流式=%d fixture=%+v",
				iter, len(streamed.ToolCalls), len(nonStream.ToolCalls), fixture)
		}
		for i := range streamed.ToolCalls {
			got, want := streamed.ToolCalls[i], nonStream.ToolCalls[i]
			if got.Name != want.Name || got.ID != want.ID || !jsonArgumentsEqual(got.Arguments, want.Arguments) {
				t.Fatalf("iter=%d 工具 #%d 不一致:\n 流式 = %+v\n 非流式 = %+v\n fixture=%+v",
					iter, i, got, want, fixture)
			}
		}
		if !streamed.HasDone {
			t.Fatalf("iter=%d 缺少 [DONE] fixture=%+v", iter, fixture)
		}
		if len(streamed.ChunkIDs) != 1 {
			t.Fatalf("iter=%d chunk id 不唯一: %v fixture=%+v", iter, streamed.ChunkIDs, fixture)
		}
	}
}

// itoa 是 strconv.Itoa 的本地别名，避免与已有 import 冲突。
func itoa(n int) string { return strconv.Itoa(n) }
