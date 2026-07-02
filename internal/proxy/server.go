package proxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/converter"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

// Server 代理服务器
// 负责接收 Visual Studio / Ollama 客户端的兼容协议请求，
// 按模型名称解析到对应 provider，并转发到上游 AI 服务。
type Server struct {
	mu             sync.RWMutex
	config         *config.AppConfig
	configMgr      *config.Manager
	registry       *provider.Registry
	store          *store.Store
	logger         *log.Logger
	server         *http.Server
	proxyKey       string
	reasoningCache *reasoningCache
}

// NewServer 创建代理服务器
func NewServer(cfg *config.AppConfig, configMgr *config.Manager, st *store.Store, logger *log.Logger) *Server {
	s := &Server{
		config:         cfg,
		configMgr:      configMgr,
		store:          st,
		logger:         logger,
		proxyKey:       strings.TrimSpace(os.Getenv("PROXY_API_KEY")),
		reasoningCache: newReasoningCache(),
	}

	s.registry = s.buildRegistry(cfg)

	return s
}

// Reconfigure 热更新代理路由配置。
// 已绑定的监听端口不会在运行中切换；端口变更仍需要进程重启。
func (s *Server) Reconfigure(cfg *config.AppConfig) {
	if cfg == nil {
		return
	}

	registry := s.buildRegistry(cfg)

	s.mu.Lock()
	s.config = cfg
	s.registry = registry
	s.mu.Unlock()

	s.logger.Info("代理配置已热更新: providers=%d models=%d", len(cfg.Providers), len(cfg.Models))
}

func (s *Server) snapshot() (*config.AppConfig, *provider.Registry) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config, s.registry
}

func (s *Server) buildRegistry(cfg *config.AppConfig) *provider.Registry {
	registry := provider.NewRegistry(cfg.DefaultModel, 5*time.Minute)
	for _, p := range cfg.Providers {
		prov := s.providerFromConfig(p)
		if prov == nil {
			continue
		}

		registry.Add(&provider.ProviderEntry{
			Provider: prov,
			Models:   []string{},
			Priority: p.Priority,
		})

		s.logger.Info("已注册提供商: %s (%s)", p.Name, p.Type)
	}
	return registry
}

func (s *Server) providerFromConfig(p config.ProviderConfig) provider.Provider {
	timeout := 60 * time.Second
	switch p.Type {
	case "ollama":
		return provider.NewOllamaProvider(p.Name, p.BaseURL, p.Enabled, timeout)
	case "openai", "custom":
		return provider.NewOpenAIProvider(p.Name, p.APIKey, p.BaseURL, p.Enabled, timeout)
	default:
		s.logger.Warn("未知提供商类型: %s", p.Type)
		return nil
	}
}

// refreshModels 刷新提供商模型列表
// 仅对启用状态的 provider 执行一次 ListModels，
// 结果写入 entry.Models，供后续模型列表接口汇总使用。
func (s *Server) refreshModels(prov provider.Provider, entry *provider.ProviderEntry) {
	if !prov.IsEnabled() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	models, err := prov.ListModels(ctx)
	if err != nil {
		s.logger.Warn("获取提供商 %s 模型列表失败: %v", prov.Name(), err)
		return
	}

	s.registry.SetModels(prov.Name(), models)

	s.logger.Info("提供商 %s 发现 %d 个模型", prov.Name(), len(models))
}

// Start 启动服务器
func (s *Server) Start() error {
	cfg, _ := s.snapshot()
	mux := http.NewServeMux()

	// 代理端点
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleListModels)

	// Ollama 兼容端点
	mux.HandleFunc("/api/chat", s.handleOllamaChat)
	mux.HandleFunc("/api/tags", s.handleOllamaTags)
	mux.HandleFunc("/api/show", s.handleOllamaShow)

	// 健康检查
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/", s.handleRoot)

	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      s.loggingMiddleware(s.authMiddleware(mux)),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	s.logger.Info("代理服务器启动于 http://localhost:%d", cfg.Port)
	return s.server.ListenAndServe()
}

// Stop 停止服务器
func (s *Server) Stop() error {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.server.Shutdown(ctx)
	}
	return nil
}

// loggingMiddleware 日志中间件
// 记录每次代理请求的方法、路径、状态码和耗时，
// 同时将请求日志写入 Store，供管理界面查看。
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(ww, r)

		elapsed := time.Since(start).Seconds() * 1000
		s.store.AddLog(store.RequestLog{
			Method:     r.Method,
			Path:       r.URL.Path,
			StatusCode: ww.statusCode,
			ElapsedMs:  elapsed,
			IsSuccess:  ww.statusCode < 400,
		})

		s.logger.Info("%s %s - %d (%.0f ms)", r.Method, r.URL.Path, ww.statusCode, elapsed)
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if s.proxyKey == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAuthorizedProxyRequest(r, s.proxyKey) {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	})
}

func isAuthorizedProxyRequest(r *http.Request, proxyKey string) bool {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return false
	}

	token := strings.TrimSpace(auth[len(prefix):])
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(proxyKey)) == 1
}

// responseWriter 记录状态码
// 通过包装 http.ResponseWriter，在 WriteHeader 被调用时缓存最终状态码。
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *responseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok && flusher != nil {
		flusher.Flush()
	}
}

// handleHealth 健康检查
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleRoot 根路径
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("VS AI Proxy is running"))
}

// handleChatCompletions OpenAI 聊天补全
// 对外暴露 OpenAI 兼容的 /v1/chat/completions 接口，
// 内部会解析 model 到具体 provider，应用默认参数后转发。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req provider.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "解析请求失败", http.StatusBadRequest)
		return
	}

	cfg, registry := s.snapshot()

	// 解析模型
	modelName := req.Model
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	baseReq := cloneChatRequest(&req)

	var lastErr error
	candidates := registry.ResolveCandidates(modelName)
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

		req := cloneChatRequest(baseReq)
		req.Model = modelID

		s.transformRequest(cfg, req, modelName, prov)

		if req.Stream {
			err := s.handleStream(w, r, prov, req, provider.ApiFormatOpenAi)
			if err != nil {
				lastErr = err
				s.logger.Warn("模型 %s 在提供商 %s 流式失败: %v", modelID, prov.Name(), err)
				continue
			}
			return
		}

		resp, err := prov.Chat(r.Context(), req)
		if err != nil {
			lastErr = err
			s.logger.Warn("模型 %s 在提供商 %s 失败: %v", modelID, prov.Name(), err)
			continue
		}
		s.cacheChatResponse(resp)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr != nil {
		http.Error(w, lastErr.Error(), http.StatusBadGateway)
	} else {
		http.Error(w, "所有候选提供商均不可用", http.StatusServiceUnavailable)
	}
}

// handleOllamaChat Ollama 聊天
// 对外暴露 /api/chat，兼容 Ollama 客户端调用。
// 内部会把 Ollama 请求转换为 OpenAI 内部格式，再转回 Ollama 响应格式。
func (s *Server) handleOllamaChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var ollamaReq map[string]any
	if err := json.Unmarshal(body, &ollamaReq); err != nil {
		http.Error(w, "解析请求失败", http.StatusBadRequest)
		return
	}

	cfg, registry := s.snapshot()

	modelName, _ := ollamaReq["model"].(string)
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	candidates := registry.ResolveCandidates(modelName)
	if len(candidates) == 0 {
		http.Error(w, fmt.Sprintf("模型 %s 无可用提供商", modelName), http.StatusBadRequest)
		return
	}

	messages := make([]provider.Message, 0)
	if msgs, ok := ollamaReq["messages"].([]any); ok {
		for _, m := range msgs {
			if msgMap, ok := m.(map[string]any); ok {
				role, _ := msgMap["role"].(string)
				content, _ := msgMap["content"].(string)
				messages = append(messages, provider.Message{Role: role, Content: content})
			}
		}
	}

	stream := false
	if s, ok := ollamaReq["stream"].(bool); ok {
		stream = s
	}

	var lastErr error
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

		req := &provider.ChatRequest{
			Model:    modelID,
			Messages: messages,
			Stream:   stream,
		}

		s.transformRequest(cfg, req, modelName, prov)

		if stream {
			err := s.handleStream(w, r, prov, req, provider.ApiFormatOllama)
			if err != nil {
				lastErr = err
				s.logger.Warn("模型 %s 在提供商 %s 流式失败: %v", modelID, prov.Name(), err)
				continue
			}
			return
		}

		resp, err := prov.Chat(r.Context(), req)
		if err != nil {
			lastErr = err
			s.logger.Warn("模型 %s 在提供商 %s 失败: %v", modelID, prov.Name(), err)
			continue
		}
		s.cacheChatResponse(resp)

		ollamaResp := map[string]any{
			"model":      modelName,
			"created_at": time.Now().Format(time.RFC3339),
			"message": map[string]any{
				"role":    "assistant",
				"content": resp.Choices[0].Message.Content,
			},
			"done": true,
		}

		if resp.Usage != nil {
			ollamaResp["prompt_eval_count"] = resp.Usage.PromptTokens
			ollamaResp["eval_count"] = resp.Usage.CompletionTokens
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ollamaResp)
		return
	}

	if lastErr != nil {
		http.Error(w, lastErr.Error(), http.StatusBadGateway)
	} else {
		http.Error(w, "所有候选提供商均不可用", http.StatusServiceUnavailable)
	}
}

// handleStream 处理流式响应
// 根据客户端协议面和 provider 上游格式执行 SSE/NDJSON 转换。
func (s *Server) handleStream(
	w http.ResponseWriter,
	r *http.Request,
	prov provider.Provider,
	req *provider.ChatRequest,
	clientFormat provider.ApiFormat,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return fmt.Errorf("response writer does not support flushing")
	}

	apiFormat := provider.ResolveApiFormat(prov)
	if clientFormat == provider.ApiFormatOpenAi && apiFormat == provider.ApiFormatOllama {
		return s.streamOllamaToOpenAI(w, r, prov, req, flusher)
	}
	if clientFormat == provider.ApiFormatOllama && apiFormat == provider.ApiFormatOllama {
		return s.streamOllamaPassthrough(w, r, prov, req, flusher)
	}
	if clientFormat == provider.ApiFormatOllama {
		return s.streamOpenAIToOllama(w, r, prov, req, flusher)
	}

	return s.streamOpenAI(w, r, prov, req, flusher)
}

// streamOpenAI OpenAI 直通流式转发
func (s *Server) streamOpenAI(w http.ResponseWriter, r *http.Request, prov provider.Provider, req *provider.ChatRequest, flusher http.Flusher) error {
	stream, err := prov.ChatStream(r.Context(), req)
	if err != nil {
		return fmt.Errorf("openai stream error: %w", err)
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if r.Context().Err() != nil {
		return r.Context().Err()
	}

	scanner := bufio.NewScanner(stream)
	acc := newStreamReasoningAccumulator()
	for scanner.Scan() {
		line := scanner.Text()
		acc.consumeOpenAISSELine(line)
		if _, writeErr := w.Write([]byte(line + "\n")); writeErr != nil {
			return writeErr
		}
		flusher.Flush()

		if r.Context().Err() != nil {
			return r.Context().Err()
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	s.cacheStreamAccumulator(acc)
	return nil
}

// streamOllamaToOpenAI 将 Ollama NDJSON 流转换为 OpenAI SSE。
func (s *Server) streamOllamaToOpenAI(w http.ResponseWriter, r *http.Request, prov provider.Provider, req *provider.ChatRequest, flusher http.Flusher) error {
	stream, err := prov.ChatStream(r.Context(), req)
	if err != nil {
		return fmt.Errorf("ollama stream error: %w", err)
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if r.Context().Err() != nil {
		return r.Context().Err()
	}

	scanner := bufio.NewScanner(stream)
	acc := newStreamReasoningAccumulator()
	for scanner.Scan() {
		line := scanner.Text()
		chunk, parseErr := converter.ParseOllamaStreamChunk(line)
		if parseErr != nil {
			if parseErr == converter.ErrStreamDone {
				break
			}
			continue
		}
		acc.consumeOllamaChunk(chunk)

		out, convErr := converter.ConvertOllamaChunkToOpenAISSE(chunk, req.Model)
		if convErr != nil {
			return convErr
		}

		if _, writeErr := w.Write(append(out, '\n')); writeErr != nil {
			return writeErr
		}
		flusher.Flush()

		if r.Context().Err() != nil {
			return r.Context().Err()
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	s.cacheStreamAccumulator(acc)
	return nil
}

// streamOllamaPassthrough 对 Ollama 客户端保持原生 NDJSON 输出。
func (s *Server) streamOllamaPassthrough(w http.ResponseWriter, r *http.Request, prov provider.Provider, req *provider.ChatRequest, flusher http.Flusher) error {
	stream, err := prov.ChatStream(r.Context(), req)
	if err != nil {
		return fmt.Errorf("ollama stream error: %w", err)
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	scanner := bufio.NewScanner(stream)
	acc := newStreamReasoningAccumulator()
	for scanner.Scan() {
		line := scanner.Text()
		chunk, parseErr := converter.ParseOllamaStreamChunk(line)
		if parseErr == nil {
			acc.consumeOllamaChunk(chunk)
		}
		if _, writeErr := w.Write([]byte(line + "\n")); writeErr != nil {
			return writeErr
		}
		flusher.Flush()

		if r.Context().Err() != nil {
			return r.Context().Err()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	s.cacheStreamAccumulator(acc)
	return nil
}

// streamOpenAIToOllama 将 OpenAI SSE 流转换为 Ollama NDJSON。
func (s *Server) streamOpenAIToOllama(w http.ResponseWriter, r *http.Request, prov provider.Provider, req *provider.ChatRequest, flusher http.Flusher) error {
	stream, err := prov.ChatStream(r.Context(), req)
	if err != nil {
		return fmt.Errorf("openai stream error: %w", err)
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	finishReason := "stop"
	scanner := bufio.NewScanner(stream)
	acc := newStreamReasoningAccumulator()
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(line[5:])
		if payload == "[DONE]" {
			break
		}

		chunk, err := parseOpenAIStreamPayload(payload)
		if err != nil {
			continue
		}
		acc.consumeOpenAIChunk(chunk)
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		if chunk.Content == "" && len(chunk.ToolCalls) == 0 {
			continue
		}

		out, err := buildOllamaStreamChunk(req.Model, chunk.Content, chunk.ToolCalls, false, "")
		if err != nil {
			return err
		}
		if _, writeErr := w.Write(append(out, '\n')); writeErr != nil {
			return writeErr
		}
		flusher.Flush()

		if r.Context().Err() != nil {
			return r.Context().Err()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	s.cacheStreamAccumulator(acc)

	out, err := buildOllamaStreamChunk(req.Model, "", nil, true, finishReason)
	if err != nil {
		return err
	}
	if _, writeErr := w.Write(append(out, '\n')); writeErr != nil {
		return writeErr
	}
	flusher.Flush()
	return nil
}

type openAIStreamChunk struct {
	Content      string
	Reasoning    string
	ToolCalls    []any
	FinishReason string
}

func parseOpenAIStreamPayload(payload string) (openAIStreamChunk, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return openAIStreamChunk{}, err
	}

	choices, _ := root["choices"].([]any)
	if len(choices) == 0 {
		return openAIStreamChunk{}, nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return openAIStreamChunk{}, nil
	}

	out := openAIStreamChunk{}
	if finish, ok := choice["finish_reason"].(string); ok {
		out.FinishReason = finish
	}

	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		delta, _ = choice["message"].(map[string]any)
	}
	if delta == nil {
		return out, nil
	}
	if content, ok := delta["content"].(string); ok {
		out.Content = content
	}
	if reasoning, ok := delta["reasoning_content"].(string); ok {
		out.Reasoning = reasoning
	}
	if toolCalls, ok := delta["tool_calls"].([]any); ok {
		out.ToolCalls = toolCalls
	}
	return out, nil
}

func buildOllamaStreamChunk(model, content string, toolCalls []any, done bool, doneReason string) ([]byte, error) {
	message := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	chunk := map[string]any{
		"model":      model,
		"created_at": time.Now().Format(time.RFC3339),
		"message":    message,
		"done":       done,
	}
	if done && doneReason != "" {
		chunk["done_reason"] = doneReason
	}
	return json.Marshal(chunk)
}

// handleListModels 列出模型
// 汇总所有启用 provider 的模型列表，并以 OpenAI /v1/models 格式返回。
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	_, registry := s.snapshot()
	models := registry.AllModels()

	items := make([]map[string]any, 0, len(models))
	for _, model := range models {
		provName := s.resolveProviderName(registry, model)
		items = append(items, map[string]any{
			"id":       model,
			"object":   "model",
			"owned_by": coalesceString(provName, "unknown"),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   items,
	})
}

// handleOllamaTags Ollama 标签列表
// 汇总所有启用 provider 的模型列表，并以 Ollama /api/tags 格式返回。
func (s *Server) handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	_, registry := s.snapshot()
	models := registry.AllModels()

	items := make([]map[string]any, 0, len(models))
	for _, model := range models {
		items = append(items, map[string]any{
			"name":        model,
			"model":       model,
			"modified_at": time.Now().Format(time.RFC3339),
			"size":        0,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"models": items,
	})
}

// handleOllamaShow Ollama 模型详情
// 当前仅返回占位详情，未从上游查询真实模型信息。
func (s *Server) handleOllamaShow(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	if model == "" {
		http.Error(w, "缺少 model 参数", http.StatusBadRequest)
		return
	}

	cfg, registry := s.snapshot()
	body, err := s.buildOllamaShowBody(cfg, registry, model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// resolveProviderName 从模型名尝试反推 provider 名称
func (s *Server) resolveProviderName(registry *provider.Registry, model string) string {
	candidates := registry.ResolveCandidates(model)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].Provider.Provider.Name()
}

func coalesceString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func float64Ptr(f float64) *float64 {
	return &f
}

func intPtr(i int) *int {
	return &i
}
