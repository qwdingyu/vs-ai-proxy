package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// Anthropic Messages API 协议转换器
//
// 使 vs-ai-proxy 接收 Anthropic 原生 Messages API 请求（POST /v1/messages），
// 转换为内部 ChatRequest 后通过现有 provider 路由转发，再将响应转换回
// Anthropic 格式。流式场景同时处理 OpenAI SSE → Anthropic event-based SSE 的转换。
// ---------------------------------------------------------------------------

// anthropicMessage 是 Anthropic Messages API 中 messages 数组的元素。
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | []contentBlock
}

// anthropicContentBlock 是 Anthropic content 数组中的块。
// Anthropic Messages API 的 content 块类型：
//   - text:      {"type":"text","text":"..."}
//   - thinking:  {"type":"thinking","thinking":"..."}
//   - tool_use:  {"type":"tool_use","id":"...","name":"...","input":{...}}
//   - tool_result: {"type":"tool_result","tool_use_id":"...","content":"..."}
type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// anthropicTool 是 Anthropic 工具定义格式。
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicToolChoice 是 Anthropic 工具选择格式。
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// anthropicRequest 是 Anthropic Messages API 的完整请求体。
type anthropicRequest struct {
	Model         string                `json:"model"`
	MaxTokens     int                   `json:"max_tokens"`
	Messages      []anthropicMessage    `json:"messages"`
	System        string                `json:"system,omitempty"`
	Temperature   *float64              `json:"temperature,omitempty"`
	TopP          *float64              `json:"top_p,omitempty"`
	TopK          *int                  `json:"top_k,omitempty"`
	StopSequences []string              `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool       `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice  `json:"tool_choice,omitempty"`
	Stream        bool                  `json:"stream,omitempty"`
	Metadata      map[string]string     `json:"metadata,omitempty"`
}

// anthropicResponse 是 Anthropic Messages API 的完整响应体。
type anthropicResponse struct {
	ID           string                    `json:"id"`
	Type         string                    `json:"type"`
	Role         string                    `json:"role"`
	Content      []anthropicContentBlock   `json:"content"`
	Model        string                    `json:"model"`
	StopReason   string                    `json:"stop_reason"`
	StopSequence *string                   `json:"stop_sequence"`
	Usage        *anthropicUsage           `json:"usage,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// anthropicStreamEvent 是 Anthropic 流式事件的 data 负载。
type anthropicStreamEvent struct {
	Type         string                  `json:"type"`
	Index        int                     `json:"index,omitempty"`
	Message      *anthropicResponse      `json:"message,omitempty"`
	ContentBlock *anthropicContentBlock  `json:"content_block,omitempty"`
	Delta        *anthropicStreamDelta   `json:"delta,omitempty"`
	Usage        *anthropicUsage         `json:"usage,omitempty"`
}

type anthropicStreamDelta struct {
	Type         string `json:"type,omitempty"`
	Text         string `json:"text,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
	PartialJSON  string `json:"partial_json,omitempty"`
}

// ---------------------------------------------------------------------------
// 请求转换：Anthropic → 内部 ChatRequest
// ---------------------------------------------------------------------------

// anthropicRequestToChatRequest 将 Anthropic 格式的请求转换为内部 ChatRequest。
func anthropicRequestToChatRequest(anthropicReq *anthropicRequest) *provider.ChatRequest {
	req := &provider.ChatRequest{
		Model:       anthropicReq.Model,
		Stream:      anthropicReq.Stream,
		Temperature: anthropicReq.Temperature,
		TopP:        anthropicReq.TopP,
		TopK:        anthropicReq.TopK,
		Stop:        anthropicReq.StopSequences,
	}

	// max_tokens 是必填字段
	if anthropicReq.MaxTokens > 0 {
		maxTokens := anthropicReq.MaxTokens
		req.MaxTokens = &maxTokens
	}

	// system 消息：Anthropic 用 system 顶层字段，转为 role=system 的消息
	messages := make([]provider.Message, 0)
	if anthropicReq.System != "" {
		messages = append(messages, provider.Message{
			Role:    "system",
			Content: anthropicReq.System,
		})
	}

	// 逐条转换 messages
	for _, msg := range anthropicReq.Messages {
		pm := provider.Message{
			Role: msg.Role,
		}

		// content 可能是字符串或 contentBlock 数组
		if len(msg.Content) > 0 && msg.Content[0] == '"' {
			// 字符串格式
			json.Unmarshal(msg.Content, &pm.Content)
		} else if len(msg.Content) > 0 && msg.Content[0] == '[' {
			// 数组格式：提取所有 text 块拼接
			var blocks []anthropicContentBlock
			if err := json.Unmarshal(msg.Content, &blocks); err == nil {
				var textParts []string
				for _, block := range blocks {
					switch block.Type {
					case "text":
						textParts = append(textParts, block.Text)
					case "tool_use":
						// tool_use 块 → tool_calls
						argsJSON, _ := json.Marshal(block.Input)
						pm.ToolCalls = append(pm.ToolCalls, provider.ToolCall{
							ID:   block.ID,
							Type: "function",
							Function: provider.FunctionCall{
								Name:      block.Name,
								Arguments: string(argsJSON),
							},
						})
					case "tool_result":
						// tool_result 块 → tool_call_id + content
						// tool_result 使用 tool_use_id 字段（不是 id）
						pm.ToolCallID = block.ToolUseID
						textParts = append(textParts, block.Text)
					}
				}
				pm.Content = strings.Join(textParts, "")
			}
		}
		messages = append(messages, pm)
	}
	req.Messages = messages

	// 工具定义转换
	if len(anthropicReq.Tools) > 0 {
		tools := make([]provider.Tool, 0, len(anthropicReq.Tools))
		for _, t := range anthropicReq.Tools {
			paramsJSON, _ := json.Marshal(t.InputSchema)
			tools = append(tools, provider.Tool{
				Type: "function",
				Function: provider.ToolFunc{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  paramsJSON,
				},
			})
		}
		req.Tools = tools
	}

	// tool_choice 转换
	if anthropicReq.ToolChoice != nil {
		if req.Extra == nil {
			req.Extra = make(map[string]json.RawMessage)
		}
		// Anthropic tool_choice 格式：{"type":"auto"} / {"type":"any"} / {"type":"tool","name":"xxx"}
		// 转换为 OpenAI 格式：{"type":"auto"} / {"type":"any"} / {"type":"function","function":{"name":"xxx"}}
		tc := map[string]interface{}{
			"type": anthropicReq.ToolChoice.Type,
		}
		if anthropicReq.ToolChoice.Type == "tool" && anthropicReq.ToolChoice.Name != "" {
			tc["type"] = "function"
			tc["function"] = map[string]interface{}{
				"name": anthropicReq.ToolChoice.Name,
			}
		}
		tcJSON, _ := json.Marshal(tc)
		req.Extra["tool_choice"] = tcJSON
	}

	return req
}

// ---------------------------------------------------------------------------
// 响应转换：内部 ChatResponse → Anthropic 格式
// ---------------------------------------------------------------------------

// chatResponseToAnthropicResponse 将内部 ChatResponse 转换为 Anthropic 格式。
func chatResponseToAnthropicResponse(resp *provider.ChatResponse, baseModel string) *anthropicResponse {
	anthropicResp := &anthropicResponse{
		ID:   "msg_" + resp.ID,
		Type: "message",
		Role: "assistant",
	}

	// 模型名
	if resp.Model != "" {
		anthropicResp.Model = resp.Model
	} else {
		anthropicResp.Model = baseModel
	}

	// finish_reason 映射
	if len(resp.Choices) > 0 {
		reason := resp.Choices[0].FinishReason
		switch reason {
		case "stop":
			anthropicResp.StopReason = "end_turn"
		case "length":
			anthropicResp.StopReason = "max_tokens"
		case "tool_calls":
			anthropicResp.StopReason = "tool_use"
		default:
			anthropicResp.StopReason = reason
		}
	} else {
		anthropicResp.StopReason = "end_turn"
	}

	// content 数组
	content := make([]anthropicContentBlock, 0)
	if len(resp.Choices) > 0 {
		msg := resp.Choices[0].Message

		// 有 reasoning_content 时先插入 thinking 块（Anthropic 用 thinking 字段，不是 text）
		if msg.Reasoning != "" {
			content = append(content, anthropicContentBlock{
				Type:     "thinking",
				Thinking: msg.Reasoning,
			})
		}

		// text 块
		if msg.Content != "" {
			content = append(content, anthropicContentBlock{
				Type: "text",
				Text: msg.Content,
			})
		}

		// tool_calls → tool_use 块
		for _, tc := range msg.ToolCalls {
			content = append(content, anthropicContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(tc.Function.Arguments),
			})
		}
	}
	anthropicResp.Content = content

	// usage 映射
	if resp.Usage != nil {
		anthropicResp.Usage = &anthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	}

	return anthropicResp
}

// ---------------------------------------------------------------------------
// 流式转换：OpenAI SSE → Anthropic event-based SSE
// ---------------------------------------------------------------------------

// anthropicStreamWriter 实现 openAIStreamEventTarget 接口，
// 将 OpenAI SSE delta 事件转换为 Anthropic event-based SSE 格式写出。
type anthropicStreamWriter struct {
	writer          io.Writer
	flusher         http.Flusher
	model           string
	messageID       string
	contentIndex    int
	hasSentStart    bool
	hasSentBlock    bool
	finishReason    string
	inputTokens     int64
	outputTokens    int64
}

func (w *anthropicStreamWriter) Write(data []byte) (int, error) {
	// 解析 OpenAI SSE 行
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(trimmed[5:])
		if payload == "[DONE]" {
			continue
		}

		var chunk struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Index        int `json:"index"`
				Delta        json.RawMessage `json:"delta"`
				FinishReason string          `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}

		// 设置模型名
		if chunk.Model != "" && w.model == "" {
			w.model = chunk.Model
		}
		// 设置消息 ID
		if chunk.ID != "" && w.messageID == "" {
			w.messageID = "msg_" + chunk.ID
		}

		// 解析 delta
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		var delta struct {
			Role      string          `json:"role"`
			Content   string          `json:"content"`
			Reasoning string          `json:"reasoning_content"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		}
		json.Unmarshal(choice.Delta, &delta)

		// 更新 finish reason
		if choice.FinishReason != "" {
			w.finishReason = choice.FinishReason
		}

		// 发送 message_start（首次收到 delta 时）
		if !w.hasSentStart {
			w.hasSentStart = true
			w.emitEvent("message_start", anthropicStreamEvent{
				Type: "message_start",
				Message: &anthropicResponse{
					ID:   w.messageID,
					Type: "message",
					Role: "assistant",
					Content: []anthropicContentBlock{},
					Model: w.model,
				},
			})
		}

		// 处理 reasoning_content → thinking 块
		if delta.Reasoning != "" {
			if !w.hasSentBlock {
				w.hasSentBlock = true
				w.emitEvent("content_block_start", anthropicStreamEvent{
					Type:  "content_block_start",
					Index: w.contentIndex,
					ContentBlock: &anthropicContentBlock{
						Type:     "thinking",
						Thinking: "",
					},
				})
			}
			w.emitEvent("content_block_delta", anthropicStreamEvent{
				Type:  "content_block_delta",
				Index: w.contentIndex,
				Delta: &anthropicStreamDelta{
					Type: "thinking_delta",
					Text: delta.Reasoning,
				},
			})
		}

		// 处理 content → text 块
		// 注意：某些上游可能在同一 chunk 中同时发送 reasoning_content 和 content，
		// 这时需要先关闭 thinking 块再开 text 块。
		if delta.Content != "" {
			if w.hasSentBlock {
				// 关闭当前块（可能是 thinking 或 text）
				w.emitEvent("content_block_stop", anthropicStreamEvent{
					Type:  "content_block_stop",
					Index: w.contentIndex,
				})
				w.contentIndex++
				w.hasSentBlock = false
			}
			// 开新的 text 块
			w.hasSentBlock = true
			w.emitEvent("content_block_start", anthropicStreamEvent{
				Type:  "content_block_start",
				Index: w.contentIndex,
				ContentBlock: &anthropicContentBlock{
					Type: "text",
					Text: "",
				},
			})
			w.outputTokens += countTokens(delta.Content)
			w.emitEvent("content_block_delta", anthropicStreamEvent{
				Type:  "content_block_delta",
				Index: w.contentIndex,
				Delta: &anthropicStreamDelta{
					Type: "text_delta",
					Text: delta.Content,
				},
			})
		}

		// 处理 tool_calls → tool_use 块
		if len(delta.ToolCalls) > 0 {
			var toolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			}
			if err := json.Unmarshal(delta.ToolCalls, &toolCalls); err == nil {
				for _, tc := range toolCalls {
					if w.hasSentBlock {
						w.emitEvent("content_block_stop", anthropicStreamEvent{
							Type:  "content_block_stop",
							Index: w.contentIndex,
						})
						w.contentIndex++
						w.hasSentBlock = false
					}
					w.emitEvent("content_block_start", anthropicStreamEvent{
						Type:  "content_block_start",
						Index: w.contentIndex,
						ContentBlock: &anthropicContentBlock{
							Type: "tool_use",
							ID:   tc.ID,
							Name: tc.Function.Name,
							Input: json.RawMessage(tc.Function.Arguments),
						},
					})
					w.emitEvent("content_block_delta", anthropicStreamEvent{
						Type:  "content_block_delta",
						Index: w.contentIndex,
						Delta: &anthropicStreamDelta{
							Type:       "input_json_delta",
							PartialJSON: tc.Function.Arguments,
						},
					})
					w.emitEvent("content_block_stop", anthropicStreamEvent{
						Type:  "content_block_stop",
						Index: w.contentIndex,
					})
					w.contentIndex++
				}
			}
		}
	}

	return len(data), nil
}

// emitEvent 写出一个 Anthropic 流式事件。
func (w *anthropicStreamWriter) emitEvent(eventType string, event anthropicStreamEvent) {
	data, _ := json.Marshal(event)
	fmt.Fprintf(w.writer, "event: %s\n", eventType)
	fmt.Fprintf(w.writer, "data: %s\n\n", data)
	w.flusher.Flush()
}

// finish 写出 message_delta + message_stop 结束事件。
// 如果没有任何内容被流式写出（hasSentStart==false），则跳过结束事件。
func (w *anthropicStreamWriter) finish() {
	if !w.hasSentStart {
		return
	}

	// 关闭最后一个 content block
	if w.hasSentBlock {
		w.emitEvent("content_block_stop", anthropicStreamEvent{
			Type:  "content_block_stop",
			Index: w.contentIndex,
		})
	}

	// 映射 finish_reason
	stopReason := "end_turn"
	switch w.finishReason {
	case "stop":
		stopReason = "end_turn"
	case "length":
		stopReason = "max_tokens"
	case "tool_calls":
		stopReason = "tool_use"
	}

	// message_delta
	w.emitEvent("message_delta", anthropicStreamEvent{
		Type: "message_delta",
		Delta: &anthropicStreamDelta{
			StopReason: stopReason,
		},
		Usage: &anthropicUsage{
			OutputTokens: w.outputTokens,
		},
	})

	// message_stop
	w.emitEvent("message_stop", anthropicStreamEvent{
		Type: "message_stop",
	})
}

// ---------------------------------------------------------------------------
// 非流式响应转换
// ---------------------------------------------------------------------------

// writeAnthropicNonStreamResponse 将内部 ChatResponse 作为 Anthropic 格式 JSON 写出。
func writeAnthropicNonStreamResponse(w http.ResponseWriter, resp *provider.ChatResponse, baseModel string) {
	anthropicResp := chatResponseToAnthropicResponse(resp, baseModel)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(anthropicResp)
}

// writeAnthropicErrorResponse 写出 Anthropic 格式的错误响应。
func writeAnthropicErrorResponse(w http.ResponseWriter, statusCode int, message string) {
	errResp := map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": message,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(errResp)
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

// countTokens 估算 token 数量（简单按字符/4 估算，用于流式输出 token 计数）。
func countTokens(text string) int64 {
	if len(text) == 0 {
		return 0
	}
	// 粗略估算：英文约 4 字符/token，中文约 1.5 字符/token
	return int64(len(text)) / 2
}

// readAnthropicRequestBody 读取并验证请求体。
func readAnthropicRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "请求体为空")
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "读取请求体失败")
		return nil, false
	}
	if len(body) == 0 {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "请求体为空")
		return nil, false
	}
	return body, true
}

// validateAnthropicRequest 校验 Anthropic 请求的必要字段。
func validateAnthropicRequest(req *anthropicRequest) error {
	if req.Model == "" {
		return fmt.Errorf("model 字段是必填的")
	}
	if req.MaxTokens <= 0 {
		return fmt.Errorf("max_tokens 字段是必填的且必须大于 0")
	}
	if len(req.Messages) == 0 {
		return fmt.Errorf("messages 字段是必填的")
	}
	return nil
}

// parseAnthropicStreamChunk 解析 OpenAI SSE 数据块，提取对人类可读的文本摘要。
// 用于 handleAnthropicMessages 流式模式中的错误诊断。
func parseAnthropicStreamChunk(data []byte) string {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return ""
	}
	if len(chunk.Choices) > 0 {
		return chunk.Choices[0].Delta.Content
	}
	return ""
}

// ---------------------------------------------------------------------------
// 工具转换辅助函数
// ---------------------------------------------------------------------------

// buildAnthropicSystemMessage 从 messages 中提取 system 角色的消息。
// Anthropic 的 system 是顶层字段，不放在 messages 数组中。
// 这里用来兼容 OpenAI 风格的 system 消息。
func buildAnthropicSystemMessage(messages []provider.Message) string {
	for _, m := range messages {
		if m.Role == "system" && m.Content != "" {
			return m.Content
		}
	}
	return ""
}

// buildAnthropicMessages 过滤掉 system 消息，只保留 user/assistant 消息。
func buildAnthropicMessages(messages []provider.Message) []anthropicMessage {
	result := make([]anthropicMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		content := m.Content
		contentJSON, _ := json.Marshal(content)
		am := anthropicMessage{
			Role:    m.Role,
			Content: contentJSON,
		}
		result = append(result, am)
	}
	return result
}

// Ensure handler_anthropic.go implements the required interfaces.
var _ io.Writer = (*anthropicStreamWriter)(nil)

// ---------------------------------------------------------------------------
// 主 Handler：handleAnthropicMessages
//
// 接收 Anthropic Messages API 协议的 POST /v1/messages 请求，
// 转换为内部 ChatRequest 后复用现有 provider 路由逻辑，
// 最后将响应转换回 Anthropic 格式。
// 流式时同时处理 OpenAI SSE → Anthropic event-based SSE 的转换。
// ---------------------------------------------------------------------------

// handleAnthropicMessages Anthropic 聊天补全
// 对外暴露 Anthropic 兼容的 /v1/messages 接口，
// 使 Anthropic 原生客户端（Claude Code CLI 等）能通过代理使用模型。
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()
	body, ok := readAnthropicRequestBody(w, r)
	if !ok {
		return
	}

	// Anthropic 官方客户端要求 anthropic-version 头，但不做严格校验
	_ = r.Header.Get("anthropic-version")

	var anthropicReq anthropicRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	if err := validateAnthropicRequest(&anthropicReq); err != nil {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// 转换为内部 ChatRequest
	req := anthropicRequestToChatRequest(&anthropicReq)
	setRequestToolDiagnosticHeader(w, req)

	cfg, registry, catalog := s.snapshot()
	if catalog != nil {
		catalog.Rebuild()
	}

	// 解析模型
	modelName := req.Model
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	baseReq := cloneChatRequest(req)

	var lastErr error
	attempts := []attemptDiagnostic{}
	candidates := applyDefenseCandidatePolicy(cfg, registry.ResolveCandidates(modelName))
	resolvedModel := registry.ResolveModel(modelName)
	setCandidateDiagnosticHeaders(w, modelName, resolvedModel, candidates)
	if len(candidates) == 0 {
		if registry.HasAmbiguousModelAlias(modelName) {
			writeProxyDiagnosticError(w, http.StatusBadRequest, ambiguousModelAliasDiagnostic(modelName, resolvedModel))
			return
		}
		writeProxyDiagnosticError(w, http.StatusBadRequest, noCandidateDiagnostic(modelName, resolvedModel, len(candidates)))
		return
	}

	for _, cand := range candidates {
		provEntry := cand.Provider
		prov := provEntry.Provider
		if !prov.IsEnabled() {
			continue
		}

		modelID := cand.ModelID
		if modelID == "" {
			modelID = modelName
		}
		attemptStart := time.Now()
		setAttemptDiagnosticHeaders(w, prov.Name(), modelID)
		setResponseLogFields(w, prov.Name(), modelName, modelID)

		req := cloneChatRequest(baseReq)
		req.Model = modelID

		s.transformRequest(cfg, req, modelName, prov)

		var profile provider.ModelProfile
		hasProfile := false
		if catalog != nil {
			if p, ok := profileForProvider(catalog, modelName, prov); ok {
				profile = p
				hasProfile = true
			}
		}
		if modelCfg, ok := findModelConfig(cfg, modelName, modelID, prov.Name()); ok {
			profile = mergeModelConfigProfile(profile, modelCfg)
			hasProfile = true
		}
		if hasProfile {
			s.applyProfileDefaults(req, profile, prov)
		}
		setUpstreamRequestBytes(w, req)
		configuredTimeout, effectiveTimeout := modelTimeoutSeconds(cfg, modelName, modelID, prov.Name(), profile, hasProfile)
		setTimeoutDiagnostic(w, configuredTimeout, effectiveTimeout)
		ctx, cancel := requestContextWithTimeout(
			r.Context(),
			effectiveTimeout,
		)

		// 流式处理
		if req.Stream {
			streamReq := r.WithContext(ctx)
			streamWriter := &streamAttemptWriter{ResponseWriter: w}
			err := s.handleAnthropicStream(streamWriter, streamReq, prov, req, modelName)
			cancel()
			if err != nil {
				if isClientGoneError(err) {
					return
				}
				lastErr = err
				attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, err)
				attempts = append(attempts, attempt)
				s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
				if isClientGoneError(err) {
					return
				}
				if streamWriter.HasWritten() {
					markWrittenStreamFailure(w, attempt)
					registry.RecordCandidateFailure(prov.Name(), err)
					return
				}
				registry.RecordCandidateFailure(prov.Name(), err)
				if shouldStopCandidateFallback(attempt.Category) {
					break
				}
				continue
			}
			registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
			return
		}

		// 非流式处理
		if provider.ResolveApiFormat(prov) == provider.ApiFormatOpenAi {
			if rawProvider, ok := prov.(rawOpenAIChatProvider); ok {
				body, err := rawProvider.ChatRaw(ctx, req)
				if err != nil {
					cancel()
					if isClientGoneError(err) {
						return
					}
					lastErr = err
					attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, err)
					attempts = append(attempts, attempt)
					s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
					registry.RecordCandidateFailure(prov.Name(), err)
					if shouldStopCandidateFallback(attempt.Category) {
						break
					}
					continue
				}
				cancel()

				// 处理非流式 SSE 聚合
				if looksLikeSSEBody(body) {
					converted, convErr := openAIStreamBodyToChatResponse(body, req.Model, allowedToolNames(req))
					if convErr != nil {
						lastErr = fmt.Errorf("解析响应失败: Anthropic 非流式 SSE 聚合失败: %w", convErr)
						attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, lastErr)
						attempts = append(attempts, attempt)
						s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
						registry.RecordCandidateFailure(prov.Name(), lastErr)
						if shouldStopCandidateFallback(attempt.Category) {
							break
						}
						continue
					}
					body = converted
				}

				// 解析 OpenAI 响应为 ChatResponse
				var chatResp provider.ChatResponse
				if err := json.Unmarshal(body, &chatResp); err != nil {
					lastErr = fmt.Errorf("解析 OpenAI 响应失败: %w", err)
					attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, lastErr)
					attempts = append(attempts, attempt)
					s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
					registry.RecordCandidateFailure(prov.Name(), lastErr)
					if shouldStopCandidateFallback(attempt.Category) {
						break
					}
					continue
				}

				// 转换为 Anthropic 格式写出
				setResponseToolDiagnosticHeader(w, &chatResp)
				setToolOutcomeDiagnosticHeader(w, req, &chatResp)
				setResponseUsage(w, chatResp.Usage)
				s.cacheChatResponse(&chatResp)

				writeAnthropicNonStreamResponse(w, &chatResp, modelName)
				registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
				return
			}
		}

		// 通用 provider 调用
		resp, err := prov.Chat(ctx, req)
		if err != nil {
			cancel()
			if isClientGoneError(err) {
				return
			}
			lastErr = err
			attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, err)
			attempts = append(attempts, attempt)
			s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
			registry.RecordCandidateFailure(prov.Name(), err)
			if shouldStopCandidateFallback(attempt.Category) {
				break
			}
			continue
		}
		cancel()
		normalizeProviderSpecificToolCalls(resp, allowedToolNames(req))
		if validationErr := validateProviderResponseToolContract(resp); validationErr != nil {
			lastErr = fmt.Errorf("解析响应失败: typed provider 响应契约无效: %w", validationErr)
			attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, lastErr)
			attempts = append(attempts, attempt)
			s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
			registry.RecordCandidateFailure(prov.Name(), lastErr)
			if shouldStopCandidateFallback(attempt.Category) {
				break
			}
			continue
		}
		setResponseToolDiagnosticHeader(w, resp)
		setToolOutcomeDiagnosticHeader(w, req, resp)
		setResponseUsage(w, resp.Usage)
		s.cacheChatResponse(resp)

		writeAnthropicNonStreamResponse(w, resp, modelName)
		registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
		return
	}

	if lastErr != nil {
		writeAnthropicErrorResponse(w, http.StatusBadGateway,
			fmt.Sprintf("所有候选者均失败: %v", lastErr))
	} else {
		writeAnthropicErrorResponse(w, http.StatusServiceUnavailable,
			"所有候选者均失败: 无可用提供商")
	}
}

// handleAnthropicStream 处理 Anthropic 协议的流式请求。
// 内部调用 provider 的 ChatStream 获取 OpenAI SS 流，再转换为 Anthropic event-based SSE。
func (s *Server) handleAnthropicStream(
	w http.ResponseWriter,
	r *http.Request,
	prov provider.Provider,
	req *provider.ChatRequest,
	modelName string,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return fmt.Errorf("response writer does not support flushing")
	}

	setProxyStreamState(w, "upstream_connecting")
	stream, err := prov.ChatStream(r.Context(), req)
	if err != nil {
		return fmt.Errorf("anthropic stream error: %w", err)
	}
	defer stream.Close()
	setProxyStreamState(w, "upstream_connected")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if r.Context().Err() != nil {
		return r.Context().Err()
	}

	scanner := newStreamScanner(stream)
	acc := newStreamReasoningAccumulator()
	anthropicWriter := &anthropicStreamWriter{
		writer:  w,
		flusher: flusher,
		model:   modelName,
	}
	streamToolSanitizer := newOpenAIStreamToolSanitizer(allowedToolNames(req))
	eventProcessor := newOpenAIStreamEventProcessor(anthropicWriter, flusher, acc, streamToolSanitizer)

	for !eventProcessor.receivedDone() && scanner.Scan() {
		if err := eventProcessor.consumeLine(scanner.Text()); err != nil {
			return fmt.Errorf("解析响应失败: OpenAI SSE: %w", err)
		}
		if eventProcessor.receivedDone() {
			break
		}
		if r.Context().Err() != nil {
			return r.Context().Err()
		}
	}
	if err := scanner.Err(); err != nil {
		return upstreamStreamInterruptedError("OpenAI SSE", err)
	}
	if err := eventProcessor.finish(); err != nil {
		return fmt.Errorf("解析响应失败: OpenAI SSE: %w", err)
	}
	if err := validateOpenAIStreamCompletion(acc, streamToolSanitizer); err != nil {
		return fmt.Errorf("解析响应失败: OpenAI SSE: %w", err)
	}
	setResponseUsage(w, acc.usage)
	setStreamToolOutcomeDiagnosticHeader(w, req, acc)
	if err := eventProcessor.commit(); err != nil {
		return fmt.Errorf("写入响应失败: Anthropic SSE: %w", err)
	}
	s.cacheStreamAccumulator(acc)
	setStreamToolDiagnosticHeader(w, acc)

	// 写出 Anthropic 流结束事件
	anthropicWriter.finish()
	return nil
}