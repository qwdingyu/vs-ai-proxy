package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

const maxDiagnosticMessageBytes = 1000

var (
	secretTokenPattern = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{12,}`)
	openAIKeyPattern   = regexp.MustCompile(`sk-[A-Za-z0-9_-]{12,}`)
	useAIKeyPattern    = regexp.MustCompile(`(?i)(api[_-]?key["'\s:=]+)[A-Za-z0-9._~+/=-]{12,}`)
	upstreamStatusCode = regexp.MustCompile(`(?i)(?:API|Ollama)\s*错误\s*(\d{3})`)
)

type attemptDiagnostic struct {
	Provider  string  `json:"provider"`
	Upstream  string  `json:"upstream_model"`
	Category  string  `json:"category"`
	Message   string  `json:"message"`
	ElapsedMs float64 `json:"elapsed_ms,omitempty"`
	Peer      string  `json:"network_peer,omitempty"`
	// UpstreamAttempts 是 provider 内部真实 HTTP 尝试明细。
	// 外层 attempt 表示“候选 provider/模型”；内层 attempt 表示同一 provider
	// 为了防御瞬态故障做过的短重试。两层分开，日志才能判断是否发生了请求放大。
	UpstreamAttempts []upstreamAttemptDiagnostic `json:"upstream_attempts,omitempty"`
}

type upstreamAttemptDiagnostic struct {
	Stage      string  `json:"stage,omitempty"`        // provider httptrace 的最后阶段，不包含 URL/Header/正文。
	Category   string  `json:"category"`               // 根据 HTTP 状态码、阶段和错误文本归一后的诊断类别。
	ElapsedMs  float64 `json:"elapsed_ms,omitempty"`   // 单次上游 HTTP 尝试耗时，单位毫秒。
	HTTPStatus int     `json:"http_status,omitempty"`  // 上游返回 HTTP 响应时记录；纯传输失败为 0。
	Peer       string  `json:"network_peer,omitempty"` // 从错误文本解析出的远端地址，可能是 CDN 边缘节点。
}

type upstreamHTTPErrorMetadata interface {
	UpstreamHTTPStatusCode() int
	UpstreamHTTPErrorBody() []byte
}

type proxyDiagnosticDetails struct {
	RequestedModel string              `json:"requested_model"`
	ResolvedModel  string              `json:"resolved_model"`
	CandidateCount int                 `json:"candidate_count"`
	Attempts       []attemptDiagnostic `json:"attempts,omitempty"`
	Hint           string              `json:"hint"`
}

type proxyDiagnosticError struct {
	Message string                 `json:"message"`
	Type    string                 `json:"type"`
	Code    string                 `json:"code"`
	Details proxyDiagnosticDetails `json:"details"`
}

func writeProxyDiagnosticError(w http.ResponseWriter, status int, diag proxyDiagnosticError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proxy-Error-Code", diagnosticHeaderValue(diag.Code))
	w.Header().Set("X-Proxy-Error-Message", diagnosticHeaderValue(diag.Message))
	w.Header().Set("X-Proxy-Error-Hint", diagnosticHeaderValue(diag.Details.Hint))
	if summary := attemptsSummary(diag.Details.Attempts); summary != "" {
		setProxyDiagnosticHeader(w, "X-Proxy-Attempts-Summary", diagnosticHeaderValue(summary))
	}
	if peer := firstAttemptNetworkPeer(diag.Details.Attempts); peer != "" {
		setProxyDiagnosticHeader(w, "X-Proxy-Network-Peer", peer)
	}
	if streamState := firstAttemptStreamState(diag.Details.Attempts); streamState != "" {
		setProxyDiagnosticHeader(w, "X-Proxy-Stream-State", streamState)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": diag})
}

func diagnosticHeaderValue(value string) string {
	value = sanitizeDiagnosticMessage(value)
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.Join(strings.Fields(value), " ")
}

func noCandidateDiagnostic(requestedModel, resolvedModel string, candidateCount int) proxyDiagnosticError {
	return proxyDiagnosticError{
		Message: "模型无法路由到任何可用提供商",
		Type:    "routing_error",
		Code:    "model_not_routable",
		Details: proxyDiagnosticDetails{
			RequestedModel: requestedModel,
			ResolvedModel:  resolvedModel,
			CandidateCount: candidateCount,
			Hint:           "检查模型名是否拼写正确、模型是否出现在 /v1/models 或 /api/tags、provider 是否启用。",
		},
	}
}

func ambiguousModelAliasDiagnostic(requestedModel, resolvedModel string) proxyDiagnosticError {
	return proxyDiagnosticError{
		Message: "模型短名匹配到多个上游模型，无法安全自动路由",
		Type:    "routing_error",
		Code:    "model_alias_ambiguous",
		Details: proxyDiagnosticDetails{
			RequestedModel: requestedModel,
			ResolvedModel:  resolvedModel,
			CandidateCount: 0,
			Hint:           "请在 Visual Studio 中选择带 provider 的模型，或直接使用完整模型名 / model@provider_id，避免短名歧义。",
		},
	}
}

func allCandidatesFailedDiagnostic(requestedModel, resolvedModel string, candidateCount int, attempts []attemptDiagnostic) proxyDiagnosticError {
	// 多 provider 候选都失败时，不能简单取最后一个错误作为总错误码。
	// 最后一个候选可能只是普通网络断开，真正需要用户处理的却是前面的 400/413/429。
	// 因此这里按“最可行动”的错误类别选主因，让日志和前端先展示最值得排查的方向。
	category := primaryFailureCategory(attempts)
	message := "所有候选提供商请求均失败"
	if candidateCount == 1 {
		message = "当前提供商请求失败"
	}
	return proxyDiagnosticError{
		Message: message,
		Type:    "upstream_error",
		Code:    category,
		Details: proxyDiagnosticDetails{
			RequestedModel: requestedModel,
			ResolvedModel:  resolvedModel,
			CandidateCount: candidateCount,
			Attempts:       attempts,
			Hint:           diagnosticHintForAttempts(category, attempts),
		},
	}
}

func primaryFailureCategory(attempts []attemptDiagnostic) string {
	if len(attempts) == 0 {
		return "provider_error"
	}
	bestCategory := "provider_error"
	bestRank := failureCategoryRank(bestCategory)
	for _, attempt := range attempts {
		category := strings.TrimSpace(attempt.Category)
		if category == "" {
			category = "provider_error"
		}
		rank := failureCategoryRank(category)
		if rank < bestRank {
			bestCategory = category
			bestRank = rank
		}
	}
	return bestCategory
}

func failureCategoryRank(category string) int {
	// 数字越小优先级越高。
	// 排序原则：先暴露用户或配置能立即修正的问题（鉴权、参数、请求体、限流），
	// 再暴露超时/上游/网络等环境和上游稳定性问题，避免关键信息被兜底错误覆盖。
	switch category {
	case "upstream_quota_exhausted":
		return 5
	case "upstream_auth_error":
		return 10
	case "upstream_message_error":
		return 15
	case "upstream_request_error":
		return 20
	case "upstream_payload_too_large":
		return 30
	case "upstream_rate_limit":
		return 40
	case "client_deadline_reached", "timeout":
		return 50
	case "upstream_server_error":
		return 60
	case "upstream_no_response":
		return 65
	case "upstream_stream_interrupted":
		return 68
	case "network_error":
		return 70
	case "proxy_parse_error":
		return 80
	case "client_gone":
		return 90
	case "upstream_api_error":
		return 100
	case "provider_error":
		return 110
	default:
		return 120
	}
}

func attemptsSummary(attempts []attemptDiagnostic) string {
	if len(attempts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		label := providerModelLabel(attempt.Provider, attempt.Upstream)
		if label == "" {
			label = "unknown"
		}
		duration := ""
		if attempt.ElapsedMs > 0 {
			duration = " " + humanDurationMs(attempt.ElapsedMs)
		}
		category := strings.TrimSpace(attempt.Category)
		if category == "" {
			category = "provider_error"
		}
		detail := upstreamAttemptsSummary(attempt.UpstreamAttempts)
		if detail != "" {
			detail = " " + detail
		}
		parts = append(parts, label+duration+" "+category+detail)
	}
	return strings.Join(parts, " | ")
}

func upstreamAttemptsSummary(attempts []upstreamAttemptDiagnostic) string {
	if len(attempts) == 0 {
		return ""
	}
	last := attempts[len(attempts)-1]
	category := strings.TrimSpace(last.Category)
	if category == "" {
		category = "provider_error"
	}
	stage := strings.TrimSpace(last.Stage)
	if stage == "" && last.HTTPStatus > 0 {
		stage = "http_" + strconv.Itoa(last.HTTPStatus)
	}
	if stage == "" {
		return "(upstream_attempts=" + strconv.Itoa(len(attempts)) + " last=" + category + ")"
	}
	return "(upstream_attempts=" + strconv.Itoa(len(attempts)) + " last=" + stage + "/" + category + ")"
}

func humanDurationMs(ms float64) string {
	if ms < 1000 {
		return strconv.Itoa(int(ms+0.5)) + "ms"
	}
	if ms < 10000 {
		return strconv.FormatFloat(ms/1000, 'f', 1, 64) + "s"
	}
	return strconv.Itoa(int(ms/1000+0.5)) + "s"
}

func diagnosticHintForAttempts(category string, attempts []attemptDiagnostic) string {
	return diagnosticHint(category)
}

func providerModelLabel(providerName, upstreamModel string) string {
	switch {
	case providerName != "" && upstreamModel != "":
		return providerName + "/" + upstreamModel
	case providerName != "":
		return providerName
	default:
		return upstreamModel
	}
}

func newAttemptDiagnostic(providerName, upstreamModel string, elapsedMs float64, err error) attemptDiagnostic {
	message := ""
	if err != nil {
		message = sanitizeDiagnosticMessage(err.Error())
	}
	upstreamAttempts := newUpstreamAttemptDiagnostics(err)
	category := classifyProxyErrorFromErr(err, message)
	if refined := refinedCategoryFromUpstreamAttempts(upstreamAttempts, category); refined != "" {
		category = refined
	}
	return attemptDiagnostic{
		Provider:         providerName,
		Upstream:         upstreamModel,
		Category:         category,
		Message:          message,
		ElapsedMs:        elapsedMs,
		Peer:             networkPeerFromMessage(message),
		UpstreamAttempts: upstreamAttempts,
	}
}

func newUpstreamAttemptDiagnostics(err error) []upstreamAttemptDiagnostic {
	attempts := provider.UpstreamAttempts(err)
	if len(attempts) == 0 {
		return nil
	}
	out := make([]upstreamAttemptDiagnostic, 0, len(attempts))
	for _, attempt := range attempts {
		message := sanitizeDiagnosticMessage(attempt.Error)
		category := classifyUpstreamAttempt(attempt, message)
		if attempt.HTTPStatus >= http.StatusInternalServerError {
			category = "upstream_server_error"
		}
		out = append(out, upstreamAttemptDiagnostic{
			Stage:      strings.TrimSpace(attempt.Stage),
			Category:   category,
			ElapsedMs:  float64(attempt.Elapsed) / float64(time.Millisecond),
			HTTPStatus: attempt.HTTPStatus,
			Peer:       networkPeerFromMessage(message),
		})
	}
	return out
}

func classifyUpstreamAttempt(attempt provider.UpstreamAttempt, message string) string {
	// HTTP 状态码优先于错误文本。很多 provider 会把 JSON error body、
	// context canceled、网关文字混在一起，优先相信明确的 HTTP 语义可减少误报。
	if attempt.HTTPStatus >= http.StatusInternalServerError {
		return "upstream_server_error"
	}
	if attempt.HTTPStatus > 0 {
		return classifyProxyErrorFromErr(nil, "API 错误 "+strconv.Itoa(attempt.HTTPStatus)+": "+message)
	}
	category := classifyProxyErrorFromErr(nil, message)
	if category == "network_error" {
		switch strings.TrimSpace(attempt.Stage) {
		case "waiting_response_headers", "receiving_response_headers":
			// 这两个阶段说明请求已经写出，上游或中间网关没有给出完整响应头。
			// 用户看到的是间歇性 502，但根因不应再笼统写成“无法连接上游”。
			return "upstream_no_response"
		}
	}
	return category
}

func refinedCategoryFromUpstreamAttempts(attempts []upstreamAttemptDiagnostic, fallback string) string {
	if len(attempts) == 0 {
		return ""
	}
	// 外层错误文本可能只是“请求失败: EOF”，内层 stage 才知道 EOF 出现在
	// connecting 还是 waiting_response_headers。只用最后一次真实上游尝试修正
	// 外层类别，避免把早期已恢复的尝试误当成本次最终主因。
	last := attempts[len(attempts)-1]
	category := strings.TrimSpace(last.Category)
	if category == "" || category == fallback {
		return ""
	}
	return category
}

func firstAttemptNetworkPeer(attempts []attemptDiagnostic) string {
	for i := len(attempts) - 1; i >= 0; i-- {
		for j := len(attempts[i].UpstreamAttempts) - 1; j >= 0; j-- {
			if peer := strings.TrimSpace(attempts[i].UpstreamAttempts[j].Peer); peer != "" {
				return peer
			}
		}
		if peer := strings.TrimSpace(attempts[i].Peer); peer != "" {
			return peer
		}
	}
	return ""
}

func firstAttemptStreamState(attempts []attemptDiagnostic) string {
	for i := len(attempts) - 1; i >= 0; i-- {
		for j := len(attempts[i].UpstreamAttempts) - 1; j >= 0; j-- {
			if state := streamStateForUpstreamStage(attempts[i].UpstreamAttempts[j].Stage); state != "" {
				return state
			}
		}
	}
	return ""
}

func streamStateForUpstreamStage(stage string) string {
	// X-Proxy-Stream-State 既用于真实流式，也用于非流式失败诊断。
	// 这里把 provider 的 HTTP 阶段映射成已有日志字段，保证管理页、JSON store
	// 和响应头看到的是同一套阶段语义。
	switch strings.TrimSpace(stage) {
	case "resolving_dns":
		return "upstream_resolving_dns"
	case "connecting":
		return "upstream_connecting"
	case "tls_handshake":
		return "upstream_tls_handshake"
	case "writing_request":
		return "upstream_writing_request"
	case "waiting_response_headers":
		return "upstream_waiting_response_headers"
	case "receiving_response_headers":
		return "upstream_receiving_response_headers"
	default:
		return ""
	}
}

func sanitizeDiagnosticMessage(message string) string {
	message = secretTokenPattern.ReplaceAllString(message, `${1}<redacted>`)
	message = openAIKeyPattern.ReplaceAllString(message, "sk-<redacted>")
	message = useAIKeyPattern.ReplaceAllString(message, `${1}<redacted>`)
	if len(message) <= maxDiagnosticMessageBytes {
		return message
	}
	return message[:maxDiagnosticMessageBytes] + "...<truncated>"
}

func classifyProxyError(message string) string {
	return classifyProxyErrorFromErr(nil, message)
}

func classifyProxyErrorFromErr(err error, message string) string {
	lower := strings.ToLower(message)
	status, upstreamDetails := upstreamErrorDetails(err, message)
	switch {
	case isQuotaExhaustedError(upstreamDetails):
		return "upstream_quota_exhausted"
	case isInvalidAssistantMessageError(upstreamDetails):
		return "upstream_message_error"
	case strings.Contains(message, "上游流中断"):
		return "upstream_stream_interrupted"
	case strings.Contains(message, "在 finish_reason 或 [DONE] 之前结束"):
		return "upstream_stream_interrupted"
	case strings.Contains(message, "在 done=true 或 [DONE] 之前结束"):
		return "upstream_stream_interrupted"
	// 上游已经明确返回 HTTP 状态码时，优先相信状态码。
	// 某些链路会把 “API 错误 400/413 ... context canceled” 混在同一错误文本里，
	// 如果先匹配 context canceled，会把参数错误/大请求错误误判成 client_gone。
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "upstream_auth_error"
	case status == http.StatusTooManyRequests:
		return "upstream_rate_limit"
	case status == http.StatusRequestEntityTooLarge:
		return "upstream_payload_too_large"
	case status == http.StatusBadRequest || status == http.StatusNotFound:
		return "upstream_request_error"
	case status >= http.StatusInternalServerError:
		return "upstream_server_error"
	case errors.Is(err, context.Canceled) || strings.Contains(lower, "context canceled") || strings.Contains(lower, "client_gone"):
		return "client_gone"
	case strings.Contains(lower, "context deadline exceeded") || strings.Contains(lower, "timeout"):
		return "timeout"
	case strings.Contains(lower, "connect: connection refused") ||
		strings.Contains(lower, "no such host") ||
		strings.Contains(lower, "network is unreachable") ||
		strings.Contains(lower, "i/o timeout") ||
		strings.Contains(lower, "use of closed network connection") ||
		strings.Contains(lower, "connection reset by peer") ||
		strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "eof") ||
		strings.Contains(lower, "请求失败"):
		return "network_error"
	case strings.Contains(message, "API 错误") || strings.Contains(message, "Ollama 错误"):
		return "upstream_api_error"
	case strings.Contains(message, "解析响应失败") || strings.Contains(message, "读取响应失败"):
		return "proxy_parse_error"
	default:
		return "provider_error"
	}
}

func upstreamErrorDetails(err error, message string) (int, string) {
	status := upstreamHTTPStatus(message)
	parts := []string{strings.ToLower(message)}
	var metadata upstreamHTTPErrorMetadata
	if errors.As(err, &metadata) {
		if metadataStatus := metadata.UpstreamHTTPStatusCode(); metadataStatus > 0 {
			status = metadataStatus
		}
		if detail := structuredUpstreamErrorText(metadata.UpstreamHTTPErrorBody()); detail != "" {
			parts = append(parts, detail)
		}
	}
	return status, strings.Join(parts, " ")
}

func structuredUpstreamErrorText(body []byte) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Error) == 0 {
		return ""
	}
	var detail struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal(envelope.Error, &detail); err != nil {
		return ""
	}
	return strings.ToLower(strings.Join([]string{detail.Type, string(detail.Code), detail.Message}, " "))
}

func isQuotaExhaustedError(details string) bool {
	for _, marker := range []string{
		"access_terminated_error",
		"insufficient_quota",
		"quota_exceeded",
		"billing_hard_limit_reached",
		"usage limit for this billing cycle",
		"quota has been exhausted",
		"quota is exhausted",
		"额度已用完",
		"额度耗尽",
		"余额不足",
	} {
		if strings.Contains(details, marker) {
			return true
		}
	}
	return false
}

func isInvalidAssistantMessageError(details string) bool {
	assistantMessage := strings.Contains(details, "role 'assistant'") ||
		strings.Contains(details, `role \"assistant\"`) ||
		strings.Contains(details, "assistant message")
	emptyMessage := strings.Contains(details, "must not be empty") ||
		strings.Contains(details, "cannot be empty") ||
		strings.Contains(details, "empty message")
	return assistantMessage && emptyMessage
}

func upstreamHTTPStatus(message string) int {
	match := upstreamStatusCode.FindStringSubmatch(message)
	if len(match) != 2 {
		return 0
	}
	status, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return status
}

type userFacingDiagnostic struct {
	Reason string
	Action string
}

func userFacingDiagnosticFor(category string) userFacingDiagnostic {
	switch category {
	case "client_deadline_reached":
		return userFacingDiagnostic{Reason: "客户端等待超时", Action: "减少会话内容，或切换响应更快的模型。"}
	case "client_gone":
		return userFacingDiagnostic{Reason: "客户端已取消", Action: "重新发送；若反复出现，请新建会话。"}
	case "network_error":
		return userFacingDiagnostic{Reason: "无法连接上游", Action: "检查网络和上游地址后重试。"}
	case "upstream_quota_exhausted":
		return userFacingDiagnostic{Reason: "上游额度已用完", Action: "等待额度刷新，或充值/升级套餐。"}
	case "upstream_auth_error":
		return userFacingDiagnostic{Reason: "上游鉴权失败", Action: "检查 API Key、账号权限或余额。"}
	case "upstream_rate_limit":
		return userFacingDiagnostic{Reason: "请求过于频繁", Action: "同一模型短时间请求过多，请等待 15-30 秒后重试；若连续出现，建议切换模型或减少并发请求。"}
	case "upstream_payload_too_large":
		return userFacingDiagnostic{Reason: "请求内容过大", Action: "减少会话历史、文件或附件后重试。"}
	case "upstream_message_error":
		return userFacingDiagnostic{Reason: "会话消息无效", Action: "新建会话后重试。"}
	case "upstream_request_error":
		return userFacingDiagnostic{Reason: "上游不接受本次请求", Action: "检查模型名、Base URL 和不兼容参数。"}
	case "upstream_server_error":
		return userFacingDiagnostic{Reason: "上游服务暂不可用", Action: "稍后重试，或切换模型。"}
	case "upstream_no_response":
		return userFacingDiagnostic{Reason: "上游接收后未响应", Action: "稍后重试，或切换到更稳定的同模型渠道。"}
	case "upstream_stream_interrupted":
		return userFacingDiagnostic{Reason: "上游响应流中断", Action: "稍后重试，或切换到更稳定的同模型渠道。"}
	case "upstream_api_error":
		return userFacingDiagnostic{Reason: "上游拒绝请求", Action: "检查账号状态和模型名称。"}
	case "timeout":
		return userFacingDiagnostic{Reason: "上游响应超时", Action: "减少会话内容，或切换响应更快的模型。"}
	case "proxy_parse_error":
		return userFacingDiagnostic{Reason: "上游响应格式不兼容", Action: "新建会话；若仍失败，检查兼容档案或切换模型。"}
	default:
		return userFacingDiagnostic{Reason: "请求失败", Action: "检查上游配置后重试。"}
	}
}

func diagnosticHint(category string) string {
	return userFacingDiagnosticFor(category).Action
}

type logDiagnosticSummary struct {
	Reason  string
	Action  string
	Summary string
}

func summarizeLogDiagnostic(code string, statusCode int, elapsedMs float64, requestBytes int64, upstreamBytes int64, networkPeer string, streamState string, requestTools string, responseTools string) logDiagnosticSummary {
	code = strings.TrimSpace(code)
	if code == "" && statusCode < http.StatusBadRequest {
		return logDiagnosticSummary{}
	}
	if code == "" && statusCode >= http.StatusBadRequest {
		code = "provider_error"
	}
	requestSize := humanBytes(requestBytes)
	upstreamSize := humanBytes(upstreamBytes)
	copy := userFacingDiagnosticFor(code)
	prefix := copy.Reason
	switch code {
	case "upstream_payload_too_large":
		prefix = "上游拒绝大请求"
	case "upstream_quota_exhausted":
		prefix = "上游额度耗尽"
	case "upstream_rate_limit":
		prefix = "上游限流"
	case "upstream_server_error":
		prefix = "上游返回 5xx"
	case "upstream_no_response":
		prefix = "上游已接收但未返回响应头"
	case "upstream_stream_interrupted":
		prefix = "上游响应流中断"
	case "upstream_message_error":
		prefix = "上游拒绝会话消息"
	case "upstream_request_error":
		prefix = "上游拒绝请求"
	case "network_error":
		prefix = "上游连接失败"
	case "proxy_parse_error":
		prefix = "上游响应无法解析"
	}
	return logDiagnosticSummary{
		Reason:  copy.Reason,
		Action:  copy.Action,
		Summary: compactDiagnosticSummary(prefix, elapsedMs, requestSize, upstreamSize, networkPeer, streamState),
	}
}

func compactDiagnosticSummary(prefix string, elapsedMs float64, requestSize string, upstreamSize string, networkPeer string, streamState string) string {
	parts := []string{prefix}
	if elapsedMs > 0 {
		parts = append(parts, "耗时 "+strconv.Itoa(int(elapsedMs+0.5))+"ms")
	}
	if requestSize != "" {
		parts = append(parts, "请求体 "+requestSize)
	}
	if upstreamSize != "" {
		parts = append(parts, "上游体 "+upstreamSize)
	}
	if strings.TrimSpace(networkPeer) != "" {
		parts = append(parts, "远端 "+strings.TrimSpace(networkPeer))
	}
	if strings.TrimSpace(streamState) != "" {
		parts = append(parts, "流状态 "+strings.TrimSpace(streamState))
	}
	return strings.Join(parts, "；")
}

func humanBytes(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	if bytes < 1024 {
		return strconv.FormatInt(bytes, 10) + " B"
	}
	if bytes < 1024*1024 {
		return strconv.FormatFloat(float64(bytes)/1024, 'f', 1, 64) + " KB"
	}
	return strconv.FormatFloat(float64(bytes)/1024/1024, 'f', 2, 64) + " MB"
}

func networkPeerFromMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	arrow := strings.LastIndex(message, "->")
	if arrow < 0 {
		return ""
	}
	peer := strings.TrimSpace(message[arrow+2:])
	if peer == "" {
		return ""
	}
	if marker := strings.Index(peer, ": "); marker >= 0 {
		peer = peer[:marker]
	}
	if space := strings.IndexAny(peer, " \t\n\r"); space >= 0 {
		peer = peer[:space]
	}
	peer = strings.Trim(peer, `"'.,;()[]{}<>`)
	return peer
}
