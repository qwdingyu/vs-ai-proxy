package benchmark

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/log"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

type Service struct {
	registry    *provider.Registry
	catalog     *provider.ModelCatalog
	logger      *log.Logger
	interval    time.Duration
	outputDir   string
	initialWait time.Duration
}

func New(registry *provider.Registry, catalog *provider.ModelCatalog, logger *log.Logger) *Service {
	intervalMinutes := envInt("BENCHMARK_INTERVAL_MINUTES", 0)
	interval := time.Duration(intervalMinutes) * time.Minute
	outputDir := os.Getenv("BENCHMARK_OUTPUT_DIR")
	if outputDir == "" {
		outputDir = filepath.Join("docs", "testing", "logs")
	}
	return &Service{
		registry:    registry,
		catalog:     catalog,
		logger:      logger,
		interval:    interval,
		outputDir:   outputDir,
		initialWait: 30 * time.Second,
	}
}

func (s *Service) Enabled() bool {
	return s != nil && s.interval > 0
}

func (s *Service) Run(ctx context.Context) {
	if !s.Enabled() {
		if s.logger != nil {
			s.logger.Info("ProviderBenchmarkService disabled (set BENCHMARK_INTERVAL_MINUTES > 0 to enable).")
		}
		return
	}
	if s.logger != nil {
		s.logger.Info("ProviderBenchmarkService enabled (interval: %s, output: %s).", s.interval, s.outputDir)
	}

	timer := time.NewTimer(s.initialWait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	for {
		if err := s.RunOnce(ctx); err != nil && s.logger != nil {
			s.logger.Warn("Benchmark cycle failed: %v", err)
		}

		timer := time.NewTimer(s.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) RunOnce(ctx context.Context) error {
	if s == nil || s.registry == nil || s.catalog == nil {
		return nil
	}

	models := uniqueModels(s.catalog.AllEntries())
	results := make([]BenchmarkResult, 0, len(models))
	for _, model := range models {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		candidates := s.registry.ResolveCandidates(model)
		if len(candidates) == 0 || candidates[0].Provider == nil {
			continue
		}

		candidate := candidates[0]
		result := s.probeModel(ctx, model, candidate.Provider.Provider, candidate.UpstreamID)
		results = append(results, result)
	}

	return s.writeReport(ctx, results)
}

type BenchmarkResult struct {
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstreamModel"`
	Success       bool   `json:"success"`
	StatusCode    int    `json:"statusCode"`
	LatencyMs     int64  `json:"latencyMs"`
	Error         string `json:"error,omitempty"`
}

func (s *Service) probeModel(ctx context.Context, model string, prov provider.Provider, upstreamModel string) BenchmarkResult {
	start := time.Now()
	req := &provider.ChatRequest{
		Model:    upstreamModel,
		Stream:   false,
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
	}

	resp, err := prov.Chat(ctx, req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return BenchmarkResult{
			Model:         model,
			Provider:      prov.Name(),
			UpstreamModel: upstreamModel,
			Success:       false,
			StatusCode:    0,
			LatencyMs:     latency,
			Error:         err.Error(),
		}
	}

	statusCode := 200
	if resp != nil && resp.Error != nil {
		statusCode = 500
	}
	return BenchmarkResult{
		Model:         model,
		Provider:      prov.Name(),
		UpstreamModel: upstreamModel,
		Success:       true,
		StatusCode:    statusCode,
		LatencyMs:     latency,
	}
}

func (s *Service) writeReport(ctx context.Context, results []BenchmarkResult) error {
	if len(results) == 0 {
		return nil
	}
	if err := os.MkdirAll(s.outputDir, 0o755); err != nil {
		return err
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Success != results[j].Success {
			return results[i].Success
		}
		return results[i].LatencyMs < results[j].LatencyMs
	})

	report := map[string]any{
		"timestampUtc": time.Now().UTC().Format(time.RFC3339),
		"totalModels":  len(results),
		"succeeded":    countSucceeded(results),
		"failed":       countFailed(results),
		"results":      results,
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	filename := filepath.Join(s.outputDir, "provider-benchmark-"+time.Now().UTC().Format("20060102-150405")+".json")
	return os.WriteFile(filename, data, 0o644)
}

func countSucceeded(results []BenchmarkResult) int {
	count := 0
	for _, r := range results {
		if r.Success {
			count++
		}
	}
	return count
}

func countFailed(results []BenchmarkResult) int {
	return len(results) - countSucceeded(results)
}

func uniqueModels(entries []provider.CatalogEntry) []string {
	seen := make(map[string]struct{}, len(entries))
	models := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.Enabled {
			continue
		}
		model := entry.Model
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return models
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}
