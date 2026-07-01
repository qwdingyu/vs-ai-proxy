package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

// Server 代理服务器
// 负责接收 Visual Studio / Ollama 客户端的兼容协议请求，
// 按模型名称解析到对应 provider，并转发到上游 AI 服务。
type Server struct {
	config    *config.AppConfig
	configMgr *config.Manager
	providers map[string]*ProviderEntry
	store     *store.Store
	logger    *log.Logger
	server    *http.Server
	mu        sync.RWMutex
}

// ProviderEntry 提供商条目
// Provider 是实际的上游调用实现；
// Models 是异步刷新出来的可用模型列表，用于 /v1/models 和 /api/tags 汇总。
type ProviderEntry struct {
	Provider provider.Provider
	Models   []string
}

// NewServer 创建代理服务器
func NewServer(cfg *config.AppConfig, configMgr *config.Manager, st *store.Store, logger *log.Logger) *Server {
	s := &Server{
		config:    cfg,
		configMgr: configMgr,
		providers: make(map[string]*ProviderEntry),
		store:     st,
		logger:    logger,
	}

	// 初始化提供商
	for _, p := range cfg.Providers {
		s.registerProvider(p)
	}

	return s
}

// registerProvider 注册提供商
// 根据配置中的 Type 创建 OpenAI 或 Ollama 适配实现，
// 并异步触发一次模型列表刷新。
func (s *Server) registerProvider(p config.ProviderConfig) {
	var prov provider.Provider
	timeout := 60 * time.Second

	switch p.Type {
	case "ollama":
		prov = provider.NewOllamaProvider(p.Name, p.BaseURL, p.Enabled, timeout)
	case "openai", "custom":
		prov = provider.NewOpenAIProvider(p.Name, p.APIKey, p.BaseURL, p.Enabled, timeout)
	default:
		s.logger.Warn("未知提供商类型: %s", p.Type)
		return
	}

	entry := &ProviderEntry{
		Provider: prov,
		Models:   []string{},
	}

	// 异步获取模型列表
	go s.refreshModels(prov, entry)

	s.mu.Lock()
	s.providers[p.Name] = entry
	s.mu.Unlock()

	s.logger.Info("已注册提供商: %s (%s)", p.Name, p.Type)
}

// refreshModels 刷新提供商模型列表
// 仅对启用状态的 provider 执行一次 ListModels，
// 结果写入 entry.Models，供后续模型列表接口汇总使用。
func (s *Server) refreshModels(prov provider.Provider, entry *ProviderEntry) {
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

	s.mu.Lock()
	entry.Models = models
	s.mu.Unlock()

	s.logger.Info("提供商 %s 发现 %d 个模型", prov.Name(), len(models))
}

// Start 启动服务器
func (s *Server) Start() error {
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
		Addr:         fmt.Sprintf(":%d", s.config.Port),
		Handler:      s.loggingMiddleware(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	s.logger.Info("代理服务器启动于 http://localhost:%d", s.config.Port)
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

	// 解析模型
	modelName := req.Model
	if modelName == "" {
		modelName = s.config.DefaultModel
	}
	provName, modelID := s.resolveModel(modelName)

	provEntry, ok := s.providers[provName]
	if !ok {
		http.Error(w, fmt.Sprintf("提供商 %s 未找到", provName), http.StatusBadRequest)
		return
	}

	prov := provEntry.Provider
	if !prov.IsEnabled() {
		http.Error(w, fmt.Sprintf("提供商 %s 未启用", provName), http.StatusBadRequest)
		return
	}

	req.Model = modelID

	// 应用默认参数
	s.applyDefaults(&req, modelName)

	// 流式处理
	if req.Stream {
		s.handleStream(w, r, prov, &req)
		return
	}

	// 非流式
	resp, err := prov.Chat(r.Context(), &req)
	if err != nil {
		s.logger.Error("聊天请求失败: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
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

	modelName, _ := ollamaReq["model"].(string)
	if modelName == "" {
		modelName = s.config.DefaultModel
	}
	provName, modelID := s.resolveModel(modelName)

	provEntry, ok := s.providers[provName]
	if !ok {
		http.Error(w, fmt.Sprintf("提供商 %s 未找到", provName), http.StatusBadRequest)
		return
	}

	prov := provEntry.Provider
	if !prov.IsEnabled() {
		http.Error(w, fmt.Sprintf("提供商 %s 未启用", provName), http.StatusBadRequest)
		return
	}

	// 转换为 OpenAI 格式
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

	req := &provider.ChatRequest{
		Model:    modelID,
		Messages: messages,
		Stream:   stream,
	}

	// 应用默认参数
	s.applyDefaults(req, modelName)

	if stream {
		s.handleStream(w, r, prov, req)
		return
	}

	resp, err := prov.Chat(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// 转换回 Ollama 格式
	ollamaResp := map[string]any{
		"model":  modelName,
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
}

// handleStream 处理流式响应
// 当前版本暂未实现真实 SSE 转发，统一返回 501。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, prov provider.Provider, req *provider.ChatRequest) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	json.NewEncoder(w).Encode(map[string]any{
		"error": "streaming is not implemented yet",
	})
}

// handleListModels 列出模型
// 汇总所有启用 provider 的模型列表，并以 OpenAI /v1/models 格式返回。
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	models := make([]map[string]any, 0)
	for _, entry := range s.providers {
		if !entry.Provider.IsEnabled() {
			continue
		}
		for _, model := range entry.Models {
			models = append(models, map[string]any{
				"id":       model,
				"object":   "model",
				"owned_by": entry.Provider.Name(),
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   models,
	})
}

// handleOllamaTags Ollama 标签列表
// 汇总所有启用 provider 的模型列表，并以 Ollama /api/tags 格式返回。
func (s *Server) handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	models := make([]map[string]any, 0)
	for _, entry := range s.providers {
		if !entry.Provider.IsEnabled() {
			continue
		}
		for _, model := range entry.Models {
			models = append(models, map[string]any{
				"name":        model,
				"model":       model,
				"modified_at": time.Now().Format(time.RFC3339),
				"size":        0,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"models": models,
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"modelfile": "",
		"parameters": "",
		"template": "",
		"details": map[string]any{
			"parent_model": "",
			"format":       "api",
			"family":       "unknown",
			"families":     []string{"unknown"},
			"parameter_size": "unknown",
			"quantization_level": "none",
		},
	})
}

// resolveModel 解析模型名称，返回 (提供商, 模型ID)
// 支持三种写法：
// 1. provider/model 前缀路由
// 2. model@provider 后缀路由
// 3. 裸 model 名称，遍历已发现模型匹配，失败则回退到默认 provider
func (s *Server) resolveModel(model string) (string, string) {
	// 检查是否有 @provider 后缀
	if idx := strings.LastIndex(model, "@"); idx > 0 {
		provName := model[idx+1:]
		modelID := model[:idx]
		return provName, modelID
	}

	// 检查是否有 provider/model 格式
	if idx := strings.Index(model, "/"); idx > 0 {
		provName := model[:idx]
		modelID := model[idx+1:]
		return provName, modelID
	}

	// 搜索模型
	s.mu.RLock()
	defer s.mu.RUnlock()

	for provName, entry := range s.providers {
		if !entry.Provider.IsEnabled() {
			continue
		}
		for _, m := range entry.Models {
			if m == model {
				return provName, model
			}
		}
	}

	// 默认使用默认模型
	provName := s.config.DefaultModel
	if idx := strings.LastIndex(s.config.DefaultModel, "@"); idx > 0 {
		provName = s.config.DefaultModel[idx+1:]
	}
	return provName, model
}

// applyDefaults 应用默认参数
// 如果请求没有显式设置 temperature / max_tokens，
// 则优先使用 ModelConfig 中的模型级默认值，否则回退到全局默认值。
func (s *Server) applyDefaults(req *provider.ChatRequest, modelName string) {
	// 查找模型配置
	for _, m := range s.config.Models {
		if m.Name == modelName || m.Name == req.Model {
			if req.Temperature == nil && m.Temperature != nil {
				req.Temperature = m.Temperature
			}
			if req.MaxTokens == nil && m.MaxTokens != nil {
				req.MaxTokens = m.MaxTokens
			}
			break
		}
	}

	// 应用全局默认
	if req.Temperature == nil {
		req.Temperature = float64Ptr(0.7)
	}
	if req.MaxTokens == nil {
		req.MaxTokens = intPtr(4096)
	}
}

func float64Ptr(f float64) *float64 {
	return &f
}

func intPtr(i int) *int {
	return &i
}
