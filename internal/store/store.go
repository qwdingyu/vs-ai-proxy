package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RequestLog 单条请求日志
type RequestLog struct {
	ID                       string    `json:"id"`
	RequestID                string    `json:"request_id,omitempty"`
	Timestamp                time.Time `json:"timestamp"`
	Method                   string    `json:"method"`
	Path                     string    `json:"path"`
	Provider                 string    `json:"provider,omitempty"`
	Model                    string    `json:"model,omitempty"`
	Upstream                 string    `json:"upstream,omitempty"`
	RequestBytes             int64     `json:"request_bytes,omitempty"`
	UpstreamBytes            int64     `json:"upstream_bytes,omitempty"`
	ConfiguredTimeoutSeconds int       `json:"configured_timeout_seconds,omitempty"`
	EffectiveTimeoutSeconds  int       `json:"effective_timeout_seconds,omitempty"`
	StatusCode               int       `json:"status_code"`
	ElapsedMs                float64   `json:"elapsed_ms"`
	IsSuccess                bool      `json:"is_success"`
	ErrorCode                string    `json:"error_code,omitempty"`
	ErrorMessage             string    `json:"error_message,omitempty"`
	ErrorHint                string    `json:"error_hint,omitempty"`
	ErrorReason              string    `json:"error_reason,omitempty"`
	ErrorAction              string    `json:"error_action,omitempty"`
	DiagnosticSummary        string    `json:"diagnostic_summary,omitempty"`
	AttemptsSummary          string    `json:"attempts_summary,omitempty"`
	CancelReason             string    `json:"cancel_reason,omitempty"`
	NetworkPeer              string    `json:"network_peer,omitempty"`
	StreamState              string    `json:"stream_state,omitempty"`
	RequestTools             string    `json:"request_tools,omitempty"`
	ResponseTools            string    `json:"response_tools,omitempty"`
	FallbackMode             string    `json:"fallback_mode,omitempty"`
	Normalization            string    `json:"normalization,omitempty"`
}

// Statistics 统计数据
type Statistics struct {
	TotalRequests int64     `json:"total_requests"`
	SuccessCount  int64     `json:"success_count"`
	FailureCount  int64     `json:"failure_count"`
	AvgLatencyMs  float64   `json:"avg_latency_ms"`
	LastUpdated   time.Time `json:"last_updated"`
}

// Store 内存中的日志与统计存储
// 所有公开方法都是并发安全的；内部更新统计时约定必须在 logMu 持有锁的前提下调用 updateStatsLocked。
type Store struct {
	logs    []RequestLog
	stats   Statistics
	logMu   sync.RWMutex
	statsMu sync.RWMutex
	maxLogs int
}

// New 创建 Store
func New(maxLogs int) *Store {
	if maxLogs <= 0 {
		maxLogs = 1000
	}
	return &Store{
		logs:    make([]RequestLog, 0, maxLogs),
		maxLogs: maxLogs,
	}
}

// AddLog 添加一条日志
// 该方法是线程安全的，会自动截断到 maxLogs 上限，并同步更新统计。
func (s *Store) AddLog(log RequestLog) {
	s.logMu.Lock()
	defer s.logMu.Unlock()

	log.ID = fmt.Sprintf("%d-%d", time.Now().UnixNano(), len(s.logs))
	if log.RequestID == "" {
		log.RequestID = log.ID
	}
	log.Timestamp = time.Now()

	s.logs = append(s.logs, log)
	if len(s.logs) > s.maxLogs {
		s.logs = s.logs[len(s.logs)-s.maxLogs:]
	}

	s.statsMu.Lock()
	s.updateStatsLocked(log)
	s.statsMu.Unlock()
}

// GetLogs 获取最近 N 条日志（最新在前）
// 该方法是线程安全的，返回的是最新的 N 条日志切片副本。
func (s *Store) GetLogs(n int) []RequestLog {
	s.logMu.RLock()
	defer s.logMu.RUnlock()

	if n <= 0 || n > len(s.logs) {
		n = len(s.logs)
	}

	// 返回最近的 n 条，按时间倒序（最新在前）
	result := make([]RequestLog, n)
	for i := 0; i < n; i++ {
		result[i] = s.logs[len(s.logs)-1-i]
	}
	return result
}

// LogPageResult 分页查询结果
type LogPageResult struct {
	Logs  []RequestLog `json:"logs"`
	Total int          `json:"total"`
	Page  int          `json:"page"`
	Size  int          `json:"size"`
}

// LogFilters 日志过滤条件
type LogFilters struct {
	Provider    string
	Model       string
	StatusCode  int
	ErrorCode   string
	RequestID   string
	ErrorReason string
	Search      string
}

// GetLogsPage 按分页获取日志（最新在前）
// page 从 1 开始，size 为每页条数。
func (s *Store) GetLogsPage(page, size int) LogPageResult {
	return s.GetLogsPageFiltered(page, size, LogFilters{})
}

// GetLogsPageFiltered 按分页与条件获取日志（最新在前）
func (s *Store) GetLogsPageFiltered(page, size int, filters LogFilters) LogPageResult {
	s.logMu.RLock()
	defer s.logMu.RUnlock()

	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = 50
	}
	filtered := make([]RequestLog, 0, len(s.logs))
	for i := len(s.logs) - 1; i >= 0; i-- {
		log := s.logs[i]
		if !matchesLogFilters(log, filters) {
			continue
		}
		filtered = append(filtered, log)
	}
	total := len(filtered)
	start := (page - 1) * size
	end := start + size
	if end > total {
		end = total
	}
	if start >= end {
		return LogPageResult{Logs: []RequestLog{}, Total: total, Page: page, Size: size}
	}

	result := append([]RequestLog(nil), filtered[start:end]...)
	return LogPageResult{Logs: result, Total: total, Page: page, Size: size}
}

// GetLatestFailure 返回当前保留日志中的最近一条失败请求。
func (s *Store) GetLatestFailure() (RequestLog, bool) {
	s.logMu.RLock()
	defer s.logMu.RUnlock()

	for i := len(s.logs) - 1; i >= 0; i-- {
		if !s.logs[i].IsSuccess {
			return s.logs[i], true
		}
	}
	return RequestLog{}, false
}

func matchesLogFilters(log RequestLog, filters LogFilters) bool {
	if filters.Provider != "" && !strings.EqualFold(strings.TrimSpace(log.Provider), strings.TrimSpace(filters.Provider)) {
		return false
	}
	if filters.Model != "" {
		model := strings.ToLower(strings.TrimSpace(log.Model))
		upstream := strings.ToLower(strings.TrimSpace(log.Upstream))
		want := strings.ToLower(strings.TrimSpace(filters.Model))
		if model != want && upstream != want && !strings.Contains(model, want) && !strings.Contains(upstream, want) {
			return false
		}
	}
	if filters.StatusCode > 0 && log.StatusCode != filters.StatusCode {
		return false
	}
	if filters.ErrorCode != "" && !strings.Contains(strings.ToLower(log.ErrorCode), strings.ToLower(filters.ErrorCode)) {
		return false
	}
	if filters.RequestID != "" && !strings.Contains(strings.ToLower(log.RequestID), strings.ToLower(filters.RequestID)) {
		return false
	}
	if filters.ErrorReason != "" && !strings.Contains(strings.ToLower(log.ErrorReason), strings.ToLower(filters.ErrorReason)) {
		return false
	}
	if filters.Search != "" {
		needle := strings.ToLower(strings.TrimSpace(filters.Search))
		joined := strings.ToLower(strings.Join([]string{
			log.Method,
			log.Path,
			log.Provider,
			log.Model,
			log.Upstream,
			log.ErrorCode,
			log.ErrorMessage,
			log.ErrorHint,
			log.ErrorReason,
			log.ErrorAction,
			log.DiagnosticSummary,
			log.AttemptsSummary,
			log.CancelReason,
			log.NetworkPeer,
			log.StreamState,
			log.RequestTools,
			log.ResponseTools,
		}, " "))
		if !strings.Contains(joined, needle) {
			return false
		}
	}
	return true
}

// GetStatistics 获取统计信息
// 该方法是线程安全的，返回当前统计快照值拷贝。
func (s *Store) GetStatistics() Statistics {
	s.statsMu.RLock()
	defer s.statsMu.RUnlock()
	return s.stats
}

// updateStatsLocked 更新统计（必须同时持有 logMu 与 statsMu 写锁）。
// logMu 保护日志截断与统计口径一致，statsMu 保护 GetStatistics 并发读取。
func (s *Store) updateStatsLocked(log RequestLog) {
	s.stats.TotalRequests++
	if log.IsSuccess {
		s.stats.SuccessCount++
	} else {
		s.stats.FailureCount++
	}

	// 更新代理端到端平均耗时
	if s.stats.TotalRequests == 1 {
		s.stats.AvgLatencyMs = log.ElapsedMs
	} else {
		s.stats.AvgLatencyMs = (s.stats.AvgLatencyMs*float64(s.stats.TotalRequests-1) + log.ElapsedMs) / float64(s.stats.TotalRequests)
	}
	s.stats.LastUpdated = time.Now()
}

// ClearLogs 清空日志
// 该方法是线程安全的，仅清空日志切片，不会重置统计。
func (s *Store) ClearLogs() {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.logs = s.logs[:0]
}

// PersistToFile 持久化到文件
// 该方法是线程安全的，仅持久化日志，不会持久化统计。
func (s *Store) PersistToFile(path string) error {
	s.logMu.RLock()
	defer s.logMu.RUnlock()

	data, err := json.MarshalIndent(s.logs, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// LoadFromFile 从文件加载
// 该方法是线程安全的，加载后会重建统计，但不会持久化 provider/models 配置。
func (s *Store) LoadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var logs []RequestLog
	if err := json.Unmarshal(data, &logs); err != nil {
		return err
	}

	s.logMu.Lock()
	defer s.logMu.Unlock()

	s.logs = logs
	if len(s.logs) > s.maxLogs {
		s.logs = s.logs[len(s.logs)-s.maxLogs:]
	}

	// 重建统计
	s.statsMu.Lock()
	s.stats = Statistics{LastUpdated: time.Now()}
	for _, log := range s.logs {
		s.updateStatsLocked(log)
	}
	s.statsMu.Unlock()

	return nil
}
