package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/renameio/maybe"
)

// RequestLog 单条请求日志
type RequestLog struct {
	ID                       string      `json:"id"`
	RequestID                string      `json:"request_id,omitempty"`
	Timestamp                time.Time   `json:"timestamp"`
	Method                   string      `json:"method"`
	Path                     string      `json:"path"`
	Provider                 string      `json:"provider,omitempty"`
	Model                    string      `json:"model,omitempty"`
	Upstream                 string      `json:"upstream,omitempty"`
	RequestBytes             int64       `json:"request_bytes,omitempty"`
	UpstreamBytes            int64       `json:"upstream_bytes,omitempty"`
	ConfiguredTimeoutSeconds int         `json:"configured_timeout_seconds,omitempty"`
	EffectiveTimeoutSeconds  int         `json:"effective_timeout_seconds,omitempty"`
	StatusCode               int         `json:"status_code"`
	ElapsedMs                float64     `json:"elapsed_ms"`
	IsSuccess                bool        `json:"is_success"`
	ErrorCode                string      `json:"error_code,omitempty"`
	ErrorMessage             string      `json:"error_message,omitempty"`
	ErrorHint                string      `json:"error_hint,omitempty"`
	ErrorReason              string      `json:"error_reason,omitempty"`
	ErrorAction              string      `json:"error_action,omitempty"`
	DiagnosticSummary        string      `json:"diagnostic_summary,omitempty"`
	AttemptsSummary          string      `json:"attempts_summary,omitempty"`
	CancelReason             string      `json:"cancel_reason,omitempty"`
	NetworkPeer              string      `json:"network_peer,omitempty"`
	StreamState              string      `json:"stream_state,omitempty"`
	RequestTools             string      `json:"request_tools,omitempty"`
	ResponseTools            string      `json:"response_tools,omitempty"`
	FallbackMode             string      `json:"fallback_mode,omitempty"`
	Normalization            string      `json:"normalization,omitempty"`
	Usage                    *TokenUsage `json:"usage,omitempty"`
}

// TokenUsage is an upstream-reported usage snapshot. Nil usage means the
// upstream did not report usage; a non-nil all-zero value means reported zero.
type TokenUsage struct {
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CachedTokens     int64  `json:"cached_tokens,omitempty"`
	ReasoningTokens  int64  `json:"reasoning_tokens,omitempty"`
	Source           string `json:"source"`
}

type ModelTokenStatistics struct {
	Provider           string `json:"provider,omitempty"`
	Model              string `json:"model,omitempty"`
	Upstream           string `json:"upstream,omitempty"`
	RequestCount       int64  `json:"request_count"`
	UsageReportedCount int64  `json:"usage_reported_count"`
	PromptTokens       int64  `json:"prompt_tokens"`
	CompletionTokens   int64  `json:"completion_tokens"`
	TotalTokens        int64  `json:"total_tokens"`
	CachedTokens       int64  `json:"cached_tokens"`
	ReasoningTokens    int64  `json:"reasoning_tokens"`
}

// TokenPeriodStatistics 是首页日/周/月用量的周期桶。
// 这里的 RequestCount 是“有 provider/model/upstream 归属的请求数”，
// UsageReportedCount 才是“上游明确返回 usage 的请求数”。这两个字段必须分开，
// 否则 UI 会把未返回 usage 的请求误读为 0 Token，破坏成本统计可信度。
type TokenPeriodStatistics struct {
	Key                string                 `json:"key"`
	Label              string                 `json:"label"`
	StartDate          string                 `json:"start_date"`
	EndDate            string                 `json:"end_date"`
	RequestCount       int64                  `json:"request_count"`
	UsageReportedCount int64                  `json:"usage_reported_count"`
	PromptTokens       int64                  `json:"prompt_tokens"`
	CompletionTokens   int64                  `json:"completion_tokens"`
	TotalTokens        int64                  `json:"total_tokens"`
	CachedTokens       int64                  `json:"cached_tokens"`
	ReasoningTokens    int64                  `json:"reasoning_tokens"`
	ModelUsage         []ModelTokenStatistics `json:"model_usage,omitempty"`
}

// TokenPeriodUsage 同时维护自然日、ISO 周和自然月。
// 前端只读这些已聚合桶，不在浏览器端重新扫描日志，避免不同页面产生不同事实源。
type TokenPeriodUsage struct {
	Daily   []TokenPeriodStatistics `json:"daily"`
	Weekly  []TokenPeriodStatistics `json:"weekly"`
	Monthly []TokenPeriodStatistics `json:"monthly"`
}

// CurrentTokenPeriod 标识服务端当前所在周期。前端用它查找“今日/本周/本月”
// 对应的已聚合桶，避免远程浏览器时区与服务进程时区不一致时选错周期。
type CurrentTokenPeriod struct {
	Key       string `json:"key"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
}

// CurrentTokenPeriods 是首页三张周期卡片使用的服务端当前周期集合。
// 注意：这里的周/月 EndDate 表示“截至今天”的展示范围，不改变历史周期桶的归档结束日。
type CurrentTokenPeriods struct {
	Daily   CurrentTokenPeriod `json:"daily"`
	Weekly  CurrentTokenPeriod `json:"weekly"`
	Monthly CurrentTokenPeriod `json:"monthly"`
}

// Statistics 统计数据
type Statistics struct {
	TotalRequests      int64                  `json:"total_requests"`
	SuccessCount       int64                  `json:"success_count"`
	FailureCount       int64                  `json:"failure_count"`
	AvgLatencyMs       float64                `json:"avg_latency_ms"`
	TokenUsageRequests int64                  `json:"token_usage_requests"`
	UsageReportedCount int64                  `json:"usage_reported_count"`
	PromptTokens       int64                  `json:"prompt_tokens"`
	CompletionTokens   int64                  `json:"completion_tokens"`
	TotalTokens        int64                  `json:"total_tokens"`
	CachedTokens       int64                  `json:"cached_tokens"`
	ReasoningTokens    int64                  `json:"reasoning_tokens"`
	ModelUsage         []ModelTokenStatistics `json:"model_usage"`
	PeriodUsage        TokenPeriodUsage       `json:"period_usage"`
	CurrentPeriods     CurrentTokenPeriods    `json:"current_periods"`
	LastUpdated        time.Time              `json:"last_updated"`
}

// Store 内存中的日志与统计存储
// 所有公开方法都是并发安全的；内部更新统计时约定必须在 logMu 持有锁的前提下调用 updateStatsLocked。
type Store struct {
	logs       []RequestLog
	stats      Statistics
	modelStats map[string]*ModelTokenStatistics
	logMu      sync.RWMutex
	statsMu    sync.RWMutex
	maxLogs    int
}

// New 创建 Store
func New(maxLogs int) *Store {
	if maxLogs <= 0 {
		maxLogs = 1000
	}
	return &Store{
		logs:       make([]RequestLog, 0, maxLogs),
		modelStats: make(map[string]*ModelTokenStatistics),
		maxLogs:    maxLogs,
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
	// 测试、导入和旧日志重建会传入历史 Timestamp；只有实时请求才补当前时间。
	if log.Timestamp.IsZero() {
		log.Timestamp = time.Now()
	}
	log.Usage = normalizeTokenUsage(log.Usage)

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
		result[i] = cloneRequestLog(s.logs[len(s.logs)-1-i])
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

	result := make([]RequestLog, end-start)
	for i := start; i < end; i++ {
		result[i-start] = cloneRequestLog(filtered[i])
	}
	return LogPageResult{Logs: result, Total: total, Page: page, Size: size}
}

// GetLatestFailure 返回当前保留日志中的最近一条失败请求。
func (s *Store) GetLatestFailure() (RequestLog, bool) {
	s.logMu.RLock()
	defer s.logMu.RUnlock()

	for i := len(s.logs) - 1; i >= 0; i-- {
		if !s.logs[i].IsSuccess {
			return cloneRequestLog(s.logs[i]), true
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
	return s.statisticsSnapshotLocked()
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

	if !isTokenUsageRequest(log) {
		return
	}
	s.stats.TokenUsageRequests++
	key := modelStatsKey(log.Provider, log.Model, log.Upstream)
	modelStats := s.modelStats[key]
	if modelStats == nil {
		modelStats = &ModelTokenStatistics{Provider: log.Provider, Model: log.Model, Upstream: log.Upstream}
		s.modelStats[key] = modelStats
	}
	modelStats.RequestCount++
	// 周期统计必须在 usage nil 判断之前更新：unknown usage 要进入覆盖率分母，
	// 但不能贡献任何 token 数值。
	updatePeriodUsage(&s.stats.PeriodUsage, log)
	if log.Usage == nil {
		return
	}
	s.stats.UsageReportedCount++
	s.stats.PromptTokens += log.Usage.PromptTokens
	s.stats.CompletionTokens += log.Usage.CompletionTokens
	s.stats.TotalTokens += log.Usage.TotalTokens
	s.stats.CachedTokens += log.Usage.CachedTokens
	s.stats.ReasoningTokens += log.Usage.ReasoningTokens
	modelStats.UsageReportedCount++
	modelStats.PromptTokens += log.Usage.PromptTokens
	modelStats.CompletionTokens += log.Usage.CompletionTokens
	modelStats.TotalTokens += log.Usage.TotalTokens
	modelStats.CachedTokens += log.Usage.CachedTokens
	modelStats.ReasoningTokens += log.Usage.ReasoningTokens
}

func isTokenUsageRequest(log RequestLog) bool {
	return strings.TrimSpace(log.Provider) != "" || strings.TrimSpace(log.Model) != "" || strings.TrimSpace(log.Upstream) != ""
}

func modelStatsKey(provider, model, upstream string) string {
	return provider + "\x00" + model + "\x00" + upstream
}

type tokenPeriod struct {
	key       string
	label     string
	startDate string
	endDate   string
}

func updatePeriodUsage(periods *TokenPeriodUsage, log RequestLog) {
	if periods == nil {
		return
	}
	timestamp := log.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	local := timestamp.Local()
	updateTokenPeriod(&periods.Daily, dayPeriod(local), log)
	updateTokenPeriod(&periods.Weekly, weekPeriod(local), log)
	updateTokenPeriod(&periods.Monthly, monthPeriod(local), log)
}

// updateTokenPeriod 只累加单个周期桶。RequestCount 无条件累加；
// token 字段只在上游 usage 非 nil 时累加，用覆盖率表达数据完整性。
func updateTokenPeriod(periods *[]TokenPeriodStatistics, period tokenPeriod, log RequestLog) {
	if periods == nil || period.key == "" {
		return
	}
	index := -1
	for i := range *periods {
		if (*periods)[i].Key == period.key {
			index = i
			break
		}
	}
	if index < 0 {
		*periods = append(*periods, TokenPeriodStatistics{
			Key:       period.key,
			Label:     period.label,
			StartDate: period.startDate,
			EndDate:   period.endDate,
		})
		index = len(*periods) - 1
	}

	stat := &(*periods)[index]
	stat.RequestCount++
	updateModelTokenStats(&stat.ModelUsage, log)
	if log.Usage == nil {
		return
	}
	stat.UsageReportedCount++
	stat.PromptTokens += log.Usage.PromptTokens
	stat.CompletionTokens += log.Usage.CompletionTokens
	stat.TotalTokens += log.Usage.TotalTokens
	stat.CachedTokens += log.Usage.CachedTokens
	stat.ReasoningTokens += log.Usage.ReasoningTokens
}

// updateModelTokenStats 复用累计模型统计结构，但作用域限定在单个周期桶内。
// 它不能写 s.modelStats，避免周期统计污染全局累计排名。
func updateModelTokenStats(modelUsage *[]ModelTokenStatistics, log RequestLog) {
	if modelUsage == nil {
		return
	}
	key := modelStatsKey(log.Provider, log.Model, log.Upstream)
	index := -1
	for i := range *modelUsage {
		if modelStatsKey((*modelUsage)[i].Provider, (*modelUsage)[i].Model, (*modelUsage)[i].Upstream) == key {
			index = i
			break
		}
	}
	if index < 0 {
		*modelUsage = append(*modelUsage, ModelTokenStatistics{Provider: log.Provider, Model: log.Model, Upstream: log.Upstream})
		index = len(*modelUsage) - 1
	}
	stat := &(*modelUsage)[index]
	stat.RequestCount++
	if log.Usage == nil {
		return
	}
	stat.UsageReportedCount++
	stat.PromptTokens += log.Usage.PromptTokens
	stat.CompletionTokens += log.Usage.CompletionTokens
	stat.TotalTokens += log.Usage.TotalTokens
	stat.CachedTokens += log.Usage.CachedTokens
	stat.ReasoningTokens += log.Usage.ReasoningTokens
}

func dayPeriod(t time.Time) tokenPeriod {
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	key := start.Format("2006-01-02")
	return tokenPeriod{key: key, label: key, startDate: key, endDate: key}
}

// weekPeriod 使用 ISO 周：周一为起点，跨年周归属 ISO week-year。
// 这与常见账单/报表的“本周”口径一致，避免周日作为起点造成中英文环境差异。
func weekPeriod(t time.Time) tokenPeriod {
	weekday := int(t.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, -(weekday - 1))
	end := start.AddDate(0, 0, 6)
	year, week := start.ISOWeek()
	key := fmt.Sprintf("%04d-W%02d", year, week)
	return tokenPeriod{
		key:       key,
		label:     key,
		startDate: start.Format("2006-01-02"),
		endDate:   end.Format("2006-01-02"),
	}
}

func monthPeriod(t time.Time) tokenPeriod {
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	end := start.AddDate(0, 1, -1)
	key := start.Format("2006-01")
	return tokenPeriod{
		key:       key,
		label:     key,
		startDate: start.Format("2006-01-02"),
		endDate:   end.Format("2006-01-02"),
	}
}

func currentTokenPeriods(now time.Time) CurrentTokenPeriods {
	local := now.Local()
	today := dayPeriod(local)
	week := weekPeriod(local)
	month := monthPeriod(local)
	return CurrentTokenPeriods{
		Daily: CurrentTokenPeriod{
			Key:       today.key,
			StartDate: today.startDate,
			EndDate:   today.endDate,
		},
		Weekly: CurrentTokenPeriod{
			Key:       week.key,
			StartDate: week.startDate,
			EndDate:   today.endDate,
		},
		Monthly: CurrentTokenPeriod{
			Key:       month.key,
			StartDate: month.startDate,
			EndDate:   today.endDate,
		},
	}
}

func normalizeTokenUsage(usage *TokenUsage) *TokenUsage {
	if usage == nil || usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 || usage.CachedTokens < 0 || usage.ReasoningTokens < 0 {
		return nil
	}
	normalized := *usage
	if normalized.TotalTokens == 0 && normalized.PromptTokens+normalized.CompletionTokens > 0 {
		normalized.TotalTokens = normalized.PromptTokens + normalized.CompletionTokens
	}
	if strings.TrimSpace(normalized.Source) == "" {
		normalized.Source = "upstream"
	}
	return &normalized
}

func cloneRequestLog(log RequestLog) RequestLog {
	if log.Usage != nil {
		usage := *log.Usage
		log.Usage = &usage
	}
	return log
}

func (s *Store) statisticsSnapshotLocked() Statistics {
	stats := s.stats
	stats.ModelUsage = make([]ModelTokenStatistics, 0, len(s.modelStats))
	for _, modelStats := range s.modelStats {
		stats.ModelUsage = append(stats.ModelUsage, *modelStats)
	}
	sortModelUsage(stats.ModelUsage)
	// PeriodUsage 内含 slice，必须深拷贝后再排序，避免读快照时改动 Store 内部顺序。
	stats.PeriodUsage = clonePeriodUsage(s.stats.PeriodUsage)
	stats.CurrentPeriods = currentTokenPeriods(time.Now())
	return stats
}

func clonePeriodUsage(periods TokenPeriodUsage) TokenPeriodUsage {
	return TokenPeriodUsage{
		Daily:   clonePeriodStats(periods.Daily),
		Weekly:  clonePeriodStats(periods.Weekly),
		Monthly: clonePeriodStats(periods.Monthly),
	}
}

func clonePeriodStats(periods []TokenPeriodStatistics) []TokenPeriodStatistics {
	out := make([]TokenPeriodStatistics, len(periods))
	for i := range periods {
		out[i] = periods[i]
		out[i].ModelUsage = append([]ModelTokenStatistics(nil), periods[i].ModelUsage...)
		sortModelUsage(out[i].ModelUsage)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key > out[j].Key
	})
	return out
}

func sortModelUsage(modelUsage []ModelTokenStatistics) {
	sort.Slice(modelUsage, func(i, j int) bool {
		if modelUsage[i].TotalTokens != modelUsage[j].TotalTokens {
			return modelUsage[i].TotalTokens > modelUsage[j].TotalTokens
		}
		left := modelStatsKey(modelUsage[i].Provider, modelUsage[i].Model, modelUsage[i].Upstream)
		right := modelStatsKey(modelUsage[j].Provider, modelUsage[j].Model, modelUsage[j].Upstream)
		return left < right
	})
}

// ClearLogs 清空日志
// 该方法是线程安全的，仅清空日志切片，不会重置统计。
func (s *Store) ClearLogs() {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.logs = s.logs[:0]
}

const storeSnapshotVersion = 1

type persistedStatisticsSnapshot struct {
	Version          int        `json:"version"`
	RetainedLogCount int        `json:"retained_log_count"`
	LatestLogID      string     `json:"latest_log_id,omitempty"`
	Statistics       Statistics `json:"statistics"`
}

// PersistToFile keeps the legacy log-array shape for rollback compatibility and
// stores cumulative statistics in a versioned sidecar.
func (s *Store) PersistToFile(path string) error {
	// Keep the established lock order (logs, then stats) while taking a consistent snapshot.
	s.logMu.RLock()
	logs := make([]RequestLog, len(s.logs))
	for i := range s.logs {
		logs[i] = cloneRequestLog(s.logs[i])
	}
	s.statsMu.RLock()
	stats := s.statisticsSnapshotLocked()
	s.statsMu.RUnlock()
	s.logMu.RUnlock()

	data, err := json.Marshal(logs)
	if err != nil {
		return err
	}
	statsData, err := json.Marshal(persistedStatisticsSnapshot{
		Version:          storeSnapshotVersion,
		RetainedLogCount: len(logs),
		LatestLogID:      latestLogID(logs),
		Statistics:       stats,
	})
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// 使用成熟的 renameio/maybe 写入快照：Unix 走原子替换，Windows 走兼容写入，避免跨平台构建失败。
	if err := maybe.WriteFile(path, data, 0644); err != nil {
		return err
	}
	return maybe.WriteFile(statisticsSidecarPath(path), statsData, 0644)
}

// LoadFromFile preserves the legacy bare log array and optionally restores the
// versioned cumulative-statistics sidecar.
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
	statisticsSnapshot, statisticsLoadErr := loadStatisticsSidecar(path)

	s.logMu.Lock()
	defer s.logMu.Unlock()

	s.logs = logs
	for i := range s.logs {
		s.logs[i].Usage = normalizeTokenUsage(s.logs[i].Usage)
	}
	if len(s.logs) > s.maxLogs {
		s.logs = s.logs[len(s.logs)-s.maxLogs:]
	}
	var persistedStats *Statistics
	if statisticsSnapshot != nil && statisticsSnapshot.matchesLogs(s.logs) {
		persistedStats = &statisticsSnapshot.Statistics
	}

	s.statsMu.Lock()
	s.modelStats = make(map[string]*ModelTokenStatistics)
	if persistedStats != nil && persistedStats.TotalRequests >= int64(len(s.logs)) {
		s.stats = *persistedStats
		for i := range persistedStats.ModelUsage {
			modelStats := persistedStats.ModelUsage[i]
			s.modelStats[modelStatsKey(modelStats.Provider, modelStats.Model, modelStats.Upstream)] = &modelStats
		}
		s.stats.ModelUsage = nil
		if isPeriodUsageEmpty(s.stats.PeriodUsage) {
			// v0.2.56 之前的 sidecar 只有累计字段，没有 period_usage。
			// 升级时保留 sidecar 的累计统计，同时用当前保留日志回填趋势；
			// 超出日志保留上限的历史周期无法反推，不能伪造。
			s.stats.PeriodUsage = rebuildPeriodUsageFromLogs(s.logs)
		}
	} else {
		s.stats = Statistics{LastUpdated: time.Now()}
		for _, log := range s.logs {
			s.updateStatsLocked(log)
		}
	}
	s.statsMu.Unlock()

	return statisticsLoadErr
}

func statisticsSidecarPath(path string) string {
	ext := filepath.Ext(path)
	if ext == "" {
		return path + ".stats"
	}
	return strings.TrimSuffix(path, ext) + ".stats" + ext
}

func isPeriodUsageEmpty(periods TokenPeriodUsage) bool {
	return len(periods.Daily) == 0 && len(periods.Weekly) == 0 && len(periods.Monthly) == 0
}

func rebuildPeriodUsageFromLogs(logs []RequestLog) TokenPeriodUsage {
	var periods TokenPeriodUsage
	for _, log := range logs {
		if isTokenUsageRequest(log) {
			updatePeriodUsage(&periods, log)
		}
	}
	return periods
}

func latestLogID(logs []RequestLog) string {
	if len(logs) == 0 {
		return ""
	}
	return logs[len(logs)-1].ID
}

func (s *persistedStatisticsSnapshot) matchesLogs(logs []RequestLog) bool {
	// maxLogs 变小时，LoadFromFile 会先截断旧 logs.json 的保留日志。
	// 只要 sidecar 记录的最新日志仍与当前最新日志一致，且 sidecar 覆盖的日志数不少于当前保留数，
	// 累计统计仍然可信；不能因为本地保留窗口缩小就丢弃累计 token。
	return s != nil && s.RetainedLogCount >= len(logs) && s.LatestLogID == latestLogID(logs)
}

func loadStatisticsSidecar(path string) (*persistedStatisticsSnapshot, error) {
	data, err := os.ReadFile(statisticsSidecarPath(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var snapshot persistedStatisticsSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("load statistics sidecar: %w", err)
	}
	if snapshot.Version != storeSnapshotVersion {
		return nil, fmt.Errorf("unsupported statistics snapshot version %d", snapshot.Version)
	}
	return &snapshot, nil
}
