package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
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

// ---------------------------------------------------------------------------
// 测试集 9：chatRequestToAnthropicRequest
// 验证内部 ChatRequest → Anthropic 请求体的转换正确性。
// ---------------------------------------------------------------------------

func TestChatRequestToAnthropicRequest_BasicText(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{{Role: "user", Content: "Hello"}},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if anthropicReq.Model != "LongCat-2.0" {
		t.Errorf("model = %q, want LongCat-2.0", anthropicReq.Model)
	}
	if anthropicReq.MaxTokens != 100 {
		t.Errorf("max_tokens = %d, want 100", anthropicReq.MaxTokens)
	}
	if len(anthropicReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(anthropicReq.Messages))
	}
	if anthropicReq.Messages[0].Role != "user" {
		t.Errorf("role = %q, want user", anthropicReq.Messages[0].Role)
	}
	// content 应该是 JSON 字符串 "Hello"（带引号）
	var content string
	if err := json.Unmarshal(anthropicReq.Messages[0].Content, &content); err != nil {
		t.Fatalf("unmarshal content failed: %v", err)
	}
	if content != "Hello" {
		t.Errorf("content = %q, want Hello", content)
	}
}

func TestChatRequestToAnthropicRequest_WithSystem(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages: []provider.Message{
			{Role: "system", Content: "You are a helpful assistant"},
			{Role: "user", Content: "Hi"},
		},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if anthropicReq.System != "You are a helpful assistant" {
		t.Errorf("system = %q, want You are a helpful assistant", anthropicReq.System)
	}
	if len(anthropicReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1 (system removed from messages)", len(anthropicReq.Messages))
	}
	if anthropicReq.Messages[0].Role != "user" {
		t.Errorf("role = %q, want user", anthropicReq.Messages[0].Role)
	}
}

func TestChatRequestToAnthropicRequest_DefaultMaxTokens(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:    "LongCat-2.0",
		Messages: []provider.Message{{Role: "user", Content: "Hi"}},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if anthropicReq.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096 (default)", anthropicReq.MaxTokens)
	}
}

func TestChatRequestToAnthropicRequest_WithTools(t *testing.T) {
	params := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"location": map[string]interface{}{"type": "string"},
		},
	}
	paramsJSON, _ := json.Marshal(params)
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{{Role: "user", Content: "What's the weather?"}},
		Tools: []provider.Tool{
			{
				Type: "function",
				Function: provider.ToolFunc{
					Name:        "get_weather",
					Description: "Get weather for a location",
					Parameters:  paramsJSON,
				},
			},
		},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if len(anthropicReq.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(anthropicReq.Tools))
	}
	if anthropicReq.Tools[0].Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", anthropicReq.Tools[0].Name)
	}
	if anthropicReq.Tools[0].Description != "Get weather for a location" {
		t.Errorf("tool description = %q, want Get weather for a location", anthropicReq.Tools[0].Description)
	}
	if len(anthropicReq.Tools[0].InputSchema) == 0 {
		t.Error("input_schema should not be empty")
	}
}

func TestChatRequestToAnthropicRequest_WithToolCalls(t *testing.T) {
	args := `{"location":"Beijing"}`
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages: []provider.Message{
			{Role: "assistant", Content: "Let me check", ToolCalls: []provider.ToolCall{
				{ID: "call_1", Type: "function", Function: provider.FunctionCall{Name: "get_weather", Arguments: args}},
			}},
		},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if len(anthropicReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(anthropicReq.Messages))
	}
	// content 应该是 contentBlock 数组
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(anthropicReq.Messages[0].Content, &blocks); err != nil {
		t.Fatalf("unmarshal content blocks failed: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks len = %d, want 2 (text + tool_use)", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "Let me check" {
		t.Errorf("block[0] = %+v, want text block", blocks[0])
	}
	if blocks[1].Type != "tool_use" || blocks[1].ID != "call_1" || blocks[1].Name != "get_weather" {
		t.Errorf("block[1] = %+v, want tool_use block", blocks[1])
	}
}

func TestChatRequestToAnthropicRequest_WithToolResult(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages: []provider.Message{
			{Role: "user", Content: "28°C, sunny", ToolCallID: "call_1"},
		},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if len(anthropicReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(anthropicReq.Messages))
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(anthropicReq.Messages[0].Content, &blocks); err != nil {
		t.Fatalf("unmarshal content blocks failed: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks len = %d, want 1", len(blocks))
	}
	if blocks[0].Type != "tool_result" {
		t.Errorf("block type = %q, want tool_result", blocks[0].Type)
	}
	if blocks[0].ToolUseID != "call_1" {
		t.Errorf("tool_use_id = %q, want call_1", blocks[0].ToolUseID)
	}
	if blocks[0].Content != "28°C, sunny" {
		t.Errorf("content = %q, want 28°C, sunny", blocks[0].Content)
	}
}

func TestChatRequestToAnthropicRequest_WithStreaming(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{{Role: "user", Content: "Hi"}},
		Stream:    true,
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if !anthropicReq.Stream {
		t.Error("stream should be true")
	}
}

func TestChatRequestToAnthropicRequest_WithParameters(t *testing.T) {
	temp := 0.7
	topP := 0.9
	topK := 50
	chatReq := &provider.ChatRequest{
		Model:       "LongCat-2.0",
		MaxTokens:   intPtr(100),
		Temperature: &temp,
		TopP:        &topP,
		TopK:        &topK,
		Stop:        []string{"\n\n", "."},
		Messages:    []provider.Message{{Role: "user", Content: "Hi"}},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if anthropicReq.Temperature == nil || *anthropicReq.Temperature != 0.7 {
		t.Errorf("temperature = %v, want 0.7", anthropicReq.Temperature)
	}
	if anthropicReq.TopP == nil || *anthropicReq.TopP != 0.9 {
		t.Errorf("top_p = %v, want 0.9", anthropicReq.TopP)
	}
	if anthropicReq.TopK == nil || *anthropicReq.TopK != 50 {
		t.Errorf("top_k = %v, want 50", anthropicReq.TopK)
	}
	if len(anthropicReq.StopSequences) != 2 {
		t.Fatalf("stop_sequences len = %d, want 2", len(anthropicReq.StopSequences))
	}
	if anthropicReq.StopSequences[0] != "\n\n" || anthropicReq.StopSequences[1] != "." {
		t.Errorf("stop_sequences = %v, want [\"\\n\\n\", \".\"]", anthropicReq.StopSequences)
	}
}

// ---------------------------------------------------------------------------
// 测试集 10：anthropicResponseToChatResponse
// 验证 Anthropic 响应 → 内部 ChatResponse 的转换正确性。
// ---------------------------------------------------------------------------

func TestAnthropicResponseToChatResponse_BasicText(t *testing.T) {
	anthropicResp := &anthropicResponse{
		ID:         "msg_cmpl_123",
		Type:       "message",
		Role:       "assistant",
		Model:      "LongCat-2.0",
		StopReason: "end_turn",
		Content: []anthropicContentBlock{
			{Type: "text", Text: "Hello!"},
		},
		Usage: &anthropicUsage{InputTokens: 10, OutputTokens: 20},
	}

	resp := anthropicResponseToChatResponse(anthropicResp)

	if resp.ID != "cmpl_123" {
		t.Errorf("id = %q, want cmpl_123 (without msg_ prefix)", resp.ID)
	}
	if resp.Model != "LongCat-2.0" {
		t.Errorf("model = %q, want LongCat-2.0", resp.Model)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", resp.Object)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hello!" {
		t.Errorf("content = %q, want Hello!", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", resp.Choices[0].FinishReason)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 20 {
		t.Errorf("usage = %+v, want input=10 output=20", resp.Usage)
	}
}

func TestAnthropicResponseToChatResponse_WithThinking(t *testing.T) {
	anthropicResp := &anthropicResponse{
		ID:         "msg_think_1",
		Type:       "message",
		Role:       "assistant",
		Model:      "LongCat-2.0",
		StopReason: "end_turn",
		Content: []anthropicContentBlock{
			{Type: "thinking", Thinking: "Let me think step by step..."},
			{Type: "text", Text: "The answer is 42."},
		},
	}

	resp := anthropicResponseToChatResponse(anthropicResp)

	if resp.Choices[0].Message.Reasoning != "Let me think step by step..." {
		t.Errorf("reasoning = %q, want Let me think step by step...", resp.Choices[0].Message.Reasoning)
	}
	if resp.Choices[0].Message.Content != "The answer is 42." {
		t.Errorf("content = %q, want The answer is 42.", resp.Choices[0].Message.Content)
	}
}

func TestAnthropicResponseToChatResponse_WithToolUse(t *testing.T) {
	anthropicResp := &anthropicResponse{
		ID:         "msg_tool_1",
		Type:       "message",
		Role:       "assistant",
		Model:      "LongCat-2.0",
		StopReason: "tool_use",
		Content: []anthropicContentBlock{
			{Type: "text", Text: "Let me check the weather."},
			{Type: "tool_use", ID: "toolu_1", Name: "get_weather", Input: json.RawMessage(`{"location":"Beijing"}`)},
		},
	}

	resp := anthropicResponseToChatResponse(anthropicResp)

	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d, want 1", len(resp.Choices[0].Message.ToolCalls))
	}
	if resp.Choices[0].Message.ToolCalls[0].ID != "toolu_1" {
		t.Errorf("tool_call id = %q, want toolu_1", resp.Choices[0].Message.ToolCalls[0].ID)
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool_call function name = %q, want get_weather", resp.Choices[0].Message.ToolCalls[0].Function.Name)
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"location":"Beijing"}` {
		t.Errorf("tool_call arguments = %q, want {\"location\":\"Beijing\"}", resp.Choices[0].Message.ToolCalls[0].Function.Arguments)
	}
}

func TestAnthropicResponseToChatResponse_StopReasonMapping(t *testing.T) {
	tests := []struct {
		anthropicStop string
		wantOpenAI    string
	}{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"stop_sequence", "stop_sequence"},
		{"", "stop"},
	}

	for _, tt := range tests {
		anthropicResp := &anthropicResponse{
			ID:         "msg_1",
			Type:       "message",
			Role:       "assistant",
			Model:      "LongCat-2.0",
			StopReason: tt.anthropicStop,
			Content:    []anthropicContentBlock{{Type: "text", Text: "Hello"}},
		}
		resp := anthropicResponseToChatResponse(anthropicResp)
		if resp.Choices[0].FinishReason != tt.wantOpenAI {
			t.Errorf("StopReason=%q: got finish_reason=%q, want %q", tt.anthropicStop, resp.Choices[0].FinishReason, tt.wantOpenAI)
		}
	}
}

func TestAnthropicResponseToChatResponse_NoMsgPrefix(t *testing.T) {
	// 某些 Anthropic 上游的 ID 可能没有 msg_ 前缀
	anthropicResp := &anthropicResponse{
		ID:         "cmpl_123",
		Type:       "message",
		Role:       "assistant",
		Model:      "LongCat-2.0",
		StopReason: "end_turn",
		Content:    []anthropicContentBlock{{Type: "text", Text: "Hi"}},
	}
	resp := anthropicResponseToChatResponse(anthropicResp)
	if resp.ID != "cmpl_123" {
		t.Errorf("id = %q, want cmpl_123", resp.ID)
	}
}

func TestAnthropicResponseToChatResponse_NilUsage(t *testing.T) {
	anthropicResp := &anthropicResponse{
		ID:         "msg_1",
		Type:       "message",
		Role:       "assistant",
		Model:      "LongCat-2.0",
		StopReason: "end_turn",
		Content:    []anthropicContentBlock{{Type: "text", Text: "Hi"}},
		Usage:      nil,
	}
	resp := anthropicResponseToChatResponse(anthropicResp)
	if resp.Usage != nil {
		t.Error("usage should be nil when anthropic usage is nil")
	}
}

// ---------------------------------------------------------------------------
// 测试集 11：SendAnthropicChatRequest 集成测试
// 使用 mock upstream 验证完整的 HTTP 请求流程。
// ---------------------------------------------------------------------------

func TestSendAnthropicChatRequest_Success(t *testing.T) {
	// mock upstream 返回 Anthropic 格式响应
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		// 验证请求头：同时支持 x-api-key（官方 Anthropic）和 Authorization: Bearer（New API 网关）
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("x-api-key = %q, want sk-test", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", r.Header.Get("Authorization"))
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("anthropic-version = %q, want 2023-06-01", r.Header.Get("anthropic-version"))
		}
		// 验证请求体
		var reqBody map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request body failed: %v", err)
		}
		if reqBody["model"] != "LongCat-2.0" {
			t.Errorf("request model = %q, want LongCat-2.0", reqBody["model"])
		}
		if reqBody["max_tokens"] != float64(100) {
			t.Errorf("max_tokens = %v, want 100", reqBody["max_tokens"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_cmpl_test","type":"message","role":"assistant","model":"LongCat-2.0","content":[{"type":"text","text":"Hello!"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20}}`))
	}))
	defer upstream.Close()

	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{{Role: "user", Content: "Hi"}},
	}

	resp, err := SendAnthropicChatRequest(context.Background(), upstream.URL, "sk-test", chatReq)
	if err != nil {
		t.Fatalf("SendAnthropicChatRequest failed: %v", err)
	}
	if resp.ID != "cmpl_test" {
		t.Errorf("id = %q, want cmpl_test", resp.ID)
	}
	if resp.Choices[0].Message.Content != "Hello!" {
		t.Errorf("content = %q, want Hello!", resp.Choices[0].Message.Content)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 20 {
		t.Errorf("usage = %+v, want input=10 output=20", resp.Usage)
	}
}

func TestSendAnthropicChatRequest_UpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Invalid model"}}`))
	}))
	defer upstream.Close()

	chatReq := &provider.ChatRequest{
		Model:     "bad-model",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{{Role: "user", Content: "Hi"}},
	}

	_, err := SendAnthropicChatRequest(context.Background(), upstream.URL, "sk-test", chatReq)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should contain HTTP status, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Invalid model") {
		t.Errorf("error should contain upstream message, got: %v", err)
	}
}

func TestSendAnthropicChatRequest_ConnectionError(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{{Role: "user", Content: "Hi"}},
	}
	// 使用无法连接的地址
	_, err := SendAnthropicChatRequest(context.Background(), "http://127.0.0.1:1", "sk-test", chatReq)
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
}

// ---------------------------------------------------------------------------
// 测试集 12：handleChatCompletions 与 anthropic 类型 provider 集成测试
// 验证完整的 HTTP 请求处理流程：OpenAI 格式请求 → Anthropic 格式转换 → 上游 → 响应转换
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_AnthropicProviderNonStream(t *testing.T) {
	// mock upstream 返回 Anthropic 格式响应
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			// 验证请求为 Anthropic 格式
			var reqBody map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
				t.Fatalf("decode request body failed: %v", err)
			}
			if reqBody["max_tokens"] == nil {
				t.Error("max_tokens is required for Anthropic API")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"msg_cmpl_test","type":"message","role":"assistant","model":"LongCat-2.0","content":[{"type":"text","text":"Hello from Anthropic!"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`))
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

	// 创建 anthropic 类型 provider，指向 mock upstream
	prov := provider.NewOpenAIProviderWithTransport(
		"longcat2", "openai", "sk-test", upstream.URL,
		"v1/messages", "v1/models", true, 5*time.Second,
	)

	// 创建 server，配置中包含 anthropic 类型
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{
		{
			ID:      "longcat2",
			Name:    "longcat2",
			Type:    "anthropic",
			BaseURL: upstream.URL,
			APIKey:  "sk-test",
			Enabled: true,
			Transport: config.TransportConfig{
				ChatPath:   "v1/messages",
				ModelsPath: "v1/models",
			},
		},
	}

	inner := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})
	handler := server.loggingMiddleware(inner)

	body := `{"model":"LongCat-2.0","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// 验证响应为 OpenAI 格式
	var resp provider.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hello from Anthropic!" {
		t.Errorf("content = %q, want Hello from Anthropic!", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", resp.Choices[0].FinishReason)
	}
}

func TestHandleChatCompletions_AnthropicProviderWithSystemMessage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			var reqBody map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
				t.Fatalf("decode request body failed: %v", err)
			}
			// 验证 system 被提至顶层而非 messages 数组
			system, ok := reqBody["system"].(string)
			if !ok || system != "You are helpful" {
				t.Errorf("system = %q, want You are helpful", system)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"msg_sys_test","type":"message","role":"assistant","model":"LongCat-2.0","content":[{"type":"text","text":"I am helpful!"}],"stop_reason":"end_turn"}`))
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

	prov := provider.NewOpenAIProviderWithTransport(
		"longcat2", "openai", "sk-test", upstream.URL,
		"v1/messages", "v1/models", true, 5*time.Second,
	)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{
		{
			ID:      "longcat2",
			Name:    "longcat2",
			Type:    "anthropic",
			BaseURL: upstream.URL,
			APIKey:  "sk-test",
			Enabled: true,
			Transport: config.TransportConfig{
				ChatPath:   "v1/messages",
				ModelsPath: "v1/models",
			},
		},
	}

	inner := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})
	handler := server.loggingMiddleware(inner)

	body := `{"model":"LongCat-2.0","messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp provider.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if resp.Choices[0].Message.Content != "I am helpful!" {
		t.Errorf("content = %q, want I am helpful!", resp.Choices[0].Message.Content)
	}
}

func TestHandleChatCompletions_AnthropicProviderNotFound(t *testing.T) {
	// provider 未在 config 中标记为 anthropic 类型，应走 OpenAI ChatRaw 路径
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	prov := provider.NewOpenAIProviderWithTransport(
		"openai-provider", "openai", "sk-test", upstream.URL,
		"chat/completions", "models", true, time.Second,
	)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{
		{
			ID:      "openai-provider",
			Name:    "openai-provider",
			Type:    "openai",
			BaseURL: upstream.URL,
			APIKey:  "sk-test",
			Enabled: true,
			Transport: config.TransportConfig{
				ChatPath:   "chat/completions",
				ModelsPath: "models",
			},
		},
	}

	inner := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})
	handler := server.loggingMiddleware(inner)

	body := `{"model":"LongCat-2.0","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// openai 类型 provider 走 ChatRaw，会得到 404
	if rec.Code < 400 {
		t.Fatalf("expected error status for openai type, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 测试集 13：handleAnthropicMessages 直通转发集成测试
// 验证 anthropic 类型 provider 的直通转发路径。
// ---------------------------------------------------------------------------

func TestHandleAnthropicMessages_PassthroughNonStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		// 验证模型名已被清理（没有 @provider 后缀）
		var reqBody map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request body failed: %v", err)
		}
		if reqBody["model"] == "LongCat-2.0@longcat2" {
			t.Error("model should not contain @provider suffix")
		}
		// 验证认证头：同时支持 x-api-key（官方 Anthropic）和 Authorization: Bearer（New API 网关）
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("x-api-key = %q, want sk-test", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", r.Header.Get("Authorization"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_passthrough","type":"message","role":"assistant","content":[{"type":"text","text":"Passthrough OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	prov := provider.NewOpenAIProviderWithTransport(
		"longcat2", "openai", "sk-test", upstream.URL,
		"v1/messages", "v1/models", true, 5*time.Second,
	)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{
		{
			ID:      "longcat2",
			Name:    "longcat2",
			Type:    "anthropic",
			BaseURL: upstream.URL,
			APIKey:  "sk-test",
			Enabled: true,
			Transport: config.TransportConfig{
				ChatPath:   "v1/messages",
				ModelsPath: "v1/models",
			},
		},
	}

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
	if len(resp.Content) != 1 || resp.Content[0].Text != "Passthrough OK" {
		t.Errorf("content = %+v, want text=Passthrough OK", resp.Content)
	}
}

// ---------------------------------------------------------------------------
// 测试集 14：Round-trip 测试
// 验证 ChatRequest ↔ Anthropic JSON ↔ ChatRequest 的往返一致性。
// ---------------------------------------------------------------------------

func TestChatRequestToAnthropicRequest_RoundTrip(t *testing.T) {
	// 原始 ChatRequest
	temp := 0.7
	topP := 0.9
	topK := 50
	original := &provider.ChatRequest{
		Model:       "LongCat-2.0",
		MaxTokens:   intPtr(100),
		Temperature: &temp,
		TopP:        &topP,
		TopK:        &topK,
		Stop:        []string{"\n\n"},
		Messages: []provider.Message{
			{Role: "system", Content: "You are helpful"},
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi there!"},
		},
	}

	// Anthropic 格式 → ChatRequest
	anthropicReq := chatRequestToAnthropicRequest(original)
	chatReq := anthropicRequestToChatRequest(anthropicReq)

	// 验证关键字段
	if chatReq.Model != original.Model {
		t.Errorf("model roundtrip: got %q, want %q", chatReq.Model, original.Model)
	}
	if chatReq.MaxTokens == nil || *chatReq.MaxTokens != 100 {
		t.Errorf("max_tokens roundtrip: got %v, want 100", chatReq.MaxTokens)
	}
	if chatReq.Temperature == nil || *chatReq.Temperature != 0.7 {
		t.Errorf("temperature roundtrip: got %v, want 0.7", chatReq.Temperature)
	}
	if chatReq.TopP == nil || *chatReq.TopP != 0.9 {
		t.Errorf("top_p roundtrip: got %v, want 0.9", chatReq.TopP)
	}
	if chatReq.TopK == nil || *chatReq.TopK != 50 {
		t.Errorf("top_k roundtrip: got %v, want 50", chatReq.TopK)
	}
	// system 消息应该被提取到 messages[0]（anthropicRequestToChatRequest 从 system 字段还原）
	if len(chatReq.Messages) < 3 {
		t.Fatalf("messages len = %d, want >= 3", len(chatReq.Messages))
	}
	if chatReq.Messages[0].Role != "system" || chatReq.Messages[0].Content != "You are helpful" {
		t.Errorf("messages[0] = %+v, want system message", chatReq.Messages[0])
	}
	if chatReq.Messages[1].Role != "user" || chatReq.Messages[1].Content != "Hello" {
		t.Errorf("messages[1] = %+v, want user message", chatReq.Messages[1])
	}
	if chatReq.Messages[2].Role != "assistant" || chatReq.Messages[2].Content != "Hi there!" {
		t.Errorf("messages[2] = %+v, want assistant message", chatReq.Messages[2])
	}
}

func TestAnthropicResponseToChatResponse_RoundTrip(t *testing.T) {
	// 原始 ChatResponse
	original := &provider.ChatResponse{
		ID:     "cmpl_test",
		Object: "chat.completion",
		Model:  "LongCat-2.0",
		Choices: []provider.Choice{
			{
				Index: 0,
				Message: provider.Message{
					Role:    "assistant",
					Content: "Hello!",
				},
				FinishReason: "stop",
			},
		},
		Usage: &provider.Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}

	// ChatResponse → Anthropic 格式 → ChatResponse
	anthropicResp := chatResponseToAnthropicResponse(original, original.Model)
	chatResp := anthropicResponseToChatResponse(anthropicResp)

	if chatResp.ID != original.ID {
		t.Errorf("id roundtrip: got %q, want %q", chatResp.ID, original.ID)
	}
	if chatResp.Model != original.Model {
		t.Errorf("model roundtrip: got %q, want %q", chatResp.Model, original.Model)
	}
	if len(chatResp.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(chatResp.Choices))
	}
	if chatResp.Choices[0].Message.Content != "Hello!" {
		t.Errorf("content roundtrip: got %q, want Hello!", chatResp.Choices[0].Message.Content)
	}
	if chatResp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason roundtrip: got %q, want stop", chatResp.Choices[0].FinishReason)
	}
	if chatResp.Usage == nil || chatResp.Usage.PromptTokens != 10 || chatResp.Usage.CompletionTokens != 20 {
		t.Errorf("usage roundtrip: got %+v", chatResp.Usage)
	}
}

func TestAnthropicResponseToChatResponse_ThinkingRoundTrip(t *testing.T) {
	// 有 thinking 的 ChatResponse
	original := &provider.ChatResponse{
		ID:     "cmpl_think",
		Object: "chat.completion",
		Model:  "LongCat-2.0",
		Choices: []provider.Choice{
			{
				Index: 0,
				Message: provider.Message{
					Role:      "assistant",
					Content:   "The answer is 42.",
					Reasoning: "Let me think step by step...",
				},
				FinishReason: "stop",
			},
		},
	}

	anthropicResp := chatResponseToAnthropicResponse(original, original.Model)
	chatResp := anthropicResponseToChatResponse(anthropicResp)

	if chatResp.Choices[0].Message.Reasoning != "Let me think step by step..." {
		t.Errorf("reasoning roundtrip: got %q, want Let me think step by step...", chatResp.Choices[0].Message.Reasoning)
	}
	if chatResp.Choices[0].Message.Content != "The answer is 42." {
		t.Errorf("content roundtrip: got %q, want The answer is 42.", chatResp.Choices[0].Message.Content)
	}
}

// ---------------------------------------------------------------------------
// 测试集 15：边界情况测试
// ---------------------------------------------------------------------------

func TestChatRequestToAnthropicRequest_EmptyMessages(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if len(anthropicReq.Messages) != 0 {
		t.Errorf("messages len = %d, want 0", len(anthropicReq.Messages))
	}
	if anthropicReq.MaxTokens != 100 {
		t.Errorf("max_tokens = %d, want 100", anthropicReq.MaxTokens)
	}
}

func TestChatRequestToAnthropicRequest_MultipleToolCalls(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages: []provider.Message{
			{
				Role:    "assistant",
				Content: "Calling multiple tools",
				ToolCalls: []provider.ToolCall{
					{ID: "call_1", Type: "function", Function: provider.FunctionCall{Name: "get_weather", Arguments: `{"city":"Beijing"}`}},
					{ID: "call_2", Type: "function", Function: provider.FunctionCall{Name: "get_time", Arguments: `{"city":"Beijing"}`}},
				},
			},
		},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if len(anthropicReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(anthropicReq.Messages))
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(anthropicReq.Messages[0].Content, &blocks); err != nil {
		t.Fatalf("unmarshal blocks failed: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("blocks len = %d, want 3 (text + 2 tool_use)", len(blocks))
	}
	if blocks[1].ID != "call_1" || blocks[2].ID != "call_2" {
		t.Errorf("tool_use IDs: got %s, %s; want call_1, call_2", blocks[1].ID, blocks[2].ID)
	}
}

func TestChatRequestToAnthropicRequest_MultipleSystemMessages(t *testing.T) {
	// 多个 system 消息，只有第一个应该被保留
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages: []provider.Message{
			{Role: "system", Content: "First system"},
			{Role: "system", Content: "Second system"},
			{Role: "user", Content: "Hi"},
		},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if anthropicReq.System != "First system" {
		t.Errorf("system = %q, want First system (only first system msg preserved)", anthropicReq.System)
	}
	if len(anthropicReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1 (only user)", len(anthropicReq.Messages))
	}
}

func TestChatRequestToAnthropicRequest_MixedRoles(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages: []provider.Message{
			{Role: "system", Content: "SYSTEM"},
			{Role: "user", Content: "USER1"},
			{Role: "assistant", Content: "ASSISTANT1"},
			{Role: "user", Content: "USER2"},
			{Role: "assistant", Content: "ASSISTANT2"},
		},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if anthropicReq.System != "SYSTEM" {
		t.Errorf("system = %q, want SYSTEM", anthropicReq.System)
	}
	if len(anthropicReq.Messages) != 4 {
		t.Fatalf("messages len = %d, want 4 (user, assistant, user, assistant)", len(anthropicReq.Messages))
	}
	roles := []string{"user", "assistant", "user", "assistant"}
	for i, expected := range roles {
		if anthropicReq.Messages[i].Role != expected {
			t.Errorf("messages[%d].role = %q, want %q", i, anthropicReq.Messages[i].Role, expected)
		}
	}
}

func TestChatRequestToAnthropicRequest_SpecialCharacters(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages: []provider.Message{
			{Role: "user", Content: "Hello 世界! 🌍\nNew line\tTab"},
			{Role: "assistant", Content: "Special chars: <>&'\"/\\"},
		},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	if len(anthropicReq.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(anthropicReq.Messages))
	}
	var content1 string
	json.Unmarshal(anthropicReq.Messages[0].Content, &content1)
	if content1 != "Hello 世界! 🌍\nNew line\tTab" {
		t.Errorf("content[0] = %q, want Hello 世界! 🌍...", content1)
	}
	var content2 string
	json.Unmarshal(anthropicReq.Messages[1].Content, &content2)
	if content2 != "Special chars: <>&'\"/\\" {
		t.Errorf("content[1] = %q, want Special chars...", content2)
	}
}

func TestChatRequestToAnthropicRequest_ToolCallsOnlyNoContent(t *testing.T) {
	// assistant 消息只有 tool_calls，没有 text content
	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages: []provider.Message{
			{
				Role: "assistant",
				ToolCalls: []provider.ToolCall{
					{ID: "call_1", Type: "function", Function: provider.FunctionCall{Name: "get_weather", Arguments: `{}`}},
				},
			},
		},
	}
	anthropicReq := chatRequestToAnthropicRequest(chatReq)

	var blocks []anthropicContentBlock
	if err := json.Unmarshal(anthropicReq.Messages[0].Content, &blocks); err != nil {
		t.Fatalf("unmarshal blocks failed: %v", err)
	}
	// 只有 tool_use 块，没有 text 块（因为 Content 为空）
	if len(blocks) != 1 {
		t.Fatalf("blocks len = %d, want 1 (only tool_use)", len(blocks))
	}
	if blocks[0].Type != "tool_use" {
		t.Errorf("block type = %q, want tool_use", blocks[0].Type)
	}
}

// ---------------------------------------------------------------------------
// 测试集 16：SendAnthropicChatRequest 进阶集成测试
// ---------------------------------------------------------------------------

func TestSendAnthropicChatRequest_WithThinking(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_think","type":"message","role":"assistant","model":"LongCat-2.0","content":[{"type":"thinking","thinking":"Let me think..."},{"type":"text","text":"Answer: 42"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":8}}`))
	}))
	defer upstream.Close()

	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{{Role: "user", Content: "What is 6*7?"}},
	}

	resp, err := SendAnthropicChatRequest(context.Background(), upstream.URL, "sk-test", chatReq)
	if err != nil {
		t.Fatalf("SendAnthropicChatRequest failed: %v", err)
	}
	if resp.Choices[0].Message.Reasoning != "Let me think..." {
		t.Errorf("reasoning = %q, want Let me think...", resp.Choices[0].Message.Reasoning)
	}
	if resp.Choices[0].Message.Content != "Answer: 42" {
		t.Errorf("content = %q, want Answer: 42", resp.Choices[0].Message.Content)
	}
}

func TestSendAnthropicChatRequest_WithToolUse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_tool","type":"message","role":"assistant","model":"LongCat-2.0","content":[{"type":"text","text":"Checking weather"},{"type":"tool_use","id":"toolu_abc","name":"get_weather","input":{"city":"Beijing"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{{Role: "user", Content: "Weather in Beijing?"}},
	}

	resp, err := SendAnthropicChatRequest(context.Background(), upstream.URL, "sk-test", chatReq)
	if err != nil {
		t.Fatalf("SendAnthropicChatRequest failed: %v", err)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d, want 1", len(resp.Choices[0].Message.ToolCalls))
	}
	if resp.Choices[0].Message.ToolCalls[0].ID != "toolu_abc" {
		t.Errorf("tool_call id = %q, want toolu_abc", resp.Choices[0].Message.ToolCalls[0].ID)
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", resp.Choices[0].Message.ToolCalls[0].Function.Name)
	}
}

func TestSendAnthropicChatRequest_EmptyResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{{Role: "user", Content: "Hi"}},
	}

	resp, err := SendAnthropicChatRequest(context.Background(), upstream.URL, "sk-test", chatReq)
	if err != nil {
		t.Fatalf("SendAnthropicChatRequest failed: %v", err)
	}
	// 空响应 `{}` 会解析为空的 anthropicResponse，但转换函数始终创建至少一个 choice
	if len(resp.Choices) == 0 {
		t.Error("expected at least 1 choice for empty response")
	}
	if resp.Choices[0].Message.Content != "" {
		t.Errorf("content = %q, want empty", resp.Choices[0].Message.Content)
	}
}

func TestSendAnthropicChatRequest_InvalidJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid json`))
	}))
	defer upstream.Close()

	chatReq := &provider.ChatRequest{
		Model:     "LongCat-2.0",
		MaxTokens: intPtr(100),
		Messages:  []provider.Message{{Role: "user", Content: "Hi"}},
	}

	_, err := SendAnthropicChatRequest(context.Background(), upstream.URL, "sk-test", chatReq)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "解析失败") {
		t.Errorf("error should contain parse failure message, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 测试集 17：forwardAnthropicRequest 模型名清理测试
// ---------------------------------------------------------------------------

func TestForwardAnthropicRequest_ModelNameCleaning(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		var reqBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&reqBody)
		upstreamModel, _ = reqBody["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`))
	}))
	defer upstream.Close()

	prov := provider.NewOpenAIProviderWithTransport(
		"longcat2", "openai", "sk-test", upstream.URL,
		"v1/messages", "v1/models", true, 5*time.Second,
	)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{
		{ID: "longcat2", Name: "longcat2", Type: "anthropic", BaseURL: upstream.URL, APIKey: "sk-test", Enabled: true,
			Transport: config.TransportConfig{ChatPath: "v1/messages", ModelsPath: "v1/models"}},
	}

	// 测试 @provider 后缀被清理
	originalBody := []byte(`{"model":"LongCat-2.0@longcat2","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(originalBody)))
	req.Header.Set("x-api-key", "sk-test")

	err := server.forwardAnthropicRequest(context.Background(), rec, req, prov, originalBody, "LongCat-2.0@longcat2")
	if err != nil {
		t.Fatalf("forwardAnthropicRequest failed: %v", err)
	}
	if upstreamModel != "LongCat-2.0" {
		t.Errorf("upstream model = %q, want LongCat-2.0 (without @provider)", upstreamModel)
	}
}

func TestForwardAnthropicRequest_AuthHeaderPrecedence(t *testing.T) {
	var upstreamAuth, upstreamApiKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		upstreamApiKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`))
	}))
	defer upstream.Close()

	prov := provider.NewOpenAIProviderWithTransport(
		"longcat2", "openai", "sk-test", upstream.URL,
		"v1/messages", "v1/models", true, 5*time.Second,
	)
	server := newTestServer(prov)
	server.config.Providers = []config.ProviderConfig{
		{ID: "longcat2", Name: "longcat2", Type: "anthropic", BaseURL: upstream.URL, APIKey: "sk-test", Enabled: true,
			Transport: config.TransportConfig{ChatPath: "v1/messages", ModelsPath: "v1/models"}},
	}

	// 测试 Authorization 优先于 x-api-key
	originalBody := []byte(`{"model":"LongCat-2.0","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(originalBody)))
	req.Header.Set("Authorization", "Bearer custom-token")
	req.Header.Set("x-api-key", "fallback-key")

	err := server.forwardAnthropicRequest(context.Background(), rec, req, prov, originalBody, "LongCat-2.0")
	if err != nil {
		t.Fatalf("forwardAnthropicRequest failed: %v", err)
	}
	if upstreamAuth != "Bearer custom-token" {
		t.Errorf("Authorization = %q, want Bearer custom-token", upstreamAuth)
	}
	if upstreamApiKey != "fallback-key" {
		t.Errorf("x-api-key = %q, want fallback-key", upstreamApiKey)
	}
}

// ---------------------------------------------------------------------------
// 测试集 18：getProviderTypeFromConfig / findProviderConfig 辅助函数测试
// ---------------------------------------------------------------------------

func TestGetProviderTypeFromConfig(t *testing.T) {
	cfg := &config.AppConfig{
		Providers: []config.ProviderConfig{
			{ID: "longcat", Name: "LongCat", Type: "openai"},
			{ID: "longcat2", Name: "longcat2", Type: "anthropic"},
			{ID: "ollama", Name: "ollama", Type: "ollama"},
		},
	}

	tests := []struct {
		name    string
		provName string
		want    string
	}{
		{"find by ID", "longcat2", "anthropic"},
		{"find by Name", "longcat2", "anthropic"},
		{"openai type", "longcat", "openai"},
		{"ollama type", "ollama", "ollama"},
		{"not found", "nonexistent", ""},
		{"nil config", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if tt.name == "nil config" {
				got = getProviderTypeFromConfig(nil, tt.provName)
			} else {
				got = getProviderTypeFromConfig(cfg, tt.provName)
			}
			if got != tt.want {
				t.Errorf("getProviderTypeFromConfig(%q) = %q, want %q", tt.provName, got, tt.want)
			}
		})
	}
}

func TestFindProviderConfig(t *testing.T) {
	cfg := &config.AppConfig{
		Providers: []config.ProviderConfig{
			{ID: "longcat", Name: "LongCat", Type: "openai", BaseURL: "https://api.longcat.chat/openai"},
			{ID: "longcat2", Name: "longcat2", Type: "anthropic", BaseURL: "https://api.longcat.chat/anthropic"},
		},
	}

	// 按 ID 查找
	pc := findProviderConfig(cfg, "longcat2")
	if pc == nil {
		t.Fatal("findProviderConfig returned nil for longcat2")
	}
	if pc.Type != "anthropic" {
		t.Errorf("type = %q, want anthropic", pc.Type)
	}
	if pc.BaseURL != "https://api.longcat.chat/anthropic" {
		t.Errorf("base_url = %q, want https://api.longcat.chat/anthropic", pc.BaseURL)
	}

	// 按 Name 查找（ID 为空时）
	pc2 := findProviderConfig(cfg, "LongCat")
	if pc2 == nil {
		t.Fatal("findProviderConfig returned nil for LongCat")
	}
	if pc2.Type != "openai" {
		t.Errorf("type = %q, want openai", pc2.Type)
	}

	// 不存在的 provider
	pc3 := findProviderConfig(cfg, "nonexistent")
	if pc3 != nil {
		t.Errorf("findProviderConfig for nonexistent should return nil, got %+v", pc3)
	}

	// nil config
	pc4 := findProviderConfig(nil, "longcat2")
	if pc4 != nil {
		t.Errorf("findProviderConfig with nil config should return nil, got %+v", pc4)
	}
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------