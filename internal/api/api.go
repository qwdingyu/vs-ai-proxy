package api

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
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
// port 仅用于默认地址参考；实际监听地址会由 config.Port 推导为 +1000 的管理端口。
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

// registerRoutes 注册路由
// 按领域分组配置、提供商、模型、测试、日志、统计，并在启用静态资源时附加 SPA 路由中间件。
func (s *Server) registerRoutes() {
	// 健康检查
	s.engine.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 配置相关
	s.engine.GET("/api/config", s.getConfig)
	s.engine.PUT("/api/config", s.saveConfig)

	// 提供商相关
	s.engine.GET("/api/providers", s.listProviders)
	s.engine.POST("/api/providers", s.addProvider)
	s.engine.PUT("/api/providers/:name", s.updateProvider)
	s.engine.DELETE("/api/providers/:name", s.deleteProvider)

	// 模型相关
	s.engine.GET("/api/models", s.listModels)
	s.engine.PUT("/api/models", s.saveModels)

	// 测试相关
	s.engine.POST("/api/test/connection", s.testConnection)
	s.engine.POST("/api/test/chat", s.testChat)

	// 日志相关
	s.engine.GET("/api/logs", s.getLogs)
	s.engine.DELETE("/api/logs", s.clearLogs)

	// 统计相关
	s.engine.GET("/api/statistics", s.getStatistics)

	// 静态文件与 SPA 路由
	if s.staticFS != nil {
		s.engine.Use(func(c *gin.Context) {
			path := c.Request.URL.Path
			if path == "/" {
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
			if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/health") || strings.HasPrefix(path, "/test/") {
				c.Next()
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

// Start 启动 API 服务
func (s *Server) Start() error {
	addr := ":8080"
	if s.config.Port > 0 {
		addr = fmt.Sprintf(":%d", s.config.Port+1000)
	}
	s.logger.Info("API 服务启动于 http://localhost%s", addr)
	return s.engine.Run(addr)
}

// getConfig 获取当前配置快照
func (s *Server) getConfig(c *gin.Context) {
	c.JSON(http.StatusOK, s.configMgr.Get())
}

// saveConfig 保存配置
// 仅做持久化验证并刷新本地缓存，不直接热重启 proxy server。
func (s *Server) saveConfig(c *gin.Context) {
	var cfg config.AppConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.configMgr.Save(&cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.config = s.configMgr.Get()
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// listProviders 列出所有提供商
func (s *Server) listProviders(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"providers": s.config.Providers})
}

// addProvider 新增提供商
// 当前仅追加到内存配置并持久化，不立即校验连通性。
func (s *Server) addProvider(c *gin.Context) {
	var p config.ProviderConfig
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.config.Providers = append(s.config.Providers, p)
	s.configMgr.Save(s.config)
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

	for i, prov := range s.config.Providers {
		if prov.Name == name {
			s.config.Providers[i] = p
			s.configMgr.Save(s.config)
			c.JSON(http.StatusOK, gin.H{"success": true})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "提供商未找到"})
}

// deleteProvider 删除提供商
func (s *Server) deleteProvider(c *gin.Context) {
	name := c.Param("name")
	for i, prov := range s.config.Providers {
		if prov.Name == name {
			s.config.Providers = append(s.config.Providers[:i], s.config.Providers[i+1:]...)
			s.configMgr.Save(s.config)
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
	s.config.Models = models
	s.configMgr.Save(s.config)
	c.JSON(http.StatusOK, gin.H{"success": true})
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
		prov = provider.NewOllamaProvider(req.Provider.Name, req.Provider.BaseURL, true, 30*time.Second)
	} else {
		prov = provider.NewOpenAIProvider(req.Provider.Name, req.Provider.APIKey, req.Provider.BaseURL, true, 30*time.Second)
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
		Message  string                 `json:"message"`
		Model    string                 `json:"model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var prov provider.Provider
	if req.Provider.Type == "ollama" {
		prov = provider.NewOllamaProvider(req.Provider.Name, req.Provider.BaseURL, true, 60*time.Second)
	} else {
		prov = provider.NewOpenAIProvider(req.Provider.Name, req.Provider.APIKey, req.Provider.BaseURL, true, 60*time.Second)
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

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"content": resp.Choices[0].Message.Content,
	})
}

// getLogs 获取日志
// 支持 limit 查询参数；未传时默认返回最近 100 条。
func (s *Server) getLogs(c *gin.Context) {
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
