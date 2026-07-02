package store

import (
	"path/filepath"
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

	if got, want := logs[0].Path, "/health"; got != want {
		t.Fatalf("first log path = %q, want %q", got, want)
	}
	if got, want := logs[1].Path, "/v1/chat/completions"; got != want {
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
