package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// recovery_policy.go 集中定义“何时可以产生第二次上游请求 / 如何记账半截流失败”。
//
// 设计目标：
//  1. 避免在 server.go / provider.go 各处再开隐式放大出口；
//  2. 用一张阶段×类别边界表（见 TestRecoveryBoundaryMatrix）锁住默认策略；
//  3. 流式已向下游写出后，HTTP 状态码无法回滚，但 RequestLog / recent_stability 必须记失败。
//
// 边界原则（与 docs/16、docs/38–41 一致）：
//  - chat POST 非幂等：writing_request / waiting_response_headers 之后不换候选、不模式互切重放；
//  - client_gone / 鉴权 / 限流 / 413 / 明确请求错误：停止候选；
//  - Defense 关闭：不走流式/非流式互切；
//  - 跨 provider 默认已在 applyDefenseCandidatePolicy 截断为 1，本文件的 stop 列表是额外保险。

// largeRequestWarnBytes 是“大包”观测阈值（约 400KB）。
// 超过该值只打 WARN，不阻断请求；阈值来自 UseAI/DeepSeek 大请求踩坑经验，不是上游硬限制。
const largeRequestWarnBytes int64 = 400 * 1024

// shouldStopCandidateFallback 判断是否应停止尝试后续候选 provider/模型。
// 返回 true 表示当前失败已表明“请求可能已提交”或“继续换线只会放大错误/计费”。
func shouldStopCandidateFallback(category string) bool {
	switch strings.TrimSpace(category) {
	case "client_gone",
		"upstream_quota_exhausted",
		"upstream_auth_error",
		"upstream_rate_limit",
		"upstream_payload_too_large",
		"upstream_message_error",
		"upstream_request_error",
		"upstream_no_response",
		"upstream_stream_interrupted":
		return true
	default:
		return false
	}
}

// canAttemptAlternateChatMode 判断是否允许流式/非流式互相兜底（可能产生第二次上游请求）。
// 必须同时满足：防御开启、客户端未取消、provider 策略允许。
func canAttemptAlternateChatMode(cfg *config.AppConfig, ctx context.Context, err error) bool {
	return proxyDefenseEnabled(cfg) && ctx != nil && ctx.Err() == nil && provider.ShouldAttemptAlternateChatMode(err)
}

// markWrittenStreamFailure 在下游流已经开始写出后，把失败记入诊断头与内部 statusCode。
// 不会调用 WriteHeader：客户端侧 HTTP 状态通常已是 200，这是 HTTP 协议限制。
func markWrittenStreamFailure(w http.ResponseWriter, attempt attemptDiagnostic) {
	category := strings.TrimSpace(attempt.Category)
	if category == "" {
		category = "provider_error"
	}
	diag := userFacingDiagnosticFor(category)
	setProxyDiagnosticHeader(w, "X-Proxy-Error-Code", diagnosticHeaderValue(category))
	setProxyDiagnosticHeader(w, "X-Proxy-Error-Message", diagnosticHeaderValue(diag.Reason))
	setProxyDiagnosticHeader(w, "X-Proxy-Error-Hint", diagnosticHeaderValue(diag.Action))
	if summary := attemptsSummary([]attemptDiagnostic{attempt}); summary != "" {
		setProxyDiagnosticHeader(w, "X-Proxy-Attempts-Summary", diagnosticHeaderValue(summary))
	}
	if peer := firstAttemptNetworkPeer([]attemptDiagnostic{attempt}); peer != "" {
		setProxyDiagnosticHeader(w, "X-Proxy-Network-Peer", peer)
	}
	// 若 attempt 带来更精确的上游阶段，且当前还不是 downstream_started，可补充；
	// 半截流场景通常已是 downstream_started，setProxyDiagnosticHeader 会覆盖写字段。
	if state := firstAttemptStreamState([]attemptDiagnostic{attempt}); state != "" {
		// 不覆盖已有 downstream_started：下游已开始时保留“下游已写出”语义。
		if current := firstNonEmptyHeader(headerFromResponseWriter(w), "X-Proxy-Stream-State"); current != "downstream_started" {
			setProxyStreamState(w, state)
		}
	}
	setResponseWriterStatus(w, http.StatusBadGateway)
}

func setResponseWriterStatus(w http.ResponseWriter, status int) {
	switch target := w.(type) {
	case *responseWriter:
		target.statusCode = status
	case *streamAttemptWriter:
		setResponseWriterStatus(target.ResponseWriter, status)
	}
}

func headerFromResponseWriter(w http.ResponseWriter) http.Header {
	if w == nil {
		return nil
	}
	return w.Header()
}

// requestLogIsSuccess 统一“请求是否算成功”的口径。
// 规则：HTTP 状态 < 400 且没有明确 error_code。半截流失败会把内部 status 记为 502 并带 error_code。
func requestLogIsSuccess(statusCode int, errorCode string) bool {
	return statusCode < http.StatusBadRequest && strings.TrimSpace(errorCode) == ""
}

// shouldWarnLargeChatRequest 判断 chat 类路径是否应提示大请求体风险。
func shouldWarnLargeChatRequest(path string, requestBytes, upstreamBytes int64) bool {
	if !isChatProxyPath(path) {
		return false
	}
	return requestBytes >= largeRequestWarnBytes || upstreamBytes >= largeRequestWarnBytes
}

func isChatProxyPath(path string) bool {
	path = strings.TrimSpace(path)
	return path == "/v1/chat/completions" || path == "/api/chat"
}
