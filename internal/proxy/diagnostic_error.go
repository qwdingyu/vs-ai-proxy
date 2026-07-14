package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	case "upstream_auth_error":
		return 10
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
	hint := diagnosticHint(category)
	if len(attempts) == 0 {
		return hint
	}
	representative := representativeAttempt(category, attempts)
	if category == "network_error" {
		if representative.Peer != "" {
			return hint + " 本次连接的远端 peer 为 " + representative.Peer + "；如果这是 Cloudflare/CDN IP（如 104.21.x.x 或 172.67.x.x），说明错误发生在客户端到 CDN/边缘节点或边缘到源站链路，不能直接等同于 new-api 源站 IP。"
		}
		return hint
	}
	if category != "upstream_payload_too_large" {
		return hint
	}
	providerName := strings.TrimSpace(representative.Provider)
	upstreamModel := strings.TrimSpace(representative.Upstream)
	if providerName == "" && upstreamModel == "" {
		return hint
	}
	return hint + " 如果同一请求在其他 provider 可成功，这通常不是代理或 nginx 全局限制，而是当前 provider/channel 对该模型的上下文、工具声明或请求体大小限制；请优先检查 " + providerModelLabel(providerName, upstreamModel) + " 在上游网关中的模型映射、渠道组和 body/context 限制。"
}

func representativeAttempt(category string, attempts []attemptDiagnostic) attemptDiagnostic {
	// 诊断文案需要指出“哪个 provider/model 触发了这个主错误”。
	// 如果直接取最后一次 attempt，会把 413 等主因错误错误归属到后续失败的候选上。
	// 从后向前找同类 attempt，既保留最近同类样本，又避免张冠李戴。
	category = strings.TrimSpace(category)
	for i := len(attempts) - 1; i >= 0; i-- {
		if strings.TrimSpace(attempts[i].Category) == category {
			return attempts[i]
		}
	}
	return attempts[len(attempts)-1]
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
	status := upstreamHTTPStatus(message)
	switch {
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

func diagnosticHint(category string) string {
	switch category {
	case "client_gone":
		return "客户端已取消或断开连接；代理不会继续重试或切换 provider，避免把一次取消放大为多次上游请求。"
	case "network_error":
		return "网络或连接生命周期异常；检查 provider base_url、DNS/CDN、代理网络、云主机防火墙、上游连接是否被重置，以及客户端/上游是否在写入过程中关闭连接。"
	case "upstream_auth_error":
		return "请求已到达上游但鉴权失败；检查 API key 是否正确、是否过期，以及 provider 是否要求额外鉴权头。"
	case "upstream_rate_limit":
		return "请求已到达上游但触发限流或额度限制；检查账号余额、免费额度、并发限制，或等待冷却后重试。"
	case "upstream_payload_too_large":
		return "请求已到达上游但请求体或上下文过大；减少历史上下文、附件/文件内容，或确认该模型是否应路由到支持更大上下文的 provider。"
	case "upstream_request_error":
		return "请求已到达上游但参数或模型不可用；检查模型名、provider 类型、base_url 与请求参数治理。"
	case "upstream_server_error":
		return "请求已到达上游但上游服务异常；可切换 provider、等待上游恢复，或查看 provider 状态页。"
	case "upstream_api_error":
		return "请求已到达上游；检查 API key、余额、模型名是否被该 provider 支持，以及上游错误正文。"
	case "timeout":
		return "上游在客户端可等待窗口内没有完成响应；代理会在 VS/Copilot 约 100 秒上限前主动结束请求，避免用户等到客户端 499。请检查上游首 token 延迟、new-api/sub2api 渠道超时/轮换策略，或把模型 timeout_seconds 调得更短。"
	case "proxy_parse_error":
		return "上游返回格式不符合当前协议转换预期；需要保存响应样本进一步排查。"
	default:
		return "检查 provider 是否启用、API key 是否填写、模型名和 provider 类型是否匹配。"
	}
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
	withPressure := func(action string, summary string) logDiagnosticSummary {
		// 旧 VS Copilot session 反复失败、新 session 恢复时，最常见的强信号是请求体膨胀。
		// 这里不把它写成确定结论，只作为“优先排查方向”补到处理建议和摘要里。
		if note := sessionPressureDiagnosticNote(requestBytes, upstreamBytes); note != "" {
			action += " " + note
			summary += "；" + note
		}
		return logDiagnosticSummary{Action: action, Summary: summary}
	}
	withToolInterruption := func(diag logDiagnosticSummary) logDiagnosticSummary {
		// 这里补充的是排障解释，不改变客户端断开或超时的原始错误分类。
		if !toolInterruptionLikely(code, requestTools, responseTools) {
			return diag
		}
		interruptionState := "499/context canceled"
		if code == "timeout" {
			interruptionState = "代理/上游超时预算"
		}
		note := fmt.Sprintf(
			"本次请求声明了工具但响应未完整返回工具调用；结合 %s，优先判断为模型/渠道响应过慢或客户端提前断开，而不是工具未注册。",
			interruptionState,
		)
		if diag.Action != "" {
			diag.Action += " " + note
		} else {
			diag.Action = note
		}
		if diag.Summary != "" {
			diag.Summary += "；" + note
		} else {
			diag.Summary = note
		}
		return diag
	}
	switch code {
	case "client_deadline_reached":
		diag := withPressure("查上游首 token、new-api/sub2api 单渠道超时和轮换；必要时减少上下文。", compactDiagnosticSummary("VS/Copilot 接近等待上限后取消", elapsedMs, requestSize, upstreamSize, networkPeer, streamState))
		return withToolInterruption(logDiagnosticSummary{
			Reason:  "客户端等待上限",
			Action:  diag.Action,
			Summary: diag.Summary,
		})
	case "client_gone":
		diag := withPressure("若耗时很短，多为用户取消/窗口关闭；若接近 100 秒，按客户端等待上限排查。", compactDiagnosticSummary("客户端在响应完成前断开", elapsedMs, requestSize, upstreamSize, networkPeer, streamState))
		return withToolInterruption(logDiagnosticSummary{
			Reason:  "客户端主动断开",
			Action:  diag.Action,
			Summary: diag.Summary,
		})
	case "timeout":
		diag := withPressure("检查模型 timeout_seconds、上游首 token 延迟和网关单渠道超时。", compactDiagnosticSummary("请求超过有效超时预算", elapsedMs, requestSize, upstreamSize, networkPeer, streamState))
		return withToolInterruption(logDiagnosticSummary{
			Reason:  "代理/上游超时",
			Action:  diag.Action,
			Summary: diag.Summary,
		})
	case "upstream_payload_too_large":
		diag := withPressure("减少历史上下文/文件内容，或切到支持更大上下文和工具声明的 provider/channel。", compactDiagnosticSummary("上游返回 413 或等价大请求错误", elapsedMs, requestSize, upstreamSize, networkPeer, streamState))
		return logDiagnosticSummary{
			Reason:  "上游拒绝大请求",
			Action:  diag.Action,
			Summary: diag.Summary,
		}
	case "upstream_rate_limit":
		return logDiagnosticSummary{
			Reason:  "上游限流/额度",
			Action:  "检查账号额度、并发限制、429 冷却和 new-api 渠道限流配置。",
			Summary: compactDiagnosticSummary("上游返回 429", elapsedMs, requestSize, upstreamSize, networkPeer, streamState),
		}
	case "upstream_server_error":
		diag := withPressure("检查 new-api/sub2api 渠道健康、上游状态页和网关重试/轮换策略。", compactDiagnosticSummary("上游返回 5xx", elapsedMs, requestSize, upstreamSize, networkPeer, streamState))
		return logDiagnosticSummary{
			Reason:  "上游服务异常",
			Action:  diag.Action,
			Summary: diag.Summary,
		}
	case "upstream_request_error":
		return logDiagnosticSummary{
			Reason:  "上游拒绝参数/模型",
			Action:  "检查模型名、provider 绑定、base_url 和不兼容参数治理。",
			Summary: compactDiagnosticSummary("上游返回 400/404", elapsedMs, requestSize, upstreamSize, networkPeer, streamState),
		}
	case "upstream_auth_error":
		return logDiagnosticSummary{
			Reason:  "上游鉴权失败",
			Action:  "检查 API key、余额、权限和 provider 所需鉴权头。",
			Summary: compactDiagnosticSummary("上游返回 401/403", elapsedMs, requestSize, upstreamSize, networkPeer, streamState),
		}
	case "network_error":
		diag := withPressure("检查本机网络、DNS/CDN、Cloudflare/WAF、源站连接和代理链路。", compactDiagnosticSummary("连接被关闭、重置或网络不可达", elapsedMs, requestSize, upstreamSize, networkPeer, streamState))
		return logDiagnosticSummary{
			Reason:  "网络/CDN/连接异常",
			Action:  diag.Action,
			Summary: diag.Summary,
		}
	case "proxy_parse_error":
		diag := withPressure("保存响应样本，检查是否 stream=false 返回 SSE、HTML 错误页或非 JSON 内容。", compactDiagnosticSummary("代理无法按协议解析上游响应", elapsedMs, requestSize, upstreamSize, networkPeer, streamState))
		return logDiagnosticSummary{
			Reason:  "上游响应格式异常",
			Action:  diag.Action,
			Summary: diag.Summary,
		}
	}
	return logDiagnosticSummary{
		Reason:  "未分类请求失败",
		Action:  "查看错误正文、provider/model/upstream、请求体大小和工具声明。",
		Summary: compactDiagnosticSummary("请求失败，需结合错误正文排查", elapsedMs, requestSize, upstreamSize, networkPeer, streamState),
	}
}

func sessionPressureDiagnosticNote(requestBytes, upstreamBytes int64) string {
	// requestBytes 是客户端发到代理的原始体积，upstreamBytes 是代理治理参数后发往上游的体积。
	// 取两者较大值作为“会话压力”估计，避免参数治理或工具声明转换后体积变化导致误判。
	pressureBytes := requestBytes
	if upstreamBytes > pressureBytes {
		pressureBytes = upstreamBytes
	}
	if pressureBytes <= 0 {
		return ""
	}
	const (
		// 这些阈值不是模型上下文硬限制，而是运维诊断阈值：
		// 达到 512KB 后，VS/Copilot 历史、文件内容和工具声明已经足以显著放大首 token 延迟和链路中断概率。
		largeContextThreshold      = 512 * 1024
		extraLargeContextThreshold = 1024 * 1024
	)
	if pressureBytes < largeContextThreshold {
		return ""
	}
	if pressureBytes >= extraLargeContextThreshold {
		return fmt.Sprintf("本次请求体/上游体约 %s，属于超大上下文；如果新建 session 后恢复，优先怀疑旧 session 历史膨胀、文件堆积或状态污染。", humanBytes(pressureBytes))
	}
	return fmt.Sprintf("本次请求体/上游体约 %s，属于大上下文；如果新建 session 后恢复，优先怀疑旧 session 历史膨胀、文件堆积或状态污染。", humanBytes(pressureBytes))
}

func toolInterruptionLikely(code string, requestTools string, responseTools string) bool {
	// 只在“客户端断开/等待上限/超时 + 请求声明了工具 + 响应没有工具”时提示工具调用中断。
	// 这避免把普通上游 4xx/5xx 误判成 create_file 未注册，也避免在已经返回 response_tools
	// 的成功工具调用上重复追加干扰性说明。
	code = strings.TrimSpace(code)
	if code != "client_gone" && code != "client_deadline_reached" && code != "timeout" {
		return false
	}
	if strings.TrimSpace(responseTools) != "" {
		return false
	}
	lower := strings.ToLower(requestTools)
	for _, name := range []string{"create_file", "apply_patch", "edit_file", "get_file", "grep_search", "code_search", "run_command_in_terminal", "powershell", "git"} {
		if strings.Contains(lower, name) {
			return true
		}
	}
	return false
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
