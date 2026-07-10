package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/api"
	"github.com/dingyuwang/vs-ai-proxy/internal/benchmark"
	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/proxy"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
	"github.com/dingyuwang/vs-ai-proxy/internal/update"
	"github.com/dingyuwang/vs-ai-proxy/web"
)

var version = "dev"

var (
	checkUpdateFn             = update.Check
	selfUpdateFn              = update.SelfUpdate
	launchReplacementFn       = update.LaunchReplacement
	launchWindowsSelfUpdateFn = update.LaunchWindowsSelfUpdate
)

var startupSelfUpdateExit = make(chan struct{}, 1)

const (
	startupUpdateCheckTimeout = 8 * time.Second
	startupSelfUpdateTimeout  = 2 * time.Minute
)

// main 进程入口
// 负责组装配置、日志、存储、代理服务与 API 服务，
// 并监听系统退出信号完成优雅停止。
func main() {
	version = resolveBuildVersion(version)

	if handled, exitCode := handleCommandLine(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(exitCode)
	}

	// 创建控制台日志器
	logger := log.NewConsole()

	loadDotEnv(logger)
	if autoSelfUpdateOnStartup(logger, os.Args[1:]) {
		os.Exit(0)
	}

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
		WriteTimeout: 210 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	warnDeprecatedManagementEnv(logger)
	listener, err := listenWithStartupPortRecovery(appAddr, logger)
	if err != nil {
		logger.Error("HTTP 服务启动失败: %v", err)
		os.Exit(1)
	}

	// 后台启动单端口 HTTP 服务：/admin 是管理面板，其它路径保留代理协议。
	go func() {
		if err := appSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

	// 监听退出信号，优雅关闭；Windows 后台自更新脚本启动后也会通知当前进程退出，
	// 这样脚本才能解除 exe 文件占用，完成 .new -> exe 和旧版本 .bak 替换。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
	case <-startupSelfUpdateExit:
		logger.Info("后台自更新已准备替换，当前进程即将退出以完成 Windows 文件替换。")
	}

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

func resolveBuildVersion(value string) string {
	value = strings.TrimSpace(value)
	if value != "" && value != "dev" {
		return value
	}
	if envVersion := strings.TrimSpace(os.Getenv("VS_AI_PROXY_VERSION")); envVersion != "" {
		return envVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		var revision string
		var modified bool
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = strings.TrimSpace(setting.Value)
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
		if revision != "" {
			if len(revision) > 12 {
				revision = revision[:12]
			}
			if modified {
				return revision + "-dirty"
			}
			return revision
		}
	}
	return "dev"
}

func autoSelfUpdateOnStartup(logger *log.Logger, args []string) bool {
	if !autoUpdateEnabled() {
		logger.Info("启动自动更新已关闭")
		return false
	}
	if !isReleaseVersion(version) {
		return false
	}

	dir := filepath.Join(config.DefaultConfigDir(), "updates")
	opts := update.Options{CurrentVersion: version, TargetDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), startupUpdateCheckTimeout)
	defer cancel()

	check, err := checkUpdateFn(ctx, opts)
	if err != nil {
		logger.Warn("启动自动更新检查失败，继续启动当前版本: %v", describeStartupUpdateError(err))
		return false
	}
	if !check.UpdateAvailable {
		logger.Info("当前已是最新版本: %s", check.CurrentVersion)
		return false
	}

	logger.Info("发现新版本: %s -> %s，已转入后台下载并安装，不阻塞当前服务启动。", check.CurrentVersion, check.LatestTag)
	go runStartupSelfUpdate(logger, opts, args)
	return false
}

func runStartupSelfUpdate(logger *log.Logger, opts update.Options, args []string) {
	ctx, cancel := context.WithTimeout(context.Background(), startupSelfUpdateTimeout)
	defer cancel()

	result, err := selfUpdateFn(ctx, opts)
	if err != nil {
		logger.Warn("后台自动更新失败，继续运行当前版本: %v", describeStartupUpdateError(err))
		return
	}
	if !result.UpdateAvailable {
		logger.Info("后台自动更新确认当前已是最新版本: %s", result.CurrentVersion)
		return
	}

	logger.Info("后台已安装新版本: %s -> %s", result.CurrentVersion, result.LatestTag)
	logger.Info("旧版本备份文件: %s", result.BackupPath)
	restartArgs := restartArgsWithoutSelfUpdate(args)
	if result.NeedsExternalApply {
		if err := launchWindowsSelfUpdateFn(result, restartArgs); err != nil {
			logger.Warn("启动 Windows 延迟替换失败，继续运行当前版本: %v", err)
			return
		}
		logger.Info("已启动后台替换脚本，当前进程将自动退出以完成替换并重启。")
		notifyStartupSelfUpdateExit()
		return
	}
	if err := launchReplacementFn(result.ExecutablePath, restartArgs); err != nil {
		logger.Warn("新版已安装，但自动重启失败，继续运行当前版本: %v", err)
		return
	}
	logger.Info("已启动新版进程，当前进程即将退出。")
}

func notifyStartupSelfUpdateExit() {
	select {
	case startupSelfUpdateExit <- struct{}{}:
	default:
	}
}

func autoUpdateEnabled() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv("VS_AI_PROXY_AUTO_UPDATE")))
	return value != "0" && value != "false" && value != "no" && value != "off"
}

func isReleaseVersion(value string) bool {
	value = strings.TrimSpace(strings.TrimPrefix(value, "v"))
	if value == "" || value == "dev" || strings.Contains(value, "dirty") {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) < 3 {
		return false
	}
	for _, part := range parts[:3] {
		if part == "" {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

func describeStartupUpdateError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "访问 GitHub Release 超时，请检查 Windows 网络、代理、防火墙或 github.com/api.github.com 可达性；本次仅跳过自动更新，不影响代理服务启动"
	}
	message := err.Error()
	if strings.Contains(message, "context deadline exceeded") || strings.Contains(message, "Client.Timeout exceeded") {
		return "访问 GitHub Release 超时，请检查 Windows 网络、代理、防火墙或 github.com/api.github.com 可达性；本次仅跳过自动更新，不影响代理服务启动"
	}
	return message
}

func handleCommandLine(args []string, stdout, stderr io.Writer) (bool, int) {
	flags := flag.NewFlagSet("vs-ai-proxy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version and exit")
	checkUpdate := flags.Bool("check-update", false, "check GitHub Releases for a newer version")
	doUpdate := flags.Bool("update", false, "check for a newer version and download the matching release asset")
	doSelfUpdate := flags.Bool("self-update", false, "download, install, and restart into the latest release")
	updateDir := flags.String("update-dir", "", "directory for downloaded updates; default is <config-dir>/updates")
	if err := flags.Parse(args); err != nil {
		return true, 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "未知参数: %s\n", strings.Join(flags.Args(), " "))
		return true, 2
	}
	if *showVersion && !*checkUpdate && !*doUpdate && !*doSelfUpdate {
		fmt.Fprintln(stdout, version)
		return true, 0
	}
	if *checkUpdate || *doUpdate || *doSelfUpdate {
		dir := strings.TrimSpace(*updateDir)
		if dir == "" {
			dir = filepath.Join(config.DefaultConfigDir(), "updates")
		}
		opts := update.Options{CurrentVersion: version, TargetDir: dir}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if *doSelfUpdate {
			result, err := update.SelfUpdate(ctx, opts)
			if err != nil {
				fmt.Fprintf(stderr, "自更新失败: %v\n", err)
				return true, 1
			}
			if !result.UpdateAvailable {
				fmt.Fprintf(stdout, "当前已是最新版本: %s\n", result.CurrentVersion)
				return true, 0
			}
			fmt.Fprintf(stdout, "已安装新版本: %s\n", result.LatestTag)
			fmt.Fprintf(stdout, "备份文件: %s\n", result.BackupPath)
			restartArgs := restartArgsWithoutSelfUpdate(os.Args[1:])
			if result.NeedsExternalApply {
				if err := update.LaunchWindowsSelfUpdate(result, restartArgs); err != nil {
					fmt.Fprintf(stderr, "启动 Windows 延迟替换失败: %v\n", err)
					return true, 1
				}
				fmt.Fprintln(stdout, "已启动后台替换脚本，当前进程退出后会完成替换并重启。")
				return true, 0
			}
			if err := update.LaunchReplacement(result.ExecutablePath, restartArgs); err != nil {
				fmt.Fprintf(stderr, "新版已安装，但自动重启失败: %v\n", err)
				return true, 1
			}
			fmt.Fprintln(stdout, "已启动新版进程，当前进程即将退出。")
			return true, 0
		}
		if *doUpdate {
			result, err := update.Download(ctx, opts)
			if err != nil {
				fmt.Fprintf(stderr, "更新失败: %v\n", err)
				return true, 1
			}
			printDownloadResult(stdout, result)
			return true, 0
		}
		result, err := update.Check(ctx, opts)
		if err != nil {
			if isRecoverableUpdateCheckError(err) {
				fmt.Fprintf(stdout, "暂时无法检查更新: %v\n", err)
				return true, 0
			}
			fmt.Fprintf(stderr, "检查更新失败: %v\n", err)
			return true, 1
		}
		printCheckResult(stdout, result)
		return true, 0
	}
	return false, 0
}

func restartArgsWithoutSelfUpdate(args []string) []string {
	out := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--self-update" {
			continue
		}
		if strings.HasPrefix(arg, "--self-update=") {
			continue
		}
		if arg == "--version" || arg == "--check-update" || arg == "--update" {
			continue
		}
		if strings.HasPrefix(arg, "--version=") || strings.HasPrefix(arg, "--check-update=") || strings.HasPrefix(arg, "--update=") {
			continue
		}
		if arg == "--update-dir" && index+1 < len(args) {
			index++
			continue
		}
		if strings.HasPrefix(arg, "--update-dir=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func isRecoverableUpdateCheckError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "GITHUB_TOKEN") || strings.Contains(message, "rate limit")
}

func printCheckResult(w io.Writer, result update.CheckResult) {
	if !result.UpdateAvailable {
		fmt.Fprintf(w, "当前已是最新版本: %s\n", result.CurrentVersion)
		return
	}
	fmt.Fprintf(w, "发现新版本: %s -> %s\n", result.CurrentVersion, result.LatestTag)
	fmt.Fprintf(w, "Release: %s\n", result.ReleaseURL)
	fmt.Fprintf(w, "匹配资产: %s\n", result.AssetName)
	fmt.Fprintln(w, "运行 --update 可自动下载更新包。")
}

func printDownloadResult(w io.Writer, result update.DownloadResult) {
	if !result.UpdateAvailable {
		fmt.Fprintf(w, "当前已是最新版本: %s\n", result.CurrentVersion)
		return
	}
	fmt.Fprintf(w, "已下载新版本: %s\n", result.LatestTag)
	fmt.Fprintf(w, "更新包: %s\n", result.ArchivePath)
	fmt.Fprintf(w, "SHA256: %s\n", result.SHA256)
	fmt.Fprintf(w, "已解压二进制: %s\n", result.BinaryPath)
	fmt.Fprintln(w, "为避免破坏正在运行的进程，程序不会自动替换当前可执行文件。请停止服务后手动替换并重启。")
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

func listenWithStartupPortRecovery(addr string, logger *log.Logger) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		return listener, nil
	}
	if !isPortBindError(err) {
		return nil, err
	}

	port, portErr := portFromAddr(addr)
	if portErr != nil {
		return nil, err
	}
	pids, lookupErr := listeningPIDs(port)
	if lookupErr != nil {
		logger.Warn("端口 %d 监听失败，且无法查询占用进程: %v；原始错误: %v", port, lookupErr, err)
		return nil, err
	}
	if len(pids) == 0 {
		logger.Warn("端口 %d 监听失败，但未发现可清理的监听进程；可能是 Windows 端口保留/权限策略: %v", port, err)
		return nil, err
	}

	killed := []int{}
	for _, pid := range pids {
		if pid == os.Getpid() || !isSafeProxyProcess(pid) {
			logger.Warn("端口 %d 被 PID %d 占用，但进程名不是 vs-ai-proxy/server，跳过清理", port, pid)
			continue
		}
		if killErr := killProcess(pid); killErr != nil {
			logger.Warn("清理端口 %d 的旧代理进程 PID %d 失败: %v", port, pid, killErr)
			continue
		}
		killed = append(killed, pid)
	}
	if len(killed) == 0 {
		return nil, err
	}
	logger.Warn("端口 %d 被旧代理进程占用，已清理 PID: %v，正在重试监听", port, killed)
	time.Sleep(500 * time.Millisecond)
	return net.Listen("tcp", addr)
}

func isPortBindError(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && strings.EqualFold(opErr.Op, "listen") {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "address already in use") ||
		strings.Contains(lower, "only one usage of each socket address") ||
		strings.Contains(lower, "access a socket in a way forbidden") ||
		strings.Contains(lower, "permission denied")
}

func portFromAddr(addr string) (int, error) {
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("invalid port %q", portText)
	}
	return port, nil
}

func listeningPIDs(port int) ([]int, error) {
	if runtime.GOOS == "windows" {
		return listeningPIDsWindows(port)
	}
	return listeningPIDsUnix(port)
}

func listeningPIDsWindows(port int) ([]int, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", fmt.Sprintf(
		"Get-NetTCPConnection -LocalPort %d -State Listen -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess -Unique",
		port,
	))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parsePIDLines(string(out)), nil
}

func listeningPIDsUnix(port int) ([]int, error) {
	cmd := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) == 0 && len(out) == 0 {
			return nil, nil
		}
		return nil, err
	}
	return parsePIDLines(string(out)), nil
}

func parsePIDLines(out string) []int {
	seen := map[int]struct{}{}
	pids := []int{}
	for _, field := range strings.Fields(out) {
		pid, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || pid <= 0 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids
}

func isSafeProxyProcess(pid int) bool {
	name := strings.ToLower(processName(pid))
	if name == "" {
		return false
	}
	name = strings.TrimSuffix(name, ".exe")
	return name == "vs-ai-proxy" || name == "server"
}

func processName(pid int) string {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", fmt.Sprintf(
			"(Get-Process -Id %d -ErrorAction SilentlyContinue).ProcessName",
			pid,
		)).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}

func killProcess(pid int) error {
	if runtime.GOOS == "windows" {
		return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	for i := 0; i < 20; i++ {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return proc.Kill()
}

func processAlive(pid int) bool {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", fmt.Sprintf(
			"if (Get-Process -Id %d -ErrorAction SilentlyContinue) { '1' }",
			pid,
		)).Output()
		return err == nil && strings.TrimSpace(string(out)) == "1"
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
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
