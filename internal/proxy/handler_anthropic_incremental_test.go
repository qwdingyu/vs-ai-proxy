package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// 流式增量性（incrementality）契约
//
// VS Copilot BYOM 固定使用 stream=true。流式路径必须"边收边吐"：
// 上游每产出一个 delta，下游就应立即收到对应的 OpenAI chunk。
//
// 若实现改成"先把整个上游响应读完/聚合，再一次性写出"，功能测试（最终内容
// 是否正确、是否以 [DONE] 结束）**依然全部通过**，但用户会盯着空白屏幕等到
// 整段回复生成完毕——实测过：上游间隔 1 秒发 3 个 token 时，下游在 T+3.03s
// 一次性收到全部内容。这类退化极难被常规断言发现，因此需要专门的增量性断言。
//
// 下面的用例不依赖 sleep 计时，而是用信号量做确定性同步：
// 上游在发出第一个 delta 后阻塞，只有确认下游已经收到它才会继续。
// 如果实现是聚合的，上游将永远不会被放行，测试以超时失败并给出明确原因。
// ---------------------------------------------------------------------------

// openAISSEChunkPayload 提取一行 SSE 的 data 载荷，非 data 行返回空串。
func openAISSEChunkPayload(line string) string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:"))
}

// downstreamDeltaField 从一行 SSE 中取出指定 delta 字段的字符串值。
func downstreamDeltaField(t *testing.T, line, field string) string {
	t.Helper()
	payload := openAISSEChunkPayload(line)
	if payload == "" || payload == "[DONE]" {
		return ""
	}
	var parsed struct {
		Choices []struct {
			Delta map[string]any `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return ""
	}
	if len(parsed.Choices) == 0 {
		return ""
	}
	value, _ := parsed.Choices[0].Delta[field].(string)
	return value
}

// anthropicDeltaEvent 构造一个 content_block_delta 的 SSE 事件块。
//
// 必须按**真实线格式**选择字段名：thinking_delta 用的是 "thinking"，
// 不是 "text"。此前这里一律用 "text"，掩盖了 anthropicStreamDelta
// 缺少 Thinking 字段导致思考内容被静默丢弃的缺陷——测试跑的是不存在的格式。
func anthropicDeltaEvent(index int, deltaType, text string) string {
	delta := map[string]any{"type": deltaType}
	switch deltaType {
	case "thinking_delta":
		delta["thinking"] = text
	case "input_json_delta":
		delta["partial_json"] = text
	default:
		delta["text"] = text
	}
	payload, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": delta,
	})
	return "event: content_block_delta\ndata: " + string(payload) + "\n\n"
}

func anthropicStreamPrelude(index int) string {
	start, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "m", "role": "assistant", "model": "LongCat-2.0",
		},
	})
	block, _ := json.Marshal(map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "text", "text": "",
		},
	})
	return "event: message_start\ndata: " + string(start) + "\n\n" +
		"event: content_block_start\ndata: " + string(block) + "\n\n"
}

// 文本 delta 必须在上游尚未结束时就到达下游。
//
// 判定方式：上游发出 FIRST 后阻塞（上限 upstreamHold），测试要求在 assertWithin
// 内看到 FIRST。聚合实现要等上游读完后才写出，因此必然超出 assertWithin；
// 增量实现只需几毫秒。两者差距被刻意拉开到 4 倍以上，避免计时抖动导致误判。
func TestAnthropicStream_TextDeltaArrivesBeforeUpstreamFinishes(t *testing.T) {
	const (
		upstreamHold = 10 * time.Second
		assertWithin = 2500 * time.Millisecond
	)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }

	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicStreamPrelude(0)))
		_, _ = w.Write([]byte(anthropicDeltaEvent(0, "text_delta", "FIRST")))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 阻塞直到测试放行；同时保留硬上限，聚合实现下上游最终也会结束，测试不会死锁。
		select {
		case <-release:
		case <-time.After(upstreamHold):
		}
		_, _ = w.Write([]byte(anthropicDeltaEvent(0, "text_delta", "SECOND")))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	// defer 顺序至关重要（LIFO）：releaseUpstream 必须最后注册、最先执行，
	// 否则上游 handler 仍阻塞时 upstream.Close()/proxy.Close() 会等待它，
	// 造成测试挂起——这正是本套件早期出现 90 秒"假通过"的原因。
	defer upstream.Close()
	proxy := httptest.NewServer(newAnthropicStreamTestServer(t, upstream.URL))
	defer proxy.Close()
	defer releaseUpstream()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	firstSeen := make(chan struct{})
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, readErr := reader.ReadString('\n')
			if got := downstreamDeltaField(t, line, "content"); got == "FIRST" {
				close(firstSeen)
				return
			}
			if readErr != nil {
				return
			}
		}
	}()

	select {
	case <-firstSeen:
		// 符合预期：上游还在阻塞，下游已经拿到 FIRST。
	case <-time.After(assertWithin):
		t.Fatalf("下游在 %s 内没有收到第一个 delta（上游会阻塞 %s）：流式被聚合成一次性输出，违反增量性契约",
			assertWithin, upstreamHold)
	}
	// 确认增量性后立即放行上游，避免测试白等整个 hold 时长。
	releaseUpstream()
}

// reasoning（thinking_delta）同样必须增量到达，否则思考型模型的"思考过程"会整段卡住。
func TestAnthropicStream_ThinkingDeltaArrivesBeforeUpstreamFinishes(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }

	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		thinking, _ := json.Marshal(map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "thinking", "thinking": "",
			},
		})
		start, _ := json.Marshal(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "m", "role": "assistant", "model": "LongCat-2.0",
			},
		})
		_, _ = w.Write([]byte("event: message_start\ndata: " + string(start) + "\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: " + string(thinking) + "\n\n"))
		_, _ = w.Write([]byte(anthropicDeltaEvent(0, "thinking_delta", "THINK1")))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		_, _ = w.Write([]byte(anthropicDeltaEvent(0, "thinking_delta", "THINK2")))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	// 同上：releaseUpstream 最后注册、最先执行，避免 Close 等待阻塞中的上游。
	defer upstream.Close()
	proxy := httptest.NewServer(newAnthropicStreamTestServer(t, upstream.URL))
	defer proxy.Close()
	defer releaseUpstream()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"LongCat-2.0","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	seen := make(chan struct{})
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, readErr := reader.ReadString('\n')
			if got := downstreamDeltaField(t, line, "reasoning_content"); got == "THINK1" {
				close(seen)
				return
			}
			if readErr != nil {
				return
			}
		}
	}()

	select {
	case <-seen:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("下游没有增量收到 thinking delta：思考过程被聚合成一次性输出")
	}
	releaseUpstream()
}

// 增量输出不得破坏最终契约：内容必须完整且以 finish_reason + [DONE] 结束。
func TestAnthropicStream_IncrementalOutputKeepsFullContract(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicStreamPrelude(0)))
		for _, tok := range []string{"Al", "pha", "Beta"} {
			_, _ = w.Write([]byte(anthropicDeltaEvent(0, "text_delta", tok)))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	defer upstream.Close()

	rec := postAnthropicStream(t, newAnthropicStreamTestServer(t, upstream.URL))
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, body)
	}
	// 内容必须完整（分片累加后等于原串），且不得重复。
	var assembled strings.Builder
	for _, line := range strings.Split(body, "\n") {
		assembled.WriteString(downstreamDeltaField(t, line, "content"))
	}
	if got := assembled.String(); got != "AlphaBeta" {
		t.Fatalf("累加内容 = %q, want %q（分片丢失或重复）", got, "AlphaBeta")
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("缺少 finish_reason=stop: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("缺少 [DONE]: %s", body)
	}
}

// ---------------------------------------------------------------------------
// 管理端流式兜底（SendAnthropicChatStreamContent）的终态独立性
//
// 该函数原先自己用 bufio.Scanner 且只认 "[DONE]" 终止，而 Anthropic 协议
// 不发 [DONE]（发 message_stop），于是它只能等传输层 EOF。
// 实测：上游发完 message_stop 后保持连接不关闭时，它会挂到 ctx 超时
//（ctx=3s 返回 3s，ctx=7s 返回 7s），管理页兜底探测随之卡住。
// ---------------------------------------------------------------------------

// message_stop 之后上游不关连接，函数必须立即返回，而不是等 ctx 超时。
func TestSendAnthropicChatStreamContent_TerminatesOnMessageStopWithoutEOF(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }

	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicStreamPrelude(0)))
		_, _ = w.Write([]byte(anthropicDeltaEvent(0, "text_delta", "ADMIN_TEXT")))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 保持连接不关闭：真实网关复用连接时的行为
		select {
		case <-release:
		case <-time.After(15 * time.Second):
		}
	})
	defer upstream.Close()
	defer releaseUpstream()

	done := make(chan struct{})
	var got string
	var gotErr error
	go func() {
		defer close(done)
		// ctx 给足 15 秒：若实现依赖超时结束，测试会明显超时失败，
		// 而不是"恰好卡在边缘"导致误判。
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		got, gotErr = SendAnthropicChatStreamContent(ctx, upstream.URL, "v1/messages", "k",
			&provider.ChatRequest{Model: "LongCat-2.0"})
	}()

	select {
	case <-done:
		if gotErr != nil {
			t.Fatalf("期望成功取到文本，得到错误: %v", gotErr)
		}
		if got != "ADMIN_TEXT" {
			t.Fatalf("文本 = %q, want %q", got, "ADMIN_TEXT")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message_stop 之后上游不关连接时挂起：管理页兜底会卡到超时")
	}
	releaseUpstream()
}

// 多个 text 分片必须完整聚合。
func TestSendAnthropicChatStreamContent_AggregatesAllTextChunks(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicStreamPrelude(0)))
		for _, token := range []string{"Al", "pha", "Beta"} {
			_, _ = w.Write([]byte(anthropicDeltaEvent(0, "text_delta", token)))
		}
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	defer upstream.Close()

	got, err := SendAnthropicChatStreamContent(context.Background(), upstream.URL, "v1/messages", "k",
		&provider.ChatRequest{Model: "LongCat-2.0"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if got != "AlphaBeta" {
		t.Fatalf("文本 = %q, want %q", got, "AlphaBeta")
	}
}

// 上游 error 事件仍必须上报真实原因（不能因复用主读取器而丢失）。
func TestSendAnthropicChatStreamContent_PropagatesErrorEvent(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\"}}\n\n"))
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"ADMIN_REASON\"}}\n\n"))
	})
	defer upstream.Close()

	_, err := SendAnthropicChatStreamContent(context.Background(), upstream.URL, "v1/messages", "k",
		&provider.ChatRequest{Model: "LongCat-2.0"})
	if err == nil {
		t.Fatal("期望 error 事件导致失败")
	}
	if !strings.Contains(err.Error(), "ADMIN_REASON") {
		t.Errorf("错误原因被吞掉: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 上游错误在并发下必须可靠上报（竞态回归）
//
// 缺陷形态：读取 goroutine 提前退出时先 signalTerminal 唤醒主流程，错误却由
// defer 才投递；而主流程醒来后对 readResultErr 只做一次非阻塞读。二者之间存在
// 竞态窗口，命中时上游错误被静默丢弃——调用方拿到 err==nil + 半截内容，
// 于是把截断的流当成功收尾（补发 finish_reason + [DONE]），上游故障原因彻底消失。
//
// 为什么是并发压力用例而不是单次断言：该窗口只在多 P 调度下才可能命中。
// 实测丢失率随 GOMAXPROCS 上升（顺序执行时 =1 为 0%、=2 约 0.03%、=4 约 0.15%、
// =8 约 0.55%；改成 8 worker 并发后升到约 0.4%~0.8%），单次运行几乎必过。
// 因此这里固定 GOMAXPROCS 并用 8 个 worker 并发跑 800 次（共 6400 次）：
// 还原修复后实测命中 50/6400 ≈ 0.78%，修复后必须恰好 0 次。
//
// 并行测试隔离（改本用例前必读）：
//   - 本用例会在用例内调整 GOMAXPROCS 这个**进程级**全局量，因此必须保证隔离。
//   - 隔离成立的前提是：本用例**不调用 t.Parallel()**。testing 包保证并行用例
//     要等该包全部顺序用例跑完后才开始，所以顺序阶段内本用例独占执行，
//     与其它用例没有时间重叠；函数退出时 defer 已把 GOMAXPROCS 还原。
//   - 本包确实存在 t.Parallel 用例（见 recovery_policy_test.go），因此这条前提
//     不是空话。**切勿给本用例加 t.Parallel()**：一旦加了，它会与其它并行用例
//     重叠，全局 GOMAXPROCS 就会被其它用例观测到，造成难排查的偶发失败。
//   - 上述前提已用 A/B 实验验证（临时用例：setter 改写 GOMAXPROCS 并持有 400ms，
//     并行 observer 采样该值）：
//     非并行 setter → observer 观测到 8（=NumCPU，未观测到特征值 11），
//     总耗时 1.115s = 0.40s + 0.70s 顺序叠加；
//     并行 setter   → observer 观测到 11（=特征值），总耗时 0.714s，确已重叠。
//   - GOMAXPROCS 虽是进程级，但 go test 每个包各起一个测试进程，
//     故不会影响其它包的用例。
//   - 之所以必须固定而不能依赖运行环境：GOMAXPROCS=1 时该窗口根本不存在，
//     用例会退化成永远通过（假绿），失去回归价值。
//
// ---------------------------------------------------------------------------
func TestAnthropicStream_UpstreamErrorEventNotSwallowedUnderConcurrency(t *testing.T) {
	// 固定并行度，理由与隔离前提见上方"并行测试隔离"说明；defer 负责还原。
	prev := runtime.GOMAXPROCS(8)
	defer runtime.GOMAXPROCS(prev)

	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"model\":\"LongCat-2.0\"}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		// 关键：先产出正文，确保上游报错时"已有部分内容"。
		// 这样错误一旦被吞，调用方拿到的是非空内容 + nil error，伪装成成功；
		// 若上游只发错误事件、没有任何正文，吞掉后会退化成"没有文本内容"，
		// 症状不同但同样丢原因，因此必须用"部分内容 + 错误"这个形态。
		write(anthropicDeltaEvent(0, "text_delta", "PARTIAL"))
		write("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"UPSTREAM_REASON\"}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	defer upstream.Close()

	const (
		workers = 8
		iters   = 800
	)
	var lost int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_, err := SendAnthropicChatStreamRequest(context.Background(), upstream.URL, "v1/messages", "k",
					&provider.ChatRequest{Model: "LongCat-2.0"})
				// 正确的行为：err 非 nil 且携带上游真实原因。
				if err == nil || !strings.Contains(err.Error(), "UPSTREAM_REASON") {
					atomic.AddInt64(&lost, 1)
				}
			}
		}()
	}
	wg.Wait()

	if lost != 0 {
		t.Fatalf("上游 error 事件被吞掉 %d/%d 次：调用方会拿到 err==nil + 半截内容，"+
			"把截断的流当成功收尾（竞态回归）", lost, workers*iters)
	}
}

// 只有思考、没有正文时仍应报"没有文本内容"（保持原有语义）。
func TestSendAnthropicChatStreamContent_ThinkingOnlyStillReportsNoText(t *testing.T) {
	upstream := anthropicErrorUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		start, _ := json.Marshal(map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "role": "assistant"}})
		block, _ := json.Marshal(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
		_, _ = w.Write([]byte("event: message_start\ndata: " + string(start) + "\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: " + string(block) + "\n\n"))
		_, _ = w.Write([]byte(anthropicDeltaEvent(0, "thinking_delta", "ONLY_THINKING")))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	defer upstream.Close()

	if _, err := SendAnthropicChatStreamContent(context.Background(), upstream.URL, "v1/messages", "k",
		&provider.ChatRequest{Model: "LongCat-2.0"}); err == nil {
		t.Fatal("只有思考内容时应当报错（与原有语义一致）")
	}
}
