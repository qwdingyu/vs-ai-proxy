package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/proxy"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
	"github.com/dingyuwang/vs-ai-proxy/internal/update"
)

func TestHandleCommandLinePrintsVersion(t *testing.T) {
	oldVersion := version
	version = "0.2.13"
	t.Cleanup(func() { version = oldVersion })

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	handled, exitCode := handleCommandLine([]string{"--version"}, &stdout, &stderr)

	if !handled {
		t.Fatalf("handled = false, want true")
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0; stderr=%s", exitCode, stderr.String())
	}
	if stdout.String() != "0.2.13\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestResolveBuildVersionUsesInjectedVersion(t *testing.T) {
	if got := resolveBuildVersion("v0.2.16"); got != "v0.2.16" {
		t.Fatalf("resolveBuildVersion() = %q, want v0.2.16", got)
	}
}

func TestResolveBuildVersionUsesEnvFallback(t *testing.T) {
	t.Setenv("VS_AI_PROXY_VERSION", "v9.9.9-env")
	if got := resolveBuildVersion("dev"); got != "v9.9.9-env" {
		t.Fatalf("resolveBuildVersion() = %q, want v9.9.9-env", got)
	}
}

func TestHandleCommandLineRejectsUnknownArgument(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	handled, exitCode := handleCommandLine([]string{"--unknown"}, &stdout, &stderr)

	if !handled {
		t.Fatalf("handled = false, want true")
	}
	if exitCode != 2 {
		t.Fatalf("exitCode = %d, want 2", exitCode)
	}
}

func TestRecoverableUpdateCheckErrorDoesNotFailCommand(t *testing.T) {
	if !isRecoverableUpdateCheckError(errors.New("GitHub API 匿名访问已触发限流；请设置 GITHUB_TOKEN")) {
		t.Fatalf("rate limit with token hint should be recoverable")
	}
	if isRecoverableUpdateCheckError(errors.New("network unreachable")) {
		t.Fatalf("unrelated errors should not be recoverable")
	}
}

func TestRestartArgsWithoutSelfUpdate(t *testing.T) {
	args := restartArgsWithoutSelfUpdate([]string{"--self-update", "--version", "--check-update", "--update", "--update-dir", "/tmp/update", "--config", "x", "--update-dir=/tmp/other"})
	want := []string{"--config", "x"}
	if len(args) != len(want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
	}
}

func TestAutoSelfUpdateOnStartupLaunchesWindowsApply(t *testing.T) {
	oldVersion := version
	oldCheckUpdate := checkUpdateFn
	oldSelfUpdate := selfUpdateFn
	oldLaunchWindows := launchWindowsSelfUpdateFn
	oldStartupSelfUpdateExit := startupSelfUpdateExit
	t.Cleanup(func() {
		version = oldVersion
		checkUpdateFn = oldCheckUpdate
		selfUpdateFn = oldSelfUpdate
		launchWindowsSelfUpdateFn = oldLaunchWindows
		startupSelfUpdateExit = oldStartupSelfUpdateExit
	})

	version = "v0.2.14"
	startupSelfUpdateExit = make(chan struct{}, 1)
	checkUpdateFn = func(_ context.Context, opts update.Options) (update.CheckResult, error) {
		if opts.CurrentVersion != "v0.2.14" {
			t.Fatalf("CurrentVersion = %q, want v0.2.14", opts.CurrentVersion)
		}
		return update.CheckResult{CurrentVersion: "v0.2.14", LatestTag: "v0.2.15", UpdateAvailable: true}, nil
	}
	selfUpdateFn = func(_ context.Context, opts update.Options) (update.SelfUpdateResult, error) {
		if opts.CurrentVersion != "v0.2.14" {
			t.Fatalf("CurrentVersion = %q, want v0.2.14", opts.CurrentVersion)
		}
		return update.SelfUpdateResult{
			DownloadResult: update.DownloadResult{CheckResult: update.CheckResult{
				CurrentVersion:  "v0.2.14",
				LatestTag:       "v0.2.15",
				UpdateAvailable: true,
			}},
			ExecutablePath:     `C:\\apps\\vs-ai-proxy.exe`,
			BackupPath:         `C:\\apps\\vs-ai-proxy.exe.bak`,
			NeedsExternalApply: true,
		}, nil
	}
	launched := make(chan struct{}, 1)
	launchWindowsSelfUpdateFn = func(result update.SelfUpdateResult, args []string) error {
		if result.LatestTag != "v0.2.15" {
			t.Fatalf("LatestTag = %q, want v0.2.15", result.LatestTag)
		}
		want := []string{"--config", "prod.toml"}
		if len(args) != len(want) || args[0] != want[0] || args[1] != want[1] {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
		launched <- struct{}{}
		return nil
	}

	var logs bytes.Buffer
	logger := log.New(&logs, log.LevelInfo, false)
	if autoSelfUpdateOnStartup(logger, []string{"--self-update", "--config", "prod.toml"}) {
		t.Fatalf("autoSelfUpdateOnStartup() = true, want false because startup must not block")
	}
	select {
	case <-launched:
	case <-time.After(time.Second):
		t.Fatalf("LaunchWindowsSelfUpdate was not called")
	}
	select {
	case <-startupSelfUpdateExit:
	case <-time.After(time.Second):
		t.Fatalf("startup self-update should notify main process to exit")
	}
}

func TestAutoSelfUpdateOnStartupContinuesWhenCheckFails(t *testing.T) {
	oldVersion := version
	oldCheckUpdate := checkUpdateFn
	t.Cleanup(func() {
		version = oldVersion
		checkUpdateFn = oldCheckUpdate
	})

	version = "0.2.14"
	checkUpdateFn = func(context.Context, update.Options) (update.CheckResult, error) {
		return update.CheckResult{}, errors.New("network unavailable")
	}

	var logs bytes.Buffer
	logger := log.New(&logs, log.LevelInfo, false)
	if autoSelfUpdateOnStartup(logger, nil) {
		t.Fatalf("autoSelfUpdateOnStartup() = true, want false")
	}
	if !strings.Contains(logs.String(), "继续启动当前版本") {
		t.Fatalf("logs = %q, want continue message", logs.String())
	}
}

func TestAutoSelfUpdateOnStartupSkipsDevVersion(t *testing.T) {
	oldVersion := version
	oldCheckUpdate := checkUpdateFn
	t.Cleanup(func() {
		version = oldVersion
		checkUpdateFn = oldCheckUpdate
	})

	version = "dev"
	checkUpdateFn = func(context.Context, update.Options) (update.CheckResult, error) {
		t.Fatalf("checkUpdateFn should not be called for dev version")
		return update.CheckResult{}, nil
	}

	logger := log.New(io.Discard, log.LevelInfo, false)
	if autoSelfUpdateOnStartup(logger, nil) {
		t.Fatalf("autoSelfUpdateOnStartup() = true, want false")
	}
}

func TestDescribeStartupUpdateErrorExplainsDeadline(t *testing.T) {
	message := describeStartupUpdateError(context.DeadlineExceeded)
	if !strings.Contains(message, "访问 GitHub Release 超时") || !strings.Contains(message, "不影响代理服务启动") {
		t.Fatalf("message = %q, want friendly timeout guidance", message)
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# comment\nPORT=12345\nSTORE_PATH=\"/tmp/vs-ai-proxy/logs.json\"\nEMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("PORT", "")
	t.Setenv("STORE_PATH", "")

	if err := loadEnvFile(path); err != nil {
		t.Fatalf("loadEnvFile() error = %v", err)
	}

	if got, want := os.Getenv("PORT"), "12345"; got != want {
		t.Fatalf("PORT = %q, want %q", got, want)
	}
	if got, want := os.Getenv("STORE_PATH"), "/tmp/vs-ai-proxy/logs.json"; got != want {
		t.Fatalf("STORE_PATH = %q, want %q", got, want)
	}
	if got := os.Getenv("EMPTY"); got != "" {
		t.Fatalf("EMPTY = %q, want empty string", got)
	}
}

func TestUnifiedHandlerRoutesAdminPrefixToManagementHandler(t *testing.T) {
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Handler", "admin")
	})
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Handler", "proxy")
	})
	handler := newUnifiedHandler(admin, proxy)

	tests := []struct {
		path string
		want string
	}{
		{path: "/admin", want: "admin"},
		{path: "/admin/", want: "admin"},
		{path: "/admin/api/config", want: "admin"},
		{path: "/api/chat", want: "proxy"},
		{path: "/api/tags", want: "proxy"},
		{path: "/v1/models", want: "proxy"},
	}

	for _, tt := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)

		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("X-Handler"); got != tt.want {
			t.Fatalf("%s routed to %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestResolveAppAddrDefaultsToLoopback(t *testing.T) {
	t.Setenv("HOST", "")

	if got, want := resolveAppAddr(12345), "127.0.0.1:12345"; got != want {
		t.Fatalf("resolveAppAddr() = %q, want %q", got, want)
	}
}

func TestResolveAppAddrUsesHostOverride(t *testing.T) {
	t.Setenv("HOST", "0.0.0.0")

	if got, want := resolveAppAddr(12345), "0.0.0.0:12345"; got != want {
		t.Fatalf("resolveAppAddr() = %q, want %q", got, want)
	}
}

func TestDisplayAddrKeepsWildcardBindVisible(t *testing.T) {
	if got, want := displayAddr("0.0.0.0:12345"), "0.0.0.0:12345"; got != want {
		t.Fatalf("displayAddr() = %q, want %q", got, want)
	}
}

func TestWatchConfigLoopReloadsProxyConfigFromDisk(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("PROXY_PORT", "")

	path := filepath.Join(t.TempDir(), "config.json")
	initial := config.DefaultConfig()
	initial.Port = 12345
	initial.DefaultModel = "before-hot-reload"
	initial.Providers = []config.ProviderConfig{config.DefaultUseAIProvider()}
	writeConfigFile(t, path, initial)

	configMgr, err := config.NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	proxySrv := proxy.NewServer(configMgr.Get(), configMgr, store.New(10), log.New(nil, log.LevelError, false))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchConfigLoop(ctx, configMgr, proxySrv, log.New(nil, log.LevelError, false))

	// 确保新写入的配置文件 mtime 晚于 watchConfigLoop 启动时记录的 mtime。
	time.Sleep(25 * time.Millisecond)
	next := config.DefaultConfig()
	next.Port = 12345
	next.DefaultModel = "after-hot-reload"
	next.Providers = []config.ProviderConfig{config.DefaultUseAIProvider()}
	writeConfigFile(t, path, next)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cfg, _, _ := proxySrv.SnapshotComponents()
		if cfg.DefaultModel == "after-hot-reload" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	cfg, _, _ := proxySrv.SnapshotComponents()
	t.Fatalf("proxy default model = %q, want after-hot-reload", cfg.DefaultModel)
}

func TestWatchConfigLoopReloadsWhenContentChangesButMTimeDoesNot(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("PROXY_PORT", "")

	path := filepath.Join(t.TempDir(), "config.json")
	initial := config.DefaultConfig()
	initial.Port = 12345
	initial.DefaultModel = "before-same-mtime"
	initial.Providers = []config.ProviderConfig{config.DefaultUseAIProvider()}
	writeConfigFile(t, path, initial)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	originalMod := info.ModTime()

	configMgr, err := config.NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	proxySrv := proxy.NewServer(configMgr.Get(), configMgr, store.New(10), log.New(nil, log.LevelError, false))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchConfigLoop(ctx, configMgr, proxySrv, log.New(nil, log.LevelError, false))

	time.Sleep(50 * time.Millisecond)
	next := config.DefaultConfig()
	next.Port = 12345
	next.DefaultModel = "after-same-mtime"
	next.Providers = []config.ProviderConfig{config.DefaultUseAIProvider()}
	writeConfigFile(t, path, next)
	if err := os.Chtimes(path, originalMod, originalMod); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	waitForProxyDefaultModel(t, proxySrv, "after-same-mtime")
}

func TestWatchConfigLoopRetriesAfterInvalidConfigIsFixed(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("PROXY_PORT", "")

	path := filepath.Join(t.TempDir(), "config.json")
	initial := config.DefaultConfig()
	initial.Port = 12345
	initial.DefaultModel = "before-invalid"
	initial.Providers = []config.ProviderConfig{config.DefaultUseAIProvider()}
	writeConfigFile(t, path, initial)

	configMgr, err := config.NewManager(path)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	proxySrv := proxy.NewServer(configMgr.Get(), configMgr, store.New(10), log.New(nil, log.LevelError, false))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchConfigLoop(ctx, configMgr, proxySrv, log.New(nil, log.LevelError, false))

	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"default_model":`), 0o644); err != nil {
		t.Fatalf("WriteFile(invalid) error = %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	cfg, _, _ := proxySrv.SnapshotComponents()
	if cfg.DefaultModel != "before-invalid" {
		t.Fatalf("proxy default model after invalid config = %q, want before-invalid", cfg.DefaultModel)
	}

	next := config.DefaultConfig()
	next.Port = 12345
	next.DefaultModel = "after-invalid-fixed"
	next.Providers = []config.ProviderConfig{config.DefaultUseAIProvider()}
	writeConfigFile(t, path, next)

	waitForProxyDefaultModel(t, proxySrv, "after-invalid-fixed")
}

func waitForProxyDefaultModel(t *testing.T, proxySrv *proxy.Server, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cfg, _, _ := proxySrv.SnapshotComponents()
		if cfg.DefaultModel == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	cfg, _, _ := proxySrv.SnapshotComponents()
	t.Fatalf("proxy default model = %q, want %q", cfg.DefaultModel, want)
}

func writeConfigFile(t *testing.T, path string, cfg *config.AppConfig) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
