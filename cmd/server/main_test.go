package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# comment\nPROXY_PORT=12345\nSTORE_PATH=\"/tmp/vs-ai-proxy/logs.json\"\nEMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("PROXY_PORT", "")
	t.Setenv("STORE_PATH", "")

	if err := loadEnvFile(path); err != nil {
		t.Fatalf("loadEnvFile() error = %v", err)
	}

	if got, want := os.Getenv("PROXY_PORT"), "12345"; got != want {
		t.Fatalf("PROXY_PORT = %q, want %q", got, want)
	}
	if got, want := os.Getenv("STORE_PATH"), "/tmp/vs-ai-proxy/logs.json"; got != want {
		t.Fatalf("STORE_PATH = %q, want %q", got, want)
	}
	if got := os.Getenv("EMPTY"); got != "" {
		t.Fatalf("EMPTY = %q, want empty string", got)
	}
}
