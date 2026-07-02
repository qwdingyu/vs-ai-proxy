package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

func TestRunOnceWritesBenchmarkReport(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeBenchmarkProvider{
		name:   "openai",
		models: []string{"gpt-test"},
	}
	registry := provider.NewRegistry("gpt-test", time.Minute)
	registry.Add(&provider.ProviderEntry{Provider: prov, Models: prov.models, Priority: 1})
	catalog := provider.NewModelCatalog(registry, "", time.Minute)

	svc := &Service{
		registry:  registry,
		catalog:   catalog,
		logger:    log.New(nil, log.LevelError, false),
		outputDir: dir,
	}

	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("expected benchmark report file")
	}

	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if got := int(report["totalModels"].(float64)); got == 0 {
		t.Fatalf("totalModels = 0, want > 0")
	}
	if prov.chatCalls == 0 {
		t.Fatalf("expected benchmark to probe provider chat")
	}
}

func TestRunOncePropagatesProviderChatErrorInReport(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeBenchmarkProvider{
		name:    "openai",
		models:  []string{"gpt-test"},
		chatErr: errors.New("boom"),
	}
	registry := provider.NewRegistry("gpt-test", time.Minute)
	registry.Add(&provider.ProviderEntry{Provider: prov, Models: prov.models, Priority: 1})
	catalog := provider.NewModelCatalog(registry, "", time.Minute)

	svc := &Service{
		registry:  registry,
		catalog:   catalog,
		logger:    log.New(nil, log.LevelError, false),
		outputDir: dir,
	}

	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	if prov.chatCalls == 0 {
		t.Fatalf("expected chat to be attempted")
	}
}

type fakeBenchmarkProvider struct {
	name      string
	models    []string
	chatErr   error
	chatCalls int
	lastReq   *provider.ChatRequest
}

func (p *fakeBenchmarkProvider) Name() string { return p.name }

func (p *fakeBenchmarkProvider) IsEnabled() bool { return true }

func (p *fakeBenchmarkProvider) ListModels(context.Context) ([]string, error) {
	return append([]string(nil), p.models...), nil
}

func (p *fakeBenchmarkProvider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	p.chatCalls++
	p.lastReq = req
	if p.chatErr != nil {
		return nil, p.chatErr
	}
	return &provider.ChatResponse{
		Model: p.name,
		Choices: []provider.Choice{{
			Message: provider.Message{Role: "assistant", Content: "pong"},
		}},
	}, nil
}

func (p *fakeBenchmarkProvider) ChatStream(context.Context, *provider.ChatRequest) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
