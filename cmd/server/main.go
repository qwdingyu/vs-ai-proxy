package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/dingyuwang/vs-ai-proxy/internal/api"
	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/proxy"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
	"github.com/dingyuwang/vs-ai-proxy/web"
)

// main 进程入口
// 负责组装配置、日志、存储、代理服务与 API 服务，
// 并监听系统退出信号完成优雅停止。
func main() {
	// 创建控制台日志器
	logger := log.NewConsole()

	// 初始化配置管理器；优先使用 CONFIG_PATH 环境变量指定的配置文件
	configPath := os.Getenv("CONFIG_PATH")
	configMgr, err := config.NewManager(configPath)
	if err != nil {
		logger.Error("加载配置失败: %v", err)
		os.Exit(1)
	}

	cfg := configMgr.Get()
	st := store.New(1000)
	staticFS := web.MustSubFS()

	// 创建代理服务器，对外提供 /v1/chat/completions、/api/chat 等兼容接口
	proxySrv := proxy.NewServer(cfg, configMgr, st, logger)
	// 创建 API 服务器，对外提供配置、提供商、模型、日志、统计及静态资源服务
	apiSrv := api.NewServer(cfg.Port, configMgr, proxySrv, st, logger, staticFS)

	// 后台启动代理服务
	go func() {
		if err := proxySrv.Start(); err != nil {
			logger.Error("代理服务异常退出: %v", err)
		}
	}()

	// 后台启动 API 管理服务
	go func() {
		if err := apiSrv.Start(); err != nil {
			logger.Error("API 服务异常退出: %v", err)
		}
	}()

	logger.Info("VS AI Proxy 已启动，代理端口=%d，管理端口=%d", cfg.Port, cfg.Port+1000)

	// 监听退出信号，优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("正在关闭服务...")
	_ = proxySrv.Stop()
	logger.Info("服务已停止")
}
