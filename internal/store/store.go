package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RequestLog 单条请求日志
type RequestLog struct {
	ID          string    `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Provider    string    `json:"provider,omitempty"`
	Model       string    `json:"model,omitempty"`
	Upstream    string    `json:"upstream,omitempty"`
	StatusCode  int       `json:"status_code"`
	ElapsedMs   float64   `json:"elapsed_ms"`
	IsSuccess   bool      `json:"is_success"`
	ErrorMessage string   `json:"error_message,omitempty"`
}

// Statistics 统计数据
type Statistics struct {
	TotalRequests int64   `json:"total_requests"`
	SuccessCount  int64   `json:"success_count"`
	FailureCount  int64   `json:"failure_count"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	LastUpdated   time.Time `json:"last_updated"`
}

// Store 内存中的日志与统计存储
// 所有公开方法都是并发安全的；内部更新统计时约定必须在 logMu 持有锁的前提下调用 updateStatsLocked。
type Store struct {
	logs      []RequestLog
	stats     Statistics
	logMu     sync.RWMutex
	statsMu   sync.RWMutex
	maxLogs   int
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
	log.Timestamp = time.Now()

	s.logs = append(s.logs, log)
	if len(s.logs) > s.maxLogs {
		s.logs = s.logs[len(s.logs)-s.maxLogs:]
	}

	s.updateStatsLocked(log)
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

// GetLogsPage 按分页获取日志（最新在前）
// page 从 1 开始，size 为每页条数。
func (s *Store) GetLogsPage(page, size int) LogPageResult {
	s.logMu.RLock()
	defer s.logMu.RUnlock()

	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = 50
	}
	total := len(s.logs)
	start := total - page*size
	if start < 0 {
		start = 0
	}
	end := total - (page-1)*size
	if end > total {
		end = total
	}
	if start >= end {
		return LogPageResult{Logs: []RequestLog{}, Total: total, Page: page, Size: size}
	}

	// 按倒序填充
	count := end - start
	result := make([]RequestLog, count)
	for i := 0; i < count; i++ {
		result[i] = s.logs[end-1-i]
	}
	return LogPageResult{Logs: result, Total: total, Page: page, Size: size}
}

// GetStatistics 获取统计信息
// 该方法是线程安全的，返回当前统计快照值拷贝。
func (s *Store) GetStatistics() Statistics {
	s.statsMu.RLock()
	defer s.statsMu.RUnlock()
	return s.stats
}

// updateStatsLocked 更新统计（必须在 logMu 持有锁时调用）
// 该方法是内部方法，假定调用方已经持有 logMu，因此不再单独加锁 statsMu，避免死锁。
func (s *Store) updateStatsLocked(log RequestLog) {
	s.stats.TotalRequests++
	if log.IsSuccess {
		s.stats.SuccessCount++
	} else {
		s.stats.FailureCount++
	}

	// 更新平均延迟
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
