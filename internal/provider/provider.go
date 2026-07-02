package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
}

// Message 消息
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Reasoning  string     `json:"reasoning_content,omitempty"`
}

// ToolCall 工具调用
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 函数调用
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool 工具定义
type Tool struct {
	Type     string   `json:"type"`
	Function ToolFunc `json:"function"`
}

// ToolFunc 工具函数
type ToolFunc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
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
	NameStr string
	APIKey  string
	BaseURL string
	Enabled bool
	Client  *http.Client
	Timeout time.Duration
}

// NewOpenAIProvider 创建 OpenAI 提供商
func NewOpenAIProvider(name, apiKey, baseURL string, enabled bool, timeout time.Duration) *OpenAIProvider {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &OpenAIProvider{
		NameStr: name,
		APIKey:  apiKey,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Enabled: enabled,
		Client:  &http.Client{Timeout: timeout},
		Timeout: timeout,
	}
}

func (p *OpenAIProvider) Name() string {
	return p.NameStr
}

func (p *OpenAIProvider) IsEnabled() bool {
	return p.Enabled
}

func (p *OpenAIProvider) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

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
		return nil, fmt.Errorf("API 错误 %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return &chatResp, nil
}

func (p *OpenAIProvider) ChatStream(ctx context.Context, req *ChatRequest) (io.ReadCloser, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("API 错误 %d", resp.StatusCode)
	}

	return resp.Body, nil
}

func (p *OpenAIProvider) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", p.modelsURL(), nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

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
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

func (p *OpenAIProvider) chatURL() string {
	return p.capabilityURL("chat", "v1/chat/completions")
}

func (p *OpenAIProvider) modelsURL() string {
	return p.capabilityURL("models", "v1/models")
}

func (p *OpenAIProvider) capabilityURL(kind, fallbackPath string) string {
	caps := GetCapabilities(p.NameStr)
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

func joinURLPath(baseURL, path string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	path = strings.TrimLeft(path, "/")
	if path == "" {
		return baseURL
	}
	return baseURL + "/" + path
}

// OllamaProvider Ollama 提供商
type OllamaProvider struct {
	NameStr string
	BaseURL string
	Enabled bool
	Client  *http.Client
}

// NewOllamaProvider 创建 Ollama 提供商
func NewOllamaProvider(name, baseURL string, enabled bool, timeout time.Duration) *OllamaProvider {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &OllamaProvider{
		NameStr: name,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Enabled: enabled,
		Client:  &http.Client{Timeout: timeout},
	}
}

func (p *OllamaProvider) Name() string {
	return p.NameStr
}

func (p *OllamaProvider) IsEnabled() bool {
	return p.Enabled
}

func (p *OllamaProvider) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
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
		m := map[string]any{
			"role":    msg.Role,
			"content": msg.Content,
		}
		if len(msg.ToolCalls) > 0 {
			m["tool_calls"] = msg.ToolCalls
		}
		if strings.TrimSpace(msg.Reasoning) != "" {
			m["reasoning_content"] = msg.Reasoning
		}
		messages = append(messages, m)
	}

	options := map[string]any{}
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
