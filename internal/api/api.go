package api

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
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

// Server API 服务器
// 对外提供管理界面所需的配置、提供商、模型、测试、日志、统计接口，
// 并在 staticFS 非空时兼做静态资源与 SPA 路由的宿主。
type Server struct {
	engine    *gin.Engine
	config    *config.AppConfig
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
	engine.Use(adminAPIAuthMiddleware())

	s := &Server{
		engine:    engine,
		config:    configMgr.Get(),
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
		if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/admin/api/") {
			// 管理 API 会返回 config.json 中的 provider api_key。
			// 即使默认只监听 127.0.0.1，也要避免浏览器磁盘缓存或中间代理缓存敏感配置。
			c.Header("Cache-Control", "no-store")
			c.Header("Pragma", "no-cache")
		}
		c.Next()
	}
}

func adminAPIAuthMiddleware() gin.HandlerFunc {
	adminKey := strings.TrimSpace(os.Getenv("ADMIN_API_KEY"))
	if adminKey == "" {
		// 单端口部署时，如果用户已经为代理设置了 PROXY_API_KEY，默认复用它保护管理 API。
		// 如需单独管理，可以显式设置 ADMIN_API_KEY。
		adminKey = strings.TrimSpace(os.Getenv("PROXY_API_KEY"))
	}

	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if adminKey == "" || !strings.HasPrefix(path, "/admin/api/") {
			c.Next()
			return
		}
		token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(adminKey)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}
		c.Next()
	}
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
	group.PUT("/config", s.saveConfig)

	// 提供商相关
	group.GET("/providers", s.listProviders)
	group.POST("/providers", s.addProvider)
	group.PUT("/providers/:name", s.updateProvider)
	group.DELETE("/providers/:name", s.deleteProvider)

	// 模型相关
	group.GET("/models", s.listModels)
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

// saveConfig 保存配置，并热更新代理路由。
func (s *Server) saveConfig(c *gin.Context) {
	var cfg config.AppConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateProviderCollection(cfg.Providers); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !s.saveAndApplyConfig(c, &cfg) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// listProviders 列出所有提供商
func (s *Server) listProviders(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"providers": s.config.Providers})
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
			if !s.saveAndApplyConfig(c, cfg) {
				return
			}
			c.JSON(http.StatusOK, gin.H{"success": true})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "提供商未找到"})
}

// listModels 列出所有模型配置
func (s *Server) listModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"models": s.config.Models})
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
	if !s.saveAndApplyConfig(c, cfg) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
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
	if err := s.configMgr.Save(cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return false
	}
	s.config = s.configMgr.Get()
	if s.proxy != nil {
		s.proxy.Reconfigure(s.config)
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
