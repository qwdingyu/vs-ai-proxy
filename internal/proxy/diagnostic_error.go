package proxy

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

const maxDiagnosticMessageBytes = 1000

var (
	secretTokenPattern = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{12,}`)
	openAIKeyPattern   = regexp.MustCompile(`sk-[A-Za-z0-9_-]{12,}`)
	useAIKeyPattern    = regexp.MustCompile(`(?i)(api[_-]?key["'\s:=]+)[A-Za-z0-9._~+/=-]{12,}`)
)

type attemptDiagnostic struct {
	Provider string `json:"provider"`
	Upstream string `json:"upstream_model"`
	Category string `json:"category"`
	Message  string `json:"message"`
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
	return proxyDiagnosticError{
		Message: "所有候选提供商请求均失败",
		Type:    "upstream_error",
		Code:    category,
		Details: proxyDiagnosticDetails{
			RequestedModel: requestedModel,
			ResolvedModel:  resolvedModel,
			CandidateCount: candidateCount,
			Attempts:       attempts,
			Hint:           diagnosticHint(category),
		},
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
		Category: classifyProxyError(message),
		Message:  message,
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
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "context deadline exceeded") || strings.Contains(lower, "timeout"):
		return "timeout"
	case strings.Contains(lower, "connect: connection refused") ||
		strings.Contains(lower, "no such host") ||
		strings.Contains(lower, "network is unreachable") ||
		strings.Contains(lower, "i/o timeout") ||
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

func diagnosticHint(category string) string {
	switch category {
	case "network_error":
		return "检查 provider base_url、DNS、代理网络、云主机防火墙，或上游连接是否被重置。"
	case "upstream_api_error":
		return "请求已到达上游；检查 API key、余额、模型名是否被该 provider 支持，以及上游错误正文。"
	case "timeout":
		return "上游响应超时；可检查网络延迟、provider 状态，或在模型配置中调整 timeout_seconds。"
	case "proxy_parse_error":
		return "上游返回格式不符合当前协议转换预期；需要保存响应样本进一步排查。"
	default:
		return "检查 provider 是否启用、API key 是否填写、模型名和 provider 类型是否匹配。"
	}
}
