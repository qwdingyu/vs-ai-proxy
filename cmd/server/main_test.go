package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/proxy"
	"github.com/dingyuwang/vs-ai-proxy/internal/store"
)

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
