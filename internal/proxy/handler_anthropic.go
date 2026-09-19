package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// Anthropic Messages API 协议转换器
//
// 使 vs-ai-proxy 接收 Anthropic 原生 Messages API 请求（POST /v1/messages），
// 转换为内部 ChatRequest 后通过现有 provider 路由转发，再将响应转换回
// Anthropic 格式。流式场景同时处理 OpenAI SSE → Anthropic event-based SSE 的转换。
// ---------------------------------------------------------------------------

// anthropicUpstreamClient 是 anthropic 直通/转换路径共用的上游 HTTP 客户端。
//
// 为什么不用 http.DefaultClient：它没有连接池纪律（无 keep-alive 复用、
// 无拨号/TLS/空闲超时、不走环境代理），云主机部署下每次请求都可能新建 TLS 握手。
// 为什么 Timeout 必须为 0：本包 5 个调用点全部由 ctx/请求上下文约束生命周期，
// 且包含流式读取——client.Timeout 会连同流式 body 一起计时，设置它等于给长回复
// 埋雷。这与 OpenAI 流式路径在 doChatStream 中清零 client.Timeout 是同一决策。
var anthropicUpstreamClient = provider.NewProviderHTTPClient(0)

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
	Content   string          `json:"content,omitempty"`
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
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	Messages      []anthropicMessage   `json:"messages"`
	System        string               `json:"system,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	TopK          *int                 `json:"top_k,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool      `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Metadata      map[string]string    `json:"metadata,omitempty"`
}

// anthropicResponse 是 Anthropic Messages API 的完整响应体。
type anthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []anthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        *anthropicUsage         `json:"usage,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// anthropicStreamEvent 是 Anthropic 流式事件的 data 负载。
type anthropicStreamEvent struct {
	Type         string                 `json:"type"`
	Index        int                    `json:"index,omitempty"`
	Message      *anthropicResponse     `json:"message,omitempty"`
	ContentBlock *anthropicContentBlock `json:"content_block,omitempty"`
	Delta        *anthropicStreamDelta  `json:"delta,omitempty"`
	Usage        *anthropicUsage        `json:"usage,omitempty"`
}

type anthropicStreamDelta struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
	// Thinking 是 thinking_delta 的真实线格式字段。
	// Anthropic 发的是 {"type":"thinking_delta","thinking":"..."}，
	// 而不是 text；缺这个字段会让扩展思考内容被静默丢弃，
	// 表现为下游 reasoning_content 只剩下 content_block_start 里的开头一小段。
	Thinking     string `json:"thinking,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
	PartialJSON  string `json:"partial_json,omitempty"`
}

// thinkingText 取 thinking_delta 的文本，兼容两种线格式：
// 规范字段 thinking，以及部分网关误用的 text。
func (d anthropicStreamDelta) thinkingText() string {
	if d.Thinking != "" {
		return d.Thinking
	}
	return d.Text
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
						// content 字段（不是 text）存放结果文本
						pm.ToolCallID = block.ToolUseID
						if block.Content != "" {
							textParts = append(textParts, block.Content)
						}
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
		anthropicResp.StopReason = openAIStopReasonToAnthropic(resp.Choices[0].FinishReason)
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
// 反向转换：内部 ChatRequest → Anthropic 请求体（用于 anthropic 类型 provider 直通）
// ---------------------------------------------------------------------------

// chatRequestToAnthropicRequest 将内部 ChatRequest 转换为 Anthropic 格式的请求体。
// 这是 anthropicRequestToChatRequest 的逆操作，用于 OpenAI 客户端 → Anthropic 上游的场景。
func chatRequestToAnthropicRequest(req *provider.ChatRequest) *anthropicRequest {
	anthropicReq := &anthropicRequest{
		Model:    req.Model,
		Messages: make([]anthropicMessage, 0),
		Stream:   req.Stream,
	}

	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		anthropicReq.MaxTokens = *req.MaxTokens
	} else {
		anthropicReq.MaxTokens = 4096 // Anthropic 要求 max_tokens 必填
	}

	if req.Temperature != nil {
		anthropicReq.Temperature = req.Temperature
	}
	if req.TopP != nil {
		anthropicReq.TopP = req.TopP
	}
	if req.TopK != nil {
		anthropicReq.TopK = req.TopK
	}
	if len(req.Stop) > 0 {
		anthropicReq.StopSequences = req.Stop
	}

	// 转换消息：system 角色提到顶层，user/assistant 放到 messages 数组
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			if anthropicReq.System == "" {
				anthropicReq.System = msg.Content
			}
			continue
		}
		contentJSON, _ := json.Marshal(msg.Content)
		am := anthropicMessage{
			Role:    msg.Role,
			Content: contentJSON,
		}

		// 处理 tool_calls 消息
		if len(msg.ToolCalls) > 0 {
			blocks := make([]anthropicContentBlock, 0)
			// 先加 text 块（如果有 content）
			if msg.Content != "" {
				blocks = append(blocks, anthropicContentBlock{
					Type: "text",
					Text: msg.Content,
				})
			}
			// 再加 tool_use 块
			for _, tc := range msg.ToolCalls {
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: json.RawMessage(tc.Function.Arguments),
				})
			}
			blocksJSON, _ := json.Marshal(blocks)
			am.Content = blocksJSON
		}

		// 处理 tool_result 消息
		if msg.ToolCallID != "" {
			blocks := []anthropicContentBlock{
				{
					Type:      "tool_result",
					ToolUseID: msg.ToolCallID,
					Content:   msg.Content,
				},
			}
			blocksJSON, _ := json.Marshal(blocks)
			am.Content = blocksJSON
		}

		anthropicReq.Messages = append(anthropicReq.Messages, am)
	}

	// 工具定义转换
	if len(req.Tools) > 0 {
		tools := make([]anthropicTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			paramsJSON, _ := json.Marshal(t.Function.Parameters)
			tools = append(tools, anthropicTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: paramsJSON,
			})
		}
		anthropicReq.Tools = tools
	}

	return anthropicReq
}

// anthropicResponseToChatResponse 将 Anthropic 格式的响应转换为内部 ChatResponse。
// 这是 chatResponseToAnthropicResponse 的逆操作，用于 OpenAI 客户端 → Anthropic 上游的场景。
// anthropicClientFacingID 把 Anthropic 的 message id 规范化成 OpenAI 风格的 id。
//
// 流式首帧（role chunk）、流式增量帧、末帧（finish），以及非流式响应与缓存，
// 必须共用同一规则；否则同一个请求在下游会出现两个不同的 id，
// 客户端按 id 关联分片时会错乱。
func anthropicClientFacingID(raw string) string {
	return strings.TrimPrefix(raw, "msg_")
}

func anthropicResponseToChatResponse(anthropicResp *anthropicResponse) *provider.ChatResponse {
	resp := &provider.ChatResponse{
		ID:     anthropicClientFacingID(anthropicResp.ID),
		Object: "chat.completion",
		Model:  anthropicResp.Model,
	}

	// stop_reason 映射
	finishReason := anthropicStopReasonToOpenAI(anthropicResp.StopReason)

	// content 数组 → 消息
	message := provider.Message{
		Role: anthropicResp.Role,
	}
	var textParts []string
	for _, block := range anthropicResp.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "thinking":
			message.Reasoning += block.Thinking
		case "tool_use":
			argsJSON, _ := json.Marshal(block.Input)
			message.ToolCalls = append(message.ToolCalls, provider.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: provider.FunctionCall{
					Name:      block.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}
	message.Content = strings.Join(textParts, "")

	resp.Choices = []provider.Choice{
		{
			Index:        0,
			Message:      message,
			FinishReason: finishReason,
		},
	}

	// usage 映射
	if anthropicResp.Usage != nil {
		resp.Usage = &provider.Usage{
			PromptTokens:     anthropicResp.Usage.InputTokens,
			CompletionTokens: anthropicResp.Usage.OutputTokens,
			TotalTokens:      anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
		}
	}

	return resp
}

func openAIStopReasonToAnthropic(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "", "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter", "refusal":
		return "refusal"
	default:
		return strings.TrimSpace(reason)
	}
}

func anthropicStopReasonToOpenAI(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "", "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return strings.TrimSpace(reason)
	}
}

func marshalAnthropicRequestBody(req *anthropicRequest, extra map[string]json.RawMessage) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return body, nil
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	for key, raw := range extra {
		if len(raw) == 0 {
			continue
		}
		switch key {
		case "metadata", "thinking", "service_tier", "output_config":
			out[key] = append(json.RawMessage(nil), raw...)
		case "tool_choice":
			converted, ok := anthropicToolChoiceRawFromOpenAI(raw)
			if ok {
				out[key] = converted
			}
		}
	}
	return json.Marshal(out)
}

func anthropicToolChoiceRawFromOpenAI(raw json.RawMessage) (json.RawMessage, bool) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false
	}
	var kind string
	if err := json.Unmarshal(root["type"], &kind); err != nil || strings.TrimSpace(kind) == "" {
		return nil, false
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "function":
		var fn struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(root["function"], &fn); err != nil || strings.TrimSpace(fn.Name) == "" {
			return nil, false
		}
		converted, _ := json.Marshal(anthropicToolChoice{Type: "tool", Name: strings.TrimSpace(fn.Name)})
		return converted, true
	case "auto", "any", "none", "tool":
		return append(json.RawMessage(nil), raw...), true
	default:
		return nil, false
	}
}

// ---------------------------------------------------------------------------
// 直通 HTTP 请求：发送 Anthropic 格式请求到上游 {base_url}/v1/messages
// ---------------------------------------------------------------------------

// SendAnthropicChatRequest 将内部 ChatRequest 转换为 Anthropic 格式后发送到上游。
// 返回 OpenAI 格式的 ChatResponse，供 handleChatCompletions 等下游使用。
func SendAnthropicChatRequest(ctx context.Context, upstreamBase string, apiKey string, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	return SendAnthropicChatRequestWithPath(ctx, upstreamBase, "v1/messages", apiKey, req)
}

// SendAnthropicChatRequestWithPath 是管理测试和 OpenAI→Anthropic provider 转换共用的
// 非流式 Anthropic 上游调用入口。chatPath 必须来自归一化后的 provider transport；
// 这里保留默认值只是为了兼容旧调用方，避免自定义网关路径被硬编码 /v1/messages 覆盖。
func SendAnthropicChatRequestWithPath(ctx context.Context, upstreamBase string, chatPath string, apiKey string, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	anthropicReq := chatRequestToAnthropicRequest(req)
	body, err := marshalAnthropicRequestBody(anthropicReq, req.Extra)
	if err != nil {
		return nil, fmt.Errorf("anthropic 请求序列化失败: %w", err)
	}

	upstreamURL := anthropicUpstreamURL(upstreamBase, chatPath)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic 请求创建失败: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	setAnthropicUpstreamAuthHeaders(httpReq, apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	httpResp, err := anthropicUpstreamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic 上游请求失败: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic 响应读取失败: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic 上游返回 %d: %s", httpResp.StatusCode, string(respBody))
	}

	var anthropicResp anthropicResponse
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("anthropic 响应解析失败: %w", err)
	}

	return anthropicResponseToChatResponse(&anthropicResp), nil
}

// SendAnthropicChatStreamRequest 发送流式 Anthropic Messages 请求，并把上游的
// Anthropic event-based SSE 聚合回一个 OpenAI 格式的 ChatResponse。
//
// 为什么必须存在这个函数：
// Visual Studio Copilot 对 /v1/chat/completions 固定使用 stream=true，而
// handleChatCompletions 的流式分支在 anthropic 判断之前就 return 了，导致
// anthropic 类型 provider 把 OpenAI 请求体打到 Anthropic endpoint，并且
// 上游的 Anthropic SSE 被原样透传给 VS（没有 choices、没有 [DONE]），
// VS 因此无法解析。这里把流式链路收敛到和一条非流式链路相同的协议转换语义：
// 上游始终收到 Anthropic 请求体，下游始终拿到 OpenAI 结构。
//
// 实现选择：聚合上游事件后用 anthropicResponseToChatResponse 复用已验证的
// 转换逻辑（含 text/thinking/tool_use 与 usage 映射），再由调用方按统一
// OpenAI SSE 契约写出。这样输入输出都与非流式路径完全同构，避免出现第三套
// 只在流式下生效的转换分支。
func SendAnthropicChatStreamRequest(ctx context.Context, upstreamBase string, chatPath string, apiKey string, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	return sendAnthropicChatStreamRequestWithHooks(ctx, upstreamBase, chatPath, apiKey, req, nil, nil)
}

// SendAnthropicChatStreamRequestWithDelta 在读取上游 Anthropic SSE 的同时，
// 对每个文本/思考增量立即回调 onDelta，使调用方能够边收边吐。
//
// 与 SendAnthropicChatStreamRequest 的唯一区别就是多了这个回调；
// onDelta 为 nil 时两者完全等价。累积语义不变：返回值仍是完整的 ChatResponse，
// 因此工具调用、finish_reason、usage 等仍按原有逻辑在流末统一处理。
//
// onDelta 返回错误时读取立即中止并把该错误返回给调用方，
// 用于在客户端断开后尽快释放上游连接。
func SendAnthropicChatStreamRequestWithDelta(
	ctx context.Context,
	upstreamBase string,
	chatPath string,
	apiKey string,
	req *provider.ChatRequest,
	onDelta func(kind string, text string) error,
	onStart func(id string, model string),
) (*provider.ChatResponse, error) {
	return sendAnthropicChatStreamRequestWithHooks(ctx, upstreamBase, chatPath, apiKey, req, onDelta, onStart)
}

func sendAnthropicChatStreamRequestWithHooks(
	ctx context.Context,
	upstreamBase string,
	chatPath string,
	apiKey string,
	req *provider.ChatRequest,
	onDelta func(kind string, text string) error,
	onStart func(id string, model string),
) (*provider.ChatResponse, error) {
	streamReq := cloneChatRequest(req)
	streamReq.Stream = true
	anthropicReq := chatRequestToAnthropicRequest(streamReq)
	body, err := marshalAnthropicRequestBody(anthropicReq, streamReq.Extra)
	if err != nil {
		return nil, fmt.Errorf("anthropic 流式请求序列化失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicUpstreamURL(upstreamBase, chatPath), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic 流式请求创建失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	setAnthropicUpstreamAuthHeaders(httpReq, apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	httpResp, err := anthropicUpstreamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic 流式上游请求失败: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("anthropic 流式上游返回 %d: %s", httpResp.StatusCode, string(respBody))
	}

	acc := newAnthropicStreamAccumulatorWithDelta(onDelta, onStart)
	// 读取放在独立 goroutine 中，主流程只等待「终态已到」或「读取结束」两个信号。
	//
	// 为什么不能直接在主流程里 for scanner.Scan()：
	// bufio.Scanner 对末尾没有换行符的行，必须等到 EOF 才返回。若上游把
	// message_stop 作为最后一行且不补换行、同时又不关闭连接（复用连接的真实网关
	// 常见行为），Scan() 会永久阻塞，主流程根本没有机会判断 sawTerminal，
	// 请求就会挂到超时。这里用完成通道让主流程能在终态到达时立即返回，
	// 不依赖上游关闭 body。
	// readDone 只广播"读取 goroutine 已收口"，不承载错误：
	// 错误一律经 readResultErr 投递（见 publishDispatchErr）。
	// 这里刻意用 struct{} 而非带 err 字段的结构体——后者会让人误以为
	// 错误也走这条通道，而实际上从来没有人往里发送过值（只 close）。
	readDone := make(chan struct{})
	readResultErr := make(chan error, 1)
	terminalSeen := make(chan struct{})

	go func() {
		// 为什么不用 bufio.Scanner / ReadString：
		// 两者对「末尾没有换行符的行」都必须等到 EOF 才返回（已实测验证）。
		// 若上游把 message_stop 作为最后一行且不补换行、同时保持连接不关闭
		// （复用连接的真实网关常见），读取会永久阻塞，主流程无法得知终态，
		// 请求挂到超时。因此这里按字节流读取，自行切分完整行，
		// 并把每次读到的「未完成行」也立即交给解析器——终态判断因此
		// 既不依赖换行符，也不依赖 EOF。
		reader := bufio.NewReaderSize(httpResp.Body, 32*1024)
		currentEvent := ""
		var dataLines []string
		var dispatchErr error
		var pending strings.Builder

		// publishDispatchErr 把读取阶段记录的解析/协议错误交给主流程。
		//
		// 必须在唤醒主流程**之前**调用。主流程被 terminalSeen 唤醒后，对 readResultErr
		// 只做一次非阻塞读；若此刻错误还没进通道，它就会被静默丢弃，下游只能看到
		// "缺少内容"的泛化失败，丢掉真实原因（例如上游 overloaded_error）。
		// 这不是理论风险：8 worker 并发、GOMAXPROCS=8 下实测约 0.4%~0.8% 的请求
		// 会命中该窗口，表现为 err==nil + 半截内容，调用方把截断的流当成功收尾。
		publishDispatchErr := func() {
			if dispatchErr == nil {
				return
			}
			select {
			case readResultErr <- dispatchErr:
			default:
			}
		}
		// defer 统一收口完成信号，覆盖函数体内每一条 return 路径。
		// 对"读到 EOF 自然结束"这条路径，它是唯一的错误投递点（该路径不调用 signalTerminal）；
		// 对提前退出路径，错误已由 signalTerminal 投递，这里的重复发送会被 default 丢弃。
		defer func() {
			publishDispatchErr()
			close(readDone)
		}()
		dispatch := func() {
			if dispatchErr != nil || len(dataLines) == 0 {
				currentEvent = ""
				return
			}
			payload := strings.TrimSpace(strings.Join(dataLines, "\n"))
			event := currentEvent
			dataLines = dataLines[:0]
			currentEvent = ""
			if payload == "" || payload == "[DONE]" {
				return
			}
			if event == "error" {
				dispatchErr = fmt.Errorf("Anthropic 流式错误事件: %s", anthropicErrorMessageFromPayload(payload))
				return
			}
			dispatchErr = acc.consume(payload)
		}
		signalTerminal := func() {
			// 必须在唤醒主流程**之前**投递错误：主流程被唤醒后对 readResultErr
			// 只做一次非阻塞读，若此时错误尚未入队就会被静默丢弃。
			publishDispatchErr()
			select {
			case terminalSeen <- struct{}{}:
			default:
			}
		}
		// handleLine 处理一个逻辑行；返回 true 表示已到达终态，应停止读取。
		handleLine := func(raw string) bool {
			line := strings.TrimSpace(raw)
			if line == "" {
				dispatch()
				return acc.sawTerminal
			}
			if strings.HasPrefix(line, ":") {
				return false
			}
			if strings.HasPrefix(line, "event:") {
				currentEvent = strings.TrimSpace(line[len("event:"):])
				return false
			}
			if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(line[len("data:"):]))
				if strings.Contains(line, `"message_stop"`) {
					dispatch()
					return acc.sawTerminal
				}
				return false
			}
			return false
		}

		buf := make([]byte, 8192)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				pending.Write(buf[:n])
				for {
					text := pending.String()
					idx := strings.IndexByte(text, '\n')
					if idx < 0 {
						break
					}
					line := text[:idx]
					rest := text[idx+1:]
					pending.Reset()
					pending.WriteString(rest)
					if stop := handleLine(line); stop {
						signalTerminal()
						return
					}
				}
				// 关键：把仍未结束的半行也立即解析，使 message_stop
				// 即使没有换行符结尾也能在读到它的瞬间被识别。
				if pending.Len() > 0 {
					if stop := handleLine(pending.String()); stop {
						signalTerminal()
						return
					}
				}
				// 回调出错（典型：下游客户端已断开）或上游 error 事件之后，
				// 继续读取没有任何意义：消费方已经不在了，只会白占连接与带宽。
				// 这里主动结束，让错误尽快回到主流程。
				if dispatchErr != nil {
					signalTerminal()
					return
				}
			}
			if readErr != nil {
				break
			}
		}
		// 上游未以空行结束最后一个事件时（部分网关直接关闭连接），仍要提交它。
		// dispatchErr（上游 error 事件 / 回调失败）由上面的 defer 统一传回主流程。
		dispatch()
	}()

	select {
	case <-terminalSeen:
		// 终态已到，无需等待上游关闭 body。注意：终态信号可能早于读取 goroutine
		// 完全退出，因此下面仍要检查已记录的解析错误。
	case <-readDone:
		// 读取 goroutine 已收口（EOF 路径）。错误同样只在下面统一读取。
	}

	// 解析过程中若产生过错误（如上游 error 事件），必须优先返回它，
	// 不能让下游只看到"没有内容"的泛化失败而丢掉真实原因。
	select {
	case err := <-readResultErr:
		if err != nil {
			return nil, err
		}
	default:
	}

	// 无论是否收到 message_stop，都返回已聚合的内容：
	// 有 message_stop 是正常完成；没有但连接正常结束（EOF）时上游可能省略终态事件，
	// 此时也不应丢弃已收到的内容。真正的空流由上层按"无 choices"失败关闭，
	// 因此这里不会把截断的流伪装成成功。
	acc.mu.Lock()
	defer acc.mu.Unlock()
	return acc.response(), nil
}

// anthropicStreamAccumulator 把 Anthropic event-based SSE 增量还原成一个完整响应。
//
// 必须处理的事件（其余事件安全忽略，保证上游新增事件类型不会导致解析失败）：
//   - message_start      : 响应 id / model / 初始 usage
//   - content_block_start: 新内容块（text / thinking / tool_use）
//   - content_block_delta: text_delta / thinking_delta / input_json_delta
//   - content_block_stop : 结束当前块
//   - message_delta      : stop_reason 与 output_tokens
type anthropicStreamAccumulator struct {
	resp          *anthropicResponse
	blocks        []anthropicContentBlock
	toolInputJSON map[int]*strings.Builder
	toolInputSeen map[int]bool
	// blockPos 把 Anthropic 协议的 content block index 映射到 a.blocks 的切片位置。
	// 协议只保证 index 唯一且递增，不保证从 0 开始连续（上游可能跳过序号），
	// 因此绝不能用切片位置当协议 index 使用。
	blockPos map[int]int
	// onDelta 在收到文本/思考增量时立即回调，使调用方能够"边收边吐"。
	// 这是流式增量性的实现基础：若不回调、等整条流读完再一次性写出，
	// 功能断言仍会通过，但用户要等整段回复生成完才看到任何输出。
	// 回调返回错误（例如下游客户端已断开）会中止读取并向上传播。
	onDelta func(kind string, text string) error
	// onStart 在 message_start 解析后立即回调，让调用方能在收到第一个增量之前
	// 就用真实的上游 id / model 写出流起始帧（OpenAI 的 role chunk）。
	// 若不回调，调用方只能用占位 id，导致首帧与末帧的 id 不一致。
	onStart func(id string, model string)
	// sawTerminal 记录是否已收到 message_stop。
	// Anthropic 的 SSE 是应用层协议：上游可能在 message_stop 之后继续挂住连接
	// （等待复用或延迟关闭 body），因此读取循环必须据此结束，而不能等待传输层 EOF。
	// 这与 dsml_stream.go 中 OpenAI 流对 [DONE] 的处理是同一个道理。
	sawTerminal bool
	// mu 保护以上字段：读取在独立 goroutine 中进行，主流程在终态到达后
	// 可能先于读取 goroutine 完全退出就调用 response()，必须避免数据竞争。
	mu sync.Mutex
}

func newAnthropicStreamAccumulator() *anthropicStreamAccumulator {
	return newAnthropicStreamAccumulatorWithDelta(nil, nil)
}

// newAnthropicStreamAccumulatorWithDelta 允许调用方注入增量回调。
// onDelta 为 nil 时行为与原来完全一致（只累积，不回调）。
func newAnthropicStreamAccumulatorWithDelta(
	onDelta func(kind string, text string) error,
	onStart func(id string, model string),
) *anthropicStreamAccumulator {
	return &anthropicStreamAccumulator{
		resp: &anthropicResponse{
			Type: "message",
			Role: "assistant",
		},
		toolInputJSON: map[int]*strings.Builder{},
		toolInputSeen: map[int]bool{},
		blockPos:      map[int]int{},
		onDelta:       onDelta,
		onStart:       onStart,
	}
}

func (a *anthropicStreamAccumulator) consume(payload string) error {
	var event anthropicStreamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		// 单个事件解析失败不应中断整条流：上游可能夹带心跳或未知字段。
		return nil
	}

	switch event.Type {
	case "message_start":
		if event.Message != nil {
			if event.Message.ID != "" {
				a.resp.ID = event.Message.ID
			}
			if event.Message.Model != "" {
				a.resp.Model = event.Message.Model
			}
			// 透出真实 id/model，让调用方在首个增量到达前就能写出正确的起始帧。
			if a.onStart != nil {
				a.onStart(event.Message.ID, event.Message.Model)
			}
			if event.Message.Role != "" {
				a.resp.Role = event.Message.Role
			}
			if event.Message.Usage != nil {
				a.resp.Usage = &anthropicUsage{
					InputTokens:  event.Message.Usage.InputTokens,
					OutputTokens: event.Message.Usage.OutputTokens,
				}
			}
		}
	case "content_block_start":
		block := anthropicContentBlock{}
		if event.ContentBlock != nil {
			block = *event.ContentBlock
		}
		// tool_use 的 input 通常由 input_json_delta 分片给出；但部分网关会把
		// 完整参数直接放在 content_block_start 里（没有 delta）。因此这里**保留**
		// 原始 Input 作为回退，改由 response() 在"确实收到过 delta"时优先使用
		// 累积结果。若在此处清空，这类网关的工具参数会变成 {}，工具静默失效。
		if block.Type == "tool_use" {
			a.toolInputJSON[event.Index] = &strings.Builder{}
		}
		a.blockPos[event.Index] = len(a.blocks)
		a.blocks = append(a.blocks, block)
		// content_block_start 可以自带初始文本（协议允许，部分网关会这么发）。
		// 这部分内容不属于任何 delta，若不在这里回调就会在增量模式下丢失，
		// 导致下游内容比上游少一截。放在 append 之后回调可保证顺序正确。
		if a.onDelta != nil {
			switch block.Type {
			case "text":
				if block.Text != "" {
					if err := a.onDelta("text", block.Text); err != nil {
						return err
					}
				}
			case "thinking":
				if block.Thinking != "" {
					if err := a.onDelta("thinking", block.Thinking); err != nil {
						return err
					}
				}
			}
		}
	case "content_block_delta":
		if event.Delta == nil {
			break
		}
		pos, ok := a.blockPos[event.Index]
		if !ok {
			// 没有对应的 content_block_start：按协议不该出现，忽略而不是错位写入。
			break
		}
		switch event.Delta.Type {
		case "text_delta":
			a.blocks[pos].Text += event.Delta.Text
			// 累积之后立即回调：调用方据此写出下游 chunk 并 flush，
			// 使用户无需等待整段回复生成完毕。
			if a.onDelta != nil && event.Delta.Text != "" {
				if err := a.onDelta("text", event.Delta.Text); err != nil {
					return err
				}
			}
		case "thinking_delta":
			thinking := event.Delta.thinkingText()
			a.blocks[pos].Thinking += thinking
			if a.onDelta != nil && thinking != "" {
				if err := a.onDelta("thinking", thinking); err != nil {
					return err
				}
			}
		case "input_json_delta":
			if builder, ok := a.toolInputJSON[event.Index]; ok {
				builder.WriteString(event.Delta.PartialJSON)
				a.toolInputSeen[event.Index] = true
			}
		default:
			// signature_delta 等事件不参与 ChatResponse 构造。
		}
	case "message_delta":
		if event.Delta != nil && event.Delta.StopReason != "" {
			a.resp.StopReason = event.Delta.StopReason
		}
		if event.Usage != nil {
			if a.resp.Usage == nil {
				a.resp.Usage = &anthropicUsage{}
			}
			// message_delta 的 usage 通常只带 output_tokens，input_tokens 保留
			// message_start 的值，避免把已有输入计数覆盖成 0。
			if event.Usage.InputTokens > 0 {
				a.resp.Usage.InputTokens = event.Usage.InputTokens
			}
			if event.Usage.OutputTokens > 0 {
				a.resp.Usage.OutputTokens = event.Usage.OutputTokens
			}
		}
	case "message_stop":
		// 应用层终态：读取循环据此结束，不等待上游关闭 body。
		a.sawTerminal = true
	case "content_block_stop", "ping":
		// 无需额外处理：块内容已在 delta 阶段累积。
	}
	return nil
}

func (a *anthropicStreamAccumulator) response() *provider.ChatResponse {
	blocks := make([]anthropicContentBlock, 0, len(a.blocks))
	// 按协议 index 升序还原顺序，而不是依赖 a.blocks 的追加顺序：
	// 上游的 index 可能不是从 0 连续递增，且我们必须用协议 index 查参数表。
	indices := make([]int, 0, len(a.blocks))
	for protoIndex := range a.blockPos {
		indices = append(indices, protoIndex)
	}
	sort.Ints(indices)
	for _, protoIndex := range indices {
		block := a.blocks[a.blockPos[protoIndex]]
		if block.Type == "tool_use" {
			raw := ""
			if builder, ok := a.toolInputJSON[protoIndex]; ok {
				raw = strings.TrimSpace(builder.String())
			}
			// 优先级：input_json_delta 累积结果 > content_block_start 自带的 input > {}。
			// 无论走哪条分支，最终都必须是合法 JSON，否则 VS 无法解析工具参数。
			switch {
			case a.toolInputSeen[protoIndex] && raw != "" && json.Valid([]byte(raw)):
				block.Input = json.RawMessage(raw)
			case len(block.Input) > 0 && json.Valid(block.Input) && !isJSONEmptyObject(block.Input):
				// 网关把完整参数放在 start 块里、没有 delta：保留它。
			default:
				block.Input = json.RawMessage(`{}`)
			}
		}
		blocks = append(blocks, block)
	}
	a.resp.Content = blocks
	return anthropicResponseToChatResponse(a.resp)
}

// SendAnthropicChatStreamContent 只服务管理端"非流式失败后流式兜底"的健康检查，
// 返回上游流式响应里累积到的正文文本。
//
// 实现上直接复用 SendAnthropicChatStreamRequest，而不是另写一套 SSE 解析。
// 原因：这个函数原先自己用 bufio.Scanner + 只认 "[DONE]" 终止，而 Anthropic
// 协议根本不发 [DONE]（它发 message_stop），因此它只能等传输层 EOF。
// 实测确认：上游发完 message_stop 后保持连接不关闭时，该函数会一直挂到
// ctx 超时（ctx 设 3s 就返回 3s，设 7s 就返回 7s），管理页兜底探测随之卡住。
// 复用主读取器可同时获得：message_stop 终态识别、无换行终态、多行 data 拼接、
// error 事件上报，且不必维护第二套会再次漂移的解析逻辑。
func SendAnthropicChatStreamContent(ctx context.Context, upstreamBase string, chatPath string, apiKey string, req *provider.ChatRequest) (string, error) {
	resp, err := SendAnthropicChatStreamRequest(ctx, upstreamBase, chatPath, apiKey, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("Anthropic 流式响应没有返回文本内容")
	}
	content := resp.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("Anthropic 流式响应没有返回文本内容")
	}
	return content, nil
}

func anthropicErrorMessageFromPayload(payload string) string {
	var root struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return payload
	}
	message := strings.TrimSpace(root.Error.Message)
	if message == "" {
		message = strings.TrimSpace(payload)
	}
	if errorType := strings.TrimSpace(root.Error.Type); errorType != "" {
		return errorType + ": " + message
	}
	return message
}

// ---------------------------------------------------------------------------
// 流式转换：OpenAI SSE → Anthropic event-based SSE
// ---------------------------------------------------------------------------

// anthropicStreamWriter 实现 openAIStreamEventTarget 接口，
// 将 OpenAI SSE delta 事件转换为 Anthropic event-based SSE 格式写出。
type anthropicStreamWriter struct {
	writer        io.Writer
	flusher       http.Flusher
	model         string
	messageID     string
	contentIndex  int
	hasSentStart  bool
	hasSentBlock  bool
	openBlockType string
	finishReason  string
	inputTokens   int64
	outputTokens  int64
	// toolBlocks 按 OpenAI tool_calls[].index 跟踪增量参数。
	// OpenAI SSE 常把同一个工具调用的 arguments 拆成多段发送；
	// Anthropic SSE 要求同一个 tool_use content block 持续接收 input_json_delta。
	// 当前状态机覆盖主流的单工具或按 index 顺序增量场景，不在这里扩展成完整并行调度器。
	toolBlocks map[int]*anthropicStreamToolBlock
}

type anthropicStreamToolBlock struct {
	contentIndex int
	id           string
	name         string
	started      bool
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
				Index        int             `json:"index"`
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
					ID:      w.messageID,
					Type:    "message",
					Role:    "assistant",
					Content: []anthropicContentBlock{},
					Model:   w.model,
				},
			})
		}

		// 处理 reasoning_content → thinking 块
		if delta.Reasoning != "" {
			if !w.hasSentBlock {
				w.hasSentBlock = true
				w.openBlockType = "thinking"
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
				w.openBlockType = ""
			}
			// 开新的 text 块
			w.hasSentBlock = true
			w.openBlockType = "text"
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
					w.writeToolCallDelta(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
				}
			}
		}
	}

	return len(data), nil
}

func (w *anthropicStreamWriter) writeToolCallDelta(index int, id string, name string, arguments string) {
	if w.toolBlocks == nil {
		w.toolBlocks = make(map[int]*anthropicStreamToolBlock)
	}
	block := w.toolBlocks[index]
	if block == nil {
		block = &anthropicStreamToolBlock{contentIndex: -1}
		w.toolBlocks[index] = block
	}
	if id != "" {
		block.id = id
	}
	if name != "" {
		block.name = name
	}
	if !block.started {
		// 开启工具块前必须关闭当前文本/思考块。Anthropic event stream 同一时刻
		// 只能有一个 active content block，否则客户端会把后续 delta 归到错误块。
		if w.hasSentBlock {
			w.emitEvent("content_block_stop", anthropicStreamEvent{
				Type:  "content_block_stop",
				Index: w.contentIndex,
			})
			w.contentIndex++
			w.hasSentBlock = false
			w.openBlockType = ""
		}
		block.contentIndex = w.contentIndex
		block.started = true
		w.hasSentBlock = true
		w.openBlockType = "tool_use"
		w.emitEvent("content_block_start", anthropicStreamEvent{
			Type:  "content_block_start",
			Index: block.contentIndex,
			ContentBlock: &anthropicContentBlock{
				Type: "tool_use",
				ID:   block.id,
				Name: block.name,
				// Anthropic 官方流式格式在 start 事件给空对象，后续通过
				// input_json_delta.partial_json 拼接完整参数；不要在这里放半截 JSON。
				Input: json.RawMessage(`{}`),
			},
		})
	}
	if arguments != "" {
		// arguments 是 OpenAI 的增量片段，可能不是合法完整 JSON；
		// Anthropic 的 partial_json 正是为这种片段设计的，不能提前 marshal 或校验。
		w.emitEvent("content_block_delta", anthropicStreamEvent{
			Type:  "content_block_delta",
			Index: block.contentIndex,
			Delta: &anthropicStreamDelta{
				Type:        "input_json_delta",
				PartialJSON: arguments,
			},
		})
	}
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
		// 工具块和文本块统一在 finish 阶段关闭。这样单 chunk 和多 chunk 工具调用
		// 都只产生一个 content_block_start / content_block_stop 生命周期。
		w.hasSentBlock = false
		w.openBlockType = ""
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
			"type":    anthropicErrorTypeForStatus(statusCode),
			"message": message,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(errResp)
}

func anthropicErrorTypeForStatus(statusCode int) string {
	switch statusCode {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if statusCode >= 500 {
			return "api_error"
		}
		return "api_error"
	}
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

// getProviderTypeFromConfig 从配置中查找 provider 的类型。
// 用于 anthropic 类型 provider 的直通转发判断。
func getProviderTypeFromConfig(cfg *config.AppConfig, providerName string) string {
	if cfg == nil {
		return ""
	}
	for _, p := range cfg.Providers {
		if p.ID == providerName || p.Name == providerName {
			return strings.ToLower(strings.TrimSpace(p.Type))
		}
	}
	return ""
}

// findProviderConfig 从配置中查找 provider 的配置信息。
func findProviderConfig(cfg *config.AppConfig, providerName string) *config.ProviderConfig {
	if cfg == nil {
		return nil
	}
	for _, p := range cfg.Providers {
		normalized := config.NormalizeProvider(p)
		if config.ProviderKey(normalized) == providerName || normalized.Name == providerName {
			return &normalized
		}
	}
	return nil
}

// anthropicProviderUpstreamConfig 返回 Anthropic 直通请求真正发往上游所需的三项配置。
// 关键边界：
//   - baseURL/chatPath 来自 provider 配置和 transport，支持 New API/One API 等自定义路径；
//   - apiKey 来自 provider.APIKey，不来自客户端请求头，避免把 PROXY_API_KEY 泄漏给上游；
//   - 返回的 chatPath 已有默认值，调用方不再拼硬编码 /v1/messages。
func anthropicProviderUpstreamConfig(cfg *config.AppConfig, providerName string) (baseURL string, chatPath string, apiKey string, ok bool) {
	providerCfg := findProviderConfig(cfg, providerName)
	if providerCfg == nil {
		return "", "", "", false
	}
	baseURL = strings.TrimRight(providerCfg.BaseURL, "/")
	chatPath = strings.Trim(providerCfg.Transport.ChatPath, "/")
	if chatPath == "" {
		chatPath = "v1/messages"
	}
	return baseURL, chatPath, strings.TrimSpace(providerCfg.APIKey), baseURL != ""
}

// anthropicUpstreamURL 只拼接“版本化 API 根 + 资源相对路径”。
// chatPath 在 config.NormalizeProvider 中已经会被规范成相对路径；这里再次 trim
// 是为了保护直接测试 helper 或旧调用方传入带斜杠路径时不会生成双斜杠。
func anthropicUpstreamURL(baseURL string, chatPath string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	chatPath = strings.Trim(chatPath, "/")
	if chatPath == "" {
		chatPath = "v1/messages"
	}
	return baseURL + "/" + chatPath
}

// setAnthropicUpstreamAuthHeaders 设置上游 provider 鉴权头。
// 同时写 Authorization 和 x-api-key 是为了兼容官方 Anthropic 与 Anthropic 协议网关；
// 但 token 来源必须是 provider.APIKey，不能复用客户端到代理的认证头。
func setAnthropicUpstreamAuthHeaders(httpReq *http.Request, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("x-api-key", apiKey)
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

	// 保存原始请求体，用于 anthropic 类型 provider 的直通转发
	originalBody := make([]byte, len(body))
	copy(originalBody, body)

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

		// anthropic 类型 provider 直通转发：
		// 将原始 Anthropic 请求直接发送到上游的 {base_url}/v1/messages，
		// 不经过 OpenAI 格式转换，避免上游 Anthropic 端点收到 OpenAI 格式请求而报 404。
		providerType := getProviderTypeFromConfig(cfg, prov.Name())
		if providerType == "anthropic" {
			streamReq := anthropicReq.Stream
			if streamReq {
				streamWriter := &streamAttemptWriter{ResponseWriter: w}
				err := s.handleAnthropicPassthroughStream(streamWriter, r, cfg, prov, originalBody, modelName)
				cancel()
				if err != nil {
					if isClientGoneError(err) {
						return
					}
					lastErr = err
					attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, err)
					attempts = append(attempts, attempt)
					s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
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

			// 非流式直通转发
			err := s.forwardAnthropicRequest(ctx, cfg, w, r, prov, originalBody, modelName)
			cancel()
			if err != nil {
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
			registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
			return
		}

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

// ---------------------------------------------------------------------------
// anthropic 类型 provider 直通转发
//
// 当 provider 类型为 "anthropic" 时，不经过 OpenAI 格式转换，
// 直接将原始 Anthropic 请求转发到上游的 {base_url}/v1/messages。
// 这适用于上游只提供 Anthropic 原生协议的情况（如 Anthropic 官方 API）。
// ---------------------------------------------------------------------------

// forwardAnthropicRequest 非流式直通转发：将原始 Anthropic 请求体 POST 到上游 {base_url}/v1/messages。
// 注意：原始请求体中的 model 可能包含 @provider 后缀（如 LongCat-2.0@longcat2），
// 需要清理后再发送给上游，因为上游不认识这个后缀。
// forwardAnthropicRequest 非流式直通转发。
//
// cfg 必须是调用方在请求开始时取得的快照：Reconfigure 会在 s.mu 保护下替换
// s.config，若这里直接读 s.config，配置热更新与在途请求并发时构成数据竞争。
func (s *Server) forwardAnthropicRequest(ctx context.Context, cfg *config.AppConfig, w http.ResponseWriter, r *http.Request, prov provider.Provider, originalBody []byte, modelName string) error {
	upstreamBase, chatPath, apiKey, ok := anthropicProviderUpstreamConfig(cfg, prov.Name())
	if !ok {
		return fmt.Errorf("anthropic passthrough: 无法找到 provider %q 的 base_url", prov.Name())
	}

	// 清理模型名：去掉 @provider 后缀和 :latest 后缀
	// StripModelTag 只处理 :latest，不处理 @provider，需要手动清理
	cleanModel := modelName
	if idx := strings.Index(cleanModel, "@"); idx >= 0 {
		cleanModel = cleanModel[:idx]
	}
	cleanModel = provider.StripModelTag(cleanModel)
	if cleanModel == "" {
		cleanModel = modelName
	}

	// 替换请求体中的 model 字段
	var bodyMap map[string]interface{}
	if err := json.Unmarshal(originalBody, &bodyMap); err != nil {
		return fmt.Errorf("anthropic passthrough: 解析请求体失败: %w", err)
	}
	bodyMap["model"] = cleanModel
	modifiedBody, err := json.Marshal(bodyMap)
	if err != nil {
		return fmt.Errorf("anthropic passthrough: 序列化请求体失败: %w", err)
	}

	upstreamURL := anthropicUpstreamURL(upstreamBase, chatPath)
	bodyReader := bytes.NewReader(modifiedBody)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bodyReader)
	if err != nil {
		return fmt.Errorf("anthropic passthrough: 创建请求失败: %w", err)
	}

	setAnthropicUpstreamAuthHeaders(httpReq, apiKey)
	httpReq.Header.Set("anthropic-version", r.Header.Get("anthropic-version"))
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := anthropicUpstreamClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("anthropic passthrough: 上游请求失败: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("anthropic passthrough: 读取响应失败: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("anthropic passthrough: 上游返回 %d: %s", httpResp.StatusCode, string(respBody))
	}

	// 直接透传 Anthropic 响应
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(respBody)
	return nil
}

// handleAnthropicPassthroughStream 流式直通转发：将原始 Anthropic 流式请求转发到上游。
// handleAnthropicPassthroughStream 流式直通转发。
//
// cfg 同样必须是请求开始时取得的快照，理由见 forwardAnthropicRequest。
func (s *Server) handleAnthropicPassthroughStream(
	w http.ResponseWriter,
	r *http.Request,
	cfg *config.AppConfig,
	prov provider.Provider,
	originalBody []byte,
	modelName string,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return fmt.Errorf("response writer does not support flushing")
	}

	upstreamBase, chatPath, apiKey, ok := anthropicProviderUpstreamConfig(cfg, prov.Name())
	if !ok {
		return fmt.Errorf("anthropic passthrough stream: 无法找到 provider %q 的 base_url", prov.Name())
	}

	upstreamURL := anthropicUpstreamURL(upstreamBase, chatPath)

	// 清理模型名：去掉 @provider 后缀和 :latest 后缀
	// StripModelTag 只处理 :latest，不处理 @provider，需要手动清理
	cleanModel := modelName
	if idx := strings.Index(cleanModel, "@"); idx >= 0 {
		cleanModel = cleanModel[:idx]
	}
	cleanModel = provider.StripModelTag(cleanModel)
	if cleanModel == "" {
		cleanModel = modelName
	}
	var bodyMap map[string]interface{}
	if err := json.Unmarshal(originalBody, &bodyMap); err != nil {
		return fmt.Errorf("anthropic passthrough stream: 解析请求体失败: %w", err)
	}
	bodyMap["model"] = cleanModel
	modifiedBody, err := json.Marshal(bodyMap)
	if err != nil {
		return fmt.Errorf("anthropic passthrough stream: 序列化请求体失败: %w", err)
	}

	bodyReader := bytes.NewReader(modifiedBody)
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bodyReader)
	if err != nil {
		return fmt.Errorf("anthropic passthrough stream: 创建请求失败: %w", err)
	}

	setAnthropicUpstreamAuthHeaders(httpReq, apiKey)
	httpReq.Header.Set("anthropic-version", r.Header.Get("anthropic-version"))
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := anthropicUpstreamClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("anthropic passthrough stream: 上游请求失败: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		respBody, _ := io.ReadAll(httpResp.Body)
		return fmt.Errorf("anthropic passthrough stream: 上游返回 %d: %s", httpResp.StatusCode, string(respBody))
	}

	// 流式透传 Anthropic 事件流
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	defer httpResp.Body.Close()
	buf := make([]byte, 4096)
	for {
		n, readErr := httpResp.Body.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("anthropic passthrough stream: 写入响应失败: %w", writeErr)
			}
			flusher.Flush()
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return fmt.Errorf("anthropic passthrough stream: 读取上游流失败: %w", readErr)
		}
	}
	return nil
}

// isJSONEmptyObject 判断 RawMessage 是否为空对象（忽略空白）。
// 用于区分"网关真的给了参数"与"start 块里的占位 {}"。
func isJSONEmptyObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "{}"
}
