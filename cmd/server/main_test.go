package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
