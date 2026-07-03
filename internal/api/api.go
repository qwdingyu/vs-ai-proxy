package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
	"github.com/dingyuwang/vs-ai-proxy/internal/proxy"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
	"github.com/gin-gonic/gin"
)

const adminSessionCookieName = "vs_ai_proxy_admin_token"

// Server API 服务器
// 对外提供管理界面所需的配置、提供商、模型、测试、日志、统计接口，
// 并在 staticFS 非空时兼做静态资源与 SPA 路由的宿主。
type Server struct {
	engine    *gin.Engine
	configMgr *config.Manager
	proxy     *proxy.Server
	store     *store.Store
	logger    *log.Logger
	staticFS  fs.FS
}

// NewServer 创建 API 服务器
// port 仅用于 standalone 兼容启动时的默认地址参考；主程序默认使用单端口，
// 并把管理界面挂载到 /admin、管理 API 挂载到 /admin/api。
func NewServer(
	port int,
	configMgr *config.Manager,
	proxySrv *proxy.Server,
	st *store.Store,
	logger *log.Logger,
	staticFS fs.FS,
) *Server {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(noStoreForManagementAPI())
	engine.Use(adminAuthMiddleware())

	s := &Server{
		engine:    engine,
		configMgr: configMgr,
		proxy:     proxySrv,
		store:     st,
		logger:    logger,
		staticFS:  staticFS,
	}

	s.registerRoutes()
	return s
}

func noStoreForManagementAPI() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/admin" || strings.HasPrefix(path, "/admin/") ||
			strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/admin/api/") {
			// 管理 API 会返回 config.json 中的 provider api_key。
			// 即使默认只监听 127.0.0.1，也要避免浏览器磁盘缓存或中间代理缓存敏感配置。
			c.Header("Cache-Control", "no-store")
			c.Header("Pragma", "no-cache")
		}
		c.Next()
	}
}

func adminAuthMiddleware() gin.HandlerFunc {
	adminKey := strings.TrimSpace(os.Getenv("ADMIN_API_KEY"))
	if adminKey == "" {
		// 单端口部署时，如果用户已经为代理设置了 PROXY_API_KEY，默认复用它保护管理 API。
		// 如需单独管理，可以显式设置 ADMIN_API_KEY。
		adminKey = strings.TrimSpace(os.Getenv("PROXY_API_KEY"))
	}

	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if adminKey == "" || !strings.HasPrefix(path, "/admin") {
			c.Next()
			return
		}

		if path == "/admin/login" && c.Request.Method == http.MethodPost {
			if adminTokenMatches(formAdminToken(c), adminKey) {
				http.SetCookie(c.Writer, &http.Cookie{
					Name:     adminSessionCookieName,
					Value:    adminKey,
					Path:     "/admin",
					MaxAge:   int((12 * time.Hour).Seconds()),
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
				})
				c.Redirect(http.StatusSeeOther, "/admin")
				c.Abort()
				return
			}

			writeAdminLoginPage(c, http.StatusUnauthorized, "Token 不正确，请重试。")
			c.Abort()
			return
		}

		if adminRequestAuthorized(c, adminKey) {
			c.Next()
			return
		}

		if strings.HasPrefix(path, "/admin/api/") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}

		writeAdminLoginPage(c, http.StatusUnauthorized, "")
		c.Abort()
	}
}

func formAdminToken(c *gin.Context) string {
	if err := c.Request.ParseForm(); err != nil {
		return ""
	}
	return strings.TrimSpace(c.Request.FormValue("token"))
}

func adminRequestAuthorized(c *gin.Context, adminKey string) bool {
	token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if adminTokenMatches(token, adminKey) {
		return true
	}
	if cookie, err := c.Request.Cookie(adminSessionCookieName); err == nil {
		return adminTokenMatches(cookie.Value, adminKey)
	}
	return false
}

func adminTokenMatches(token string, adminKey string) bool {
	token = strings.TrimSpace(token)
	if token == "" || adminKey == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(adminKey)) == 1
}

func writeAdminLoginPage(c *gin.Context, status int, message string) {
	if message == "" {
		message = "请输入 .env 中配置的 ADMIN_API_KEY。"
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(status, `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>VS AI Proxy Admin Login</title>
  <style>
    body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0f1115;color:#e6edf3;font-family:ui-sans-serif,system-ui,-apple-system,Segoe UI,sans-serif}
    form{width:min(360px,calc(100vw - 32px));background:#161b22;border:1px solid #30363d;border-radius:10px;padding:20px;box-shadow:0 20px 60px rgba(0,0,0,.35)}
    h1{font-size:18px;margin:0 0 8px}
    p{margin:0 0 14px;color:#8b949e;font-size:13px;line-height:1.5}
    input{width:100%;box-sizing:border-box;background:#0f1115;color:#e6edf3;border:1px solid #30363d;border-radius:8px;padding:10px;margin:4px 0 12px}
    button{width:100%;border:0;border-radius:8px;padding:10px;background:#238636;color:white;cursor:pointer}
  </style>
</head>
<body>
  <form method="post" action="/admin/login">
    <h1>VS AI Proxy 管理面板</h1>
    <p>%s</p>
    <input name="token" type="password" autofocus autocomplete="current-password" placeholder="ADMIN_API_KEY" />
    <button type="submit">进入管理面板</button>
  </form>
</body>
</html>`, message)
}

// Handler 返回管理端 HTTP 处理器，供 cmd/server 挂载到 /admin。
func (s *Server) Handler() http.Handler {
	return s.engine
}

// registerRoutes 注册路由
// 按领域分组配置、提供商、模型、测试、日志、统计，并在启用静态资源时附加 SPA 路由中间件。
func (s *Server) registerRoutes() {
	// 健康检查
	s.engine.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	s.registerManagementAPIRoutes("/api")
	s.registerManagementAPIRoutes("/admin/api")

	// 静态文件与 SPA 路由
	if s.staticFS != nil {
		s.engine.Use(func(c *gin.Context) {
			path := c.Request.URL.Path
			if path == "/" || path == "/admin" || path == "/admin/" {
				c.Header("Cache-Control", "no-cache")
				c.Header("Content-Type", "text/html")
				f, err := s.staticFS.Open("index.html")
				if err != nil {
					c.String(http.StatusNotFound, "not found")
					return
				}
				defer f.Close()
				data, err := io.ReadAll(f)
				if err != nil {
					c.String(http.StatusInternalServerError, "read failed")
					return
				}
				c.Data(http.StatusOK, "text/html", data)
				return
			}
			if strings.HasPrefix(path, "/assets/") {
				c.FileFromFS(strings.TrimPrefix(path, "/"), http.FS(s.staticFS))
				return
			}
			if strings.HasPrefix(path, "/admin/assets/") {
				c.FileFromFS(strings.TrimPrefix(path, "/admin/"), http.FS(s.staticFS))
				return
			}
			if strings.HasPrefix(path, "/api/") ||
				strings.HasPrefix(path, "/admin/api/") ||
				strings.HasPrefix(path, "/health") ||
				strings.HasPrefix(path, "/test/") {
				c.Next()
				return
			}
			if strings.HasPrefix(path, "/admin/") {
				c.FileFromFS("index.html", http.FS(s.staticFS))
				return
			}
			c.FileFromFS("index.html", http.FS(s.staticFS))
		})
	}
	s.engine.NoRoute(func(c *gin.Context) {
		if s.staticFS != nil {
			c.FileFromFS("index.html", http.FS(s.staticFS))
			return
		}
		c.String(http.StatusNotFound, "not found")
	})
}

func (s *Server) registerManagementAPIRoutes(prefix string) {
	group := s.engine.Group(prefix)

	// 配置相关
	group.GET("/config", s.getConfig)
	group.POST("/config/validate", s.validateConfig)
	group.PUT("/config", s.saveConfig)

	// 提供商相关
	group.GET("/providers", s.listProviders)
	group.GET("/providers/health", s.getProviderHealth)
	group.POST("/providers/probe", s.probeProvider)
	group.POST("/providers", s.addProvider)
	group.PUT("/providers/:name", s.updateProvider)
	group.DELETE("/providers/:name", s.deleteProvider)

	// 模型相关
	group.GET("/models", s.listModels)
	group.GET("/models/metadata", s.getModelMetadata)
	group.PUT("/models", s.saveModels)

	// 测试相关
	group.POST("/test/connection", s.testConnection)
	group.POST("/test/chat", s.testChat)

	// 日志相关
	group.GET("/logs", s.getLogs)
	group.DELETE("/logs", s.clearLogs)

	// 统计相关
	group.GET("/statistics", s.getStatistics)
}

// getConfig 获取当前配置快照
func (s *Server) getConfig(c *gin.Context) {
	c.JSON(http.StatusOK, s.configMgr.Get())
}

type providerHealthResponse struct {
	Provider            string     `json:"provider"`
	Successes           int        `json:"successes"`
	Failures            int        `json:"failures"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	SuccessRate         float64    `json:"success_rate"`
	LatencyMs           float64    `json:"latency_ms"`
	LastSuccess         *time.Time `json:"last_success,omitempty"`
	LastFailure         *time.Time `json:"last_failure,omitempty"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	CooldownRemainingMs int64      `json:"cooldown_remaining_ms"`
	LastError           string     `json:"last_error,omitempty"`
}

func (s *Server) getProviderHealth(c *gin.Context) {
	if s.proxy == nil {
		c.JSON(http.StatusOK, []providerHealthResponse{})
		return
	}

	health := s.proxy.ProviderHealthSnapshot()
	_, registry, _ := s.proxy.SnapshotComponents()
	providerNames := map[string]struct{}{}
	for providerName := range health {
		providerNames[providerName] = struct{}{}
	}
	if registry != nil {
		for _, providerName := range registry.ProviderNames() {
			providerNames[providerName] = struct{}{}
		}
	}

	names := make([]string, 0, len(providerNames))
	for providerName := range providerNames {
		names = append(names, providerName)
	}
	sort.Strings(names)

	out := make([]providerHealthResponse, 0, len(names))
	now := time.Now()
	for _, providerName := range names {
		item := health[providerName]
		total := item.Successes + item.Failures
		successRate := 0.0
		if total > 0 {
			successRate = float64(item.Successes) / float64(total)
		}

		remaining := int64(0)
		if item.CooldownUntil.After(now) {
			remaining = item.CooldownUntil.Sub(now).Milliseconds()
		}
		out = append(out, providerHealthResponse{
			Provider:            providerName,
			Successes:           item.Successes,
			Failures:            item.Failures,
			ConsecutiveFailures: item.ConsecutiveFailures,
			SuccessRate:         successRate,
			LatencyMs:           float64(item.LatencyEWMA.Microseconds()) / 1000,
			LastSuccess:         nonZeroTimePtr(item.LastSuccess),
			LastFailure:         nonZeroTimePtr(item.LastFailure),
			CooldownUntil:       futureTimePtr(item.CooldownUntil, now),
			CooldownRemainingMs: remaining,
			LastError:           item.LastError,
		})
	}
	c.JSON(http.StatusOK, out)
}

func nonZeroTimePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func futureTimePtr(value time.Time, now time.Time) *time.Time {
	if !value.After(now) {
		return nil
	}
	return &value
}

// saveConfig 保存配置，并热更新代理路由。
func (s *Server) saveConfig(c *gin.Context) {
	var cfg config.AppConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result := validateAppConfig(&cfg)
	if !result.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "配置校验失败", "validation": result})
		return
	}
	if !s.saveAndApplyConfig(c, &cfg) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// validateConfig 供 Web 管理端保存前预检，避免错误模型/provider 关系写入 config.json 后
// 才在 Visual Studio Copilot 请求链路里表现为 502/503。
func (s *Server) validateConfig(c *gin.Context) {
	var cfg config.AppConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result := validateAppConfig(&cfg)
	c.JSON(http.StatusOK, result)
}

// listProviders 列出所有提供商
func (s *Server) listProviders(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"providers": s.configMgr.Get().Providers})
}

// addProvider 新增提供商并热更新代理路由。
func (s *Server) addProvider(c *gin.Context) {
	var p config.ProviderConfig
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p = config.NormalizeProvider(p)
	if err := validateProviderConfig(p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfg := s.cloneConfig()
	if providerIDExists(cfg.Providers, config.ProviderKey(p), "") {
		c.JSON(http.StatusConflict, gin.H{"error": "提供商 ID 已存在"})
		return
	}
	cfg.Providers = append(cfg.Providers, p)
	if !s.saveAndApplyConfig(c, cfg) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// updateProvider 更新已有提供商
func (s *Server) updateProvider(c *gin.Context) {
	name := c.Param("name")
	var p config.ProviderConfig
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p = config.NormalizeProvider(p)
	if err := validateProviderConfig(p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	cfg := s.cloneConfig()
	for i, prov := range cfg.Providers {
		if providerParamMatches(prov, name) {
			currentKey := config.ProviderKey(prov)
			if strings.EqualFold(currentKey, config.UseAIProviderID) &&
				!strings.EqualFold(config.ProviderKey(p), config.UseAIProviderID) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "UseAI 是内置提供商，ID 不能修改"})
				return
			}
			if providerIDExists(cfg.Providers, config.ProviderKey(p), currentKey) {
				c.JSON(http.StatusConflict, gin.H{"error": "提供商 ID 已存在"})
				return
			}
			cfg.Providers[i] = p
			result := validateAppConfig(cfg)
			if !result.Valid {
				c.JSON(http.StatusBadRequest, gin.H{"error": "配置校验失败", "validation": result})
				return
			}
			if !s.saveAndApplyConfig(c, cfg) {
				return
			}
			c.JSON(http.StatusOK, gin.H{"success": true})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "提供商未找到"})
}

// deleteProvider 删除提供商
func (s *Server) deleteProvider(c *gin.Context) {
	name := c.Param("name")
	cfg := s.cloneConfig()
	for i, prov := range cfg.Providers {
		if providerParamMatches(prov, name) {
			if strings.EqualFold(config.ProviderKey(prov), config.UseAIProviderID) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "UseAI 是内置提供商，不能删除；如需停用可在配置中关闭启用状态"})
				return
			}
			cfg.Providers = append(cfg.Providers[:i], cfg.Providers[i+1:]...)
			result := validateAppConfig(cfg)
			if !result.Valid {
				c.JSON(http.StatusBadRequest, gin.H{"error": "配置校验失败", "validation": result})
				return
			}
			if !s.saveAndApplyConfig(c, cfg) {
				return
			}
			c.JSON(http.StatusOK, gin.H{"success": true})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "提供商未找到"})
}

type providerProbeRequest struct {
	Provider config.ProviderConfig `json:"provider"`
}

type providerProbeAttempt struct {
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	Error   string `json:"error,omitempty"`
}

type providerProbeResult struct {
	Reachable        bool                   `json:"reachable"`
	DetectedType     string                 `json:"detected_type,omitempty"`
	CorrectedBaseURL string                 `json:"corrected_base_url,omitempty"`
	ModelsCount      int                    `json:"models_count"`
	ModelsPreview    []string               `json:"models_preview,omitempty"`
	Message          string                 `json:"message"`
	Attempts         []providerProbeAttempt `json:"attempts,omitempty"`
}

func (s *Server) probeProvider(c *gin.Context) {
	var req providerProbeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	providerCfg := config.NormalizeProvider(req.Provider)
	if strings.TrimSpace(providerCfg.BaseURL) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "提供商 Base URL 不能为空"})
		return
	}

	result := probeProviderConfig(c.Request.Context(), providerCfg)
	c.JSON(http.StatusOK, result)
}

func probeProviderConfig(ctx context.Context, providerCfg config.ProviderConfig) providerProbeResult {
	providerType := strings.ToLower(strings.TrimSpace(providerCfg.Type))
	if providerType == "" || providerType == "custom" {
		providerType = "openai"
	}
	if providerType == "ollama" {
		return probeOllamaProvider(ctx, providerCfg)
	}
	return probeOpenAIProvider(ctx, providerCfg)
}

func probeOpenAIProvider(ctx context.Context, providerCfg config.ProviderConfig) providerProbeResult {
	attempts := []providerProbeAttempt{}
	for _, baseURL := range openAIProbeBaseURLs(providerCfg.BaseURL) {
		prov := provider.NewOpenAIProviderWithCapability(
			config.ProviderKey(providerCfg),
			providerCapabilityNameFromConfig(providerCfg),
			providerCfg.APIKey,
			baseURL,
			true,
			10*time.Second,
		)
		models, err := prov.ListModels(ctx)
		if err == nil && len(models) > 0 {
			return successfulProbeResult("openai", baseURL, models, attempts)
		}
		attempts = append(attempts, providerProbeAttempt{
			Type:    "openai",
			BaseURL: baseURL,
			Error:   probeErrorMessage(err, len(models)),
		})
	}
	return failedProbeResult("OpenAI-compatible endpoint 探测失败", attempts)
}

func probeOllamaProvider(ctx context.Context, providerCfg config.ProviderConfig) providerProbeResult {
	attempts := []providerProbeAttempt{}
	for _, baseURL := range ollamaProbeBaseURLs(providerCfg.BaseURL) {
		prov := provider.NewOllamaProviderWithCapability(
			config.ProviderKey(providerCfg),
			providerCapabilityNameFromConfig(providerCfg),
			baseURL,
			true,
			10*time.Second,
		)
		models, err := prov.ListModels(ctx)
		if err == nil && len(models) > 0 {
			return successfulProbeResult("ollama", baseURL, models, attempts)
		}
		attempts = append(attempts, providerProbeAttempt{
			Type:    "ollama",
			BaseURL: baseURL,
			Error:   probeErrorMessage(err, len(models)),
		})
	}
	return failedProbeResult("Ollama endpoint 探测失败", attempts)
}

func successfulProbeResult(providerType, baseURL string, models []string, attempts []providerProbeAttempt) providerProbeResult {
	preview := models
	if len(preview) > 10 {
		preview = preview[:10]
	}
	return providerProbeResult{
		Reachable:        true,
		DetectedType:     providerType,
		CorrectedBaseURL: baseURL,
		ModelsCount:      len(models),
		ModelsPreview:    append([]string(nil), preview...),
		Message:          fmt.Sprintf("探测成功，发现 %d 个模型", len(models)),
		Attempts:         attempts,
	}
}

func failedProbeResult(message string, attempts []providerProbeAttempt) providerProbeResult {
	return providerProbeResult{
		Reachable: false,
		Message:   message,
		Attempts:  attempts,
	}
}

func probeErrorMessage(err error, modelCount int) string {
	if err != nil {
		return err.Error()
	}
	if modelCount == 0 {
		return "模型列表为空"
	}
	return ""
}

func openAIProbeBaseURLs(rawBaseURL string) []string {
	base := normalizeProbeBaseURL(rawBaseURL, "openai")
	trimmed := trimKnownProviderEndpointSuffix(base)
	baseSite := probeBaseSite(trimmed)
	return uniqueStrings([]string{
		trimmed,
		strings.TrimRight(trimmed, "/") + "/v1",
		baseSite,
		strings.TrimRight(baseSite, "/") + "/v1",
		strings.TrimRight(baseSite, "/") + "/api",
		strings.TrimRight(baseSite, "/") + "/api/v1",
	})
}

func ollamaProbeBaseURLs(rawBaseURL string) []string {
	base := normalizeProbeBaseURL(rawBaseURL, "ollama")
	trimmed := trimKnownProviderEndpointSuffix(base)
	return uniqueStrings([]string{trimmed, probeBaseSite(trimmed)})
}

func normalizeProbeBaseURL(rawBaseURL, providerType string) string {
	value := strings.TrimRight(strings.TrimSpace(rawBaseURL), "/")
	if value == "" || strings.Contains(value, "://") {
		return value
	}
	if providerType == "ollama" {
		return "http://" + value
	}
	return "https://" + value
}

func trimKnownProviderEndpointSuffix(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	lower := strings.ToLower(base)
	for _, suffix := range []string{
		"/chat/completions",
		"/models",
		"/api/tags",
		"/api/chat",
	} {
		if strings.HasSuffix(lower, suffix) {
			return strings.TrimRight(base[:len(base)-len(suffix)], "/")
		}
	}
	return base
}

func probeBaseSite(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	schemeEnd := strings.Index(baseURL, "://")
	if schemeEnd < 0 {
		return baseURL
	}
	rest := baseURL[schemeEnd+3:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return baseURL
	}
	return baseURL[:schemeEnd+3+slash]
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimRight(strings.TrimSpace(value), "/")
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

// listModels 列出所有模型配置
func (s *Server) listModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"models": s.configMgr.Get().Models})
}

// saveModels 保存模型配置
func (s *Server) saveModels(c *gin.Context) {
	var models []config.ModelConfig
	if err := c.ShouldBindJSON(&models); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfg := s.cloneConfig()
	cfg.Models = append([]config.ModelConfig(nil), models...)
	for i := range cfg.Models {
		cfg.Models[i] = config.NormalizeModel(cfg.Models[i])
	}
	result := validateAppConfig(cfg)
	if !result.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "配置校验失败", "validation": result})
		return
	}
	if !s.saveAndApplyConfig(c, cfg) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

type configValidationIssue struct {
	Code    string `json:"code"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

type configValidationResult struct {
	Valid    bool                    `json:"valid"`
	Errors   []configValidationIssue `json:"errors"`
	Warnings []configValidationIssue `json:"warnings"`
}

func (r *configValidationResult) addError(code, field, message string) {
	r.Errors = append(r.Errors, configValidationIssue{Code: code, Field: field, Message: message})
	r.Valid = false
}

func (r *configValidationResult) addWarning(code, field, message string) {
	r.Warnings = append(r.Warnings, configValidationIssue{Code: code, Field: field, Message: message})
}

func validateAppConfig(cfg *config.AppConfig) configValidationResult {
	result := configValidationResult{Valid: true}
	if cfg == nil {
		result.addError("config_nil", "config", "配置不能为空")
		return result
	}
	if err := validateProviderCollection(cfg.Providers); err != nil {
		result.addError("provider_invalid", "providers", err.Error())
		return result
	}
	normalized := config.CloneAppConfig(cfg)
	config.EnsureBuiltInProviders(normalized)
	validateModelCollection(normalized.Models, normalized.Providers, &result)
	return result
}

func validateModelCollection(models []config.ModelConfig, providers []config.ProviderConfig, result *configValidationResult) {
	providerRefs := providerReferenceSet(providers)
	seen := map[string]struct{}{}
	basenameToUpstreams := map[string]map[string]struct{}{}

	for i, model := range models {
		model = config.NormalizeModel(model)
		field := fmt.Sprintf("models[%d]", i)
		name := strings.TrimSpace(model.Name)
		if name == "" {
			result.addError("model_name_empty", field+".name", "模型名称不能为空")
			continue
		}

		providerID := strings.TrimSpace(model.ProviderID)
		if providerID != "" {
			if _, ok := providerRefs[strings.ToLower(providerID)]; !ok {
				result.addError(
					"model_provider_not_found",
					field+".provider_id",
					fmt.Sprintf("模型 %q 绑定的 provider_id %q 不存在；请填写 providers[].id，留空表示按优先级自动路由", name, providerID),
				)
			}
		}

		dupKey := strings.ToLower(name) + "@" + strings.ToLower(providerID)
		if _, ok := seen[dupKey]; ok {
			result.addError("model_duplicate", field, fmt.Sprintf("模型 %q 在同一 provider 绑定下重复", name))
		}
		seen[dupKey] = struct{}{}

		if model.Enabled {
			basename := strings.ToLower(provider.ModelBasename(name))
			if basename != "" && !strings.EqualFold(basename, name) {
				if basenameToUpstreams[basename] == nil {
					basenameToUpstreams[basename] = map[string]struct{}{}
				}
				basenameToUpstreams[basename][strings.ToLower(name)] = struct{}{}
			}
		}
	}

	for basename, upstreams := range basenameToUpstreams {
		if len(upstreams) > 1 {
			result.addWarning(
				"model_alias_ambiguous",
				"models",
				fmt.Sprintf("短模型名 %q 对应多个上游模型；Visual Studio 回传短名时可能需要使用 model@provider_id 明确路由", basename),
			)
		}
	}
}

func providerReferenceSet(providers []config.ProviderConfig) map[string]struct{} {
	refs := map[string]struct{}{}
	for _, p := range providers {
		p = config.NormalizeProvider(p)
		for _, value := range []string{config.ProviderKey(p), p.Name, p.DisplayName} {
			value = strings.TrimSpace(value)
			if value != "" {
				refs[strings.ToLower(value)] = struct{}{}
			}
		}
	}
	return refs
}

type modelMetadataResponse struct {
	Found           bool   `json:"found"`
	Source          string `json:"source,omitempty"`
	Model           string `json:"model,omitempty"`
	Provider        string `json:"provider,omitempty"`
	ContextLength   *int   `json:"context_length,omitempty"`
	MaxOutputTokens *int   `json:"max_output_tokens,omitempty"`
	SupportsTools   *bool  `json:"supports_tools,omitempty"`
	SupportsVision  *bool  `json:"supports_vision,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

func (s *Server) getModelMetadata(c *gin.Context) {
	model := strings.TrimSpace(c.Query("name"))
	providerID := strings.TrimSpace(c.Query("provider_id"))
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "模型名称不能为空"})
		return
	}

	profile, source, ok := s.lookupModelProfile(model, providerID)
	if !ok {
		c.JSON(http.StatusOK, modelMetadataResponse{Found: false})
		return
	}

	c.JSON(http.StatusOK, modelMetadataResponse{
		Found:           true,
		Source:          source,
		Model:           profile.Model,
		Provider:        profile.Provider,
		ContextLength:   copyIntPtr(profile.ContextLength),
		MaxOutputTokens: copyIntPtr(profile.MaxOutputTokens),
		SupportsTools:   copyBoolPtr(profile.SupportsTools),
		SupportsVision:  copyBoolPtr(profile.SupportsVision),
		ReasoningEffort: profile.ReasoningEffort,
	})
}

func (s *Server) enrichModelDefaults(cfg *config.AppConfig) {
	if cfg == nil {
		return
	}
	catalog := s.modelMetadataCatalog()
	for i := range cfg.Models {
		model := config.NormalizeModel(cfg.Models[i])
		profile, _, ok := lookupModelProfile(catalog, model.Name, model.ProviderID)
		if ok {
			applyProfileDefaults(&model, profile)
		}
		applySafeModelFallbacks(&model)
		cfg.Models[i] = model
	}
}

func (s *Server) lookupModelProfile(model, providerID string) (provider.ModelProfile, string, bool) {
	return lookupModelProfile(s.modelMetadataCatalog(), model, providerID)
}

func lookupModelProfile(catalog *provider.ModelCatalog, model, providerID string) (provider.ModelProfile, string, bool) {
	if catalog == nil {
		return provider.ModelProfile{}, "", false
	}
	if providerID != "" {
		if profile, ok := catalog.Profile(model, providerID); ok {
			return profile, "provider_profile", true
		}
	}
	if profile, ok := catalog.ProfileAny(model); ok {
		return profile, "model_catalog", true
	}
	return provider.ModelProfile{}, "", false
}

func (s *Server) modelMetadataCatalog() *provider.ModelCatalog {
	configDir := ""
	if s.configMgr != nil {
		configDir = filepath.Dir(s.configMgr.ConfigPath())
	}
	return provider.NewModelCatalog(nil, configDir, 0)
}

func applyProfileDefaults(model *config.ModelConfig, profile provider.ModelProfile) {
	if model.ContextLength == nil || *model.ContextLength <= 0 {
		model.ContextLength = copyIntPtr(profile.ContextLength)
	}
	if model.MaxOutputTokens == nil || *model.MaxOutputTokens <= 0 {
		model.MaxOutputTokens = copyIntPtr(profile.MaxOutputTokens)
	}
	if model.SupportsTools == nil {
		model.SupportsTools = copyBoolPtr(profile.SupportsTools)
	}
	if model.SupportsVision == nil {
		model.SupportsVision = copyBoolPtr(profile.SupportsVision)
	}
	if strings.TrimSpace(model.ReasoningEffort) == "" {
		model.ReasoningEffort = profile.ReasoningEffort
	}
}

func applySafeModelFallbacks(model *config.ModelConfig) {
	if model.ContextLength == nil || *model.ContextLength <= 0 {
		model.ContextLength = intPtr(128000)
	}
	if model.MaxOutputTokens == nil || *model.MaxOutputTokens <= 0 {
		model.MaxOutputTokens = intPtr(4096)
	}
	if model.SupportsTools == nil {
		model.SupportsTools = boolPtr(true)
	}
	if model.SupportsVision == nil {
		model.SupportsVision = boolPtr(false)
	}
}

func copyIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func copyBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func intPtr(value int) *int {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}

func (s *Server) cloneConfig() *config.AppConfig {
	src := s.configMgr.Get()
	cfg := *src
	cfg.Providers = append([]config.ProviderConfig(nil), src.Providers...)
	cfg.Models = append([]config.ModelConfig(nil), src.Models...)
	return &cfg
}

func providerParamMatches(provider config.ProviderConfig, param string) bool {
	provider = config.NormalizeProvider(provider)
	param = strings.TrimSpace(param)
	return strings.EqualFold(config.ProviderKey(provider), param) ||
		strings.EqualFold(provider.Name, param) ||
		strings.EqualFold(provider.DisplayName, param)
}

func providerIDExists(providers []config.ProviderConfig, candidateID string, exceptID string) bool {
	candidateID = strings.TrimSpace(candidateID)
	exceptID = strings.TrimSpace(exceptID)
	if candidateID == "" {
		return false
	}
	for _, p := range providers {
		key := config.ProviderKey(p)
		if strings.EqualFold(key, exceptID) {
			continue
		}
		if strings.EqualFold(key, candidateID) {
			return true
		}
	}
	return false
}

func validateProviderCollection(providers []config.ProviderConfig) error {
	seen := map[string]struct{}{}
	for _, p := range providers {
		p = config.NormalizeProvider(p)
		if err := validateProviderConfig(p); err != nil {
			return err
		}
		key := strings.ToLower(config.ProviderKey(p))
		if _, ok := seen[key]; ok {
			return errors.New("提供商 ID 已重复")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateProviderConfig(p config.ProviderConfig) error {
	p = config.NormalizeProvider(p)
	if config.ProviderKey(p) == "" {
		return errors.New("提供商 ID 不能为空")
	}
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("提供商名称不能为空")
	}
	if strings.EqualFold(strings.TrimSpace(p.Name), config.UseAIProviderName) &&
		!strings.EqualFold(config.ProviderKey(p), config.UseAIProviderID) {
		return errors.New("UseAI 是内置提供商，ID 必须为 useai")
	}
	switch strings.ToLower(strings.TrimSpace(p.Type)) {
	case "openai", "ollama", "custom":
	default:
		return errors.New("提供商类型必须是 openai、ollama 或 custom")
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return errors.New("提供商 Base URL 不能为空")
	}
	return nil
}

func providerCapabilityNameFromConfig(p config.ProviderConfig) string {
	p = config.NormalizeProvider(p)
	// provider 包集中维护能力推断规则，避免管理测试接口和真实代理路由出现分叉。
	return provider.InferCapabilityName(config.ProviderKey(p), p.Name, p.BaseURL, p.Type)
}

func (s *Server) saveAndApplyConfig(c *gin.Context, cfg *config.AppConfig) bool {
	s.enrichModelDefaults(cfg)
	if err := s.configMgr.Save(cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return false
	}
	if s.proxy != nil {
		s.proxy.Reconfigure(s.configMgr.Get())
	}
	return true
}

// testConnection 测试提供商连接
// 简化实现：通过 ListModels 探测连通性并返回模型列表。
func (s *Server) testConnection(c *gin.Context) {
	var req struct {
		Provider config.ProviderConfig `json:"provider"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var prov provider.Provider
	if req.Provider.Type == "ollama" {
		cfg := config.NormalizeProvider(req.Provider)
		prov = provider.NewOllamaProviderWithCapability(config.ProviderKey(cfg), providerCapabilityNameFromConfig(cfg), cfg.BaseURL, true, 30*time.Second)
	} else {
		cfg := config.NormalizeProvider(req.Provider)
		prov = provider.NewOpenAIProviderWithCapability(config.ProviderKey(cfg), providerCapabilityNameFromConfig(cfg), cfg.APIKey, cfg.BaseURL, true, 30*time.Second)
	}

	models, err := prov.ListModels(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "连接成功",
		"models":  models,
	})
}

// testChat 测试单轮聊天
func (s *Server) testChat(c *gin.Context) {
	var req struct {
		Provider config.ProviderConfig `json:"provider"`
		Message  string                `json:"message"`
		Model    string                `json:"model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var prov provider.Provider
	if req.Provider.Type == "ollama" {
		cfg := config.NormalizeProvider(req.Provider)
		prov = provider.NewOllamaProviderWithCapability(config.ProviderKey(cfg), providerCapabilityNameFromConfig(cfg), cfg.BaseURL, true, 60*time.Second)
	} else {
		cfg := config.NormalizeProvider(req.Provider)
		prov = provider.NewOpenAIProviderWithCapability(config.ProviderKey(cfg), providerCapabilityNameFromConfig(cfg), cfg.APIKey, cfg.BaseURL, true, 60*time.Second)
	}

	chatReq := &provider.ChatRequest{
		Model:    req.Model,
		Messages: []provider.Message{{Role: "user", Content: req.Message}},
		Stream:   false,
	}

	resp, err := prov.Chat(c.Request.Context(), chatReq)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": err.Error()})
		return
	}
	if len(resp.Choices) == 0 {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": "上游响应没有 choices"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"content": resp.Choices[0].Message.Content,
	})
}

// getLogs 获取日志
// 支持 page/page_size 和兼容旧的 limit 参数。
// 当 page 存在时使用分页模式返回 LogPageResult；否则使用 limit 返回旧格式。
func (s *Server) getLogs(c *gin.Context) {
	// 分页模式：page 参数存在时启用
	pageStr := c.Query("page")
	if pageStr != "" {
		page := 1
		pageSize := 50
		if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
		if ps := c.Query("page_size"); ps != "" {
			if p, err := strconv.Atoi(ps); err == nil && p > 0 {
				pageSize = p
			}
		}
		result := s.store.GetLogsPage(page, pageSize)
		c.JSON(http.StatusOK, result)
		return
	}

	// 兼容旧模式：仅 limit 参数
	limit := 100
	if l := c.Query("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	logs := s.store.GetLogs(limit)
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}

// clearLogs 清空日志
func (s *Server) clearLogs(c *gin.Context) {
	s.store.ClearLogs()
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// getStatistics 获取统计
func (s *Server) getStatistics(c *gin.Context) {
	stats := s.store.GetStatistics()
	c.JSON(http.StatusOK, stats)
}
