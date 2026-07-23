package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
		ToolOutcome:       "truncated_no_tools",
	})
	s.AddLog(RequestLog{Method: "POST", Path: "/v1/chat/completions", Provider: "deepseek", Model: "deepseek-v4-flash", StatusCode: 200, IsSuccess: true, RequestID: "req-ok"})

	filters := LogFilters{
		Provider:    "useai",
		Model:       "step-router",
		StatusCode:  502,
		ErrorCode:   "network",
		RequestID:   "network-1",
		ErrorReason: "连接异常",
		Search:      "truncated_no_tools",
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

func TestGetRecentStabilitySummaryGroupsByProviderModelAndUpstream(t *testing.T) {
	s := New(100)
	base := time.Date(2026, 7, 23, 10, 0, 0, 0, time.Local)
	logs := []RequestLog{
		{Timestamp: base, Provider: "useai", Model: "gpt-5.5", Upstream: "gpt-5.5", StatusCode: 200, ElapsedMs: 10, RequestBytes: 100, IsSuccess: true},
		{Timestamp: base.Add(time.Minute), Provider: "useai", Model: "gpt-5.5", Upstream: "gpt-5.5", StatusCode: 502, ErrorCode: "upstream_stream_interrupted", ErrorReason: "流中断", StreamState: "upstream_connected", CancelReason: "client_gone", ElapsedMs: 20, RequestBytes: 200, IsSuccess: false},
		{Timestamp: base.Add(2 * time.Minute), Provider: "useai", Model: "gpt-5.5", Upstream: "gpt-5.5", StatusCode: 200, ElapsedMs: 30, RequestBytes: 300, IsSuccess: true},
		{Timestamp: base.Add(3 * time.Minute), Provider: "useai", Model: "gpt-5.5", Upstream: "gpt-5.5", StatusCode: 200, ElapsedMs: 40, RequestBytes: 400, IsSuccess: true},
		{Timestamp: base.Add(4 * time.Minute), Provider: "deepseek", Model: "deepseek-v4-flash", Upstream: "deepseek-v4-flash", StatusCode: 502, ErrorCode: "client_gone", ErrorReason: "客户端断开", StreamState: "waiting_response_headers", CancelReason: "client_deadline_reached", ElapsedMs: 15, RequestBytes: 150, IsSuccess: false},
		{Timestamp: base.Add(5 * time.Minute), Provider: "deepseek", Model: "deepseek-v4-flash", Upstream: "deepseek-v4-flash", StatusCode: 200, ElapsedMs: 25, RequestBytes: 250, IsSuccess: true},
		{Timestamp: base.Add(6 * time.Minute), Provider: "deepseek", Model: "deepseek-v4-flash", Upstream: "deepseek-v4-flash", StatusCode: 502, ErrorCode: "upstream_stream_interrupted", ErrorReason: "上游流中断", StreamState: "upstream_connected", CancelReason: "client_gone", ElapsedMs: 35, RequestBytes: 350, IsSuccess: false},
		{Method: "GET", Path: "/health", StatusCode: 200, IsSuccess: true},
	}
	for _, log := range logs {
		s.AddLog(log)
	}

	summaries := s.GetRecentStabilitySummary(100)
	if got, want := len(summaries), 2; got != want {
		t.Fatalf("len(summaries) = %d, want %d: %#v", got, want, summaries)
	}
	if summaries[0].Provider != "deepseek" || summaries[0].Failures != 2 || summaries[0].Runs != 3 {
		t.Fatalf("first summary = %#v, want deepseek with 2 failures", summaries[0])
	}
	if summaries[0].SuccessRate != 1.0/3.0 {
		t.Fatalf("first success rate = %v, want 1/3", summaries[0].SuccessRate)
	}
	if summaries[0].LatestFailure == nil || summaries[0].LatestFailure.ErrorCode != "upstream_stream_interrupted" || summaries[0].LatestFailure.CancelReason != "client_gone" {
		t.Fatalf("latest failure = %#v, want newest deepseek failure sample", summaries[0].LatestFailure)
	}
	if got := summaries[0].TopErrorCodes; len(got) != 2 || got[0].Key != "client_gone" || got[0].Count != 1 || got[1].Key != "upstream_stream_interrupted" || got[1].Count != 1 {
		t.Fatalf("top error codes = %#v, want two equally frequent codes sorted by key", got)
	}
	if got := summaries[0].TopStreamStates; len(got) != 2 || got[0].Count != 1 || got[1].Count != 1 {
		t.Fatalf("top stream states = %#v, want two tracked states", got)
	}
	if got := summaries[0].TopCancelReasons; len(got) != 2 || got[0].Key != "client_deadline_reached" || got[1].Key != "client_gone" {
		t.Fatalf("top cancel reasons = %#v, want both tracked reasons", got)
	}
	if summaries[1].Provider != "useai" || summaries[1].Failures != 1 || summaries[1].Runs != 4 {
		t.Fatalf("second summary = %#v, want useai with 1 failure", summaries[1])
	}
	if summaries[1].RequestBytesP50 != 200 || summaries[1].RequestBytesP95 != 400 || summaries[1].ElapsedMsP50 != 20 || summaries[1].ElapsedMsP95 != 40 {
		t.Fatalf("percentiles = %#v, want request 200/400 and elapsed 20/40", summaries[1])
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

func TestTokenStatisticsDistinguishUnknownAndReportedZero(t *testing.T) {
	s := New(10)
	s.AddLog(RequestLog{Provider: "kimi", Model: "kimi-for-coding", Upstream: "kimi-for-coding", StatusCode: 200, IsSuccess: true})
	s.AddLog(RequestLog{
		Provider: "kimi", Model: "kimi-for-coding", Upstream: "kimi-for-coding", StatusCode: 200, IsSuccess: true,
		Usage: &TokenUsage{},
	})

	stats := s.GetStatistics()
	if stats.TokenUsageRequests != 2 || stats.UsageReportedCount != 1 {
		t.Fatalf("usage coverage = %d/%d, want 1/2", stats.UsageReportedCount, stats.TokenUsageRequests)
	}
	if len(stats.ModelUsage) != 1 || stats.ModelUsage[0].RequestCount != 2 || stats.ModelUsage[0].UsageReportedCount != 1 {
		t.Fatalf("model usage = %#v, want one model with coverage 1/2", stats.ModelUsage)
	}
	logs := s.GetLogs(2)
	if logs[0].Usage == nil || logs[0].Usage.Source != "upstream" {
		t.Fatalf("reported zero log usage = %#v, want non-nil upstream usage", logs[0].Usage)
	}
	if logs[1].Usage != nil {
		t.Fatalf("unknown log usage = %#v, want nil", logs[1].Usage)
	}
}

func TestTokenStatisticsAggregateDetailsAndSortModels(t *testing.T) {
	s := New(10)
	s.AddLog(RequestLog{
		Provider: "zhipu", Model: "glm", Upstream: "glm-5.1", StatusCode: 200, IsSuccess: true,
		Usage: &TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, CachedTokens: 40, ReasoningTokens: 7, Source: "upstream"},
	})
	s.AddLog(RequestLog{
		Provider: "kimi", Model: "kimi", Upstream: "kimi-for-coding", StatusCode: 200, IsSuccess: true,
		Usage: &TokenUsage{PromptTokens: 10, CompletionTokens: 5},
	})

	stats := s.GetStatistics()
	if stats.PromptTokens != 110 || stats.CompletionTokens != 25 || stats.TotalTokens != 135 {
		t.Fatalf("token totals = %#v, want prompt=110 completion=25 total=135", stats)
	}
	if stats.CachedTokens != 40 || stats.ReasoningTokens != 7 {
		t.Fatalf("detail totals = cached %d reasoning %d, want 40/7", stats.CachedTokens, stats.ReasoningTokens)
	}
	if len(stats.ModelUsage) != 2 || stats.ModelUsage[0].Upstream != "glm-5.1" {
		t.Fatalf("model usage order = %#v, want highest total first", stats.ModelUsage)
	}
}

func TestTokenStatisticsAggregateDailyWeeklyMonthlyUsage(t *testing.T) {
	s := New(10)
	dayOne := time.Date(2026, 7, 18, 9, 0, 0, 0, time.Local)
	dayTwo := time.Date(2026, 7, 19, 10, 0, 0, 0, time.Local)

	s.AddLog(RequestLog{
		Timestamp: dayOne, Provider: "xiaomimimo", Model: "mimo-v2.5", Upstream: "mimo-v2.5", StatusCode: 200, IsSuccess: true,
		Usage: &TokenUsage{PromptTokens: 100, CompletionTokens: 30, TotalTokens: 130, CachedTokens: 20, ReasoningTokens: 9},
	})
	s.AddLog(RequestLog{
		Timestamp: dayOne, Provider: "xiaomimimo", Model: "mimo-v2.5", Upstream: "mimo-v2.5", StatusCode: 200, IsSuccess: true,
	})
	s.AddLog(RequestLog{
		Timestamp: dayTwo, Provider: "useai", Model: "step-router-v1", Upstream: "step-router-v1", StatusCode: 200, IsSuccess: true,
		Usage: &TokenUsage{PromptTokens: 40, CompletionTokens: 10, TotalTokens: 50},
	})

	stats := s.GetStatistics()
	if got, want := len(stats.PeriodUsage.Daily), 2; got != want {
		t.Fatalf("daily periods = %d, want %d: %#v", got, want, stats.PeriodUsage.Daily)
	}
	latestDay := stats.PeriodUsage.Daily[0]
	if latestDay.Key != "2026-07-19" || latestDay.StartDate != "2026-07-19" || latestDay.EndDate != "2026-07-19" || latestDay.TotalTokens != 50 || latestDay.RequestCount != 1 || latestDay.UsageReportedCount != 1 {
		t.Fatalf("latest daily period = %#v, want 2026-07-19 with date range, 50 tokens and 1/1 coverage", latestDay)
	}
	previousDay := stats.PeriodUsage.Daily[1]
	if previousDay.Key != "2026-07-18" || previousDay.StartDate != "2026-07-18" || previousDay.EndDate != "2026-07-18" || previousDay.TotalTokens != 130 || previousDay.RequestCount != 2 || previousDay.UsageReportedCount != 1 {
		t.Fatalf("previous daily period = %#v, want 2026-07-18 with date range, 130 tokens and 1/2 coverage", previousDay)
	}
	if len(previousDay.ModelUsage) != 1 || previousDay.ModelUsage[0].RequestCount != 2 || previousDay.ModelUsage[0].UsageReportedCount != 1 {
		t.Fatalf("daily model usage = %#v, want model coverage 1/2", previousDay.ModelUsage)
	}
	if got, want := len(stats.PeriodUsage.Weekly), 1; got != want {
		t.Fatalf("weekly periods = %d, want %d", got, want)
	}
	if stats.PeriodUsage.Weekly[0].RequestCount != 3 || stats.PeriodUsage.Weekly[0].UsageReportedCount != 2 || stats.PeriodUsage.Weekly[0].TotalTokens != 180 {
		t.Fatalf("weekly period = %#v, want 3 requests, 2 reported, 180 tokens", stats.PeriodUsage.Weekly[0])
	}
	if got, want := len(stats.PeriodUsage.Monthly), 1; got != want {
		t.Fatalf("monthly periods = %d, want %d", got, want)
	}
	if stats.PeriodUsage.Monthly[0].Key != "2026-07" || stats.PeriodUsage.Monthly[0].StartDate != "2026-07-01" || stats.PeriodUsage.Monthly[0].EndDate != "2026-07-31" || stats.PeriodUsage.Monthly[0].TotalTokens != 180 {
		t.Fatalf("monthly period = %#v, want 2026-07 date range and 180 tokens", stats.PeriodUsage.Monthly[0])
	}
}

func TestCurrentTokenPeriodsUseServerCalendarAndEndToday(t *testing.T) {
	loc := time.FixedZone("server-zone", 8*60*60)
	now := time.Date(2026, 7, 18, 16, 30, 0, 0, loc)

	periods := currentTokenPeriods(now)

	if periods.Daily != (CurrentTokenPeriod{Key: "2026-07-18", StartDate: "2026-07-18", EndDate: "2026-07-18"}) {
		t.Fatalf("daily current period = %#v, want 2026-07-18 only", periods.Daily)
	}
	if periods.Weekly != (CurrentTokenPeriod{Key: "2026-W29", StartDate: "2026-07-13", EndDate: "2026-07-18"}) {
		t.Fatalf("weekly current period = %#v, want ISO week start through today", periods.Weekly)
	}
	if periods.Monthly != (CurrentTokenPeriod{Key: "2026-07", StartDate: "2026-07-01", EndDate: "2026-07-18"}) {
		t.Fatalf("monthly current period = %#v, want month start through today", periods.Monthly)
	}
}

func TestTokenStatisticsUsesISOWeekAcrossYearBoundary(t *testing.T) {
	s := New(10)
	weekEnd := time.Date(2026, 12, 31, 23, 0, 0, 0, time.Local)
	nextYearSameISOWeek := time.Date(2027, 1, 1, 9, 0, 0, 0, time.Local)

	s.AddLog(RequestLog{
		Timestamp: weekEnd, Provider: "useai", Model: "step-router-v1", Upstream: "step-router-v1", StatusCode: 200, IsSuccess: true,
		Usage: &TokenUsage{TotalTokens: 10},
	})
	s.AddLog(RequestLog{
		Timestamp: nextYearSameISOWeek, Provider: "useai", Model: "step-router-v1", Upstream: "step-router-v1", StatusCode: 200, IsSuccess: true,
		Usage: &TokenUsage{TotalTokens: 15},
	})

	stats := s.GetStatistics()
	if got, want := len(stats.PeriodUsage.Weekly), 1; got != want {
		t.Fatalf("weekly periods = %d, want %d: %#v", got, want, stats.PeriodUsage.Weekly)
	}
	week := stats.PeriodUsage.Weekly[0]
	if week.Key != "2026-W53" || week.StartDate != "2026-12-28" || week.EndDate != "2027-01-03" || week.TotalTokens != 25 {
		t.Fatalf("ISO week period = %#v, want 2026-W53 spanning 2026-12-28..2027-01-03 with 25 tokens", week)
	}
	if got, want := len(stats.PeriodUsage.Monthly), 2; got != want {
		t.Fatalf("monthly periods = %d, want %d: %#v", got, want, stats.PeriodUsage.Monthly)
	}
}

func TestVersionedSnapshotPreservesCumulativeTokensBeyondLogLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.json")
	s := New(2)
	day := time.Date(2026, 7, 18, 9, 0, 0, 0, time.Local)
	for i := int64(1); i <= 3; i++ {
		s.AddLog(RequestLog{
			Timestamp: day, Provider: "useai", Model: "step-router-v1", Upstream: "step-router-v1", StatusCode: 200, IsSuccess: true,
			Usage: &TokenUsage{PromptTokens: i, CompletionTokens: i, TotalTokens: i * 2},
		})
	}
	if err := s.PersistToFile(path); err != nil {
		t.Fatalf("PersistToFile() error = %v", err)
	}
	logData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read logs snapshot: %v", err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(logData), []byte("[")) {
		t.Fatalf("logs.json must remain a bare array for old-binary rollback compatibility: %s", logData)
	}
	if _, err := os.Stat(statisticsSidecarPath(path)); err != nil {
		t.Fatalf("statistics sidecar missing: %v", err)
	}
	loaded := New(2)
	if err := loaded.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if got := len(loaded.GetLogs(10)); got != 2 {
		t.Fatalf("retained logs = %d, want 2", got)
	}
	stats := loaded.GetStatistics()
	if stats.TotalRequests != 3 || stats.PromptTokens != 6 || stats.TotalTokens != 12 {
		t.Fatalf("loaded cumulative statistics = %#v, want 3 requests and 12 tokens", stats)
	}
	if got, want := len(stats.PeriodUsage.Daily), 1; got != want {
		t.Fatalf("loaded period usage = %#v, want persisted daily period", stats.PeriodUsage)
	}
}

func TestVersionedSnapshotSurvivesSmallerMaxLogsOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.json")
	s := New(5)
	day := time.Date(2026, 7, 18, 9, 0, 0, 0, time.Local)
	for i := int64(1); i <= 4; i++ {
		s.AddLog(RequestLog{
			Timestamp: day, Provider: "useai", Model: "step-router-v1", Upstream: "step-router-v1", StatusCode: 200, IsSuccess: true,
			Usage: &TokenUsage{TotalTokens: i},
		})
	}
	if err := s.PersistToFile(path); err != nil {
		t.Fatalf("PersistToFile() error = %v", err)
	}

	loaded := New(2)
	if err := loaded.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	stats := loaded.GetStatistics()
	if stats.TotalRequests != 4 || stats.TotalTokens != 10 {
		t.Fatalf("loaded cumulative statistics = %#v, want sidecar totals despite smaller maxLogs", stats)
	}
	if got := len(loaded.GetLogs(10)); got != 2 {
		t.Fatalf("retained logs = %d, want 2 after smaller maxLogs", got)
	}
}

func TestLoadFromFileAcceptsLegacyBareLogArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.json")
	legacy := `[{"provider":"legacy","model":"m","upstream":"m","status_code":200,"is_success":true,"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3,"source":"upstream"}}]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy snapshot: %v", err)
	}
	s := New(10)
	if err := s.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile() legacy error = %v", err)
	}
	stats := s.GetStatistics()
	if stats.TotalRequests != 1 || stats.TotalTokens != 3 || stats.UsageReportedCount != 1 {
		t.Fatalf("legacy statistics = %#v, want usage rebuilt", stats)
	}
}

func TestLoadIgnoresStaleStatisticsSidecarAfterOldBinaryWritesLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.json")
	s := New(10)
	s.AddLog(RequestLog{Provider: "zhipu", Model: "glm", StatusCode: 200, IsSuccess: true, Usage: &TokenUsage{TotalTokens: 10}})
	if err := s.PersistToFile(path); err != nil {
		t.Fatalf("PersistToFile() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	var logs []RequestLog
	if err := json.Unmarshal(data, &logs); err != nil {
		t.Fatalf("unmarshal logs: %v", err)
	}
	logs = append(logs, RequestLog{ID: "old-binary-log", Provider: "kimi", Model: "kimi", StatusCode: 200, IsSuccess: true, Usage: &TokenUsage{TotalTokens: 4}})
	data, err = json.Marshal(logs)
	if err != nil {
		t.Fatalf("marshal old-binary logs: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write old-binary logs: %v", err)
	}

	loaded := New(10)
	if err := loaded.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	stats := loaded.GetStatistics()
	if stats.TotalRequests != 2 || stats.TotalTokens != 14 {
		t.Fatalf("statistics = %#v, want rebuild from two current logs", stats)
	}
}

func TestLoadBackfillsPeriodUsageFromRetainedLogsWhenSidecarPredatesPeriods(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.json")
	day := time.Date(2026, 7, 18, 9, 0, 0, 0, time.Local)
	logs := []RequestLog{
		{ID: "log-1", Timestamp: day, Provider: "xiaomimimo", Model: "mimo-v2.5", Upstream: "mimo-v2.5", StatusCode: 200, IsSuccess: true, Usage: &TokenUsage{TotalTokens: 10}},
		{ID: "log-2", Timestamp: day, Provider: "xiaomimimo", Model: "mimo-v2.5", Upstream: "mimo-v2.5", StatusCode: 200, IsSuccess: true},
	}
	data, err := json.Marshal(logs)
	if err != nil {
		t.Fatalf("marshal logs: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write logs: %v", err)
	}
	sidecar := persistedStatisticsSnapshot{
		Version:          storeSnapshotVersion,
		RetainedLogCount: len(logs),
		LatestLogID:      latestLogID(logs),
		Statistics: Statistics{
			TotalRequests:      99,
			TokenUsageRequests: 99,
			UsageReportedCount: 98,
			TotalTokens:        1234,
			ModelUsage: []ModelTokenStatistics{{
				Provider:           "xiaomimimo",
				Model:              "mimo-v2.5",
				Upstream:           "mimo-v2.5",
				RequestCount:       99,
				UsageReportedCount: 98,
				TotalTokens:        1234,
			}},
		},
	}
	sidecarData, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	if err := os.WriteFile(statisticsSidecarPath(path), sidecarData, 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	loaded := New(10)
	if err := loaded.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	stats := loaded.GetStatistics()
	if stats.TotalRequests != 99 || stats.TotalTokens != 1234 {
		t.Fatalf("cumulative statistics = %#v, want preserved sidecar totals", stats)
	}
	if got, want := len(stats.PeriodUsage.Daily), 1; got != want {
		t.Fatalf("daily periods = %d, want %d: %#v", got, want, stats.PeriodUsage.Daily)
	}
	period := stats.PeriodUsage.Daily[0]
	if period.Key != "2026-07-18" || period.RequestCount != 2 || period.UsageReportedCount != 1 || period.TotalTokens != 10 {
		t.Fatalf("backfilled period = %#v, want retained-log coverage 1/2 and 10 tokens", period)
	}
}
