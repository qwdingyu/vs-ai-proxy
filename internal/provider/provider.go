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
	"os"
	"strings"
	"time"
)

// ChatRequest 聊天请求
type ChatRequest struct {
	Model           string                     `json:"model"`
	Messages        []Message                  `json:"messages"`
	Temperature     *float64                   `json:"temperature,omitempty"`
	TopP            *float64                   `json:"top_p,omitempty"`
	TopK            *int                       `json:"top_k,omitempty"`
	MaxTokens       *int                       `json:"max_tokens,omitempty"`
	ReasoningEffort string                     `json:"reasoning_effort,omitempty"`
	Stream          bool                       `json:"stream"`
	Tools           []Tool                     `json:"tools,omitempty"`
	Stop            []string                   `json:"stop,omitempty"`
	Extra           map[string]json.RawMessage `json:"-"`
	OptionsExtra    map[string]json.RawMessage `json:"-"`
}

// Message 消息
type Message struct {
	Role         string                     `json:"role"`
	Content      string                     `json:"content"`
	ContentRaw   json.RawMessage            `json:"-"`
	ToolCalls    []ToolCall                 `json:"tool_calls,omitempty"`
	ToolCallID   string                     `json:"tool_call_id,omitempty"`
	FunctionCall *FunctionCall              `json:"function_call,omitempty"`
	Reasoning    string                     `json:"reasoning_content,omitempty"`
	Extra        map[string]json.RawMessage `json:"-"`
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
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
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

// OpenAIProvider OpenAI 兼容提供商
type OpenAIProvider struct {
	NameStr        string
	CapabilityName string
	APIKey         string
	BaseURL        string
	Enabled        bool
	Client         *http.Client
	Timeout        time.Duration
}

type providerHTTPError struct {
	StatusCode int
	Body       []byte
	Message    string
}

func (e *providerHTTPError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
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
		Client:         newProviderHTTPClient(timeout),
		Timeout:        timeout,
	}
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

	return &chatResp, nil
}

func parseOpenAIChatSSEAsResponse(body []byte, model string) (*ChatResponse, error) {
	if !bytes.Contains(body, []byte("data:")) {
		return nil, fmt.Errorf("响应不是 SSE")
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var content strings.Builder
	finishReason := "stop"
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "[DONE]" {
			break
		}
		chunkContent, chunkFinish, err := parseOpenAIChatSSEChunk(payload)
		if err != nil {
			continue
		}
		content.WriteString(chunkContent)
		if chunkFinish != "" {
			finishReason = chunkFinish
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(content.String()) == "" {
		return nil, fmt.Errorf("SSE 响应没有文本内容")
	}
	return &ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-sse-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: "assistant", Content: content.String()},
			FinishReason: finishReason,
		}},
	}, nil
}

func parseOpenAIChatSSEChunk(payload string) (string, string, error) {
	var root struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return "", "", err
	}
	if len(root.Choices) == 0 {
		return "", "", nil
	}
	choice := root.Choices[0]
	if choice.Delta.Content != "" {
		return choice.Delta.Content, choice.FinishReason, nil
	}
	return choice.Message.Content, choice.FinishReason, nil
}

func (p *OpenAIProvider) ChatRaw(ctx context.Context, req *ChatRequest) ([]byte, error) {
	ctx, cancel := providerOperationContext(ctx, p.Timeout)
	defer cancel()

	req.Stream = false
	body, err := marshalOpenAIChatCompletionsRequest(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	// New API 内部可能配置多个渠道，但实测单渠道 5xx/EOF 有时会直接透出。
	// 代理侧只对瞬态错误做短重试，给上游网关重新选择渠道的机会；4xx 不重试，避免放大参数/鉴权错误。
	var lastErr error
	for attempt := 0; attempt < openAIProviderMaxAttempts; attempt++ {
		respBody, err := p.doChatRaw(ctx, body)
		if err == nil {
			return respBody, nil
		}
		lastErr = err
		if !shouldRetryOpenAIProviderError(err) || attempt == openAIProviderMaxAttempts-1 {
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
	return nil, lastErr
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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &providerHTTPError{
			StatusCode: resp.StatusCode,
			Body:       respBody,
			Message:    fmt.Sprintf("API 错误 %d: %s", resp.StatusCode, responseBodyPreview(respBody)),
		}
	}

	return respBody, nil
}

const openAIProviderMaxAttempts = 3

func openAIProviderRetryDelay(attempt int) time.Duration {
	return time.Duration(attempt+1) * 200 * time.Millisecond
}

func shouldRetryOpenAIProviderError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *providerHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode >= http.StatusInternalServerError
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "empty reply") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "eof") ||
		strings.Contains(lower, "stream error") ||
		strings.Contains(lower, "timeout")
}

// ShouldAttemptAlternateChatMode 判断流式与非流式之间是否值得做一次协议兜底。
// 只允许服务端 5xx、网络瞬态错误和响应协议不兼容触发；鉴权、参数、限流和取消
// 不切换模式，避免重复计费、请求放大以及在客户端已放弃后继续访问上游。
func ShouldAttemptAlternateChatMode(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
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

func (p *OpenAIProvider) ChatStream(ctx context.Context, req *ChatRequest) (io.ReadCloser, error) {
	ctx, cancel := providerOperationContext(ctx, p.Timeout)

	req.Stream = true
	body, err := marshalOpenAIChatCompletionsRequest(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	// 流式路径同样要短重试。管理测试页和 VS 流式下游都会走这里，
	// 若首个 New API 渠道短暂 503，重试可避免用户看到一次性失败。
	var lastErr error
	for attempt := 0; attempt < openAIProviderMaxAttempts; attempt++ {
		stream, err := p.doChatStream(ctx, body)
		if err == nil {
			return &cancelReadCloser{ReadCloser: stream, cancel: cancel}, nil
		}
		lastErr = err
		if !shouldRetryOpenAIProviderError(err) || attempt == openAIProviderMaxAttempts-1 {
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
	return nil, lastErr
}

func marshalOpenAIChatCompletionsRequest(req *ChatRequest) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return normalizeOpenAIChatCompletionsRequestBody(body)
}

func normalizeOpenAIChatCompletionsRequestBody(body []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	// OpenAI-compatible 网关（New API、sub2api 等）普遍以 /v1/chat/completions
	// 接收请求，最稳妥的输出 token 字段是 max_tokens。VS / Copilot 或
	// 新式 Responses 风格客户端可能发送 max_output_tokens / max_completion_tokens；
	// 这些别名必须在出 provider 前统一收敛，否则 strict 上游会直接 400。
	for _, alias := range []string{"max_completion_tokens", "max_output_tokens"} {
		maxOutput, ok := raw[alias]
		if !ok {
			continue
		}
		if _, hasMaxTokens := raw["max_tokens"]; !hasMaxTokens {
			raw["max_tokens"] = maxOutput
		}
		delete(raw, alias)
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
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("API 错误 %d，且读取错误响应失败: %w", resp.StatusCode, readErr)
		}
		return nil, &providerHTTPError{
			StatusCode: resp.StatusCode,
			Body:       respBody,
			Message:    fmt.Sprintf("API 错误 %d: %s", resp.StatusCode, responseBodyPreview(respBody)),
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
	return client.Do(req)
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
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
	if strings.TrimSpace(p.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.EqualFold(p.capabilityName(), "openrouter") || strings.EqualFold(p.NameStr, "openrouter") {
		applyOpenRouterHeaders(req)
	}
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

	// 转换为 OpenAI 格式
	var ollamaResp map[string]any
	if err := json.Unmarshal(respBody, &ollamaResp); err != nil {
		return nil, err
	}

	message, _ := ollamaResp["message"].(map[string]any)
	content := ""
	reasoning := ""
	if message != nil {
		content, _ = message["content"].(string)
		reasoning, _ = message["thinking"].(string)
		if reasoning == "" {
			reasoning, _ = message["reasoning_content"].(string)
		}
	}

	chatResp := &ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: "assistant", Content: content, Reasoning: reasoning},
			FinishReason: "stop",
		}},
	}

	if usage, ok := ollamaResp["prompt_eval_count"].(float64); ok {
		promptTokens := int(usage)
		completionTokens := 0
		if evalCount, ok := ollamaResp["eval_count"].(float64); ok {
			completionTokens = int(evalCount)
		}
		chatResp.Usage = &Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		}
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

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
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
		options["max_tokens"] = *req.MaxTokens
	}
	if strings.TrimSpace(req.ReasoningEffort) != "" {
		options["reasoning_effort"] = req.ReasoningEffort
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
	}
	return ollamaReq
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
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
