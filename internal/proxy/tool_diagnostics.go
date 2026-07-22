package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

const maxToolDiagnosticNames = 8

func requestToolSummary(req *provider.ChatRequest) string {
	if req == nil {
		return ""
	}
	names := []string{}
	for _, tool := range req.Tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name != "" {
			names = append(names, name)
		}
	}
	if raw := req.Extra["functions"]; len(raw) > 0 {
		var functions []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &functions) == nil {
			for _, function := range functions {
				name := strings.TrimSpace(function.Name)
				if name != "" {
					names = append(names, name)
				}
			}
		}
	}
	return formatToolSummary("declared", names)
}

func responseToolSummaryFromChatResponse(resp *provider.ChatResponse) string {
	if resp == nil {
		return ""
	}
	names := []string{}
	for _, choice := range resp.Choices {
		for _, call := range choice.Message.ToolCalls {
			name := strings.TrimSpace(call.Function.Name)
			if name != "" {
				names = append(names, name)
			}
		}
		if choice.Message.FunctionCall != nil {
			name := strings.TrimSpace(choice.Message.FunctionCall.Name)
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return formatToolSummary("returned", names)
}

func responseToolSummaryFromRawOpenAIJSON(body []byte) string {
	var resp provider.ChatResponse
	if json.Unmarshal(body, &resp) != nil {
		return ""
	}
	return responseToolSummaryFromChatResponse(&resp)
}

func toolSummaryFromAccumulator(acc *streamReasoningAccumulator) string {
	if acc == nil || !acc.hasToolCalls {
		return ""
	}
	if len(acc.toolCallNames) > 0 {
		return formatToolSummary("streamed", acc.toolCallNames)
	}
	return "streamed: count>=1"
}

func formatToolSummary(prefix string, names []string) string {
	unique := uniqueSortedToolNames(names)
	if len(unique) == 0 {
		return ""
	}
	shown := unique
	if len(shown) > maxToolDiagnosticNames {
		shown = shown[:maxToolDiagnosticNames]
	}
	summary := fmt.Sprintf("%s: %s", prefix, strings.Join(shown, ","))
	if len(unique) > len(shown) {
		summary += fmt.Sprintf(" +%d", len(unique)-len(shown))
	}
	return summary
}

func uniqueSortedToolNames(names []string) []string {
	seen := map[string]string{}
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, exists := seen[key]; !exists {
			seen[key] = trimmed
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, seen[key])
	}
	return out
}

func setRequestToolDiagnosticHeader(w http.ResponseWriter, req *provider.ChatRequest) {
	if summary := requestToolSummary(req); summary != "" {
		setProxyDiagnosticHeader(w, "X-Proxy-Request-Tools", summary)
	}
}

func setResponseToolDiagnosticHeader(w http.ResponseWriter, resp *provider.ChatResponse) {
	if summary := responseToolSummaryFromChatResponse(resp); summary != "" {
		setProxyDiagnosticHeader(w, "X-Proxy-Response-Tools", summary)
	}
}

func setToolOutcomeDiagnosticHeader(w http.ResponseWriter, req *provider.ChatRequest, resp *provider.ChatResponse) {
	if !requestDeclaresTools(req) || !chatResponseWasTruncatedWithoutTools(resp) {
		return
	}
	setProxyDiagnosticHeader(w, "X-Proxy-Tool-Outcome", "truncated_no_tools")
}

func setRawResponseToolDiagnosticHeader(w http.ResponseWriter, body []byte) {
	if summary := responseToolSummaryFromRawOpenAIJSON(body); summary != "" {
		setProxyDiagnosticHeader(w, "X-Proxy-Response-Tools", summary)
	}
}

func setRawToolOutcomeDiagnosticHeader(w http.ResponseWriter, req *provider.ChatRequest, body []byte) {
	if !requestDeclaresTools(req) {
		return
	}
	var resp provider.ChatResponse
	if json.Unmarshal(body, &resp) != nil {
		return
	}
	setToolOutcomeDiagnosticHeader(w, req, &resp)
}

func setStreamToolDiagnosticHeader(w http.ResponseWriter, acc *streamReasoningAccumulator) {
	if summary := toolSummaryFromAccumulator(acc); summary != "" {
		setProxyDiagnosticHeader(w, "X-Proxy-Response-Tools", summary)
	}
}

func setStreamToolOutcomeDiagnosticHeader(w http.ResponseWriter, req *provider.ChatRequest, acc *streamReasoningAccumulator) {
	if !requestDeclaresTools(req) || acc == nil || acc.hasToolCalls {
		return
	}
	if strings.EqualFold(strings.TrimSpace(acc.finishReason), "length") {
		setProxyDiagnosticHeader(w, "X-Proxy-Tool-Outcome", "truncated_no_tools")
	}
}

func chatResponseWasTruncatedWithoutTools(resp *provider.ChatResponse) bool {
	if resp == nil || len(resp.Choices) == 0 {
		return false
	}
	sawLength := false
	for _, choice := range resp.Choices {
		if len(choice.Message.ToolCalls) > 0 || choice.Message.FunctionCall != nil {
			return false
		}
		if strings.EqualFold(strings.TrimSpace(choice.FinishReason), "length") {
			sawLength = true
		}
	}
	return sawLength
}

func setProxyFallbackMode(w http.ResponseWriter, mode string) {
	setProxyDiagnosticHeader(w, "X-Proxy-Fallback-Mode", mode)
}

func setProxyToolNormalization(w http.ResponseWriter, mode string) {
	setProxyDiagnosticHeader(w, "X-Proxy-Tool-Call-Normalization", mode)
}

func setProxyStreamState(w http.ResponseWriter, state string) {
	setProxyDiagnosticHeader(w, "X-Proxy-Stream-State", state)
}

func setTimeoutDiagnostic(w http.ResponseWriter, configuredTimeoutSeconds, effectiveTimeoutSeconds int) {
	if configuredTimeoutSeconds > 0 {
		setProxyDiagnosticHeader(w, "X-Proxy-Configured-Timeout-Seconds", strconv.Itoa(configuredTimeoutSeconds))
	}
	if effectiveTimeoutSeconds > 0 {
		setProxyDiagnosticHeader(w, "X-Proxy-Effective-Timeout-Seconds", strconv.Itoa(effectiveTimeoutSeconds))
	}
}

func setProxyDiagnosticHeader(w http.ResponseWriter, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" || w == nil {
		return
	}
	w.Header().Set(key, value)
	setResponseWriterDiagnosticField(w, key, value)
}

func setResponseWriterDiagnosticField(w http.ResponseWriter, key, value string) {
	switch target := w.(type) {
	case *responseWriter:
		switch key {
		case "X-Proxy-Request-Tools":
			target.requestTools = value
		case "X-Proxy-Response-Tools":
			target.responseTools = value
		case "X-Proxy-Tool-Outcome":
			target.toolOutcome = value
		case "X-Proxy-Fallback-Mode":
			target.fallbackMode = value
		case "X-Proxy-Tool-Call-Normalization":
			target.normalization = value
		case "X-Proxy-Stream-State":
			target.streamState = value
		case "X-Proxy-Configured-Timeout-Seconds":
			target.configuredTimeoutSeconds = positiveAtoi(value)
		case "X-Proxy-Effective-Timeout-Seconds":
			target.effectiveTimeoutSeconds = positiveAtoi(value)
		}
	case *streamAttemptWriter:
		setResponseWriterDiagnosticField(target.ResponseWriter, key, value)
	}
}

func positiveAtoi(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}
