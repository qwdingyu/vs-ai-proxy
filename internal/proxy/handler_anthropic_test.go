package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// testFlusher 实现 http.Flusher 接口，用于测试中模拟 HTTP 响应 flush。
type testFlusher struct {
	*httptest.ResponseRecorder
}

func (f *testFlusher) Flush() {}

// ---------------------------------------------------------------------------
// 测试集 1：anthropicRequestToChatRequest
// 验证 Anthropic Messages API 请求 → 内部 ChatRequest 的转换正确性。
// ---------------------------------------------------------------------------

func TestAnthropicRequestToChatRequest_BasicText(t *testing.T) {
	raw := `{"model":"LongCat-2.0","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	chatReq := anthropicRequestToChatRequest(&req)

	if chatReq.Model != "LongCat-2.0" {
		t.Errorf("model = %q, want LongCat-2.0", chatReq.Model)
	}
	if chatReq.MaxTokens == nil || *chatReq.MaxTokens != 100 {
		t.Errorf("max_tokens = %v, want 100", chatReq.MaxTokens)
	}
	if len(chatReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(chatReq.Messages))
	}
	if chatReq.Messages[0].Role != "user" || chatReq.Messages[0].Content != "Hello" {
		t.Errorf("message = %+v, want role=user content=Hello", chatReq.Messages[0])
	}
}

func TestAnthropicRequestToChatRequest_WithSystem(t *testing.T) {
	raw := `{"model":"LongCat-2.0","max_tokens":100,"system":"You are helpful","messages":[{"role":"user","content":"Hi"}]}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	chatReq := anthropicRequestToChatRequest(&req)

	if len(chatReq.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2 (system + user)", len(chatReq.Messages))
	}
	if chatReq.Messages[0].Role != "system" || chatReq.Messages[0].Content != "You are helpful" {
		t.Errorf("first message should be system, got %+v", chatReq.Messages[0])
	}
	if chatReq.Messages[1].Role != "user" || chatReq.Messages[1].Content != "Hi" {
		t.Errorf("second message should be user, got %+v", chatReq.Messages[1])
	}
}

func TestAnthropicRequestToChatRequest_WithStreaming(t *testing.T) {
	raw := `{"model":"LongCat-2.0","max_tokens":100,"messages":[{"role":"user","content":"Hi"}],"stream":true}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	chatReq := anthropicRequestToChatRequest(&req)

	if !chatReq.Stream {
		t.Error("stream should be true")
	}
}

func TestAnthropicRequestToChatRequest_WithTools(t *testing.T) {
	raw := `{
		"model":"LongCat-2.0","max_tokens":100,
		"messages":[{"role":"user","content":"What's the weather?"}],
		"tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"location":{"type":"string"}}}}],
		"tool_choice":{"type":"auto"}
	}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	chatReq := anthropicRequestToChatRequest(&req)

	if len(chatReq.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(chatReq.Tools))
	}
	if chatReq.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", chatReq.Tools[0].Function.Name)
	}
	if chatReq.Extra == nil {
		t.Fatal("Extra should not be nil when tool_choice is set")
	}
	tcRaw, ok := chatReq.Extra["tool_choice"]
	if !ok {
		t.Fatal("tool_choice should be in Extra")
	}
	if !strings.Contains(string(tcRaw), `"auto"`) {
		t.Errorf("tool_choice = %s, want auto", string(tcRaw))
	}
}

func TestAnthropicRequestToChatRequest_WithToolChoiceFunction(t *testing.T) {
	raw := `{
		"model":"LongCat-2.0","max_tokens":100,
		"messages":[{"role":"user","content":"Weather?"}],
		"tool_choice":{"type":"tool","name":"get_weather"}
	}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	chatReq := anthropicRequestToChatRequest(&req)

	tcRaw := string(chatReq.Extra["tool_choice"])
	if !strings.Contains(tcRaw, `"function"`) || !strings.Contains(tcRaw, `"get_weather"`) {
		t.Errorf("tool_choice should be converted to OpenAI function format, got %s", tcRaw)
	}
}

func TestAnthropicRequestToChatRequest_WithContentBlocks(t *testing.T) {
	raw := `{"model":"LongCat-2.0","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]}]}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	chatReq := anthropicRequestToChatRequest(&req)

	if len(chatReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(chatReq.Messages))
	}
	if chatReq.Messages[0].Content != "Hello" {
		t.Errorf("content = %q, want Hello", chatReq.Messages[0].Content)
	}
}

func TestAnthropicRequestToChatRequest_WithToolResult(t *testing.T) {
	raw := `{"model":"LongCat-2.0","max_tokens":100,"messages":[
		{"role":"user","content":[{"type":"text","text":"What's 2+2?"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"tu_01","name":"calculator","input":{"expr":"2+2"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_01","content":"4"}]}
	]}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	chatReq := anthropicRequestToChatRequest(&req)

	if len(chatReq.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(chatReq.Messages))
	}
	if len(chatReq.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool_calls len = %d, want 1", len(chatReq.Messages[1].ToolCalls))
	}
	if chatReq.Messages[1].ToolCalls[0].Function.Name != "calculator" {
		t.Errorf("tool name = %q, want calculator", chatReq.Messages[1].ToolCalls[0].Function.Name)
	}
	if chatReq.Messages[2].ToolCallID != "tu_01" {
		t.Errorf("tool_call_id = %q, want tu_01", chatReq.Messages[2].ToolCallID)
	}
}

func TestAnthropicRequestToChatRequest_WithTemperature(t *testing.T) {
	raw := `{"model":"LongCat-2.0","max_tokens":100,"messages":[{"role":"user","content":"Hi"}],"temperature":0.7,"top_p":0.9,"top_k":50}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	chatReq := anthropicRequestToChatRequest(&req)

	if chatReq.Temperature == nil || *chatReq.Temperature != 0.7 {
		t.Errorf("temperature = %v, want 0.7", chatReq.Temperature)
	}
	if chatReq.TopP == nil || *chatReq.TopP != 0.9 {
		t.Errorf("top_p = %v, want 0.9", chatReq.TopP)
	}
	if chatReq.TopK == nil || *chatReq.TopK != 50 {
		t.Errorf("top_k = %v, want 50", chatReq.TopK)
	}
}

func TestAnthropicRequestToChatRequest_WithStopSequences(t *testing.T) {
	raw := `{"model":"LongCat-2.0","max_tokens":100,"messages":[{"role":"user","content":"Hi"}],"stop_sequences":["\n\n","."]}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	chatReq := anthropicRequestToChatRequest(&req)

	if len(chatReq.Stop) != 2 {
		t.Fatalf("stop len = %d, want 2", len(chatReq.Stop))
	}
}

// ---------------------------------------------------------------------------
// 测试集 2：chatResponseToAnthropicResponse
// 验证内部 ChatResponse → Anthropic 格式响应的转换正确性。
// ---------------------------------------------------------------------------

func TestChatResponseToAnthropicResponse_BasicText(t *testing.T) {
	resp := &provider.ChatResponse{
		ID:      "chatcmpl-abc123",
		Model:   "LongCat-2.0",
		Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "Hello!"}, FinishReason: "stop"}},
		Usage:   &provider.Usage{PromptTokens: 10, CompletionTokens: 20},
	}

	anthropicResp := chatResponseToAnthropicResponse(resp, "LongCat-2.0")

	if anthropicResp.ID != "msg_chatcmpl-abc123" {
		t.Errorf("id = %q, want msg_chatcmpl-abc123", anthropicResp.ID)
	}
	if anthropicResp.Type != "message" {
		t.Errorf("type = %q, want message", anthropicResp.Type)
	}
	if anthropicResp.Role != "assistant" {
		t.Errorf("role = %q, want assistant", anthropicResp.Role)
	}
	if anthropicResp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", anthropicResp.StopReason)
	}
	if len(anthropicResp.Content) != 1 {
		t.Fatalf("content len = %d, want 1", len(anthropicResp.Content))
	}
	if anthropicResp.Content[0].Type != "text" || anthropicResp.Content[0].Text != "Hello!" {
		t.Errorf("content block = %+v, want text=Hello!", anthropicResp.Content[0])
	}
	if anthropicResp.Usage.InputTokens != 10 || anthropicResp.Usage.OutputTokens != 20 {
		t.Errorf("usage = %+v, want input=10 output=20", anthropicResp.Usage)
	}
}

func TestChatResponseToAnthropicResponse_WithReasoning(t *testing.T) {
	resp := &provider.ChatResponse{
		ID:      "chatcmpl-def456",
		Model:   "LongCat-2.0",
		Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "Hi!", Reasoning: "Let me think..."}, FinishReason: "stop"}},
		Usage:   &provider.Usage{PromptTokens: 5, CompletionTokens: 10},
	}

	anthropicResp := chatResponseToAnthropicResponse(resp, "LongCat-2.0")

	// 验证有两个 content block：thinking + text
	if len(anthropicResp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (thinking + text)", len(anthropicResp.Content))
	}
	// thinking 块使用 Thinking 字段（不是 Text）
	if anthropicResp.Content[0].Type != "thinking" || anthropicResp.Content[0].Thinking != "Let me think..." {
		t.Errorf("first block should be thinking with Thinking field, got type=%q text=%q thinking=%q",
			anthropicResp.Content[0].Type, anthropicResp.Content[0].Text, anthropicResp.Content[0].Thinking)
	}
	if anthropicResp.Content[0].Text != "" {
		t.Errorf("thinking block should not have Text field set, got %q", anthropicResp.Content[0].Text)
	}
	if anthropicResp.Content[1].Type != "text" || anthropicResp.Content[1].Text != "Hi!" {
		t.Errorf("second block should be text, got %+v", anthropicResp.Content[1])
	}
}

func TestChatResponseToAnthropicResponse_WithToolCalls(t *testing.T) {
	resp := &provider.ChatResponse{
		ID:    "chatcmpl-ghi789",
		Model: "LongCat-2.0",
		Choices: []provider.Choice{{
			Index: 0,
			Message: provider.Message{
				Role:    "assistant",
				Content: "Let me check the weather",
				ToolCalls: []provider.ToolCall{{
					ID:   "call_01",
					Type: "function",
					Function: provider.FunctionCall{
						Name:      "get_weather",
						Arguments: `{"location":"NYC"}`,
					},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}

	anthropicResp := chatResponseToAnthropicResponse(resp, "LongCat-2.0")

	if anthropicResp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", anthropicResp.StopReason)
	}
	if len(anthropicResp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (text + tool_use)", len(anthropicResp.Content))
	}
	if anthropicResp.Content[0].Type != "text" {
		t.Errorf("first block type = %q, want text", anthropicResp.Content[0].Type)
	}
	if anthropicResp.Content[1].Type != "tool_use" {
		t.Errorf("second block type = %q, want tool_use", anthropicResp.Content[1].Type)
	}
	if anthropicResp.Content[1].ID != "call_01" || anthropicResp.Content[1].Name != "get_weather" {
		t.Errorf("tool_use = %+v, want id=call_01 name=get_weather", anthropicResp.Content[1])
	}
}

func TestChatResponseToAnthropicResponse_FinishReasonMapping(t *testing.T) {
	tests := []struct {
		openAIReason  string
		wantAnthropic string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"content_filter", "content_filter"},
	}

	for _, tt := range tests {
		resp := &provider.ChatResponse{
			ID:      "test",
			Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "hi"}, FinishReason: tt.openAIReason}},
		}
		anthropicResp := chatResponseToAnthropicResponse(resp, "test")
		if anthropicResp.StopReason != tt.wantAnthropic {
			t.Errorf("finish_reason %q → stop_reason %q, want %q", tt.openAIReason, anthropicResp.StopReason, tt.wantAnthropic)
		}
	}
}

func TestChatResponseToAnthropicResponse_EmptyChoices(t *testing.T) {
	resp := &provider.ChatResponse{
		ID:    "test",
		Model: "LongCat-2.0",
	}
	anthropicResp := chatResponseToAnthropicResponse(resp, "LongCat-2.0")

	if anthropicResp.StopReason != "end_turn" {
		t.Errorf("stop_reason with empty choices = %q, want end_turn", anthropicResp.StopReason)
	}
	if len(anthropicResp.Content) != 0 {
		t.Errorf("content with empty choices = %d, want 0", len(anthropicResp.Content))
	}
}

func TestChatResponseToAnthropicResponse_UsageNil(t *testing.T) {
	resp := &provider.ChatResponse{
		ID:      "test",
		Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "hi"}, FinishReason: "stop"}},
	}
	anthropicResp := chatResponseToAnthropicResponse(resp, "test")

	if anthropicResp.Usage != nil {
		t.Errorf("usage should be nil when input has no usage")
	}
}

// ---------------------------------------------------------------------------
// 测试集 3：anthropicStreamWriter
// 验证 OpenAI SSE → Anthropic event-based SSE 的流式转换正确性。
// ---------------------------------------------------------------------------

func TestAnthropicStreamWriter_BasicText(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher := &testFlusher{rec}
	writer := &anthropicStreamWriter{
		writer:  rec,
		flusher: flusher,
		model:   "LongCat-2.0",
	}

	// 首 chunk：角色+内容
	_, err := writer.Write([]byte(`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}` + "\n\n"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	// 终态 chunk
	_, err = writer.Write([]byte(`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	writer.finish()

	body := rec.Body.String()
	// 验证完整事件序列
	requiredEvents := []string{"message_start", "content_block_start", "content_block_delta", "message_delta", "message_stop"}
	for _, event := range requiredEvents {
		if !strings.Contains(body, "event: "+event) {
			t.Errorf("missing event: %s", event)
		}
	}
	if !strings.Contains(body, `"type":"text"`) {
		t.Error("text block should be created")
	}
	if !strings.Contains(body, `"text_delta"`) {
		t.Error("missing text_delta in delta")
	}
	if !strings.Contains(body, `"Hello"`) {
		t.Error("missing content text")
	}
}

func TestAnthropicStreamWriter_WithReasoning(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher := &testFlusher{rec}
	writer := &anthropicStreamWriter{
		writer:  rec,
		flusher: flusher,
		model:   "LongCat-2.0",
	}

	// reasoning chunk
	writer.Write([]byte(`data: {"id":"cmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think"},"finish_reason":null}]}` + "\n\n"))
	// text chunk
	writer.Write([]byte(`data: {"id":"cmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n"))
	// done
	writer.Write([]byte(`data: {"id":"cmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
	writer.finish()

	body := rec.Body.String()
	// 验证 thinking 块
	if !strings.Contains(body, `"thinking_delta"`) {
		t.Error("missing thinking_delta")
	}
	if !strings.Contains(body, `"Let me think"`) {
		t.Error("missing reasoning content")
	}
	// 验证 thinking 块使用 thinking 字段而不是 text 字段
	if strings.Contains(body, `"type":"thinking","text"`) {
		t.Error("thinking block should not use text field, use thinking field instead")
	}
	// 验证 text 块在 thinking 之后
	thinkingIdx := strings.Index(body, "thinking_delta")
	textIdx := strings.Index(body, "text_delta")
	if thinkingIdx < 0 || textIdx < 0 || textIdx < thinkingIdx {
		t.Error("text_delta should appear after thinking_delta")
	}
}

func TestAnthropicStreamWriter_ToolCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher := &testFlusher{rec}
	writer := &anthropicStreamWriter{
		writer:  rec,
		flusher: flusher,
		model:   "LongCat-2.0",
	}

	writer.Write([]byte(`data: {"id":"cmpl-2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_01","type":"function","function":{"name":"get_weather","arguments":"{\"loc\":\"NYC\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n"))
	writer.finish()

	body := rec.Body.String()
	if !strings.Contains(body, `"tool_use"`) {
		t.Error("missing tool_use content block")
	}
	if !strings.Contains(body, `"input_json_delta"`) {
		t.Error("missing input_json_delta")
	}
	if !strings.Contains(body, `"call_01"`) {
		t.Error("missing tool call ID")
	}
	if !strings.Contains(body, `"get_weather"`) {
		t.Error("missing tool name")
	}
}

func TestAnthropicStreamWriter_FinishReasonMapping(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher := &testFlusher{rec}
	writer := &anthropicStreamWriter{
		writer:  rec,
		flusher: flusher,
		model:   "LongCat-2.0",
	}

	writer.Write([]byte(`data: {"id":"cmpl-3","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}` + "\n\n"))
	writer.finish()

	body := rec.Body.String()
	if !strings.Contains(body, `"end_turn"`) {
		t.Errorf("stop_reason should be end_turn, got %q", body)
	}
}

func TestAnthropicStreamWriter_NoContent(t *testing.T) {
	// 无内容时 finish() 应跳过，不发送任何事件
	rec := httptest.NewRecorder()
	flusher := &testFlusher{rec}
	writer := &anthropicStreamWriter{
		writer:  rec,
		flusher: flusher,
		model:   "LongCat-2.0",
	}
	writer.finish()

	body := rec.Body.String()
	if body != "" {
		t.Errorf("expected empty body when no content, got %q", body)
	}
}

func TestAnthropicStreamWriter_EmptyDelta(t *testing.T) {
	// 空 delta（只有 role）应只触发 message_start，不创建 content block
	rec := httptest.NewRecorder()
	flusher := &testFlusher{rec}
	writer := &anthropicStreamWriter{
		writer:  rec,
		flusher: flusher,
		model:   "LongCat-2.0",
	}

	writer.Write([]byte(`data: {"id":"cmpl-4","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n"))
	writer.finish()

	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Error("missing message_start")
	}
	if strings.Contains(body, "event: content_block") {
		t.Error("should not have content_block events with empty delta")
	}
}

// ---------------------------------------------------------------------------
// 测试集 4：writeAnthropicErrorResponse
// 验证 Anthropic 格式错误响应的正确性。
// ---------------------------------------------------------------------------

func TestWriteAnthropicErrorResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAnthropicErrorResponse(rec, 400, "test error")

	if rec.Code != 400 {
		t.Errorf("status code = %d, want 400", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error response failed: %v", err)
	}
	if resp["type"] != "error" {
		t.Errorf("type = %q, want error", resp["type"])
	}
	errObj, ok := resp["error"].(map[string]interface{})
	if !ok {
		t.Fatal("error field missing")
	}
	if errObj["message"] != "test error" {
		t.Errorf("error message = %q, want test error", errObj["message"])
	}
}

// ---------------------------------------------------------------------------
// 测试集 5：validateAnthropicRequest
// 验证 Anthropic 请求校验逻辑。
// ---------------------------------------------------------------------------

func TestValidateAnthropicRequest_MissingModel(t *testing.T) {
	req := &anthropicRequest{MaxTokens: 100, Messages: []anthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	if err := validateAnthropicRequest(req); err == nil {
		t.Error("expected error for missing model")
	}
}

func TestValidateAnthropicRequest_MissingMaxTokens(t *testing.T) {
	req := &anthropicRequest{Model: "test", Messages: []anthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	if err := validateAnthropicRequest(req); err == nil {
		t.Error("expected error for missing max_tokens")
	}
}

func TestValidateAnthropicRequest_EmptyMessages(t *testing.T) {
	req := &anthropicRequest{Model: "test", MaxTokens: 100, Messages: []anthropicMessage{}}
	if err := validateAnthropicRequest(req); err == nil {
		t.Error("expected error for empty messages")
	}
}

func TestValidateAnthropicRequest_Valid(t *testing.T) {
	req := &anthropicRequest{Model: "test", MaxTokens: 100, Messages: []anthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	if err := validateAnthropicRequest(req); err != nil {
		t.Errorf("unexpected error for valid request: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 测试集 6：handleAnthropicMessages 集成测试
// 使用新 testServer + mock upstream 验证完整的 /v1/messages HTTP 处理流程。
// ---------------------------------------------------------------------------

func TestHandleAnthropicMessages_NonStreamIntegration(t *testing.T) {
	// 启动 mock upstream，返回 OpenAI 格式响应
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"chatcmpl-test","model":"LongCat-2.0","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`))
			return
		}
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	// 创建真实 provider 指向 mock upstream
	prov := provider.NewOpenAIProviderWithCapability("longcat", "openai", "sk-test", upstream.URL+"/v1", true, 5*time.Second)
	server := newTestServer(prov)
	inner := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
	})
	handler := server.loggingMiddleware(inner)

	body := `{"model":"LongCat-2.0","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp anthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if resp.Type != "message" {
		t.Errorf("type = %q, want message", resp.Type)
	}
	if resp.Role != "assistant" {
		t.Errorf("role = %q, want assistant", resp.Role)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello!" {
		t.Errorf("content = %+v, want text=Hello!", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 20 {
		t.Errorf("usage = %+v, want input=10 output=20", resp.Usage)
	}
}

func TestHandleAnthropicMessages_StreamIntegration(t *testing.T) {
	// 启动 mock upstream，返回 OpenAI SSE 流
	streamBody := strings.Join([]string{
		`data: {"id":"cmpl-s1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		`data: {"id":"cmpl-s1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(streamBody))
			return
		}
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"LongCat-2.0"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	prov := provider.NewOpenAIProviderWithCapability("longcat", "openai", "sk-test", upstream.URL+"/v1", true, 5*time.Second)
	server := newTestServer(prov)
	inner := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
	})
	handler := server.loggingMiddleware(inner)

	body := `{"model":"LongCat-2.0","max_tokens":100,"messages":[{"role":"user","content":"Hello"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	bodyStr := rec.Body.String()
	// 验证 Anthropic event-based SSE 格式
	requiredEvents := []string{"message_start", "content_block_start", "content_block_delta", "message_delta", "message_stop"}
	for _, event := range requiredEvents {
		if !strings.Contains(bodyStr, "event: "+event) {
			t.Errorf("missing event: %s in %s", event, bodyStr)
		}
	}
	if !strings.Contains(bodyStr, `"text_delta"`) {
		t.Error("missing text_delta in stream")
	}
	if !strings.Contains(bodyStr, `"Hello"`) {
		t.Error("missing content in stream")
	}
}

func TestHandleAnthropicMessages_InvalidModel(t *testing.T) {
	prov := provider.NewOpenAIProviderWithCapability("longcat", "openai", "sk-test", "http://127.0.0.1:1/v1", true, time.Second)
	server := newTestServer(prov)
	inner := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
	})
	handler := server.loggingMiddleware(inner)

	body := `{"model":"nonexistent-model","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// 应该返回错误（400 或 502）
	if rec.Code < 400 {
		t.Fatalf("expected error status, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error response failed: %v", err)
	}
	if resp["type"] != "error" {
		t.Errorf("response type = %q, want error", resp["type"])
	}
}

func TestHandleAnthropicMessages_MissingMaxTokens(t *testing.T) {
	server := newOpenServer()
	inner := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
	})
	handler := server.loggingMiddleware(inner)

	body := `{"model":"LongCat-2.0","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAnthropicMessages_EmptyBody(t *testing.T) {
	server := newOpenServer()
	inner := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
	})
	handler := server.loggingMiddleware(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAnthropicMessages_WrongMethod(t *testing.T) {
	server := newOpenServer()
	inner := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/messages", server.handleAnthropicMessages)
	})
	handler := server.loggingMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 405 {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 测试集 7：countTokens 辅助函数
// ---------------------------------------------------------------------------

func TestCountTokens(t *testing.T) {
	if countTokens("") != 0 {
		t.Errorf("empty string should return 0")
	}
	if countTokens("hello") != 2 {
		t.Errorf("hello should return 2 (5 bytes/2), got %d", countTokens("hello"))
	}
	if countTokens("你好") != 3 {
		t.Errorf("你好 should return 3 (6 bytes/2), got %d", countTokens("你好"))
	}
}

// ---------------------------------------------------------------------------
// 测试集 8：buildAnthropicSystemMessage / buildAnthropicMessages 辅助函数
// ---------------------------------------------------------------------------

func TestBuildAnthropicSystemMessage(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "Hi"},
	}
	system := buildAnthropicSystemMessage(msgs)
	if system != "You are helpful" {
		t.Errorf("system message = %q, want You are helpful", system)
	}
}

func TestBuildAnthropicSystemMessage_NoSystem(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "Hi"},
	}
	system := buildAnthropicSystemMessage(msgs)
	if system != "" {
		t.Errorf("expected empty system, got %q", system)
	}
}

func TestBuildAnthropicMessages(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "Hi"},
		{Role: "assistant", Content: "Hello!"},
	}
	result := buildAnthropicMessages(msgs)

	if len(result) != 2 {
		t.Fatalf("result len = %d, want 2 (system filtered out)", len(result))
	}
	if result[0].Role != "user" {
		t.Errorf("first role = %q, want user", result[0].Role)
	}
	if result[1].Role != "assistant" {
		t.Errorf("second role = %q, want assistant", result[1].Role)
	}
}

// ---------------------------------------------------------------------------
// 测试集 9：readAnthropicRequestBody 工具函数
// ---------------------------------------------------------------------------

func TestReadAnthropicRequestBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"test":"data"}`))
	body, ok := readAnthropicRequestBody(rec, req)
	if !ok {
		t.Fatal("readAnthropicRequestBody returned false")
	}
	if !strings.Contains(string(body), "test") {
		t.Errorf("body = %q, want test data", string(body))
	}
}

func TestReadAnthropicRequestBody_NilBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, ok := readAnthropicRequestBody(rec, req)
	if ok {
		t.Error("expected false for nil body")
	}
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestReadAnthropicRequestBody_EmptyBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	_, ok := readAnthropicRequestBody(rec, req)
	if ok {
		t.Error("expected false for empty body")
	}
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}