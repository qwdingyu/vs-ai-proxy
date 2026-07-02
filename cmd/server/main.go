package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/api"
	"github.com/dingyuwang/vs-ai-proxy/internal/benchmark"
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

	loadDotEnv(logger)

	// 初始化配置管理器；优先使用 CONFIG_PATH 环境变量指定的配置文件
	configPath := os.Getenv("CONFIG_PATH")
	configMgr, err := config.NewManager(configPath)
	if err != nil {
		logger.Error("加载配置失败: %v", err)
		os.Exit(1)
	}

	cfg := configMgr.Get()
	storePath := resolveStorePath(configMgr.ConfigPath(), os.Getenv("STORE_PATH"))
	st := store.New(1000)
	if err := st.LoadFromFile(storePath); err != nil {
		logger.Warn("加载请求日志失败: %v", err)
	}
	staticFS := web.MustSubFS()

	// 创建代理服务器，对外提供 /v1/chat/completions、/api/chat 等兼容接口
	proxySrv := proxy.NewServer(cfg, configMgr, st, logger)
	// 创建 API 服务器，对外提供配置、提供商、模型、日志、统计及静态资源服务
	apiSrv := api.NewServer(cfg.Port, configMgr, proxySrv, st, logger, staticFS)
	_, registry, catalog := proxySrv.SnapshotComponents()
	benchSvc := benchmark.New(registry, catalog, logger)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go benchSvc.Run(ctx)
	go persistStoreLoop(ctx, st, storePath, logger)

	logger.Info("VS AI Proxy 已启动，代理端口=%d，管理端口=%d", cfg.Port, cfg.Port+1000)

	// 监听退出信号，优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("正在关闭服务...")
	cancel()
	_ = proxySrv.Stop()
	if err := st.PersistToFile(storePath); err != nil {
		logger.Warn("保存请求日志失败: %v", err)
	}
	logger.Info("服务已停止")
}

func loadDotEnv(logger *log.Logger) {
	for _, path := range dotEnvSearchPaths() {
		if err := loadEnvFile(path); err == nil {
			logger.Info("已加载环境配置文件: %s", path)
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("加载环境配置文件失败: %v", err)
			return
		}
	}
}

func dotEnvSearchPaths() []string {
	paths := make([]string, 0, 2)
	if exe, err := os.Executable(); err == nil && exe != "" {
		paths = append(paths, filepath.Join(filepath.Dir(exe), ".env"))
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		paths = append(paths, filepath.Join(cwd, ".env"))
	}
	return paths
}

func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		value = strings.Trim(value, `"'`)
		if key != "" {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("设置环境变量 %s 失败: %w", key, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func resolveStorePath(configPath, override string) string {
	if trimmed := filepath.Clean(strings.TrimSpace(override)); trimmed != "." && trimmed != "" {
		return trimmed
	}

	if configPath != "" {
		dir := filepath.Dir(configPath)
		return filepath.Join(dir, "logs.json")
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		return "logs.json"
	}
	return filepath.Join(configDir, "vs-ai-proxy", "logs.json")
}

func persistStoreLoop(ctx context.Context, st *store.Store, path string, logger *log.Logger) {
	if st == nil || path == "" {
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.PersistToFile(path); err != nil {
				logger.Warn("定期保存请求日志失败: %v", err)
			}
		}
	}
}
