package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
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

var version = "dev"

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
	logger.Info("运行配置文件: %s", configMgr.ConfigPath())
	logger.Info("请求日志文件: %s", storePath)
	st := store.New(1000)
	if err := st.LoadFromFile(storePath); err != nil {
		logger.Warn("加载请求日志失败: %v", err)
	}
	staticFS := web.MustSubFS()

	proxy.SetBuildVersion(version)
	// 创建代理服务器，对外提供 /v1/chat/completions、/api/chat 等兼容接口
	proxySrv := proxy.NewServer(cfg, configMgr, st, logger)
	// 创建 API 服务器，对外提供配置、提供商、模型、日志、统计及静态资源服务
	apiSrv := api.NewServer(cfg.Port, configMgr, proxySrv, st, logger, staticFS)
	_, registry, catalog := proxySrv.SnapshotComponents()
	benchSvc := benchmark.New(registry, catalog, logger)

	appAddr := resolveAppAddr(cfg.Port)
	appSrv := &http.Server{
		Addr:         appAddr,
		Handler:      newUnifiedHandler(apiSrv.Handler(), proxySrv.Handler()),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	warnDeprecatedManagementEnv(logger)

	// 后台启动单端口 HTTP 服务：/admin 是管理面板，其它路径保留代理协议。
	go func() {
		if err := appSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常退出: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go benchSvc.Run(ctx)
	go persistStoreLoop(ctx, st, storePath, logger)
	go watchConfigLoop(ctx, configMgr, proxySrv, logger)

	publicAddr := displayAddr(appAddr)
	logger.Info("VS AI Proxy %s 已启动，监听地址=http://%s，管理面板=http://%s/admin", version, publicAddr, publicAddr)
	logPublicAccessHint(appAddr, logger)

	// 监听退出信号，优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("正在关闭服务...")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := appSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("关闭 HTTP 服务失败: %v", err)
	}
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

	return filepath.Join(config.DefaultConfigDir(), "logs.json")
}

func newUnifiedHandler(adminHandler, proxyHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") {
			adminHandler.ServeHTTP(w, r)
			return
		}
		proxyHandler.ServeHTTP(w, r)
	})
}

func resolveAppAddr(port int) string {
	host := strings.TrimSpace(os.Getenv("HOST"))
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func displayAddr(addr string) string {
	return addr
}

func logPublicAccessHint(addr string, logger *log.Logger) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	if host == "0.0.0.0" || host == "::" || host == "" {
		logger.Info("当前监听所有网卡；云主机公网访问请使用 http://<服务器公网IP>:%s/admin，并确保安全组/防火墙放行该端口且已设置 ADMIN_API_KEY 或 PROXY_API_KEY", port)
	}
}

func warnDeprecatedManagementEnv(logger *log.Logger) {
	if strings.TrimSpace(os.Getenv("MANAGEMENT_PORT")) != "" ||
		strings.TrimSpace(os.Getenv("MANAGEMENT_HOST")) != "" {
		logger.Warn("MANAGEMENT_PORT/MANAGEMENT_HOST 已废弃；当前版本只启动单端口服务，请使用 PORT 和 HOST，管理面板路径为 /admin")
	}
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

func watchConfigLoop(ctx context.Context, configMgr *config.Manager, proxySrv *proxy.Server, logger *log.Logger) {
	if configMgr == nil || proxySrv == nil {
		return
	}
	path := configMgr.ConfigPath()
	if path == "" {
		return
	}

	lastMod, lastHash, err := configFileFingerprint(path)
	if err != nil {
		logger.Warn("初始化配置文件监控失败: %v", err)
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mod, hash, err := configFileFingerprint(path)
			if err != nil {
				logger.Warn("检查配置文件变更失败: %v", err)
				continue
			}
			if !mod.After(lastMod) && hash == lastHash {
				continue
			}

			oldPort := configMgr.Get().Port
			cfg, err := configMgr.Reload()
			if err != nil {
				logger.Warn("热加载配置失败: %v", err)
				continue
			}
			lastMod = mod
			lastHash = hash
			proxySrv.Reconfigure(cfg)
			if cfg.Port != oldPort {
				logger.Warn("配置文件端口已变更为 %d；当前监听端口仍为 %d，端口变更需要重启进程", cfg.Port, oldPort)
			}
			logger.Info("已热加载配置文件: %s (providers=%d models=%d)", path, len(cfg.Providers), len(cfg.Models))
		}
	}
}

func configFileFingerprint(path string) (time.Time, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, "", err
	}
	sum := sha256.Sum256(data)
	return info.ModTime(), fmt.Sprintf("%x", sum), nil
}
