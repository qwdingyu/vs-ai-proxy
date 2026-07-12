package store

import (
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
