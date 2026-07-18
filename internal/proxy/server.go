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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/converter"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
	"github.com/dingyuwang/vs-ai-proxy/internal/requestmeta"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

const (
	defaultModelTimeoutSeconds        = 180
	clientDeadlineDiagnosticThreshold = 90_000
	maxChatRequestBodyBytes           = 32 << 20
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
		prov := s.providerFromConfig(cfg, p)
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
			Aliases:  []string{config.ProviderKey(p), p.Name, p.DisplayName},
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

func (s *Server) providerFromConfig(cfg *config.AppConfig, p config.ProviderConfig) provider.Provider {
	p = config.NormalizeProvider(p)
	timeout := 60 * time.Second
	defenseEnabled := proxyDefenseEnabled(cfg)
	id := config.ProviderKey(p)
	// id 是 provider 实例名，参与日志/路由/model@provider_id；
	// capability 是能力注册表名，决定 OpenAI/Ollama 路径、header 和参数过滤。
	// 例如 useai-paid 的 id 不在能力表中，但 capability 应归一到 useai。
	capability := providerCapabilityNameFromConfig(p)
	switch p.Type {
	case "ollama":
		return provider.NewOllamaProviderWithCapability(id, capability, p.BaseURL, p.Enabled, timeout)
	case "openai", "custom":
		prov := provider.NewOpenAIProviderWithCapability(id, capability, p.APIKey, p.BaseURL, p.Enabled, timeout)
		prov.SetDefenseEnabled(defenseEnabled)
		return prov
	default:
		s.logger.Warn("未知提供商类型: %s", p.Type)
		return nil
	}
}

func proxyDefenseEnabled(cfg *config.AppConfig) bool {
	if cfg == nil || cfg.Defense.Enabled == nil {
		return true
	}
	return *cfg.Defense.Enabled
}

func applyDefenseCandidatePolicy(cfg *config.AppConfig, candidates []provider.Candidate) []provider.Candidate {
	// VS Stable 默认只执行首选候选：new-api/sub2api 这类上游网关内部本身负责渠道轮换。
	// 代理层跨 provider 自动兜底会掩盖真实 provider 错误、放大请求与计费，
	// 还可能把“绑定 provider 的模型”悄悄路由到另一个 provider。
	// 防御开关仍控制 provider 内短重试、稳定 UA、协议兜底与冷却；不再代表跨 provider fallback。
	if len(candidates) <= 1 {
		return candidates
	}
	return candidates[:1]
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

	// /models 是运行态发现结果，不应覆盖用户在 config.json / Web 导入中明确绑定的模型。
	// 一些上游网关会按 key/group/User-Agent 返回不同模型列表；若直接覆盖，VS 已选择的
	// “Provider - model” 会在启动后突然无法路由，导致大请求还没到上游就失败。
	models = mergeProviderModels(s.configuredModelsForProviderName(prov.Name()), models)
	s.registry.MergeModels(prov.Name(), models)

	s.logger.Info("提供商 %s 发现 %d 个模型", prov.Name(), len(models))
}

func (s *Server) configuredModelsForProviderName(providerName string) []string {
	cfg, _, _ := s.snapshot()
	if cfg == nil {
		return nil
	}
	for _, p := range cfg.Providers {
		p = config.NormalizeProvider(p)
		if strings.EqualFold(config.ProviderKey(p), providerName) || strings.EqualFold(strings.TrimSpace(p.Name), providerName) {
			return configuredModelsForProvider(cfg, p)
		}
	}
	return nil
}

func mergeProviderModels(configured []string, discovered []string) []string {
	out := make([]string, 0, len(configured)+len(discovered))
	seen := map[string]struct{}{}
	appendModel := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	for _, model := range configured {
		appendModel(model)
	}
	for _, model := range discovered {
		appendModel(model)
	}
	return out
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
		WriteTimeout: time.Duration(defaultModelTimeoutSeconds+30) * time.Second,
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
		requestID := requestIDFromRequest(r, start)
		w.Header().Set("X-Proxy-Request-ID", requestID)
		ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		reqWithID := r.WithContext(requestmeta.ContextWithRequestID(r.Context(), requestID))
		next.ServeHTTP(ww, reqWithID)
		ww.finalizeRequestStatus(reqWithID)

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
		attemptsSummary := firstNonEmptyHeader(ww.Header(), "X-Proxy-Attempts-Summary")
		errorCode, errorMessage, errorHint = enrichClientGoneDiagnostics(ww.statusCode, elapsed, r.ContentLength, errorCode, errorMessage, errorHint)
		cancelReason := requestCancelReason(ww.statusCode, elapsed, errorCode, r.Context().Err())
		networkPeer := firstNonEmptyHeader(ww.Header(), "X-Proxy-Network-Peer")
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
		streamState := ww.streamState
		if streamState == "" {
			streamState = firstNonEmptyHeader(ww.Header(), "X-Proxy-Stream-State")
		}
		configuredTimeout := ww.configuredTimeoutSeconds
		if configuredTimeout == 0 {
			configuredTimeout = firstPositiveHeaderInt(ww.Header(), "X-Proxy-Configured-Timeout-Seconds")
		}
		effectiveTimeout := ww.effectiveTimeoutSeconds
		if effectiveTimeout == 0 {
			effectiveTimeout = firstPositiveHeaderInt(ww.Header(), "X-Proxy-Effective-Timeout-Seconds")
		}
		logErrorCode := errorCode
		if logErrorCode == "" && ww.statusCode >= http.StatusBadRequest {
			logErrorCode = fallbackLogErrorCode(ww.statusCode)
		}
		diagSummary := summarizeLogDiagnostic(logErrorCode, ww.statusCode, elapsed, r.ContentLength, ww.upstreamBytes, networkPeer, streamState, requestTools, responseTools)

		s.store.AddLog(store.RequestLog{
			RequestID:                requestID,
			Method:                   r.Method,
			Path:                     r.URL.Path,
			Provider:                 provider,
			Model:                    model,
			Upstream:                 upstream,
			RequestBytes:             r.ContentLength,
			UpstreamBytes:            ww.upstreamBytes,
			ConfiguredTimeoutSeconds: configuredTimeout,
			EffectiveTimeoutSeconds:  effectiveTimeout,
			StatusCode:               ww.statusCode,
			ElapsedMs:                elapsed,
			IsSuccess:                ww.statusCode < 400,
			ErrorCode:                logErrorCode,
			ErrorMessage:             errorMessage,
			ErrorHint:                errorHint,
			ErrorReason:              diagSummary.Reason,
			ErrorAction:              diagSummary.Action,
			DiagnosticSummary:        diagSummary.Summary,
			AttemptsSummary:          attemptsSummary,
			CancelReason:             cancelReason,
			NetworkPeer:              networkPeer,
			RequestTools:             requestTools,
			ResponseTools:            responseTools,
			FallbackMode:             fallbackMode,
			Normalization:            normalization,
			StreamState:              streamState,
			Usage:                    ww.usage,
		})

		// 成功日志也必须带 provider/requested_model/upstream。排查并发请求或候选 fallback 时，
		// 只看相邻时间戳会把不同请求串在一起，request_id + 路由字段才是可信关联键。
		contextSuffix := consoleRouteSuffix(provider, model, upstream)
		if logErrorCode != "" {
			s.logger.Info("%s %s - %d (%.0f ms) request_id=%s%s error_code=%s%s", r.Method, r.URL.Path, ww.statusCode, elapsed, requestID, contextSuffix, logErrorCode, consoleDiagnosticSuffix(diagSummary, attemptsSummary, r.ContentLength, ww.upstreamBytes))
		} else {
			s.logger.Info("%s %s - %d (%.0f ms) request_id=%s%s", r.Method, r.URL.Path, ww.statusCode, elapsed, requestID, contextSuffix)
		}
	})
}

func fallbackLogErrorCode(statusCode int) string {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "upstream_auth_error"
	case http.StatusTooManyRequests:
		return "upstream_rate_limit"
	case http.StatusRequestEntityTooLarge:
		return "upstream_payload_too_large"
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed:
		return "upstream_request_error"
	}
	if statusCode >= http.StatusInternalServerError {
		return "provider_error"
	}
	if statusCode >= http.StatusBadRequest {
		return "provider_error"
	}
	return ""
}

func consoleDiagnosticSuffix(diag logDiagnosticSummary, attemptsSummary string, requestBytes int64, upstreamBytes int64) string {
	parts := []string{}
	if strings.TrimSpace(diag.Reason) != "" {
		parts = append(parts, "reason="+quoteLogValue(diag.Reason))
	}
	if strings.TrimSpace(diag.Action) != "" {
		parts = append(parts, "action="+quoteLogValue(diag.Action))
	}
	if strings.TrimSpace(attemptsSummary) != "" {
		parts = append(parts, "attempts="+quoteLogValue(attemptsSummary))
	}
	if requestSize := humanBytes(requestBytes); requestSize != "" {
		parts = append(parts, "request_bytes="+quoteLogValue(requestSize))
	}
	if upstreamSize := humanBytes(upstreamBytes); upstreamSize != "" {
		parts = append(parts, "upstream_bytes="+quoteLogValue(upstreamSize))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

func consoleRouteSuffix(providerName, requestedModel, upstreamModel string) string {
	parts := []string{}
	if strings.TrimSpace(providerName) != "" {
		parts = append(parts, compactLogField("provider", providerName))
	}
	if strings.TrimSpace(requestedModel) != "" {
		parts = append(parts, compactLogField("requested_model", requestedModel))
	}
	if strings.TrimSpace(upstreamModel) != "" {
		parts = append(parts, compactLogField("upstream", upstreamModel))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

// compactLogField 让常见 ASCII 模型名保持 provider=xxx 的紧凑形态，
// 含空格、中文或其它特殊字符时转为带引号字段，避免控制台日志被拆列或跨行。
// 这里传入的只有 provider / requested model / upstream model，不能用于 API key 等敏感值。
func compactLogField(key, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.IndexFunc(value, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= 'A' && r <= 'Z':
			return false
		case r >= '0' && r <= '9':
			return false
		case r == '-' || r == '_' || r == '.' || r == ':' || r == '/' || r == '@':
			return false
		default:
			return true
		}
	}) < 0 {
		return key + "=" + value
	}
	return key + "=" + quoteLogValue(value)
}

func quoteLogValue(value string) string {
	value = sanitizeDiagnosticMessage(value)
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strconv.Quote(strings.Join(strings.Fields(value), " "))
}

func enrichClientGoneDiagnostics(statusCode int, elapsedMs float64, requestBytes int64, code, message, hint string) (string, string, string) {
	if statusCode != 499 || code != "client_gone" || elapsedMs < clientDeadlineDiagnosticThreshold {
		return code, message, hint
	}
	message = "客户端等待超时。"
	hint = "减少会话内容，或切换响应更快的模型。"
	return "client_deadline_reached", message, hint
}

func requestCancelReason(statusCode int, elapsedMs float64, code string, err error) string {
	if statusCode != 499 && code != "client_gone" && code != "client_deadline_reached" {
		return ""
	}
	if code == "client_deadline_reached" || elapsedMs >= clientDeadlineDiagnosticThreshold {
		return "client_deadline_reached"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "server_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "client_canceled"
	}
	if err != nil {
		return "context_closed"
	}
	return "client_gone"
}

func firstNonEmptyHeader(header http.Header, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(header.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func firstPositiveHeaderInt(header http.Header, keys ...string) int {
	for _, key := range keys {
		value := strings.TrimSpace(header.Get(key))
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
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
	statusCode               int
	provider                 string
	model                    string
	upstream                 string
	upstreamBytes            int64
	configuredTimeoutSeconds int
	effectiveTimeoutSeconds  int
	requestTools             string
	responseTools            string
	fallbackMode             string
	normalization            string
	streamState              string
	usage                    *store.TokenUsage
}

func requestIDFromRequest(r *http.Request, start time.Time) string {
	if r != nil {
		for _, header := range []string{"X-Request-ID", "X-Correlation-ID", "X-Proxy-Request-ID"} {
			if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
				if sanitized := sanitizeRequestID(value); sanitized != "" {
					return sanitized
				}
			}
		}
	}
	return fmt.Sprintf("%d", start.UnixNano())
}

func requestIDFromContext(ctx context.Context) string {
	return requestmeta.RequestIDFromContext(ctx)
}

func sanitizeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 96 {
		value = value[:96]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.' || r == ':':
			return r
		default:
			return -1
		}
	}, value)
}

func (w *responseWriter) finalizeRequestStatus(r *http.Request) {
	if w == nil || r == nil || r.Context().Err() == nil {
		return
	}
	if w.statusCode < http.StatusBadRequest {
		w.statusCode = 499
	}
	w.Header().Set("X-Proxy-Error-Code", "client_gone")
	w.Header().Set("X-Proxy-Error-Message", "客户端已取消请求。")
	w.Header().Set("X-Proxy-Error-Hint", "重新发送；若反复出现，请新建会话。")
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

func setUpstreamRequestBytes(w http.ResponseWriter, req *provider.ChatRequest) {
	if rw, ok := w.(*responseWriter); ok && req != nil {
		if n, err := provider.OpenAIChatCompletionsRequestBytes(req); err == nil {
			rw.upstreamBytes = int64(n)
		}
	}
}

type streamAttemptWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *streamAttemptWriter) WriteHeader(code int) {
	w.wrote = true
	setProxyStreamState(w.ResponseWriter, "downstream_started")
	w.ResponseWriter.WriteHeader(code)
}

func (w *streamAttemptWriter) Write(data []byte) (int, error) {
	w.wrote = true
	setProxyStreamState(w.ResponseWriter, "downstream_started")
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

	defer r.Body.Close()
	body, ok := readChatRequestBody(w, r)
	if !ok {
		return
	}

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
		ctx = requestmeta.ContextWithRequestID(ctx, requestIDFromContext(r.Context()))

		if req.Stream {
			streamReq := r.WithContext(ctx)
			streamWriter := &streamAttemptWriter{ResponseWriter: w}
			err := s.handleStream(streamWriter, streamReq, prov, req, provider.ApiFormatOpenAi)
			cancel()
			if err != nil {
				lastErr = err
				attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, err)
				attempts = append(attempts, attempt)
				s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
				if isClientGoneError(err) {
					return
				}
				if streamWriter.HasWritten() {
					// 已经写出 SSE 后不能切换候选，但协议截断/错误帧仍然是 provider 失败，
					// 不能记成成功污染健康排序和后续诊断。
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

		if provider.ResolveApiFormat(prov) == provider.ApiFormatOpenAi {
			if rawProvider, ok := prov.(rawOpenAIChatProvider); ok {
				body, err := rawProvider.ChatRaw(ctx, req)
				if err != nil {
					// VS Copilot 真实环境里，UseAI/gpt-5.5 非流式可能直接透出
					// upstream_server_error，但同一 provider 的流式链路可用。
					// 在还没写响应给下游时，尝试用流式聚合成非流式 JSON，避免 VS 端失败。
					if canAttemptAlternateChatMode(cfg, ctx, err) {
						fallbackResp, fallbackErr := collectOpenAIStreamChatResponse(ctx, prov, req)
						if fallbackErr == nil {
							normalizeProviderSpecificToolCalls(fallbackResp, allowedToolNames(req))
							fallbackBody, marshalErr := json.Marshal(fallbackResp)
							if marshalErr != nil {
								fallbackErr = fmt.Errorf("编码备用非流式响应失败: %w", marshalErr)
							} else {
								cancel()
								setProxyFallbackMode(w, "nonstream-to-stream")
								setResponseToolDiagnosticHeader(w, fallbackResp)
								setResponseUsage(w, fallbackResp.Usage)
								w.Header().Set("Content-Type", "application/json")
								w.WriteHeader(http.StatusOK)
								if _, writeErr := w.Write(append(fallbackBody, '\n')); writeErr != nil {
									// 已经提交 HTTP 头后无法切换候选；记录失败但绝不把
									// 未送达的响应写入 reasoning cache。
									registry.RecordCandidateFailure(prov.Name(), writeErr)
									return
								}
								s.cacheChatResponse(fallbackResp)
								registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
								return
							}
						}
						err = alternateChatModeFailure(err, fallbackErr)
					}
					cancel()
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
				// Visual Studio Copilot 适配：
				// 部分 OpenAI-compatible 上游即使 stream=false 也会返回 SSE data: chunk。
				// 非流式下游期望 JSON；一旦确认是 SSE，聚合失败必须作为候选失败处理，
				// 不能再把 SSE 正文伪装成 application/json 200 返回。
				if looksLikeSSEBody(body) {
					converted, convErr := openAIStreamBodyToChatResponse(body, req.Model, allowedToolNames(req))
					if convErr != nil {
						lastErr = fmt.Errorf("解析响应失败: 非流式 SSE 聚合失败: %w", convErr)
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
				// Visual Studio Copilot 适配：
				// raw OpenAI 响应直接透传能最大化保留上游扩展字段，但 VS 对
				// finish_reason 比 Web/ curl 更严格，写回前需要做最小兼容归一化。
				body = normalizeOpenAIChatResponseForVisualStudio(body)
				body = normalizeProviderSpecificToolCallsInOpenAIJSON(body, allowedToolNames(req))
				if validationErr := validateOpenAIChatResponseBody(body); validationErr != nil {
					lastErr = validationErr
					attempt := newAttemptDiagnostic(
						prov.Name(),
						modelID,
						time.Since(attemptStart).Seconds()*1000,
						validationErr,
					)
					attempts = append(attempts, attempt)
					s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
					registry.RecordCandidateFailure(prov.Name(), validationErr)
					if shouldStopCandidateFallback(attempt.Category) {
						break
					}
					continue
				}
				setRawResponseToolDiagnosticHeader(w, body)
				setRawOpenAIResponseUsage(w, body)
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
			if canAttemptAlternateChatMode(cfg, ctx, err) {
				fallbackResp, fallbackErr := collectOpenAIStreamChatResponse(ctx, prov, req)
				if fallbackErr == nil {
					normalizeProviderSpecificToolCalls(fallbackResp, allowedToolNames(req))
					fallbackBody, marshalErr := json.Marshal(fallbackResp)
					if marshalErr != nil {
						fallbackErr = fmt.Errorf("编码备用非流式响应失败: %w", marshalErr)
					} else {
						cancel()
						setProxyFallbackMode(w, "nonstream-to-stream")
						setResponseToolDiagnosticHeader(w, fallbackResp)
						setResponseUsage(w, fallbackResp.Usage)
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						if _, writeErr := w.Write(append(fallbackBody, '\n')); writeErr != nil {
							// 已经提交 HTTP 头后无法切换候选；记录失败但绝不把
							// 未送达的响应写入 reasoning cache。
							registry.RecordCandidateFailure(prov.Name(), writeErr)
							return
						}
						s.cacheChatResponse(fallbackResp)
						registry.RecordCandidateSuccess(prov.Name(), time.Since(attemptStart))
						return
					}
				}
				err = alternateChatModeFailure(err, fallbackErr)
			}
			cancel()
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
		setResponseUsage(w, resp.Usage)
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

	defer r.Body.Close()
	body, ok := readChatRequestBody(w, r)
	if !ok {
		return
	}

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
	candidates := applyDefenseCandidatePolicy(cfg, registry.ResolveCandidates(modelName))
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
			if v, ok := firstIntOption(options, "num_ctx"); ok {
				contextLength := v
				req.ContextLength = &contextLength
			}
			if v, ok := firstIntOption(options, "num_predict", "max_tokens", "max_completion_tokens", "max_output_tokens"); ok {
				maxTokens := v
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

		if stream {
			streamReq := r.WithContext(ctx)
			streamWriter := &streamAttemptWriter{ResponseWriter: w}
			err := s.handleStream(streamWriter, streamReq, prov, req, provider.ApiFormatOllama)
			cancel()
			if err != nil {
				lastErr = err
				attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, err)
				attempts = append(attempts, attempt)
				s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
				if streamWriter.HasWritten() {
					// 已经写出部分 Ollama 响应后发生协议/网络错误，不能切换候选，
					// 但仍必须记录 provider 失败，避免健康排序把半截响应当成功。
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

		if provider.ResolveApiFormat(prov) == provider.ApiFormatOllama {
			rawProvider, ok := prov.(rawOllamaChatProvider)
			if ok {
				body, err := rawProvider.ChatRaw(ctx, req)
				cancel()
				if err != nil {
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
				body = normalizeDSMLToolCallsInOllamaJSON(body, allowedToolNames(req))
				body, protocolErr := normalizeOllamaNativeChatResponse(body)
				if protocolErr != nil {
					protocolErr = fmt.Errorf("解析 Ollama 响应失败: %w", protocolErr)
				}
				// typed 结构只用于验证 OpenAI 语义是否完整；成功后仍返回原生
				// Ollama body，保留 thinking、耗时字段和 object arguments 等扩展。
				if protocolErr == nil {
					converted, convErr := converter.OllamaChatResponse2OpenAI(body, req.Model)
					if convErr != nil {
						protocolErr = fmt.Errorf("解析 Ollama 响应失败: %w", convErr)
					} else {
						var typed provider.ChatResponse
						if unmarshalErr := json.Unmarshal(converted, &typed); unmarshalErr != nil {
							protocolErr = fmt.Errorf("解析 Ollama 响应失败: %w", unmarshalErr)
						} else {
							normalizeProviderSpecificToolCalls(&typed, allowedToolNames(req))
							if validationErr := validateProviderResponseToolContract(&typed); validationErr != nil {
								// 这是代理对上游 Ollama 正文的契约解析失败，不是
								// provider 网络/HTTP 失败；保持诊断分类可操作。
								protocolErr = fmt.Errorf("解析 Ollama 响应失败: %w", validationErr)
							}
							if protocolErr == nil {
								setResponseToolDiagnosticHeader(w, &typed)
								s.cacheChatResponse(&typed)
							}
						}
					}
				}
				if protocolErr != nil {
					lastErr = protocolErr
					attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, protocolErr)
					attempts = append(attempts, attempt)
					s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
					registry.RecordCandidateFailure(prov.Name(), protocolErr)
					if shouldStopCandidateFallback(attempt.Category) {
						break
					}
					continue
				}
				setRawOllamaResponseUsage(w, body)
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
			attempt := newAttemptDiagnostic(prov.Name(), modelID, time.Since(attemptStart).Seconds()*1000, err)
			attempts = append(attempts, attempt)
			s.logProviderAttemptFailureForRequest(r.Context(), modelName, modelID, prov.Name(), attempt)
			registry.RecordCandidateFailure(prov.Name(), err)
			if shouldStopCandidateFallback(attempt.Category) {
				break
			}
			continue
		}
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
		setResponseUsage(w, resp.Usage)
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

func readChatRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	return readRequestBody(w, r, maxChatRequestBodyBytes)
}

func readRequestBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, bool) {
	limitText := fmt.Sprintf("%d 字节", maxBytes)
	if maxBytes > 0 && maxBytes%(1<<20) == 0 {
		limitText = fmt.Sprintf("%d MiB", maxBytes>>20)
	}
	if r.ContentLength > maxBytes {
		http.Error(w, "请求体超过 "+limitText+" 限制", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err == nil {
		return body, true
	}
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		http.Error(w, "请求体超过 "+limitText+" 限制", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	http.Error(w, "读取请求体失败", http.StatusBadRequest)
	return nil, false
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
	setProxyStreamState(w, "upstream_connecting")
	stream, err := prov.ChatStream(r.Context(), req)
	if err != nil {
		// VS Copilot 的 /v1/chat/completions stream=true 真实路径中，UseAI/New API
		// 可能流式 503，但非流式同模型可用。尚未向下游写出 SSE 时，
		// 尝试反向兜底：非流式拿到结果后合成为 OpenAI SSE，避免直接 502。
		cfg, _, _ := s.snapshot()
		if canAttemptAlternateChatMode(cfg, r.Context(), err) {
			fallbackReq := cloneChatRequest(req)
			fallbackReq.Stream = false
			resp, fallbackErr := prov.Chat(r.Context(), fallbackReq)
			if fallbackErr == nil {
				normalizeProviderSpecificToolCalls(resp, allowedToolNames(req))
				// fallback provider 可能直接返回 typed 响应；先完成和主路径
				// 相同的工具契约校验，避免无效调用污染缓存或成功诊断头。
				if validationErr := validateProviderResponseToolContract(resp); validationErr == nil {
					setProxyFallbackMode(w, "stream-to-nonstream")
					setResponseToolDiagnosticHeader(w, resp)
					setResponseUsage(w, resp.Usage)
					if writeErr := writeOpenAIChatResponseAsSSE(w, flusher, resp); writeErr == nil {
						s.cacheChatResponse(resp)
						return nil
					} else {
						fallbackErr = fmt.Errorf("写入备用非流式响应失败: %w", writeErr)
					}
				} else {
					fallbackErr = fmt.Errorf("备用非流式响应契约无效: %w", validationErr)
				}
			}
			if fallbackErr != nil {
				return alternateChatModeFailure(err, fallbackErr)
			}
		}
		return fmt.Errorf("openai stream error: %w", err)
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
	buffered, dsmlResp, detectedDSML, probeErr := probeOpenAIStreamForDSML(scanner, allowedToolNames(req))
	if probeErr != nil {
		return fmt.Errorf("解析响应失败: OpenAI SSE: %w", probeErr)
	}
	if detectedDSML {
		setProxyToolNormalization(w, "dsml")
		fillMissingStreamResponseModel(dsmlResp, req.Model)
		normalizeProviderSpecificToolCalls(dsmlResp, allowedToolNames(req))
		setResponseToolDiagnosticHeader(w, dsmlResp)
		setResponseUsage(w, dsmlResp.Usage)
		s.cacheChatResponse(dsmlResp)
		return writeOpenAIChatResponseAsSSE(w, flusher, dsmlResp)
	}
	streamToolSanitizer := newOpenAIStreamToolSanitizer(allowedToolNames(req))
	eventProcessor := newOpenAIStreamEventProcessor(w, flusher, acc, streamToolSanitizer)
	for _, line := range buffered {
		if err := eventProcessor.consumeLine(line); err != nil {
			return fmt.Errorf("解析响应失败: OpenAI SSE: %w", err)
		}
		if eventProcessor.receivedDone() {
			break
		}
	}
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
		if flushErr := eventProcessor.flushPendingBeforeReadError(); flushErr != nil {
			return fmt.Errorf("解析响应失败: OpenAI SSE: %w", flushErr)
		}
		return err
	}
	if err := eventProcessor.finish(); err != nil {
		return fmt.Errorf("解析响应失败: OpenAI SSE: %w", err)
	}
	if !eventProcessor.receivedDone() && acc.finished {
		// 上游已给出完整 finish_reason 但省略 [DONE] 时，补齐标准下游终态。
		// 没有 finish_reason 的 EOF 仍由后续 validateOpenAIStreamCompletion 拒绝，
		// 因此不会把真正截断的流伪装成成功。
		if err := eventProcessor.consumeLine("data: [DONE]"); err != nil {
			return fmt.Errorf("解析响应失败: OpenAI SSE: %w", err)
		}
		if err := eventProcessor.finish(); err != nil {
			return fmt.Errorf("解析响应失败: OpenAI SSE: %w", err)
		}
	}
	if err := validateOpenAIStreamCompletion(acc, streamToolSanitizer); err != nil {
		return fmt.Errorf("解析响应失败: OpenAI SSE: %w", err)
	}
	setResponseUsage(w, acc.usage)
	if err := eventProcessor.commit(); err != nil {
		return fmt.Errorf("写入响应失败: OpenAI SSE: %w", err)
	}
	s.cacheStreamAccumulator(acc)
	setStreamToolDiagnosticHeader(w, acc)
	return nil
}

// streamOllamaToOpenAI 将 Ollama NDJSON 流转换为 OpenAI SSE。
func (s *Server) streamOllamaToOpenAI(w http.ResponseWriter, r *http.Request, prov provider.Provider, req *provider.ChatRequest, flusher http.Flusher) error {
	setProxyStreamState(w, "upstream_connecting")
	stream, err := prov.ChatStream(r.Context(), req)
	if err != nil {
		return fmt.Errorf("ollama stream error: %w", err)
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
	ollamaAcc := newStreamReasoningAccumulator()
	openAIAcc := newStreamReasoningAccumulator()
	streamToolSanitizer := newOpenAIStreamToolSanitizer(allowedToolNames(req))
	eventProcessor := newOpenAIStreamEventProcessor(w, flusher, openAIAcc, streamToolSanitizer)
	ollamaToolCallIDs := map[int]string{}
	var terminalChunk []byte
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			continue
		}
		chunk, parseErr := converter.ParseOllamaStreamChunk(line)
		if parseErr != nil {
			if errors.Is(parseErr, converter.ErrStreamDone) {
				ollamaAcc.finished = true
				break
			}
			return fmt.Errorf("解析 Ollama 流失败: %w", parseErr)
		}
		if normalizeErr := normalizeOllamaStreamToolCallEnvelopes(chunk, ollamaToolCallIDs); normalizeErr != nil {
			return fmt.Errorf("解析 Ollama 流失败: %w", normalizeErr)
		}
		ollamaAcc.consumeOllamaChunk(chunk)

		out, convErr := converter.ConvertOllamaChunkToOpenAISSE(chunk, req.Model)
		if convErr != nil {
			return convErr
		}
		if done, _ := chunk["done"].(bool); done {
			terminalChunk = out
			break
		}
		if processErr := consumeConvertedOpenAISSEEvent(eventProcessor, out); processErr != nil {
			return fmt.Errorf("解析 Ollama 工具流失败: %w", processErr)
		}

		if r.Context().Err() != nil {
			return r.Context().Err()
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	if err := validateOllamaStreamCompletion(ollamaAcc); err != nil {
		return fmt.Errorf("解析 Ollama 流失败: %w", err)
	}
	if len(terminalChunk) > 0 {
		if err := consumeConvertedOpenAISSEEvent(eventProcessor, terminalChunk); err != nil {
			return fmt.Errorf("解析 Ollama 工具终态失败: %w", err)
		}
	}
	if err := eventProcessor.consumeLine("data: [DONE]"); err != nil {
		return fmt.Errorf("解析 Ollama 工具终态失败: %w", err)
	}
	if err := eventProcessor.consumeLine(""); err != nil {
		return fmt.Errorf("解析 Ollama 工具终态失败: %w", err)
	}
	if err := eventProcessor.finish(); err != nil {
		return fmt.Errorf("解析 Ollama 工具流失败: %w", err)
	}
	if err := validateOpenAIStreamCompletion(openAIAcc, streamToolSanitizer); err != nil {
		return fmt.Errorf("解析 Ollama 工具流失败: %w", err)
	}
	setResponseUsage(w, ollamaAcc.usage)
	if err := eventProcessor.commit(); err != nil {
		return fmt.Errorf("写入 Ollama 工具流失败: %w", err)
	}
	s.cacheStreamAccumulator(ollamaAcc)
	setStreamToolDiagnosticHeader(w, ollamaAcc)
	return nil
}

// streamOllamaPassthrough 对 Ollama 客户端保持原生 NDJSON 输出。
func (s *Server) streamOllamaPassthrough(w http.ResponseWriter, r *http.Request, prov provider.Provider, req *provider.ChatRequest, flusher http.Flusher) error {
	setProxyStreamState(w, "upstream_connecting")
	stream, err := prov.ChatStream(r.Context(), req)
	if err != nil {
		return fmt.Errorf("ollama stream error: %w", err)
	}
	defer stream.Close()
	setProxyStreamState(w, "upstream_connected")

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
	setResponseUsage(w, acc.usage)
	s.cacheStreamAccumulator(acc)
	setStreamToolDiagnosticHeader(w, acc)
	return nil
}

// streamOpenAIToOllama 将 OpenAI SSE 流转换为 Ollama NDJSON。
func (s *Server) streamOpenAIToOllama(w http.ResponseWriter, r *http.Request, prov provider.Provider, req *provider.ChatRequest, flusher http.Flusher) error {
	setProxyStreamState(w, "upstream_connecting")
	stream, err := prov.ChatStream(r.Context(), req)
	if err != nil {
		return fmt.Errorf("openai stream error: %w", err)
	}
	defer stream.Close()
	setProxyStreamState(w, "upstream_connected")

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	scanner := newStreamScanner(stream)
	acc := newStreamReasoningAccumulator()
	streamToolSanitizer := newOpenAIStreamToolSanitizer(allowedToolNames(req))
	ollamaWriter := &openAIToOllamaStreamWriter{
		writer:       w,
		model:        req.Model,
		finishReason: "stop",
	}
	eventProcessor := newOpenAIStreamEventProcessor(
		ollamaWriter,
		flusher,
		acc,
		streamToolSanitizer,
	)
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
		if flushErr := eventProcessor.flushPendingBeforeReadError(); flushErr != nil {
			return fmt.Errorf("解析响应失败: OpenAI SSE: %w", flushErr)
		}
		return err
	}
	if err := eventProcessor.finish(); err != nil {
		return fmt.Errorf("解析响应失败: OpenAI SSE: %w", err)
	}
	if err := validateOpenAIStreamCompletion(acc, streamToolSanitizer); err != nil {
		return fmt.Errorf("解析响应失败: OpenAI SSE: %w", err)
	}
	setResponseUsage(w, acc.usage)
	if err := eventProcessor.commit(); err != nil {
		return fmt.Errorf("写入响应失败: Ollama NDJSON: %w", err)
	}
	s.cacheStreamAccumulator(acc)
	setStreamToolDiagnosticHeader(w, acc)

	out, err := buildOllamaStreamChunk(req.Model, "", nil, true, ollamaWriter.finishReason)
	if err != nil {
		return err
	}
	if _, writeErr := w.Write(append(out, '\n')); writeErr != nil {
		return writeErr
	}
	flusher.Flush()
	return nil
}

func consumeConvertedOpenAISSEEvent(processor *openAIStreamEventProcessor, event []byte) error {
	line := strings.TrimRight(string(event), "\r\n")
	if err := processor.consumeLine(line); err != nil {
		return err
	}
	return processor.consumeLine("")
}

// openAIToOllamaStreamWriter 只接收 event processor 已校验并按事件提交的 SSE。
// 它负责把每个逻辑 delta 转成 NDJSON；done=true 仍由调用方在整体校验后统一写出。
type openAIToOllamaStreamWriter struct {
	writer       io.Writer
	model        string
	finishReason string
}

func (w *openAIToOllamaStreamWriter) Write(data []byte) (int, error) {
	var payload strings.Builder
	flushPayload := func() error {
		if payload.Len() == 0 {
			return nil
		}
		err := w.writePayload(strings.TrimSpace(payload.String()))
		payload.Reset()
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			if payload.Len() > 0 {
				if err := flushPayload(); err != nil {
					return 0, err
				}
			}
			payload.WriteString(strings.TrimSpace(trimmed[5:]))
			continue
		}
		if payload.Len() == 0 {
			continue
		}
		if trimmed == "" && (json.Valid([]byte(payload.String())) || strings.TrimSpace(payload.String()) == "[DONE]") {
			if err := flushPayload(); err != nil {
				return 0, err
			}
			continue
		}
		payload.WriteByte('\n')
		payload.WriteString(line)
	}
	if err := flushPayload(); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (w *openAIToOllamaStreamWriter) writePayload(payload string) error {
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	chunk, err := parseOpenAIStreamPayload(payload)
	if err != nil {
		return err
	}
	if chunk.FinishReason != "" {
		w.finishReason = chunk.FinishReason
	}
	if chunk.Content == "" && len(chunk.ToolCalls) == 0 {
		if chunk.Refusal == "" {
			return nil
		}
		chunk.Content = chunk.Refusal
	}
	out, err := buildOllamaStreamChunk(w.model, chunk.Content, chunk.ToolCalls, false, "")
	if err != nil {
		return err
	}
	if _, err := w.writer.Write(append(out, '\n')); err != nil {
		return err
	}
	return nil
}

func newStreamScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	// direct SSE 的单事件上限必须和完整工具尾部的 64 MiB 总上限一致；
	// Scanner 按需扩容，不会为普通小 chunk 预分配 64 MiB。
	scanner.Buffer(make([]byte, 64*1024), int(maxAggregatedOpenAIStreamBytes))
	return scanner
}

type openAIStreamChunk struct {
	Content   string
	Reasoning string
	// Refusal 与 Content 独立；OpenAI 允许只返回拒绝内容而不返回普通文本。
	Refusal      string
	ToolCalls    []any
	FinishReason string
	Usage        *provider.Usage
}

func parseOpenAIStreamPayload(payload string) (openAIStreamChunk, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return openAIStreamChunk{}, err
	}
	if streamErr, ok := root["error"]; ok && streamErr != nil {
		encoded, err := json.Marshal(streamErr)
		if err != nil {
			return openAIStreamChunk{}, errors.New("upstream SSE error")
		}
		return openAIStreamChunk{}, fmt.Errorf("upstream SSE error: %s", sanitizeDiagnosticMessage(string(encoded)))
	}

	out := openAIStreamChunk{}
	if rawUsage, ok := root["usage"]; ok && rawUsage != nil {
		if encoded, err := json.Marshal(rawUsage); err == nil {
			var usage provider.Usage
			if json.Unmarshal(encoded, &usage) == nil {
				out.Usage = provider.NormalizeUsage(&usage)
			}
		}
	}

	choices, _ := root["choices"].([]any)
	if len(choices) == 0 {
		return out, nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return out, nil
	}

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
	if refusal, ok := delta["refusal"].(string); ok {
		out.Refusal = refusal
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
) (int, int) {
	configuredTimeout := defaultModelTimeoutSeconds
	if modelCfg, ok := findModelConfig(cfg, requestedModel, upstreamModel, providerName); ok && modelCfg.TimeoutSeconds != nil && *modelCfg.TimeoutSeconds > 0 {
		configuredTimeout = *modelCfg.TimeoutSeconds
	} else if hasProfile && profile.TimeoutSeconds != nil && *profile.TimeoutSeconds > 0 {
		configuredTimeout = *profile.TimeoutSeconds
	}

	return configuredTimeout, effectiveClientBoundTimeoutSeconds(cfg, configuredTimeout)
}

func effectiveClientBoundTimeoutSeconds(cfg *config.AppConfig, configuredTimeout int) int {
	if configuredTimeout <= 0 {
		return 0
	}
	// VS/Copilot 实测会在约 100 秒取消 /v1/chat/completions。
	// 如果代理允许 180/300 秒上游超时，用户只能等到客户端断开并看到 499，
	// 上游网关也没有机会在客户端预算内完成失败切换。因此默认把有效上游预算
	// 压到 client_timeout_budget_seconds：保留配置中“更短”的超时，但不让
	// “更长”的 profile 超过客户端可等待窗口。
	budget := config.DefaultClientTimeoutBudgetSeconds
	if cfg != nil && cfg.Defense.ClientTimeoutBudgetSeconds != nil && *cfg.Defense.ClientTimeoutBudgetSeconds > 0 {
		budget = *cfg.Defense.ClientTimeoutBudgetSeconds
	}
	if configuredTimeout > budget {
		return budget
	}
	return configuredTimeout
}

func profileForProvider(catalog *provider.ModelCatalog, model string, prov provider.Provider) (provider.ModelProfile, bool) {
	if catalog == nil || prov == nil {
		return provider.ModelProfile{}, false
	}
	var merged provider.ModelProfile
	found := false
	merge := func(profile provider.ModelProfile, ok bool) {
		if !ok {
			return
		}
		if !found {
			merged = profile
			found = true
			return
		}
		merged = provider.MergeModelProfiles(merged, profile)
	}

	merge(catalog.ProfileAny(model))
	selectionProviders := []string{prov.Name()}
	capabilityName := provider.CapabilityNameOf(prov)
	if strings.TrimSpace(capabilityName) != "" && !strings.EqualFold(capabilityName, prov.Name()) {
		selectionProviders = append(selectionProviders, capabilityName)
	}
	if provider.ResolveApiFormat(prov) == provider.ApiFormatOpenAi && !containsFold(selectionProviders, "openai") {
		selectionProviders = append(selectionProviders, "openai")
	}
	merge(catalog.ProfileFromSelections(model, selectionProviders...))
	merge(catalog.Profile(model, prov.Name()))
	return merged, found
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func requestContextWithTimeout(parent context.Context, timeoutSeconds int) (context.Context, context.CancelFunc) {
	if timeoutSeconds <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, time.Duration(timeoutSeconds)*time.Second)
}

func canAttemptAlternateChatMode(cfg *config.AppConfig, ctx context.Context, err error) bool {
	// 流式/非流式互相兜底可能产生第二次上游请求，必须受同一个防御开关控制。
	// 客户端已取消时不兜底，避免用户离开后代理继续消耗上游额度。
	return proxyDefenseEnabled(cfg) && ctx != nil && ctx.Err() == nil && provider.ShouldAttemptAlternateChatMode(err)
}

// alternateChatModeFailure 保留备用模式和初始模式的完整失败链。
// 备用模式是最后实际执行的步骤，放在错误前部并使用 %w，便于诊断分类和
// errors.Is/As 反映最接近客户端结果的原因；初始错误仍保留用于现场还原。
func alternateChatModeFailure(initialErr, fallbackErr error) error {
	if fallbackErr == nil {
		return initialErr
	}
	if initialErr == nil {
		return fallbackErr
	}
	return fmt.Errorf("备用聊天模式失败: %w（初始模式错误: %v）", fallbackErr, initialErr)
}

func shouldStopCandidateFallback(category string) bool {
	switch category {
	case "client_gone", "upstream_quota_exhausted", "upstream_auth_error", "upstream_rate_limit", "upstream_payload_too_large", "upstream_message_error", "upstream_request_error":
		return true
	default:
		return false
	}
}

func (s *Server) logProviderAttemptFailureForRequest(ctx context.Context, requestedModel, modelID, providerName string, attempt attemptDiagnostic) {
	s.logProviderAttemptFailure(requestIDFromContext(ctx), requestedModel, modelID, providerName, attempt)
}

// logProviderAttemptFailure 记录单个候选 provider 的失败。
// 这不是最终客户端请求日志：同一个 request_id 可能先有多个 WARN attempt，
// 最后仍由 loggingMiddleware 写一条 200/4xx/5xx 的最终请求日志。
func (s *Server) logProviderAttemptFailure(requestID, requestedModel, modelID, providerName string, attempt attemptDiagnostic) {
	if s == nil || s.logger == nil {
		return
	}
	if strings.TrimSpace(attempt.Message) != "" {
		s.logger.Debug("模型 %s（%s）上游原始错误: request_id=%s%s %s", modelID, providerName, strings.TrimSpace(requestID), consoleRouteSuffix(providerName, requestedModel, modelID), diagnosticHeaderValue(attempt.Message))
	}
	reason := userFacingDiagnosticFor(attempt.Category).Reason
	s.logger.Warn("模型 %s（%s）失败: request_id=%s%s reason=%s", modelID, providerName, strings.TrimSpace(requestID), consoleRouteSuffix(providerName, requestedModel, modelID), reason)
}

func isClientGoneError(err error) bool {
	if err == nil {
		return false
	}
	if category := classifyProxyErrorFromErr(err, err.Error()); category != "client_gone" {
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
	if catalog != nil {
		catalog.Rebuild()
	}
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

func firstIntOption(src map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		value, ok := intOption(src[key])
		if ok {
			return value, true
		}
	}
	return 0, false
}

func intOption(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		intValue := int(typed)
		return intValue, typed == float64(intValue)
	case int:
		return typed, true
	case json.Number:
		int64Value, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		intValue := int(int64Value)
		return intValue, int64(intValue) == int64Value
	default:
		return 0, false
	}
}

func providerOllamaOptionKnownFields() map[string]struct{} {
	return map[string]struct{}{
		"temperature":           {},
		"top_p":                 {},
		"top_k":                 {},
		"max_tokens":            {},
		"max_completion_tokens": {},
		"max_output_tokens":     {},
		"num_predict":           {},
		"num_ctx":               {},
		"reasoning_effort":      {},
	}
}
