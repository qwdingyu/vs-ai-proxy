package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/requestmeta"
)

// ChatRequest 聊天请求
type ChatRequest struct {
	Model           string                     `json:"model"`
	Messages        []Message                  `json:"messages"`
	Temperature     *float64                   `json:"temperature,omitempty"`
	TopP            *float64                   `json:"top_p,omitempty"`
	TopK            *int                       `json:"top_k,omitempty"`
	MaxTokens       *int                       `json:"max_tokens,omitempty"`
	ContextLength   *int                       `json:"-"`
	ReasoningEffort string                     `json:"reasoning_effort,omitempty"`
	Stream          bool                       `json:"stream"`
	Tools           []Tool                     `json:"tools,omitempty"`
	Stop            []string                   `json:"stop,omitempty"`
	Extra           map[string]json.RawMessage `json:"-"`
	OptionsExtra    map[string]json.RawMessage `json:"-"`
}

// Message 消息
type Message struct {
	Role         string          `json:"role"`
	Content      string          `json:"content"`
	ContentRaw   json.RawMessage `json:"-"`
	ToolCalls    []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID   string          `json:"tool_call_id,omitempty"`
	FunctionCall *FunctionCall   `json:"function_call,omitempty"`
	Reasoning    string          `json:"reasoning_content,omitempty"`
	// Refusal 是 OpenAI 标准的拒绝内容；它可以在 content 为空时独立构成合法响应。
	Refusal string                     `json:"refusal,omitempty"`
	Extra   map[string]json.RawMessage `json:"-"`
}

// ToolCall 工具调用
type ToolCall struct {
	ID       string                     `json:"id"`
	Type     string                     `json:"type"`
	Function FunctionCall               `json:"function"`
	Extra    map[string]json.RawMessage `json:"-"`
}

// FunctionCall 函数调用
type FunctionCall struct {
	Name      string                     `json:"name"`
	Arguments string                     `json:"arguments"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// Tool 工具定义
type Tool struct {
	Type     string                     `json:"type"`
	Function ToolFunc                   `json:"function"`
	Extra    map[string]json.RawMessage `json:"-"`
}

// ToolFunc 工具函数
type ToolFunc struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Parameters  any                        `json:"parameters"`
	Extra       map[string]json.RawMessage `json:"-"`
}

// ChatResponse 聊天响应
type ChatResponse struct {
	ID      string    `json:"id"`
	Object  string    `json:"object"`
	Created int64     `json:"created"`
	Model   string    `json:"model"`
	Choices []Choice  `json:"choices"`
	Usage   *Usage    `json:"usage,omitempty"`
	Error   *APIError `json:"error,omitempty"`
}

// Choice 选择
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage 使用情况
type Usage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens,omitempty"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

// NormalizeUsage returns an independent, non-negative usage snapshot.
// A non-nil all-zero result is meaningful: the upstream explicitly reported zero usage.
func NormalizeUsage(usage *Usage) *Usage {
	if usage == nil || usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return nil
	}
	normalized := *usage
	if usage.PromptTokensDetails != nil {
		if usage.PromptTokensDetails.CachedTokens < 0 {
			return nil
		}
		details := *usage.PromptTokensDetails
		normalized.PromptTokensDetails = &details
	}
	if usage.CompletionTokensDetails != nil {
		if usage.CompletionTokensDetails.ReasoningTokens < 0 {
			return nil
		}
		details := *usage.CompletionTokensDetails
		normalized.CompletionTokensDetails = &details
	}
	if normalized.TotalTokens == 0 && normalized.PromptTokens+normalized.CompletionTokens > 0 {
		normalized.TotalTokens = normalized.PromptTokens + normalized.CompletionTokens
	}
	return &normalized
}

// APIError API 错误
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// StreamChunk 流式响应块
type StreamChunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Provider 定义 AI 提供商接口
type Provider interface {
	// Name 提供商名称
	Name() string

	// Chat 发送聊天请求
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)

	// ChatStream 发送流式聊天请求
	ChatStream(ctx context.Context, req *ChatRequest) (io.ReadCloser, error)

	// ListModels 获取模型列表
	ListModels(ctx context.Context) ([]string, error)

	// IsEnabled 是否启用
	IsEnabled() bool
}

func requestIDFromContext(ctx context.Context) string {
	return requestmeta.RequestIDFromContext(ctx)
}

// OpenAIProvider OpenAI 兼容提供商
type OpenAIProvider struct {
	NameStr        string
	CapabilityName string
	APIKey         string
	BaseURL        string
	Enabled        bool
	DefenseEnabled bool
	Client         *http.Client
	Timeout        time.Duration
}

type providerHTTPError struct {
	StatusCode int
	Body       []byte
	RetryAfter time.Duration
	Message    string
}

const (
	maxProviderResponseBodyBytes      int64 = 64 << 20
	maxProviderErrorResponseBodyBytes int64 = 1 << 20
)

var errProviderResponseBodyTooLarge = errors.New("上游响应体超过大小限制")

// ErrOpenAIStreamTooLarge 表示待聚合的 OpenAI SSE 超过调用方指定的总字节上限。
var ErrOpenAIStreamTooLarge = errors.New("OpenAI SSE 响应超过大小限制")

// UpstreamAttempt 描述一次真实发往上游 provider 的 HTTP 尝试。
//
// 设计边界：
//  1. 一次客户端请求可能在 provider 内部发生短重试，因此 proxy 不能只记录最后一个 error 文本。
//  2. 诊断必须能区分“尚未提交请求”和“请求已写出但上游未响应”，否则会把上游/网关问题虚报成本机连接问题。
//  3. 这里刻意只保存阶段、耗时、HTTP 状态码和脱敏错误摘要；不保存 URL、Header、请求体或响应体，
//     避免诊断日志泄漏 API Key 或用户提示词。
type UpstreamAttempt struct {
	Stage      string        // httptrace 记录的最后网络阶段，例如 connecting / waiting_response_headers。
	Elapsed    time.Duration // 单次 HTTP 尝试耗时，不包含 provider 外层候选 fallback 耗时。
	HTTPStatus int           // 上游已返回 HTTP 响应时的状态码；传输层失败时为 0。
	Error      string        // 单次尝试的脱敏错误文本，用于后续 proxy 分类和日志摘要。
}

// upstreamAttemptCarrier 是 provider 错误携带结构化上游尝试明细的内部协议。
// proxy 层通过 errors.As 读取它，不需要依赖 OpenAIProvider 的具体错误类型。
type upstreamAttemptCarrier interface {
	UpstreamAttempts() []UpstreamAttempt
}

// upstreamAttemptsError 包装最终错误，并附带本次 provider 内部短重试的所有尝试。
// Error/Unwrap 保持原错误链，避免破坏既有 errors.Is / errors.As 逻辑。
type upstreamAttemptsError struct {
	err      error
	attempts []UpstreamAttempt
}

func (e *upstreamAttemptsError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *upstreamAttemptsError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *upstreamAttemptsError) UpstreamAttempts() []UpstreamAttempt {
	if e == nil || len(e.attempts) == 0 {
		return nil
	}
	return append([]UpstreamAttempt(nil), e.attempts...)
}

// upstreamTransportError 表示 http.Client.Do 在建立请求、连接、TLS、写请求或等待响应头阶段失败。
//
// Stage 来自 httptrace，是两个核心决策的依据：
//  1. 重试安全性：writing_request / waiting_response_headers 之后，非幂等 chat POST 可能已经被上游接收。
//  2. 诊断准确性：waiting_response_headers 说明代理已完成上传并等待响应头，不能再笼统报“无法连接上游”。
type upstreamTransportError struct {
	Stage string
	Err   error
}

func (e *upstreamTransportError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	stage := strings.TrimSpace(e.Stage)
	if stage == "" {
		stage = "preparing_request"
	}
	return fmt.Sprintf("upstream_stage=%s: %v", stage, e.Err)
}

func (e *upstreamTransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// UpstreamAttempts 从任意 provider 错误中提取结构化尝试明细。
// 返回副本，调用方可以安全读取，但不应把它当成持久化业务状态。
func UpstreamAttempts(err error) []UpstreamAttempt {
	if err == nil {
		return nil
	}
	var carrier upstreamAttemptCarrier
	if !errors.As(err, &carrier) {
		return nil
	}
	return carrier.UpstreamAttempts()
}

func (e *providerHTTPError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// UpstreamHTTPStatusCode and UpstreamHTTPErrorBody expose provider-neutral
// diagnostics without exporting the concrete transport error type.
func (e *providerHTTPError) UpstreamHTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.StatusCode
}

func (e *providerHTTPError) UpstreamHTTPErrorBody() []byte {
	if e == nil {
		return nil
	}
	return bytes.Clone(e.Body)
}

// NewOpenAIProvider 创建 OpenAI 提供商
func NewOpenAIProvider(name, apiKey, baseURL string, enabled bool, timeout time.Duration) *OpenAIProvider {
	return NewOpenAIProviderWithCapability(name, "", apiKey, baseURL, enabled, timeout)
}

func NewOpenAIProviderWithCapability(name, capabilityName, apiKey, baseURL string, enabled bool, timeout time.Duration) *OpenAIProvider {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &OpenAIProvider{
		NameStr:        name,
		CapabilityName: capabilityName,
		APIKey:         apiKey,
		BaseURL:        strings.TrimRight(baseURL, "/"),
		Enabled:        enabled,
		DefenseEnabled: true,
		Client:         newProviderHTTPClient(timeout),
		Timeout:        timeout,
	}
}

func (p *OpenAIProvider) SetDefenseEnabled(enabled bool) {
	p.DefenseEnabled = enabled
}

func (p *OpenAIProvider) Name() string {
	return p.NameStr
}

func (p *OpenAIProvider) IsEnabled() bool {
	return p.Enabled
}

func (p *OpenAIProvider) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	respBody, err := p.ChatRaw(ctx, req)
	if err != nil {
		return nil, err
	}
	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		// New API / 部分 OpenAI-compatible 网关存在一个协议坑：
		// 客户端明确 stream=false 时，上游仍可能以 text/event-stream 返回 data: chunk。
		// 这里把可聚合的 SSE 转为普通 ChatResponse，避免下游非流式 JSON 解析失败。
		if sseResp, sseErr := parseOpenAIChatSSEAsResponse(respBody, req.Model); sseErr == nil {
			return sseResp, nil
		}
		return nil, fmt.Errorf("解析响应失败: %w; body_preview=%q", err, responseBodyPreview(respBody))
	}
	if err := normalizeAndValidateChatResponseTools(&chatResp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	chatResp.Usage = NormalizeUsage(chatResp.Usage)

	return &chatResp, nil
}

func parseOpenAIChatSSEAsResponse(body []byte, model string) (*ChatResponse, error) {
	resp, err := CollectOpenAIChatSSE(bytes.NewReader(body), model, maxProviderResponseBodyBytes)
	if err != nil {
		return nil, err
	}
	message := resp.Choices[0].Message
	if strings.TrimSpace(message.Content) == "" &&
		strings.TrimSpace(message.Reasoning) == "" &&
		strings.TrimSpace(message.Refusal) == "" &&
		len(message.ToolCalls) == 0 && message.FunctionCall == nil {
		return nil, fmt.Errorf("SSE 响应没有文本、推理内容或工具调用")
	}
	return resp, nil
}

// CollectOpenAIChatSSE 将 OpenAI-compatible SSE 聚合为一个完整响应。
// maxBytes 限制读取的原始流总量；传入非正数时使用 provider 的默认响应上限。
func CollectOpenAIChatSSE(reader io.Reader, model string, maxBytes int64) (*ChatResponse, error) {
	if reader == nil {
		return nil, fmt.Errorf("SSE reader 不能为空")
	}
	if maxBytes <= 0 {
		maxBytes = maxProviderResponseBodyBytes
	}
	body, err := readOpenAIChatSSEBodyUntilDone(reader, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("读取 SSE 响应失败: %w", err)
	}
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	if !bytes.Contains(body, []byte("data:")) {
		return nil, fmt.Errorf("响应不是 SSE")
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	// 总量已经受 maxBytes 保护，单个 data 行可以安全放宽到本次实际响应大小，
	// 避免大型但合法的工具参数被 Scanner 的固定 token 上限误判为截断。
	scanner.Buffer(make([]byte, 64*1024), len(body)+1)
	acc := openAIChatSSEAccumulator{
		toolCalls:    map[int]*openAIChatSSEToolCall{},
		finishReason: "stop",
	}
	parsedPayload := false
	sawDone := false
	eventType := ""
	dataLines := []string{}
	consumeEvent := func() error {
		if len(dataLines) == 0 {
			eventType = ""
			return nil
		}

		payload := strings.TrimSpace(strings.Join(dataLines, "\n"))
		currentEvent := strings.TrimSpace(eventType)
		dataLines = dataLines[:0]
		eventType = ""
		if payload == "" {
			return nil
		}
		if payload == "[DONE]" {
			sawDone = true
			return nil
		}
		if strings.EqualFold(currentEvent, "error") {
			message := openAIResponseErrorMessage(json.RawMessage(payload))
			return fmt.Errorf("上游 SSE 错误: %s", message)
		}
		if err := acc.consume([]byte(payload)); err != nil {
			return err
		}
		parsedPayload = true
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			if err := consumeEvent(); err != nil {
				return nil, err
			}
			if sawDone {
				break
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			if len(dataLines) > 0 {
				if err := consumeEvent(); err != nil {
					return nil, err
				}
			}
			eventType = strings.TrimSpace(line[6:])
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		// 部分兼容网关省略 SSE 事件间的空行。若已缓存的数据本身是完整 JSON，
		// 下一条 data 到来时先结算上一事件；无效 JSON 则继续按标准多行 data 拼接。
		if len(dataLines) > 0 {
			pending := strings.TrimSpace(strings.Join(dataLines, "\n"))
			if pending == "[DONE]" || json.Valid([]byte(pending)) {
				if err := consumeEvent(); err != nil {
					return nil, err
				}
				if sawDone {
					break
				}
			}
		}
		dataLines = append(dataLines, strings.TrimSpace(line[5:]))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !sawDone {
		if err := consumeEvent(); err != nil {
			return nil, err
		}
	}
	if !parsedPayload {
		return nil, fmt.Errorf("SSE 响应没有 JSON 数据")
	}
	if !acc.explicitFinish && !sawDone {
		return nil, fmt.Errorf("SSE 响应在 finish_reason 或 [DONE] 之前结束")
	}

	truncated := isOpenAITruncationFinishReason(acc.finishReason)
	toolCalls, toolErr := acc.buildToolCalls()
	if toolErr != nil {
		if !truncated {
			return nil, toolErr
		}
		toolCalls = []ToolCall{}
	}
	legacyFunctionCall, legacyErr := acc.buildLegacyFunctionCall()
	if legacyErr != nil {
		if !truncated {
			return nil, legacyErr
		}
		legacyFunctionCall = nil
	}
	if truncated && (len(acc.toolCalls) > 0 || acc.legacyFunctionCall != nil) {
		// length/content_filter 表示模型输出没有正常完成。即使当前参数片段恰好是
		// 合法 JSON，也不能证明 schema 所需字段已经生成完毕，因此禁止下游执行。
		toolCalls = []ToolCall{}
		legacyFunctionCall = nil
	}

	if len(toolCalls) > 0 {
		switch strings.ToLower(strings.TrimSpace(acc.finishReason)) {
		case "", "stop", "function_call", "tool_calls":
			acc.finishReason = "tool_calls"
		}
	}
	if legacyFunctionCall != nil && len(toolCalls) == 0 {
		switch strings.ToLower(strings.TrimSpace(acc.finishReason)) {
		case "", "stop", "function_call":
			acc.finishReason = "function_call"
		}
	}
	now := time.Now().Unix()
	return &ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-sse-%d", now),
		Object:  "chat.completion",
		Created: now,
		Model:   model,
		Choices: []Choice{{
			Index: 0,
			Message: Message{
				Role:         "assistant",
				Content:      acc.content.String(),
				Reasoning:    acc.reasoning.String(),
				Refusal:      acc.refusal.String(),
				ToolCalls:    toolCalls,
				FunctionCall: legacyFunctionCall,
			},
			FinishReason: acc.finishReason,
		}},
		Usage: NormalizeUsage(acc.usage),
	}, nil
}

// readOpenAIChatSSEBodyUntilDone 以应用层 data:[DONE] 作为读取终点。
// 有些网关发送 DONE 后会复用或延迟关闭 HTTP body；如果这里继续等待 EOF，
// 非流式 SSE 兜底会在已有完整响应时错误触发 timeout。ReadString 能处理超过
// Scanner 默认上限的单行，累计字节仍受统一 maxBytes 限制。
func readOpenAIChatSSEBodyUntilDone(reader io.Reader, maxBytes int64) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("SSE reader 不能为空")
	}
	var body bytes.Buffer
	buffered := bufio.NewReaderSize(io.LimitReader(reader, maxBytes+1), 64*1024)
	for {
		line, err := buffered.ReadString('\n')
		if len(line) > 0 {
			if int64(body.Len()+len(line)) > maxBytes {
				return nil, fmt.Errorf("%w: 最大允许 %d 字节", ErrOpenAIStreamTooLarge, maxBytes)
			}
			body.WriteString(line)
			if isOpenAIChatSSEDoneLine(line) {
				return body.Bytes(), nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return body.Bytes(), nil
			}
			return nil, err
		}
	}
}

func isOpenAIChatSSEDoneLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "data:") && strings.TrimSpace(trimmed[5:]) == "[DONE]"
}

// openAIChatSSEAccumulator 是非流请求收到 SSE 时的最小协议状态机。
// 它只负责 provider 层必须保证的完整性：事件错误不能吞、工具分片按 index 合并、
// arguments 必须在返回前成为完整 JSON；工具名白名单等业务规则仍由 proxy 层处理。
type openAIChatSSEAccumulator struct {
	content            strings.Builder
	reasoning          strings.Builder
	refusal            strings.Builder
	toolCalls          map[int]*openAIChatSSEToolCall
	legacyFunctionCall *openAIChatSSEToolCall
	finishReason       string
	explicitFinish     bool
	usage              *Usage
}

type openAIChatSSEToolCall struct {
	id        string
	typeName  string
	name      string
	arguments strings.Builder
}

type openAIChatSSEMessageChunk struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	Thinking         string `json:"thinking"`
	Refusal          string `json:"refusal"`
	FunctionCall     *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function_call"`
	ToolCalls []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func (a *openAIChatSSEAccumulator) consume(payload []byte) error {
	var root struct {
		Error   json.RawMessage `json:"error"`
		Usage   *Usage          `json:"usage"`
		Choices []struct {
			Delta        openAIChatSSEMessageChunk `json:"delta"`
			Message      openAIChatSSEMessageChunk `json:"message"`
			FinishReason string                    `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &root); err != nil {
		return fmt.Errorf("解析 SSE 数据失败: %w", err)
	}
	if message := openAIResponseErrorMessage(root.Error); message != "" {
		return fmt.Errorf("上游 SSE 错误: %s", message)
	}
	if root.Usage != nil {
		a.usage = NormalizeUsage(root.Usage)
	}
	if len(root.Choices) == 0 {
		return nil
	}

	choice := root.Choices[0]
	if err := a.consumeMessage(choice.Delta); err != nil {
		return err
	}
	if err := a.consumeMessage(choice.Message); err != nil {
		return err
	}
	if strings.TrimSpace(choice.FinishReason) != "" {
		a.finishReason = choice.FinishReason
		a.explicitFinish = true
	}
	return nil
}

func (a *openAIChatSSEAccumulator) consumeMessage(message openAIChatSSEMessageChunk) error {
	a.content.WriteString(message.Content)
	a.reasoning.WriteString(message.ReasoningContent)
	a.reasoning.WriteString(message.Thinking)
	a.refusal.WriteString(message.Refusal)
	if message.FunctionCall != nil {
		if a.legacyFunctionCall == nil {
			a.legacyFunctionCall = &openAIChatSSEToolCall{}
		}
		appendOpenAIIdentityFragment(&a.legacyFunctionCall.name, message.FunctionCall.Name)
		if err := appendOpenAIToolArguments(
			&a.legacyFunctionCall.arguments,
			message.FunctionCall.Arguments,
		); err != nil {
			return fmt.Errorf("SSE legacy function_call 参数无效: %w", err)
		}
	}

	for _, chunk := range message.ToolCalls {
		current := a.toolCalls[chunk.Index]
		if current == nil {
			current = &openAIChatSSEToolCall{typeName: "function"}
			a.toolCalls[chunk.Index] = current
		}
		if chunk.ID != "" {
			appendOpenAIIdentityFragment(&current.id, chunk.ID)
		}
		if chunk.Type != "" {
			current.typeName = chunk.Type
		}
		if chunk.Function.Name != "" {
			appendOpenAIIdentityFragment(&current.name, chunk.Function.Name)
		}
		if err := appendOpenAIToolArguments(&current.arguments, chunk.Function.Arguments); err != nil {
			return fmt.Errorf("SSE 工具调用 %d 参数无效: %w", chunk.Index, err)
		}
	}
	return nil
}

func appendOpenAIIdentityFragment(current *string, fragment string) {
	if current == nil || fragment == "" {
		return
	}
	switch {
	case *current == "":
		*current = fragment
	case *current == fragment:
		return
	case strings.HasPrefix(fragment, *current):
		*current = fragment
	default:
		*current += fragment
	}
}

func appendOpenAIToolArguments(dst *strings.Builder, raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}

	var fragment string
	if err := json.Unmarshal(raw, &fragment); err == nil {
		dst.WriteString(fragment)
		return nil
	}
	if !json.Valid(raw) {
		return fmt.Errorf("不是有效 JSON")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return err
	}
	dst.Write(compact.Bytes())
	return nil
}

func (a *openAIChatSSEAccumulator) buildToolCalls() ([]ToolCall, error) {
	if len(a.toolCalls) == 0 {
		return []ToolCall{}, nil
	}

	indexes := make([]int, 0, len(a.toolCalls))
	for index := range a.toolCalls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	calls := make([]ToolCall, 0, len(indexes))
	for _, index := range indexes {
		call := a.toolCalls[index]
		if strings.TrimSpace(call.name) == "" {
			return nil, fmt.Errorf("SSE 工具调用 %d 缺少函数名", index)
		}
		arguments := strings.TrimSpace(call.arguments.String())
		if arguments == "" || !json.Valid([]byte(arguments)) {
			return nil, fmt.Errorf("SSE 工具调用 %d 的参数不完整", index)
		}
		calls = append(calls, ToolCall{
			ID:   call.id,
			Type: call.typeName,
			Function: FunctionCall{
				Name:      call.name,
				Arguments: arguments,
			},
		})
	}
	return calls, nil
}

func (a *openAIChatSSEAccumulator) buildLegacyFunctionCall() (*FunctionCall, error) {
	if a.legacyFunctionCall == nil {
		return nil, nil
	}
	if strings.TrimSpace(a.legacyFunctionCall.name) == "" {
		return nil, fmt.Errorf("SSE legacy function_call 缺少函数名")
	}
	arguments := strings.TrimSpace(a.legacyFunctionCall.arguments.String())
	if arguments == "" || !json.Valid([]byte(arguments)) {
		return nil, fmt.Errorf("SSE legacy function_call 参数不完整")
	}
	return &FunctionCall{
		Name:      a.legacyFunctionCall.name,
		Arguments: arguments,
	}, nil
}

func isOpenAITruncationFinishReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "content_filter":
		return true
	default:
		return false
	}
}

// normalizeAndValidateChatResponseTools 是 typed provider 响应的统一工具协议边界。
// fallback 可能直接调用 Provider.Chat，不能依赖 proxy raw JSON 路径再做一次校验。
func normalizeAndValidateChatResponseTools(resp *ChatResponse) error {
	if resp == nil {
		return fmt.Errorf("聊天响应为空")
	}
	for choiceIndex := range resp.Choices {
		choice := &resp.Choices[choiceIndex]
		message := &choice.Message
		if isOpenAITruncationFinishReason(choice.FinishReason) {
			message.ToolCalls = nil
			message.FunctionCall = nil
			continue
		}
		if len(message.ToolCalls) > 0 && message.FunctionCall != nil {
			return fmt.Errorf("choice %d 同时包含 tool_calls 和 function_call", choiceIndex)
		}
		for callIndex, call := range message.ToolCalls {
			typeName := strings.TrimSpace(call.Type)
			if typeName != "" && !strings.EqualFold(typeName, "function") {
				return fmt.Errorf(
					"choice %d tool call %d 的 type 无效: %q",
					choiceIndex,
					callIndex,
					call.Type,
				)
			}
			if strings.TrimSpace(call.Function.Name) == "" {
				return fmt.Errorf("choice %d tool call %d 缺少函数名", choiceIndex, callIndex)
			}
			arguments := strings.TrimSpace(call.Function.Arguments)
			if arguments == "" || !json.Valid([]byte(arguments)) {
				return fmt.Errorf("choice %d tool call %d 参数不完整", choiceIndex, callIndex)
			}
		}
		if len(message.ToolCalls) > 0 {
			choice.FinishReason = "tool_calls"
		}
		if message.FunctionCall != nil {
			name := strings.TrimSpace(message.FunctionCall.Name)
			arguments := strings.TrimSpace(message.FunctionCall.Arguments)
			if name == "" || arguments == "" || !json.Valid([]byte(arguments)) {
				return fmt.Errorf("choice %d function_call 参数不完整", choiceIndex)
			}
			choice.FinishReason = "function_call"
		}
		hasNoToolCalls := len(message.ToolCalls) == 0 && message.FunctionCall == nil
		if hasNoToolCalls && strings.TrimSpace(choice.FinishReason) == "" {
			choice.FinishReason = "stop"
		}
	}
	return nil
}

func (p *OpenAIProvider) ChatRaw(ctx context.Context, req *ChatRequest) ([]byte, error) {
	ctx, cancel := providerOperationContext(ctx, p.Timeout)
	defer cancel()

	req.Stream = false
	body, err := p.marshalOpenAIChatCompletionsRequest(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	// New API / sub2api 内部可能配置多个渠道，但实测单渠道 5xx/EOF 有时会直接透出。
	// 防御开启时，代理侧只对瞬态错误做短重试，给上游网关重新选择渠道的机会；4xx 不重试，避免放大参数/鉴权错误。
	var lastErr error
	attempts := []UpstreamAttempt{}
	for attempt := 0; attempt < p.openAIProviderMaxAttempts(); attempt++ {
		attemptStart := time.Now()
		respBody, err := p.doChatRaw(ctx, body)
		if err == nil {
			return respBody, nil
		}
		lastErr = err
		attempts = append(attempts, newUpstreamAttempt(err, time.Since(attemptStart)))
		if !p.shouldRetryOpenAIProviderError(err) || attempt == p.openAIProviderMaxAttempts()-1 {
			break
		}
		if delay := openAIProviderRetryDelay(attempt); delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, withUpstreamAttempts(lastErr, attempts)
}

func (p *OpenAIProvider) doChatRaw(ctx context.Context, body []byte) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	p.applyOpenAIRequestHeaders(httpReq, "application/json")

	resp, err := p.doChatHTTPRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	var respBody []byte
	if resp.StatusCode == http.StatusOK {
		// stream=false 的兼容上游也可能返回 SSE，并且 Content-Type 可能缺失
		// 或错误；成功正文统一用有界 reader 读取。普通 JSON 仍读到 EOF，
		// SSE 则在应用层 DONE 处停止，不依赖响应头猜测协议。
		respBody, err = readOpenAIChatSSEBodyUntilDone(resp.Body, maxProviderResponseBodyBytes)
	} else {
		respBody, err = readProviderResponseBody(resp)
	}
	if err != nil {
		return nil, fmt.Errorf("读取响应失败（HTTP %d）: %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return nil, &providerHTTPError{
			StatusCode: resp.StatusCode,
			Body:       respBody,
			RetryAfter: retryAfter,
			Message:    providerHTTPErrorMessage(resp.StatusCode, respBody, retryAfter),
		}
	}
	if message := openAIResponseErrorFromBody(respBody); message != "" {
		// OpenAI-compatible 网关有时用 HTTP 200 包装顶层 error；必须在原始 provider
		// 边界转为错误，否则上层会把没有 choices 的响应误记为成功并停止故障转移。
		return nil, &providerHTTPError{
			StatusCode: resp.StatusCode,
			Body:       respBody,
			Message:    fmt.Sprintf("API 错误 %d: %s", resp.StatusCode, message),
		}
	}

	return respBody, nil
}

func withUpstreamAttempts(err error, attempts []UpstreamAttempt) error {
	if err == nil || len(attempts) == 0 {
		return err
	}
	return &upstreamAttemptsError{
		err:      err,
		attempts: append([]UpstreamAttempt(nil), attempts...),
	}
}

// newUpstreamAttempt 将一次 provider 内部尝试的最终错误转换为结构化摘要。
// HTTP 错误优先保存状态码；传输错误优先保存 httptrace 阶段。
func newUpstreamAttempt(err error, elapsed time.Duration) UpstreamAttempt {
	attempt := UpstreamAttempt{
		Elapsed: elapsed,
		Error:   "",
	}
	if err != nil {
		attempt.Error = err.Error()
	}
	var httpErr *providerHTTPError
	if errors.As(err, &httpErr) {
		attempt.HTTPStatus = httpErr.StatusCode
	}
	var transportErr *upstreamTransportError
	if errors.As(err, &transportErr) {
		attempt.Stage = strings.TrimSpace(transportErr.Stage)
	}
	return attempt
}

const openAIProviderMaxAttempts = 3

func openAIProviderRetryDelay(attempt int) time.Duration {
	return time.Duration(attempt+1) * 200 * time.Millisecond
}

func (p *OpenAIProvider) openAIProviderMaxAttempts() int {
	// 防御关闭用于排查上游原始行为，此时不能在代理侧额外重试，
	// 否则用户看到的请求次数、耗时和错误会与真实上游表现不一致。
	if p == nil || !p.DefenseEnabled {
		return 1
	}
	// 防御开启时最多 3 次 provider 内部尝试，用于覆盖 New API/sub2api
	// 偶发 5xx 或连接建立前失败。该重试不是跨 provider fallback，也不应突破
	// providerOperationContext 给整次请求设置的总预算。
	return openAIProviderMaxAttempts
}

func (p *OpenAIProvider) shouldRetryOpenAIProviderError(err error) bool {
	// 只重试可恢复的传输/服务端瞬态错误；400/401/403/404/429 都不在这里重试，
	// 避免把参数错误、鉴权错误、模型不存在或限流放大成更多上游请求。
	if p == nil || !p.DefenseEnabled {
		return false
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *providerHTTPError
	if errors.As(err, &httpErr) {
		// 保留历史防御行为：上游明确返回 5xx 时允许短重试，主要用于
		// new-api/sub2api 单个失败渠道直接透出 503 的场景。它会产生第二次
		// 上游请求，因此受 DefenseEnabled 控制；4xx/429 不在这里盲目重试。
		return httpErr.StatusCode >= http.StatusInternalServerError
	}
	var transportErr *upstreamTransportError
	if errors.As(err, &transportErr) {
		return isRetryableUpstreamTransportError(transportErr)
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "empty reply") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "eof") ||
		strings.Contains(lower, "stream error") ||
		strings.Contains(lower, "timeout")
}

func isRetryableUpstreamTransportError(err *upstreamTransportError) bool {
	if err == nil || err.Err == nil {
		return false
	}
	if errors.Is(err.Err, context.Canceled) || errors.Is(err.Err, context.DeadlineExceeded) {
		return false
	}
	stage := strings.TrimSpace(err.Stage)
	switch stage {
	case "resolving_dns", "connecting", "tls_handshake", "preparing_request":
		// 这些阶段还没有把完整 chat POST 提交给上游业务处理，短重试不会造成
		// 重复计费或重复工具调用；是否可重试仍由具体连接错误类型决定。
		return isRetryableConnectionBreak(err.Err)
	default:
		// writing_request / waiting_response_headers 之后，上游可能已经收到部分或全部
		// 非幂等 chat 请求。代理层不能盲目重放，否则可能重复计费或重复执行工具。
		// 这里也不通过 Idempotency-Key 把 POST 标记为 replayable，避免 Transport
		// 在读取首响应失败等“可能已提交”场景内部重放。
		return false
	}
}

// isRetryableConnectionBreak 只判断连接建立前后的短暂网络错误。
// 该函数不单独决定是否重试；调用方还必须结合 httptrace 阶段，避免重放已提交请求。
func isRetryableConnectionBreak(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "empty reply") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "i/o timeout") ||
		strings.Contains(lower, "no such host") ||
		strings.Contains(lower, "network is unreachable") ||
		strings.Contains(lower, "use of closed network connection") ||
		strings.Contains(lower, "eof")
}

// ShouldAttemptAlternateChatMode 判断流式与非流式之间是否值得做一次协议兜底。
// 只允许服务端 5xx、网络瞬态错误和响应协议不兼容触发；鉴权、参数、限流和取消
// 不切换模式，避免重复计费、请求放大以及在客户端已放弃后继续访问上游。
func ShouldAttemptAlternateChatMode(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if attempts := UpstreamAttempts(err); len(attempts) > 0 {
		last := attempts[len(attempts)-1]
		if last.HTTPStatus > 0 {
			// 带 UpstreamAttempts 的 HTTP 状态来自真实 provider HTTP 调用；
			// 此时 chat POST 已经到达上游并返回了响应，不能再通过流式/非流式
			// 互切发起第二次同内容请求。provider 内部短重试已经在返回该错误前完成。
			return false
		}
		if strings.TrimSpace(last.Stage) != "" {
			return false
		}
	}
	var httpErr *providerHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode >= http.StatusInternalServerError
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "empty reply") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "unexpected eof") ||
		strings.Contains(lower, "stream error") ||
		strings.Contains(lower, "api 错误 5") ||
		strings.Contains(lower, "upstream_server_error") ||
		strings.Contains(lower, "do_request_failed") ||
		strings.Contains(lower, "解析响应失败") ||
		strings.Contains(lower, "response is not")
}

func responseBodyPreview(body []byte) string {
	const maxPreviewBytes = 300
	preview := strings.TrimSpace(string(body))
	if len(preview) > maxPreviewBytes {
		preview = preview[:maxPreviewBytes] + "..."
	}
	return preview
}

func openAIResponseErrorFromBody(body []byte) string {
	trimmed := bytes.TrimPrefix(bytes.TrimSpace(body), []byte{0xEF, 0xBB, 0xBF})
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return ""
	}
	return openAIResponseErrorMessage(envelope.Error)
}

func openAIResponseErrorMessage(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}

	var detail struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(trimmed, &detail); err == nil && strings.TrimSpace(detail.Message) != "" {
		return detail.Message
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil && strings.TrimSpace(text) != "" {
		return text
	}
	return responseBodyPreview(trimmed)
}

func (p *OpenAIProvider) ChatStream(ctx context.Context, req *ChatRequest) (io.ReadCloser, error) {
	ctx, cancel := providerOperationContext(ctx, p.Timeout)

	req.Stream = true
	body, err := p.marshalOpenAIChatCompletionsRequest(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	// 流式路径同样要短重试。管理测试页和 VS 流式下游都会走这里，
	// 若首个 New API 渠道短暂 503，重试可避免用户看到一次性失败。
	var lastErr error
	attempts := []UpstreamAttempt{}
	for attempt := 0; attempt < p.openAIProviderMaxAttempts(); attempt++ {
		attemptStart := time.Now()
		stream, err := p.doChatStream(ctx, body)
		if err == nil {
			return &cancelReadCloser{ReadCloser: stream, cancel: cancel}, nil
		}
		lastErr = err
		attempts = append(attempts, newUpstreamAttempt(err, time.Since(attemptStart)))
		if !p.shouldRetryOpenAIProviderError(err) || attempt == p.openAIProviderMaxAttempts()-1 {
			break
		}
		if delay := openAIProviderRetryDelay(attempt); delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				cancel()
				return nil, ctx.Err()
			}
		}
	}
	cancel()
	return nil, withUpstreamAttempts(lastErr, attempts)
}

func marshalOpenAIChatCompletionsRequest(req *ChatRequest) ([]byte, error) {
	return marshalOpenAIChatCompletionsRequestWithOutputTokenParam(req, "max_tokens")
}

func (p *OpenAIProvider) marshalOpenAIChatCompletionsRequest(req *ChatRequest) ([]byte, error) {
	outputTokenParam := OutputTokenParamFor(p.capabilityName())
	return marshalOpenAIChatCompletionsRequestWithOutputTokenParam(req, outputTokenParam)
}

func marshalOpenAIChatCompletionsRequestWithOutputTokenParam(req *ChatRequest, outputTokenParam string) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return normalizeOpenAIChatCompletionsRequestBody(body, outputTokenParam)
}

func OpenAIChatCompletionsRequestBytes(req *ChatRequest) (int, error) {
	body, err := marshalOpenAIChatCompletionsRequest(req)
	if err != nil {
		return 0, err
	}
	return len(body), nil
}

func normalizeOpenAIChatCompletionsRequestBody(body []byte, outputTokenParam string) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	// OpenAI-compatible 网关（New API、sub2api 等）普遍以 /v1/chat/completions
	// 接收请求，最稳妥的输出 token 字段是 max_tokens。VS / Copilot 或
	// 新式 Responses 风格客户端可能发送 max_output_tokens / max_completion_tokens；
	// 这些别名必须在出 provider 前统一收敛，否则 strict 上游会直接 400。
	//
	// 注意：这里的“收敛目标”不能硬编码。MiMo 实测在 chat/completions 下
	// 同时接受 max_tokens 和 max_completion_tokens，但二者预算语义不同：
	// max_tokens=32 可能截断在 reasoning_content 阶段，max_completion_tokens=32
	// 能正常给出 content。因此字段名必须由 provider capability 决定。
	target := normalizeOutputTokenParam(outputTokenParam)
	for _, alias := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		maxOutput, ok := raw[alias]
		if !ok {
			continue
		}
		if _, hasTarget := raw[target]; !hasTarget {
			raw[target] = maxOutput
		}
		if alias != target {
			delete(raw, alias)
		}
	}
	return json.Marshal(raw)
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *cancelReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}

func providerOperationContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	// 代理层的单模型 timeout_seconds 是更精确的请求预算；一旦调用方已经设置
	// deadline，provider 必须继承它，不能再用默认 60 秒把长推理模型提前截断。
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

func (p *OpenAIProvider) doChatStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	p.applyOpenAIRequestHeaders(httpReq, "text/event-stream")

	resp, err := p.doChatHTTPRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := readProviderResponseBody(resp)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("API 错误 %d，且读取错误响应失败: %w", resp.StatusCode, readErr)
		}
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return nil, &providerHTTPError{
			StatusCode: resp.StatusCode,
			Body:       respBody,
			RetryAfter: retryAfter,
			Message:    providerHTTPErrorMessage(resp.StatusCode, respBody, retryAfter),
		}
	}

	return resp.Body, nil
}

func (p *OpenAIProvider) doChatHTTPRequest(req *http.Request) (*http.Response, error) {
	// ChatRaw/ChatStream 已在 operation context 上统一控制总预算。这里关闭
	// http.Client.Timeout，避免它按单次 Do 再启动一个计时器：否则 180 秒模型
	// 仍会被默认 60 秒提前截断，重试又可能为每次尝试重新获得 60 秒。
	client := *p.Client
	client.Timeout = 0
	tracedReq, requestTrace := traceUpstreamHTTPRequest(req)
	resp, err := client.Do(tracedReq)
	if err != nil {
		return nil, &upstreamTransportError{Stage: requestTrace.name(), Err: err}
	}
	return resp, nil
}

type upstreamHTTPStage int32

const (
	// 阶段值按一次 HTTP 请求的正常时序递增；trace 回调只能前进，不能因并发回调回退。
	upstreamStagePreparing upstreamHTTPStage = iota
	// upstreamStageResolvingDNS 表示正在把上游主机名解析为网络地址。
	upstreamStageResolvingDNS
	// upstreamStageConnecting 表示正在建立 TCP 连接或连接到系统配置的 HTTP 代理。
	upstreamStageConnecting
	// upstreamStageTLSHandshake 表示已建立底层连接，正在协商 TLS。
	upstreamStageTLSHandshake
	// upstreamStageWritingRequest 表示已取得连接，正在发送请求头或请求正文。
	upstreamStageWritingRequest
	// upstreamStageWaitingResponseHeaders 表示请求已完整写出，正在等待响应头首字节。
	upstreamStageWaitingResponseHeaders
	// upstreamStageReceivingResponseHeaders 表示已收到响应头首字节，Client.Do 正在完成响应建立。
	upstreamStageReceivingResponseHeaders
)

// upstreamHTTPTrace 只记录不含 URL、Header、正文的网络阶段。
// 现场超时时可据此区分 Windows 本机网络问题、请求上传卡住和上游迟迟不返回响应头。
type upstreamHTTPTrace struct {
	stage atomic.Int32
}

// traceUpstreamHTTPRequest 只替换请求 context 来挂载标准库 trace；Clone 会保留
// Body、GetBody、Header 和 ContentLength，因此不会改变发送给上游的业务请求。
func traceUpstreamHTTPRequest(req *http.Request) (*http.Request, *upstreamHTTPTrace) {
	traceState := &upstreamHTTPTrace{}
	setStage := func(stage upstreamHTTPStage) {
		next := int32(stage)
		for {
			current := traceState.stage.Load()
			if next <= current || traceState.stage.CompareAndSwap(current, next) {
				return
			}
		}
	}
	trace := &httptrace.ClientTrace{
		GetConn: func(string) {
			// 一个 Client.Do 可能跟随重定向发起多个 HTTP hop；每个 hop
			// 都必须从准备阶段重新计时，不能把上一跳的最远阶段带过来。
			traceState.stage.Store(int32(upstreamStagePreparing))
		},
		DNSStart: func(httptrace.DNSStartInfo) {
			setStage(upstreamStageResolvingDNS)
		},
		ConnectStart: func(_, _ string) {
			setStage(upstreamStageConnecting)
		},
		TLSHandshakeStart: func() {
			setStage(upstreamStageTLSHandshake)
		},
		GotConn: func(httptrace.GotConnInfo) {
			setStage(upstreamStageWritingRequest)
		},
		WroteHeaders: func() {
			setStage(upstreamStageWritingRequest)
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				setStage(upstreamStageWaitingResponseHeaders)
			}
		},
		GotFirstResponseByte: func() {
			setStage(upstreamStageReceivingResponseHeaders)
		},
	}
	ctx := httptrace.WithClientTrace(req.Context(), trace)
	return req.Clone(ctx), traceState
}

func (t *upstreamHTTPTrace) name() string {
	if t == nil {
		return "preparing_request"
	}
	switch upstreamHTTPStage(t.stage.Load()) {
	case upstreamStageResolvingDNS:
		return "resolving_dns"
	case upstreamStageConnecting:
		return "connecting"
	case upstreamStageTLSHandshake:
		return "tls_handshake"
	case upstreamStageWritingRequest:
		return "writing_request"
	case upstreamStageWaitingResponseHeaders:
		return "waiting_response_headers"
	case upstreamStageReceivingResponseHeaders:
		return "receiving_response_headers"
	default:
		return "preparing_request"
	}
}

func (p *OpenAIProvider) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", p.modelsURL(), nil)
	if err != nil {
		return nil, err
	}
	p.applyOpenAIRequestHeaders(httpReq, "application/json")

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := readProviderResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("读取模型列表响应失败（HTTP %d）: %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取模型列表失败: %s", responseBodyPreview(body))
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w; body_preview=%q", err, responseBodyPreview(body))
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

func newProviderHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       120 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     false,
			DisableCompression:    true,
		},
	}
}

func (p *OpenAIProvider) applyOpenAIRequestHeaders(req *http.Request, accept string) {
	// 部分 new-api / sub2api 部署前面有 WAF，默认 Go/Python UA 可能被拦。
	// 防御模式下统一发送稳定 UA；关闭防御时保留 Go 默认行为，便于复现原始问题。
	if p == nil || p.DefenseEnabled {
		req.Header.Set("User-Agent", providerUserAgent())
		req.Header.Set("X-Requested-With", "vs-ai-proxy")
	}
	if strings.TrimSpace(p.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	applyRequestIDHeaders(req)
	if strings.EqualFold(p.capabilityName(), "openrouter") || strings.EqualFold(p.NameStr, "openrouter") {
		applyOpenRouterHeaders(req)
	}
}

func applyRequestIDHeaders(req *http.Request) {
	requestID := requestIDFromContext(req.Context())
	if requestID == "" {
		return
	}
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("X-Proxy-Request-ID", requestID)
}

func providerUserAgent() string {
	if value := firstEnv("VS_AI_PROXY_USER_AGENT", "PROVIDER_USER_AGENT", "OPENAI_COMPAT_USER_AGENT"); value != "" {
		return value
	}
	return "vs-ai-proxy"
}

func providerHTTPErrorMessage(statusCode int, body []byte, retryAfter time.Duration) string {
	message := fmt.Sprintf("API 错误 %d: %s", statusCode, responseBodyPreview(body))
	if retryAfter > 0 {
		message = fmt.Sprintf("%s; retry_after_seconds=%d", message, int(retryAfter.Seconds()))
	}
	return message
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	// Retry-After 既可能是秒数，也可能是 HTTP date。这里只解析为冷却预算，
	// 不在 provider 内部等待重试，避免长时间占用 VS/Copilot 的请求链路。
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now).Round(time.Second)
}

func applyOpenRouterHeaders(req *http.Request) {
	if referer := firstEnv("PROVIDER_OPENROUTER_REFERER", "OPENROUTER_HTTP_REFERER"); referer != "" {
		req.Header.Set("HTTP-Referer", referer)
	}
	if title := firstEnv("PROVIDER_OPENROUTER_TITLE", "OPENROUTER_X_TITLE"); title != "" {
		req.Header.Set("X-Title", title)
	}
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		value := strings.TrimSpace(os.Getenv(key))
		if value != "" {
			return value
		}
	}
	return ""
}

func (p *OpenAIProvider) chatURL() string {
	return p.capabilityURL("chat", "v1/chat/completions")
}

func (p *OpenAIProvider) modelsURL() string {
	return p.capabilityURL("models", "v1/models")
}

func (p *OpenAIProvider) capabilityURL(kind, fallbackPath string) string {
	// 已知 provider 使用能力注册表里的路径；未知 OpenAI-compatible provider 使用标准 fallback。
	caps := GetCapabilities(p.capabilityName())
	path := fallbackPath
	switch kind {
	case "chat":
		if strings.TrimSpace(caps.ChatPath) != "" {
			path = caps.ChatPath
		}
	case "models":
		if strings.TrimSpace(caps.ModelsPath) != "" {
			path = caps.ModelsPath
		}
	}
	return joinURLPath(p.BaseURL, path)
}

func (p *OpenAIProvider) capabilityName() string {
	// NameStr 是 provider 实例 ID，例如 useai-paid / sensenova；CapabilityName 是协议能力名，
	// 例如 useai / openrouter / ollama。二者拆开后，一个 provider 类型可以配置多个 API Key 实例。
	if strings.TrimSpace(p.CapabilityName) != "" {
		return p.CapabilityName
	}
	return p.NameStr
}

func joinURLPath(baseURL, path string) string {
	// Web UI 允许用户填写两类 base_url：
	// - https://host
	// - https://host/v1
	// fallback path 又是 v1/chat/completions。如果不去重，第二类会变成 /v1/v1/...
	baseURL = strings.TrimRight(baseURL, "/")
	path = strings.TrimLeft(path, "/")
	if path == "" {
		return baseURL
	}
	path = trimOverlappingPathSegments(baseURL, path)
	if path == "" {
		return baseURL
	}
	return baseURL + "/" + path
}

func trimOverlappingPathSegments(baseURL, path string) string {
	// 从最长重叠片段开始匹配，既能处理 /v1 + v1/models，
	// 也能处理 /v1beta/openai + v1beta/openai/models。
	baseParts := strings.Split(strings.Trim(baseURL, "/"), "/")
	pathParts := strings.Split(strings.Trim(path, "/"), "/")
	maxOverlap := min(len(baseParts), len(pathParts))
	for overlap := maxOverlap; overlap > 0; overlap-- {
		matches := true
		for i := 0; i < overlap; i++ {
			if baseParts[len(baseParts)-overlap+i] != pathParts[i] {
				matches = false
				break
			}
		}
		if matches {
			return strings.Join(pathParts[overlap:], "/")
		}
	}
	return strings.Join(pathParts, "/")
}

// OllamaProvider Ollama 提供商
type OllamaProvider struct {
	NameStr        string
	CapabilityName string
	BaseURL        string
	Enabled        bool
	Client         *http.Client
}

// NewOllamaProvider 创建 Ollama 提供商
func NewOllamaProvider(name, baseURL string, enabled bool, timeout time.Duration) *OllamaProvider {
	return NewOllamaProviderWithCapability(name, "", baseURL, enabled, timeout)
}

func NewOllamaProviderWithCapability(name, capabilityName, baseURL string, enabled bool, timeout time.Duration) *OllamaProvider {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &OllamaProvider{
		NameStr:        name,
		CapabilityName: capabilityName,
		BaseURL:        strings.TrimRight(baseURL, "/"),
		Enabled:        enabled,
		Client:         newProviderHTTPClient(timeout),
	}
}

func (p *OllamaProvider) Name() string {
	return p.NameStr
}

func (p *OllamaProvider) IsEnabled() bool {
	return p.Enabled
}

func (p *OllamaProvider) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	respBody, err := p.ChatRaw(ctx, req)
	if err != nil {
		return nil, err
	}

	// 原生 Ollama 的工具参数既可能是 JSON 字符串，也可能直接是对象。
	// 使用 provider 的强类型工具结构统一解码，避免 map 路径丢失 tool_calls 或输出非 JSON 参数。
	var ollamaResp struct {
		Message struct {
			Role             string     `json:"role"`
			Content          string     `json:"content"`
			Thinking         string     `json:"thinking"`
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []ToolCall `json:"tool_calls"`
		} `json:"message"`
		DoneReason      string `json:"done_reason"`
		PromptEvalCount *int64 `json:"prompt_eval_count"`
		EvalCount       *int64 `json:"eval_count"`
	}
	if err := json.Unmarshal(respBody, &ollamaResp); err != nil {
		return nil, err
	}

	reasoning := ollamaResp.Message.Thinking
	if reasoning == "" {
		reasoning = ollamaResp.Message.ReasoningContent
	}
	finishReason := strings.TrimSpace(ollamaResp.DoneReason)
	if finishReason == "" {
		finishReason = "stop"
	}

	chatResp := &ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []Choice{{
			Index: 0,
			Message: Message{
				Role:      "assistant",
				Content:   ollamaResp.Message.Content,
				ToolCalls: ollamaResp.Message.ToolCalls,
				Reasoning: reasoning,
			},
			FinishReason: finishReason,
		}},
	}

	if ollamaResp.PromptEvalCount != nil || ollamaResp.EvalCount != nil {
		promptTokens := int64(0)
		completionTokens := int64(0)
		if ollamaResp.PromptEvalCount != nil {
			promptTokens = *ollamaResp.PromptEvalCount
		}
		if ollamaResp.EvalCount != nil {
			completionTokens = *ollamaResp.EvalCount
		}
		chatResp.Usage = &Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		}
	}
	if err := normalizeAndValidateChatResponseTools(chatResp); err != nil {
		return nil, fmt.Errorf("解析 Ollama 响应失败: %w", err)
	}

	return chatResp, nil
}

func (p *OllamaProvider) ChatRaw(ctx context.Context, req *ChatRequest) ([]byte, error) {
	ollamaReq := p.buildChatRequest(req, false)
	body, err := json.Marshal(ollamaReq)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyRequestIDHeaders(httpReq)

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := readProviderResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败（HTTP %d）: %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama 错误 %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func (p *OllamaProvider) ChatStream(ctx context.Context, req *ChatRequest) (io.ReadCloser, error) {
	req.Stream = true
	body, err := json.Marshal(p.buildChatRequest(req, true))
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyRequestIDHeaders(httpReq)

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("Ollama 错误 %d", resp.StatusCode)
	}

	return resp.Body, nil
}

func (p *OllamaProvider) buildChatRequest(req *ChatRequest, stream bool) map[string]any {
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, msg := range req.Messages {
		messages = append(messages, messageToMap(msg))
	}

	options := map[string]any{}
	for key, raw := range req.OptionsExtra {
		if _, exists := ollamaOptionKnownFields[key]; exists || len(raw) == 0 {
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err == nil {
			options[key] = value
		}
	}
	if req.Temperature != nil {
		options["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		options["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		options["top_k"] = *req.TopK
	}
	if req.MaxTokens != nil {
		options["num_predict"] = *req.MaxTokens
	}
	if req.ContextLength != nil {
		options["num_ctx"] = *req.ContextLength
	}
	if strings.TrimSpace(req.ReasoningEffort) != "" {
		options["reasoning_effort"] = req.ReasoningEffort
	}
	if len(req.Stop) > 0 {
		// Ollama 原生 ChatRequest 把停止词定义在 options.stop，而不是请求顶层。
		// 放错层级时字段虽然还在 JSON 中，却会被原生 Ollama 静默忽略。
		options["stop"] = req.Stop
	}

	ollamaReq := map[string]any{
		"model":    req.Model,
		"messages": messages,
		"stream":   stream,
	}
	if len(options) > 0 {
		ollamaReq["options"] = options
	}
	if len(req.Tools) > 0 {
		ollamaReq["tools"] = req.Tools
	} else if tools := legacyFunctionsToTools(req.Extra["functions"]); len(tools) > 0 {
		// Visual Studio 仍可能发送 legacy functions；Ollama 原生接口只接受
		// tools，因此在 provider 边界做一次结构化转换，避免声明在中转时消失。
		ollamaReq["tools"] = tools
	}
	for _, field := range []string{"tool_choice", "parallel_tool_calls", "function_call"} {
		if value, ok := decodeRequestExtraValue(req.Extra[field]); ok {
			// 保留工具选择/并行控制和 legacy 强制调用语义；支持该字段的
			// Ollama-compatible 网关可以直接使用，不支持的原生版本会忽略未知字段。
			ollamaReq[field] = value
		}
	}
	return ollamaReq
}

// legacyFunctionsToTools 把 OpenAI legacy functions 转成 Ollama 支持的 tools。
// 非法或空声明不在这里猜测修复，避免生成名称为空、客户端无法执行的工具。
func legacyFunctionsToTools(raw json.RawMessage) []Tool {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var functions []ToolFunc
	if err := json.Unmarshal(raw, &functions); err != nil {
		return nil
	}
	tools := make([]Tool, 0, len(functions))
	for _, function := range functions {
		// 不静默丢弃缺少 name 的声明；让上游按原始工具契约返回明确错误，
		// 否则请求中的 functions 数量会在 provider 边界无提示地改变。
		tools = append(tools, Tool{Type: "function", Function: function})
	}
	return tools
}

// decodeRequestExtraValue 将已由 ChatRequest 保留的扩展字段恢复为结构化值。
// 返回 false 表示字段缺失、为 null 或无法解码，调用方不应向上游发送占位值。
func decodeRequestExtraValue(raw json.RawMessage) (any, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	return value, true
}

func messageToMap(msg Message) map[string]any {
	data, err := json.Marshal(msg)
	if err != nil {
		return map[string]any{
			"role":    msg.Role,
			"content": msg.Content,
		}
	}

	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil || out == nil {
		return map[string]any{
			"role":    msg.Role,
			"content": msg.Content,
		}
	}
	applyOllamaImageContent(out)
	return out
}

func applyOllamaImageContent(msg map[string]any) {
	parts, ok := msg["content"].([]any)
	if !ok || len(parts) == 0 {
		return
	}

	textParts := []string{}
	images := []string{}
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch part["type"] {
		case "text":
			if text, ok := part["text"].(string); ok && strings.TrimSpace(text) != "" {
				textParts = append(textParts, text)
			}
		case "image_url":
			image, _ := part["image_url"].(map[string]any)
			if url, ok := image["url"].(string); ok && strings.TrimSpace(url) != "" {
				images = append(images, url)
			}
		}
	}

	msg["content"] = strings.Join(textParts, "\n")
	if len(images) > 0 {
		msg["images"] = images
	}
}

func (p *OllamaProvider) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", p.BaseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := readProviderResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("读取模型列表响应失败（HTTP %d）: %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取模型列表失败: %s", string(body))
	}

	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	models := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		if m.Name != "" {
			models = append(models, m.Name)
		}
	}
	return models, nil
}

func readProviderResponseBody(resp *http.Response) ([]byte, error) {
	if resp.StatusCode != http.StatusOK {
		return io.ReadAll(io.LimitReader(resp.Body, maxProviderErrorResponseBodyBytes))
	}
	return readBoundedBody(resp.Body, maxProviderResponseBodyBytes)
}

func readBoundedBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: 最大允许 %d 字节", errProviderResponseBodyTooLarge, limit)
	}
	return body, nil
}

// SSEEvent SSE 事件
type SSEEvent struct {
	Event string
	Data  string
}

// StreamReader 读取 SSE 流
func StreamReader(r io.Reader) <-chan SSEEvent {
	ch := make(chan SSEEvent, 10)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(line[5:])
				if data == "[DONE]" {
					ch <- SSEEvent{Event: "done", Data: ""}
					return
				}
				ch <- SSEEvent{Event: "message", Data: data}
			}
		}
	}()
	return ch
}
