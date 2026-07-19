package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
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
		w.Header().Set("X-Proxy-Attempts-Summary", diagnosticHeaderValue(summary))
	}
	if peer := firstAttemptNetworkPeer(diag.Details.Attempts); peer != "" {
		w.Header().Set("X-Proxy-Network-Peer", peer)
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
		parts = append(parts, label+duration+" "+category)
	}
	return strings.Join(parts, " | ")
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
	return attemptDiagnostic{
		Provider:  providerName,
		Upstream:  upstreamModel,
		Category:  classifyProxyErrorFromErr(err, message),
		Message:   message,
		ElapsedMs: elapsedMs,
		Peer:      networkPeerFromMessage(message),
	}
}

func firstAttemptNetworkPeer(attempts []attemptDiagnostic) string {
	for i := len(attempts) - 1; i >= 0; i-- {
		if peer := strings.TrimSpace(attempts[i].Peer); peer != "" {
			return peer
		}
	}
	return ""
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
