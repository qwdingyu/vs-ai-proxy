package store

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPersistAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.json")

	s := New(10)
	s.AddLog(RequestLog{Method: "GET", Path: "/health", StatusCode: 200, ElapsedMs: 12.5, IsSuccess: true})
	s.AddLog(RequestLog{Method: "POST", Path: "/v1/chat/completions", StatusCode: 502, ElapsedMs: 33.1, IsSuccess: false})

	if err := s.PersistToFile(path); err != nil {
		t.Fatalf("PersistToFile() error = %v", err)
	}

	loaded := New(10)
	if err := loaded.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}

	logs := loaded.GetLogs(10)
	if got, want := len(logs), 2; got != want {
		t.Fatalf("GetLogs() len = %d, want %d", got, want)
	}

	// GetLogs 现在返回最新在前（倒序）
	if got, want := logs[0].Path, "/v1/chat/completions"; got != want {
		t.Fatalf("first log (newest) path = %q, want %q", got, want)
	}
	if got, want := logs[1].Path, "/health"; got != want {
		t.Fatalf("second log path = %q, want %q", got, want)
	}

	stats := loaded.GetStatistics()
	if got, want := stats.TotalRequests, int64(2); got != want {
		t.Fatalf("TotalRequests = %d, want %d", got, want)
	}
	if got, want := stats.SuccessCount, int64(1); got != want {
		t.Fatalf("SuccessCount = %d, want %d", got, want)
	}
	if got, want := stats.FailureCount, int64(1); got != want {
		t.Fatalf("FailureCount = %d, want %d", got, want)
	}
}

func TestPersistToFileWritesCompactJSONSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.json")

	s := New(10)
	s.AddLog(RequestLog{Method: "POST", Path: "/v1/chat/completions", StatusCode: 502, ElapsedMs: 33.1, IsSuccess: false, ErrorReason: "上游服务异常"})

	if err := s.PersistToFile(path); err != nil {
		t.Fatalf("PersistToFile() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	if bytes.Contains(data, []byte("\n  ")) {
		t.Fatalf("logs.json should be compact JSON, got %q", string(data))
	}

	loaded := New(10)
	if err := loaded.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	logs := loaded.GetLogs(1)
	if len(logs) != 1 || logs[0].ErrorReason != "上游服务异常" {
		t.Fatalf("loaded logs = %#v, want persisted diagnostic log", logs)
	}
}

func TestPersistToFileRespectsMaxLogsSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.json")

	s := New(3)
	for i := 0; i < 5; i++ {
		s.AddLog(RequestLog{Method: "POST", Path: "/v1/chat/completions", Model: string(rune('a' + i)), StatusCode: 200, IsSuccess: true})
	}

	if err := s.PersistToFile(path); err != nil {
		t.Fatalf("PersistToFile() error = %v", err)
	}

	loaded := New(10)
	if err := loaded.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	logs := loaded.GetLogs(10)
	if got, want := len(logs), 3; got != want {
		t.Fatalf("loaded logs len = %d, want %d", got, want)
	}
	if logs[0].Model != "e" || logs[1].Model != "d" || logs[2].Model != "c" {
		t.Fatalf("loaded logs = %#v, want newest retained models e,d,c", logs)
	}
}

func TestPersistToFileDoesNotHoldLogLockDuringDiskWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.json")
	s := New(1000)
	for i := 0; i < 100; i++ {
		s.AddLog(RequestLog{Method: "POST", Path: "/v1/chat/completions", StatusCode: 200, IsSuccess: true})
	}

	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.AddLog(RequestLog{Method: "GET", Path: "/health", StatusCode: 200, IsSuccess: true})
			}
		}()
	}
	for i := 0; i < 10; i++ {
		if err := s.PersistToFile(path); err != nil {
			t.Fatalf("PersistToFile() error = %v", err)
		}
	}
	wg.Wait()
}

func TestGetLogsPageFilteredFiltersByProviderStatusAndSearch(t *testing.T) {
	s := New(10)
	s.AddLog(RequestLog{Method: "GET", Path: "/health", Provider: "useai", Model: "gpt-5.5", StatusCode: 200, IsSuccess: true, RequestID: "req-1", ErrorReason: "ok"})
	s.AddLog(RequestLog{Method: "POST", Path: "/v1/chat/completions", Provider: "deepseek", Model: "deepseek-v4-flash", StatusCode: 502, IsSuccess: false, ErrorCode: "upstream_server_error", ErrorReason: "上游服务异常", DiagnosticSummary: "上游返回 5xx"})

	result := s.GetLogsPageFiltered(1, 10, LogFilters{Provider: "deepseek", StatusCode: 502, Search: "5xx"})
	if got, want := len(result.Logs), 1; got != want {
		t.Fatalf("len = %d, want %d", got, want)
	}
	if got := result.Logs[0].Provider; got != "deepseek" {
		t.Fatalf("provider = %q, want deepseek", got)
	}
	if got := result.Total; got != 1 {
		t.Fatalf("total = %d, want 1", got)
	}
}

func TestGetLogsPageFilteredCoversDiagnosticFields(t *testing.T) {
	s := New(10)
	s.AddLog(RequestLog{
		Method:            "POST",
		Path:              "/v1/chat/completions",
		Provider:          "useai",
		Model:             "UseAI - step-router-v1",
		Upstream:          "step-router-v1",
		StatusCode:        502,
		IsSuccess:         false,
		ErrorCode:         "network_error",
		ErrorReason:       "网络/CDN/连接异常",
		ErrorAction:       "检查 Cloudflare/WAF",
		DiagnosticSummary: "连接被关闭；请求体 619.2 KB",
		AttemptsSummary:   "useai/step-router-v1 22s network_error",
		RequestID:         "req-network-1",
		NetworkPeer:       "104.21.57.81:443",
		StreamState:       "upstream_connecting",
		RequestTools:      "declared: create_file,get_file",
	})
	s.AddLog(RequestLog{Method: "POST", Path: "/v1/chat/completions", Provider: "deepseek", Model: "deepseek-v4-flash", StatusCode: 200, IsSuccess: true, RequestID: "req-ok"})

	filters := LogFilters{
		Provider:    "useai",
		Model:       "step-router",
		StatusCode:  502,
		ErrorCode:   "network",
		RequestID:   "network-1",
		ErrorReason: "连接异常",
		Search:      "create_file",
	}
	result := s.GetLogsPageFiltered(1, 10, filters)

	if got, want := len(result.Logs), 1; got != want {
		t.Fatalf("len = %d, want %d", got, want)
	}
	if got := result.Logs[0].RequestID; got != "req-network-1" {
		t.Fatalf("RequestID = %q, want req-network-1", got)
	}
}

func TestGetLogsPageFilteredRejectsMismatchedDiagnosticReason(t *testing.T) {
	s := New(10)
	s.AddLog(RequestLog{Provider: "useai", StatusCode: 502, ErrorReason: "网络/CDN/连接异常", IsSuccess: false})

	result := s.GetLogsPageFiltered(1, 10, LogFilters{ErrorReason: "上游服务异常"})

	if got := len(result.Logs); got != 0 {
		t.Fatalf("len = %d, want 0", got)
	}
}

func TestGetLogsPageFilteredKeepsNewestFirstOrder(t *testing.T) {
	s := New(10)
	s.AddLog(RequestLog{Method: "GET", Path: "/first", StatusCode: 200, IsSuccess: true})
	s.AddLog(RequestLog{Method: "GET", Path: "/second", StatusCode: 200, IsSuccess: true})
	s.AddLog(RequestLog{Method: "GET", Path: "/third", StatusCode: 200, IsSuccess: true})

	page1 := s.GetLogsPageFiltered(1, 2, LogFilters{})
	if got, want := len(page1.Logs), 2; got != want {
		t.Fatalf("page1 len = %d, want %d", got, want)
	}
	if page1.Logs[0].Path != "/third" || page1.Logs[1].Path != "/second" {
		t.Fatalf("page1 order = %#v, want newest first", page1.Logs)
	}

	page2 := s.GetLogsPageFiltered(2, 2, LogFilters{})
	if got, want := len(page2.Logs), 1; got != want {
		t.Fatalf("page2 len = %d, want %d", got, want)
	}
	if page2.Logs[0].Path != "/first" {
		t.Fatalf("page2 order = %#v, want oldest remaining", page2.Logs)
	}
}

func TestGetLatestFailureScansAllRetainedLogs(t *testing.T) {
	s := New(100)
	s.AddLog(RequestLog{Method: "POST", Path: "/first-failure", StatusCode: 502, IsSuccess: false, RequestID: "failure-1"})
	for i := 0; i < 60; i++ {
		s.AddLog(RequestLog{Method: "GET", Path: "/health", StatusCode: 200, IsSuccess: true})
	}

	log, ok := s.GetLatestFailure()
	if !ok {
		t.Fatalf("GetLatestFailure() ok = false, want true")
	}
	if got, want := log.RequestID, "failure-1"; got != want {
		t.Fatalf("RequestID = %q, want %q", got, want)
	}
}

func TestGetLatestFailureReturnsNewestFailure(t *testing.T) {
	s := New(100)
	s.AddLog(RequestLog{Method: "POST", Path: "/older", StatusCode: 502, IsSuccess: false, RequestID: "older"})
	s.AddLog(RequestLog{Method: "GET", Path: "/health", StatusCode: 200, IsSuccess: true})
	s.AddLog(RequestLog{Method: "POST", Path: "/newer", StatusCode: 499, IsSuccess: false, RequestID: "newer"})

	log, ok := s.GetLatestFailure()
	if !ok {
		t.Fatalf("GetLatestFailure() ok = false, want true")
	}
	if got, want := log.RequestID, "newer"; got != want {
		t.Fatalf("RequestID = %q, want %q", got, want)
	}
}

func TestStoreConcurrentAddLogAndStatistics(t *testing.T) {
	s := New(5000)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.AddLog(RequestLog{Method: "POST", Path: "/v1/chat/completions", Provider: "useai", StatusCode: 200, ElapsedMs: float64(worker + i), IsSuccess: true})
				_ = s.GetStatistics()
			}
		}(worker)
	}
	wg.Wait()

	stats := s.GetStatistics()
	if got, want := stats.TotalRequests, int64(1600); got != want {
		t.Fatalf("TotalRequests = %d, want %d", got, want)
	}
	if got, want := stats.SuccessCount, int64(1600); got != want {
		t.Fatalf("SuccessCount = %d, want %d", got, want)
	}
}
