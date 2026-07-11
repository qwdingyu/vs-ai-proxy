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
	Provider string `json:"provider"`
	Upstream string `json:"upstream_model"`
	Category string `json:"category"`
	Message  string `json:"message"`
	Peer     string `json:"network_peer,omitempty"`
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
	w.Header().Set("X-Proxy-Error-Code", diag.Code)
	w.Header().Set("X-Proxy-Error-Message", sanitizeDiagnosticMessage(diag.Message))
	w.Header().Set("X-Proxy-Error-Hint", sanitizeDiagnosticMessage(diag.Details.Hint))
	if peer := firstAttemptNetworkPeer(diag.Details.Attempts); peer != "" {
		w.Header().Set("X-Proxy-Network-Peer", peer)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": diag})
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
	category := "provider_error"
	if len(attempts) > 0 {
		category = attempts[len(attempts)-1].Category
	}
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

func diagnosticHintForAttempts(category string, attempts []attemptDiagnostic) string {
	hint := diagnosticHint(category)
	if len(attempts) == 0 {
		return hint
	}
	last := attempts[len(attempts)-1]
	if category == "network_error" {
		if last.Peer != "" {
			return hint + " 本次连接的远端 peer 为 " + last.Peer + "；如果这是 Cloudflare/CDN IP（如 104.21.x.x 或 172.67.x.x），说明错误发生在客户端到 CDN/边缘节点或边缘到源站链路，不能直接等同于 new-api 源站 IP。"
		}
		return hint
	}
	if category != "upstream_payload_too_large" {
		return hint
	}
	providerName := strings.TrimSpace(last.Provider)
	upstreamModel := strings.TrimSpace(last.Upstream)
	if providerName == "" && upstreamModel == "" {
		return hint
	}
	return hint + " 如果同一请求在其他 provider 可成功，这通常不是代理或 nginx 全局限制，而是当前 provider/channel 对该模型的上下文、工具声明或请求体大小限制；请优先检查 " + providerModelLabel(providerName, upstreamModel) + " 在上游网关中的模型映射、渠道组和 body/context 限制。"
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

func newAttemptDiagnostic(providerName, upstreamModel string, err error) attemptDiagnostic {
	message := ""
	if err != nil {
		message = sanitizeDiagnosticMessage(err.Error())
	}
	return attemptDiagnostic{
		Provider: providerName,
		Upstream: upstreamModel,
		Category: classifyProxyErrorFromErr(err, message),
		Message:  message,
		Peer:     networkPeerFromMessage(message),
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
	case errors.Is(err, context.Canceled) || strings.Contains(lower, "context canceled") || strings.Contains(lower, "client_gone"):
		return "client_gone"
	case strings.Contains(lower, "context deadline exceeded") || strings.Contains(lower, "timeout"):
		return "timeout"
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
