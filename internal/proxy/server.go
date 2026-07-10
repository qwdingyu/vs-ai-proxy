package proxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	catalog        *provider.ModelCatalog
	store          *store.Store
	logger         *log.Logger
	server         *http.Server
	proxyKey       string
	reasoningCache *reasoningCache
	mux            *http.ServeMux
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
	s.catalog = provider.NewModelCatalog(s.registry, s.configDir(), 5*time.Minute)

	return s
}

// Reconfigure 热更新代理路由配置。
// 已绑定的监听端口不会在运行中切换；端口变更仍需要进程重启。
func (s *Server) Reconfigure(cfg *config.AppConfig) {
	if cfg == nil {
		return
	}

	registry := s.buildRegistry(cfg)
	catalog := provider.NewModelCatalog(registry, s.configDir(), 5*time.Minute)

	s.mu.Lock()
	s.config = cfg
	s.registry = registry
	s.catalog = catalog
	s.mu.Unlock()

	s.logger.Info("代理配置已热更新: providers=%d models=%d", len(cfg.Providers), len(cfg.Models))
}

func (s *Server) snapshot() (*config.AppConfig, *provider.Registry, *provider.ModelCatalog) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.catalog == nil {
		return s.config, s.registry, provider.NewModelCatalog(s.registry, s.configDir(), 5*time.Minute)
	}
	return s.config, s.registry, s.catalog
}

// SnapshotComponents 返回当前配置、registry 与 catalog 的只读快照，供后台任务使用。
func (s *Server) SnapshotComponents() (*config.AppConfig, *provider.Registry, *provider.ModelCatalog) {
	return s.snapshot()
}

// ProviderHealthSnapshot 返回 provider 运行态健康快照，供管理端只读展示。
func (s *Server) ProviderHealthSnapshot() map[string]provider.ProviderHealth {
	_, registry, _ := s.snapshot()
	if registry == nil {
		return map[string]provider.ProviderHealth{}
	}
	return registry.ProviderHealthSnapshot()
}

func (s *Server) configDir() string {
	if s != nil && s.configMgr != nil {
		if path := strings.TrimSpace(s.configMgr.ConfigPath()); path != "" {
			return filepath.Dir(path)
		}
	}
	return config.DefaultConfigDir()
}

func (s *Server) buildRegistry(cfg *config.AppConfig) *provider.Registry {
	registry := provider.NewRegistry(cfg.DefaultModel, 5*time.Minute)
	for _, p := range cfg.Providers {
		p = config.NormalizeProvider(p)
		prov := s.providerFromConfig(p)
		if prov == nil {
			continue
		}

		registry.Add(&provider.ProviderEntry{
			Provider: prov,
			// Web 配置闭环：
			// config.json 中显式绑定到该 provider 的模型先作为路由种子，
			// 避免上游 /models 发现失败或尚未刷新时，Visual Studio 已保存的模型无法路由。
			Models:   configuredModelsForProvider(cfg, p),
			Priority: p.Priority,
		})

		s.logger.Info("已注册提供商: %s (%s)", config.ProviderKey(p), p.Type)
	}
	return registry
}

func configuredModelsForProvider(cfg *config.AppConfig, p config.ProviderConfig) []string {
	if cfg == nil {
		return nil
	}

	p = config.NormalizeProvider(p)
	providerID := config.ProviderKey(p)
	models := []string{}
	for _, model := range cfg.Models {
		model = config.NormalizeModel(model)
		if !model.Enabled || strings.TrimSpace(model.Name) == "" {
			continue
		}
		modelProvider := strings.TrimSpace(model.ProviderID)
		if modelProvider == "" {
			modelProvider = strings.TrimSpace(model.Provider)
		}
		if modelProvider == "" {
			continue
		}
		if strings.EqualFold(modelProvider, providerID) || strings.EqualFold(modelProvider, strings.TrimSpace(p.Name)) {
			models = append(models, model.Name)
		}
	}
	return models
}

func (s *Server) providerFromConfig(p config.ProviderConfig) provider.Provider {
	p = config.NormalizeProvider(p)
	timeout := 60 * time.Second
	id := config.ProviderKey(p)
	// id 是 provider 实例名，参与日志/路由/model@provider_id；
	// capability 是能力注册表名，决定 OpenAI/Ollama 路径、header 和参数过滤。
	// 例如 useai-paid 的 id 不在能力表中，但 capability 应归一到 useai。
	capability := providerCapabilityNameFromConfig(p)
	switch p.Type {
	case "ollama":
		return provider.NewOllamaProviderWithCapability(id, capability, p.BaseURL, p.Enabled, timeout)
	case "openai", "custom":
		return provider.NewOpenAIProviderWithCapability(id, capability, p.APIKey, p.BaseURL, p.Enabled, timeout)
	default:
		s.logger.Warn("未知提供商类型: %s", p.Type)
		return nil
	}
}

func providerCapabilityNameFromConfig(p config.ProviderConfig) string {
	return provider.InferCapabilityName(config.ProviderKey(p), p.Name, p.BaseURL, p.Type)
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

// Handler 返回代理协议处理器。
//
// 该处理器只承载 Visual Studio / OpenAI / Ollama 兼容端点，供 cmd/server
// 在单端口模式下挂到根路径；Web 管理端必须通过 /admin 独立分流，避免和
// Ollama 的 /api/chat、/api/tags、/api/show 等协议路径冲突。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	return s.loggingMiddleware(s.authMiddleware(mux))
}

// RegisterRoutes 注册代理协议路由。
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// OpenAI-compatible endpoints used by Visual Studio / Copilot clients.
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleListModels)

	// Ollama-compatible endpoints used by BYOM model discovery and chat.
	mux.HandleFunc("/api/chat", s.handleOllamaChat)
	mux.HandleFunc("/api/tags", s.handleOllamaTags)
	mux.HandleFunc("/api/show", s.handleOllamaShow)
	mux.HandleFunc("/api/version", s.handleOllamaVersion)

	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/", s.handleRoot)
}

// Start 启动服务器
func (s *Server) Start() error {
	cfg, _, _ := s.snapshot()
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

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

		// 优先使用 responseWriter 上 handler 直接设置的字段，
		// 兜底从响应头读取（兼容测试代码等不走 handler 直接设置头部的路径）。
		provider := ww.provider
		if provider == "" {
			provider = firstNonEmptyHeader(ww.Header(), "X-Proxy-Provider", "X-Proxy-Primary-Provider")
		}
		model := ww.model
		if model == "" {
			model = firstNonEmptyHeader(ww.Header(), "X-Proxy-Requested-Model", "X-Proxy-Resolved-Model")
		}
		upstream := ww.upstream
		if upstream == "" {
			upstream = firstNonEmptyHeader(ww.Header(), "X-Proxy-Upstream-Model", "X-Proxy-Primary-Upstream")
		}
		errorCode := firstNonEmptyHeader(ww.Header(), "X-Proxy-Error-Code")
		errorMessage := firstNonEmptyHeader(ww.Header(), "X-Proxy-Error-Message")
		errorHint := firstNonEmptyHeader(ww.Header(), "X-Proxy-Error-Hint")
		requestTools := ww.requestTools
		if requestTools == "" {
			requestTools = firstNonEmptyHeader(ww.Header(), "X-Proxy-Request-Tools")
		}
		responseTools := ww.responseTools
		if responseTools == "" {
			responseTools = firstNonEmptyHeader(ww.Header(), "X-Proxy-Response-Tools")
		}
		fallbackMode := ww.fallbackMode
		if fallbackMode == "" {
			fallbackMode = firstNonEmptyHeader(ww.Header(), "X-Proxy-Fallback-Mode")
		}
		normalization := ww.normalization
		if normalization == "" {
			normalization = firstNonEmptyHeader(ww.Header(), "X-Proxy-Tool-Call-Normalization")
		}

		s.store.AddLog(store.RequestLog{
			Method:        r.Method,
			Path:          r.URL.Path,
			Provider:      provider,
			Model:         model,
			Upstream:      upstream,
			StatusCode:    ww.statusCode,
			ElapsedMs:     elapsed,
			IsSuccess:     ww.statusCode < 400,
			ErrorCode:     errorCode,
			ErrorMessage:  errorMessage,
			ErrorHint:     errorHint,
			RequestTools:  requestTools,
			ResponseTools: responseTools,
			FallbackMode:  fallbackMode,
			Normalization: normalization,
		})

		s.logger.Info("%s %s - %d (%.0f ms)", r.Method, r.URL.Path, ww.statusCode, elapsed)
	})
}

func firstNonEmptyHeader(header http.Header, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(header.Get(key)); value != "" {
			return value
		}
	}
	return ""
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

// responseWriter 记录状态码及提供商/模型信息
// 通过包装 http.ResponseWriter，在 WriteHeader 被调用时缓存最终状态码。
// provider/model/upstream 字段由 handler 在请求处理过程中设置，
// 供 loggingMiddleware 在请求完成后读取并写入 Store。
type responseWriter struct {
	http.ResponseWriter
	statusCode    int
	provider      string
	model         string
	upstream      string
	requestTools  string
	responseTools string
	fallbackMode  string
	normalization string
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

// setResponseLogFields 尝试从 http.ResponseWriter 中解出 *responseWriter，
// 并设置 provider/model/upstream 信息供 loggingMiddleware 记录到 Store。
// 当 w 类型不是 *responseWriter 时静默忽略（此时走响应头的兜底逻辑）。
func setResponseLogFields(w http.ResponseWriter, provider, model, upstream string) {
	if rw, ok := w.(*responseWriter); ok {
		if provider != "" {
			rw.provider = provider
		}
		if model != "" {
			rw.model = model
		}
		if upstream != "" {
			rw.upstream = upstream
		}
	}
}

type streamAttemptWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *streamAttemptWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *streamAttemptWriter) Write(data []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(data)
}

func (w *streamAttemptWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok && flusher != nil {
		flusher.Flush()
	}
}

func (w *streamAttemptWriter) HasWritten() bool {
	return w.wrote
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
	setRequestToolDiagnosticHeader(w, &req)

	cfg, registry, catalog := s.snapshot()
	if catalog != nil {
		catalog.Rebuild()
	}

	// 解析模型
	modelName := req.Model
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	baseReq := cloneChatRequest(&req)

	var lastErr error
	attempts := []attemptDiagnostic{}
	candidates := registry.ResolveCandidates(modelName)
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
			if p, ok := catalog.Profile(modelName, prov.Name()); ok {
				profile = p
				hasProfile = true
				s.applyProfileDefaults(req, profile, prov)
			}
		}
		ctx, cancel := requestContextWithTimeout(
			r.Context(),
			modelTimeoutSeconds(cfg, modelName, modelID, prov.Name(), profile, hasProfile),
		)

		if req.Stream {
			streamReq := r.WithContext(ctx)
			streamWriter := &streamAttemptWriter{ResponseWriter: w}
			err := s.handleStream(streamWriter, streamReq, prov, req, provider.ApiFormatOpenAi)
			cancel()
			if err != nil {
				lastErr = err
				attempts = append(attempts, newAttemptDiagnostic(prov.Name(), modelID, err))
				s.logger.Warn("模型 %s 在提供商 %s 流式失败: %v", modelID, prov.Name(), err)
				if isClientGoneError(err) {
					return
				}
				if streamWriter.HasWritten() {
					registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
					return
				}
				registry.RecordCandidateFailure(prov.Name(), err)
				continue
			}
			registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
			return
		}

		if provider.ResolveApiFormat(prov) == provider.ApiFormatOpenAi {
			if rawProvider, ok := prov.(rawOpenAIChatProvider); ok {
				body, err := rawProvider.ChatRaw(ctx, req)
				if err != nil {
					// VS Copilot 真实环境里，UseAI/gpt-5.5 非流式可能直接透出
					// upstream_server_error，但同一 provider 的流式链路可用。
					// 在还没写响应给下游时，尝试用流式聚合成非流式 JSON，避免 VS 端失败。
					if canAttemptAlternateChatMode(ctx, err) {
						if fallbackResp, fallbackErr := collectOpenAIStreamChatResponse(ctx, prov, req); fallbackErr == nil {
							cancel()
							setProxyFallbackMode(w, "nonstream-to-stream")
							normalizeProviderSpecificToolCalls(fallbackResp, allowedToolNames(req))
							setResponseToolDiagnosticHeader(w, fallbackResp)
							s.cacheChatResponse(fallbackResp)
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusOK)
							_ = json.NewEncoder(w).Encode(fallbackResp)
							registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
							return
						}
					}
					cancel()
					lastErr = err
					attempts = append(attempts, newAttemptDiagnostic(prov.Name(), modelID, err))
					s.logger.Warn("模型 %s 在提供商 %s 失败: %v", modelID, prov.Name(), err)
					registry.RecordCandidateFailure(prov.Name(), err)
					continue
				}
				cancel()
				// Visual Studio Copilot 适配：
				// 部分 OpenAI-compatible 上游即使 stream=false 也会返回 SSE data: chunk。
				// 非流式下游期望 JSON，直接透传会导致解析失败，因此先尽力聚合为标准 chat.completion。
				if converted, convErr := openAIStreamBodyToChatResponse(body, req.Model); convErr == nil {
					body = converted
				}
				// Visual Studio Copilot 适配：
				// raw OpenAI 响应直接透传能最大化保留上游扩展字段，但 VS 对
				// finish_reason 比 Web/ curl 更严格，写回前需要做最小兼容归一化。
				body = normalizeOpenAIChatResponseForVisualStudio(body)
				body = normalizeProviderSpecificToolCallsInOpenAIJSON(body, allowedToolNames(req))
				setRawResponseToolDiagnosticHeader(w, body)
				s.cacheRawOpenAIChatResponse(body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(body)
				registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
				return
			}
		}

		resp, err := prov.Chat(ctx, req)
		if err != nil {
			if canAttemptAlternateChatMode(ctx, err) {
				if fallbackResp, fallbackErr := collectOpenAIStreamChatResponse(ctx, prov, req); fallbackErr == nil {
					cancel()
					setProxyFallbackMode(w, "nonstream-to-stream")
					normalizeProviderSpecificToolCalls(fallbackResp, allowedToolNames(req))
					setResponseToolDiagnosticHeader(w, fallbackResp)
					s.cacheChatResponse(fallbackResp)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(fallbackResp)
					registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
					return
				}
			}
			cancel()
			lastErr = err
			attempts = append(attempts, newAttemptDiagnostic(prov.Name(), modelID, err))
			s.logger.Warn("模型 %s 在提供商 %s 失败: %v", modelID, prov.Name(), err)
			registry.RecordCandidateFailure(prov.Name(), err)
			continue
		}
		cancel()
		normalizeProviderSpecificToolCalls(resp, allowedToolNames(req))
		setResponseToolDiagnosticHeader(w, resp)
		s.cacheChatResponse(resp)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
		registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
		return
	}

	if lastErr != nil {
		writeProxyDiagnosticError(w, http.StatusBadGateway, allCandidatesFailedDiagnostic(modelName, resolvedModel, len(candidates), attempts))
	} else {
		writeProxyDiagnosticError(w, http.StatusServiceUnavailable, allCandidatesFailedDiagnostic(modelName, resolvedModel, len(candidates), attempts))
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

	cfg, registry, catalog := s.snapshot()
	if catalog != nil {
		catalog.Rebuild()
	}

	modelName, _ := ollamaReq["model"].(string)
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	candidates := registry.ResolveCandidates(modelName)
	resolvedModel := registry.ResolveModel(modelName)
	if len(candidates) == 0 {
		if registry.HasAmbiguousModelAlias(modelName) {
			writeProxyDiagnosticError(w, http.StatusBadRequest, ambiguousModelAliasDiagnostic(modelName, resolvedModel))
			return
		}
		writeProxyDiagnosticError(w, http.StatusBadRequest, noCandidateDiagnostic(modelName, resolvedModel, len(candidates)))
		return
	}

	messages := make([]provider.Message, 0)
	if msgs, ok := ollamaReq["messages"].([]any); ok {
		for _, m := range msgs {
			if msgMap, ok := m.(map[string]any); ok {
				msg, err := messageFromMap(msgMap)
				if err == nil {
					messages = append(messages, msg)
				}
			}
		}
	}

	stream := false
	if s, ok := ollamaReq["stream"].(bool); ok {
		stream = s
	}

	var lastErr error
	attempts := []attemptDiagnostic{}
	setCandidateDiagnosticHeaders(w, modelName, resolvedModel, candidates)
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

		req := &provider.ChatRequest{
			Model:    modelID,
			Messages: messages,
			Stream:   stream,
		}

		if options, ok := ollamaReq["options"].(map[string]any); ok {
			req.OptionsExtra = rawMessagesFromMap(options, providerOllamaOptionKnownFields())
			if v, ok := options["temperature"].(float64); ok {
				req.Temperature = float64Ptr(v)
			}
			if v, ok := options["top_p"].(float64); ok {
				req.TopP = float64Ptr(v)
			}
			if v, ok := options["top_k"].(float64); ok {
				topK := int(v)
				req.TopK = &topK
			}
			if v, ok := options["max_tokens"].(float64); ok {
				maxTokens := int(v)
				req.MaxTokens = &maxTokens
			}
			if v, ok := options["reasoning_effort"].(string); ok {
				req.ReasoningEffort = v
			}
		}

		if tools, ok := ollamaReq["tools"].([]any); ok {
			req.Tools = buildTools(tools)
		}
		setRequestToolDiagnosticHeader(w, req)

		if stop, ok := ollamaReq["stop"].([]any); ok {
			req.Stop = buildStop(stop)
		}

		s.transformRequest(cfg, req, modelName, prov)

		var profile provider.ModelProfile
		hasProfile := false
		if catalog != nil {
			if p, ok := catalog.Profile(modelName, prov.Name()); ok {
				profile = p
				hasProfile = true
				s.applyProfileDefaults(req, profile, prov)
			}
		}
		ctx, cancel := requestContextWithTimeout(
			r.Context(),
			modelTimeoutSeconds(cfg, modelName, modelID, prov.Name(), profile, hasProfile),
		)

		if stream {
			streamReq := r.WithContext(ctx)
			streamWriter := &streamAttemptWriter{ResponseWriter: w}
			err := s.handleStream(streamWriter, streamReq, prov, req, provider.ApiFormatOllama)
			cancel()
			if err != nil {
				lastErr = err
				attempts = append(attempts, newAttemptDiagnostic(prov.Name(), modelID, err))
				s.logger.Warn("模型 %s 在提供商 %s 流式失败: %v", modelID, prov.Name(), err)
				if streamWriter.HasWritten() {
					registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
					return
				}
				registry.RecordCandidateFailure(prov.Name(), err)
				continue
			}
			registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
			return
		}

		if provider.ResolveApiFormat(prov) == provider.ApiFormatOllama {
			rawProvider, ok := prov.(rawOllamaChatProvider)
			if ok {
				body, err := rawProvider.ChatRaw(ctx, req)
				cancel()
				if err != nil {
					lastErr = err
					attempts = append(attempts, newAttemptDiagnostic(prov.Name(), modelID, err))
					s.logger.Warn("模型 %s 在提供商 %s 失败: %v", modelID, prov.Name(), err)
					registry.RecordCandidateFailure(prov.Name(), err)
					continue
				}
				if converted, convErr := converter.OllamaChatResponse2OpenAI(body, req.Model); convErr == nil {
					var typed provider.ChatResponse
					if json.Unmarshal(converted, &typed) == nil {
						normalizeProviderSpecificToolCalls(&typed, allowedToolNames(req))
						setResponseToolDiagnosticHeader(w, &typed)
						s.cacheChatResponse(&typed)
					}
				}

				body = normalizeDSMLToolCallsInOllamaJSON(body, allowedToolNames(req))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write(ensureOllamaContentFromThinking(body))
				registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
				return
			}
		}

		resp, err := prov.Chat(ctx, req)
		cancel()
		if err != nil {
			lastErr = err
			attempts = append(attempts, newAttemptDiagnostic(prov.Name(), modelID, err))
			s.logger.Warn("模型 %s 在提供商 %s 失败: %v", modelID, prov.Name(), err)
			registry.RecordCandidateFailure(prov.Name(), err)
			continue
		}
		normalizeProviderSpecificToolCalls(resp, allowedToolNames(req))
		setResponseToolDiagnosticHeader(w, resp)
		s.cacheChatResponse(resp)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(buildOllamaChatResponse(modelName, resp))
		registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
		return
	}

	if lastErr != nil {
		writeProxyDiagnosticError(w, http.StatusBadGateway, allCandidatesFailedDiagnostic(modelName, resolvedModel, len(candidates), attempts))
	} else {
		writeProxyDiagnosticError(w, http.StatusServiceUnavailable, allCandidatesFailedDiagnostic(modelName, resolvedModel, len(candidates), attempts))
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
		// VS Copilot 的 /v1/chat/completions stream=true 真实路径中，UseAI/New API
		// 可能流式 503，但非流式同模型可用。尚未向下游写出 SSE 时，
		// 尝试反向兜底：非流式拿到结果后合成为 OpenAI SSE，避免直接 502。
		if canAttemptAlternateChatMode(r.Context(), err) {
			fallbackReq := cloneChatRequest(req)
			fallbackReq.Stream = false
			if resp, fallbackErr := prov.Chat(r.Context(), fallbackReq); fallbackErr == nil {
				setProxyFallbackMode(w, "stream-to-nonstream")
				normalizeProviderSpecificToolCalls(resp, allowedToolNames(req))
				setResponseToolDiagnosticHeader(w, resp)
				s.cacheChatResponse(resp)
				if writeErr := writeOpenAIChatResponseAsSSE(w, flusher, resp); writeErr == nil {
					return nil
				}
			}
		}
		return fmt.Errorf("openai stream error: %w", err)
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if r.Context().Err() != nil {
		return r.Context().Err()
	}

	scanner := newStreamScanner(stream)
	acc := newStreamReasoningAccumulator()
	buffered, dsmlResp, detectedDSML, probeErr := probeOpenAIStreamForDSML(scanner, allowedToolNames(req))
	if probeErr != nil {
		return probeErr
	}
	if detectedDSML {
		setProxyToolNormalization(w, "dsml")
		fillMissingStreamResponseModel(dsmlResp, req.Model)
		normalizeProviderSpecificToolCalls(dsmlResp, allowedToolNames(req))
		setResponseToolDiagnosticHeader(w, dsmlResp)
		s.cacheChatResponse(dsmlResp)
		return writeOpenAIChatResponseAsSSE(w, flusher, dsmlResp)
	}
	if err := writeBufferedOpenAIStreamLinesWithTools(w, flusher, buffered, acc, allowedToolNames(req)); err != nil {
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		// Visual Studio Copilot 适配：
		// VS 的 OpenAI .NET SDK 在流式模式下会逐个解析 SSE chunk；
		// 如果上游在任意 chunk 中返回 finish_reason:""，VS 会在客户端直接抛
		// Unknown ChatFinishReason value。非流式响应归一化不能覆盖这里。
		line = normalizeOpenAIStreamLineForVisualStudioWithTools(line, allowedToolNames(req))
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
	setStreamToolDiagnosticHeader(w, acc)
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

	scanner := newStreamScanner(stream)
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
	setStreamToolDiagnosticHeader(w, acc)
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

	scanner := newStreamScanner(stream)
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
	setStreamToolDiagnosticHeader(w, acc)
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
	scanner := newStreamScanner(stream)
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
	setStreamToolDiagnosticHeader(w, acc)

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

func newStreamScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return scanner
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
	if len(out.ToolCalls) == 0 {
		if functionCall, ok := delta["function_call"].(map[string]any); ok && functionCall != nil {
			out.ToolCalls = []any{map[string]any{
				"id":       "function_call",
				"type":     "function",
				"function": functionCall,
			}}
		}
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

func modelTimeoutSeconds(
	cfg *config.AppConfig,
	requestedModel string,
	upstreamModel string,
	providerName string,
	profile provider.ModelProfile,
	hasProfile bool,
) int {
	if hasProfile && profile.TimeoutSeconds != nil && *profile.TimeoutSeconds > 0 {
		return *profile.TimeoutSeconds
	}

	modelCfg, ok := findModelConfig(cfg, requestedModel, upstreamModel, providerName)
	if !ok || modelCfg.TimeoutSeconds == nil || *modelCfg.TimeoutSeconds <= 0 {
		return 60
	}
	return *modelCfg.TimeoutSeconds
}

func requestContextWithTimeout(parent context.Context, timeoutSeconds int) (context.Context, context.CancelFunc) {
	if timeoutSeconds <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, time.Duration(timeoutSeconds)*time.Second)
}

func canAttemptAlternateChatMode(ctx context.Context, err error) bool {
	return ctx != nil && ctx.Err() == nil && provider.ShouldAttemptAlternateChatMode(err)
}

func isClientGoneError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || strings.Contains(strings.ToLower(err.Error()), "client_gone")
}

func setCandidateDiagnosticHeaders(
	w http.ResponseWriter,
	requestedModel string,
	resolvedModel string,
	candidates []provider.Candidate,
) {
	header := w.Header()
	// Visual Studio Copilot 排障适配：
	// VS 客户端错误日志通常不会打印代理内部路由细节；这些响应头让 Web 日志、
	// curl 和抓包都能看到 requested/resolved/upstream 三段模型名，快速区分
	// 客户端展示名问题、模型拼写问题、provider 路由问题和上游问题。
	header.Set("X-Proxy-Requested-Model", requestedModel)
	header.Set("X-Proxy-Resolved-Model", resolvedModel)
	header.Set("X-Proxy-Candidate-Count", fmt.Sprintf("%d", len(candidates)))
	if len(candidates) == 0 || candidates[0].Provider == nil || candidates[0].Provider.Provider == nil {
		return
	}
	header.Set("X-Proxy-Primary-Provider", candidates[0].Provider.Provider.Name())
	header.Set("X-Proxy-Primary-Upstream", coalesceString(candidates[0].UpstreamID, candidates[0].ModelID))
}

func setAttemptDiagnosticHeaders(w http.ResponseWriter, providerName string, upstreamModel string) {
	header := w.Header()
	header.Set("X-Proxy-Provider", providerName)
	header.Set("X-Proxy-Upstream-Model", upstreamModel)
}

// handleOllamaShow Ollama 模型详情。
// 响应来自本地 catalog/model 配置，而不是向上游 provider 透传查询。
func (s *Server) handleOllamaShow(w http.ResponseWriter, r *http.Request) {
	model, err := modelFromOllamaShowRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if model == "" {
		http.Error(w, "缺少 model 参数", http.StatusBadRequest)
		return
	}

	cfg, registry, catalog := s.snapshot()
	body, err := s.buildOllamaShowBody(cfg, registry, catalog, model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func modelFromOllamaShowRequest(r *http.Request) (string, error) {
	if r.Method == http.MethodGet {
		return strings.TrimSpace(r.URL.Query().Get("model")), nil
	}
	if r.Method != http.MethodPost {
		return "", fmt.Errorf("method not allowed")
	}

	defer r.Body.Close()
	var body struct {
		Model string `json:"model"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("解析请求失败")
	}
	return coalesceString(body.Model, body.Name), nil
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

func buildTools(raw []any) []provider.Tool {
	out := make([]provider.Tool, 0, len(raw))
	for _, item := range raw {
		data, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var tool provider.Tool
		if err := json.Unmarshal(data, &tool); err != nil {
			continue
		}
		if strings.TrimSpace(tool.Type) == "" {
			tool.Type = "function"
		}
		out = append(out, tool)
	}
	return out
}

func buildStop(raw []any) []string {
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		v, ok := item.(string)
		if !ok {
			continue
		}
		out = append(out, v)
	}
	return out
}

func stringValue(m map[string]any, key, fallback string) string {
	v, ok := m[key].(string)
	if !ok {
		return fallback
	}
	return v
}

func messageFromMap(m map[string]any) (provider.Message, error) {
	if hasOllamaImages(m) {
		m = cloneAnyMap(m)
		m["content"] = openAIContentFromOllamaImages(m)
		delete(m, "images")
	}

	data, err := json.Marshal(m)
	if err != nil {
		return provider.Message{}, err
	}
	var msg provider.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return provider.Message{}, err
	}
	return msg, nil
}

func hasOllamaImages(m map[string]any) bool {
	images, ok := m["images"].([]any)
	return ok && len(images) > 0
}

func openAIContentFromOllamaImages(m map[string]any) []map[string]any {
	content := []map[string]any{{
		"type": "text",
		"text": stringValue(m, "content", ""),
	}}

	images, _ := m["images"].([]any)
	for _, raw := range images {
		url, ok := raw.(string)
		if !ok || strings.TrimSpace(url) == "" {
			continue
		}
		if !strings.HasPrefix(url, "data:") && !strings.HasPrefix(url, "http") {
			url = "data:image/png;base64," + url
		}
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": url,
			},
		})
	}
	return content
}

func cloneAnyMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func rawMessagesFromMap(src map[string]any, known map[string]struct{}) map[string]json.RawMessage {
	if len(src) == 0 {
		return map[string]json.RawMessage{}
	}

	out := map[string]json.RawMessage{}
	for key, value := range src {
		if _, exists := known[key]; exists {
			continue
		}
		data, err := json.Marshal(value)
		if err != nil {
			continue
		}
		out[key] = data
	}
	return out
}

func providerOllamaOptionKnownFields() map[string]struct{} {
	return map[string]struct{}{
		"temperature":      {},
		"top_p":            {},
		"top_k":            {},
		"max_tokens":       {},
		"reasoning_effort": {},
	}
}
