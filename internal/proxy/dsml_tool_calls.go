package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

var (
	dsmlToolCallsBlockPattern  = regexp.MustCompile(`(?s)<｜DSML｜tool_calls>\s*(.*?)\s*</｜DSML｜tool_calls>`)
	dsmlInvokePattern          = regexp.MustCompile(`(?s)<｜DSML｜invoke\s+name="([^"]+)">\s*(.*?)\s*</｜DSML｜invoke>`)
	dsmlParameterPattern       = regexp.MustCompile(`(?s)<｜DSML｜parameter\s+([^>]*)>(.*?)</｜DSML｜parameter>`)
	dsmlNameAttributePattern   = regexp.MustCompile(`(?:^|\s)name="([^"]+)"`)
	dsmlStringAttributePattern = regexp.MustCompile(`(?:^|\s)string="([^"]+)"`)
)

const (
	maxDSMLContentBytes   = 1024 * 1024
	maxDSMLToolCalls      = 32
	maxDSMLParameters     = 64
	maxDSMLArgumentsBytes = 256 * 1024
)

func normalizeDSMLToolCallsInChatResponse(resp *provider.ChatResponse, allowedTools map[string]struct{}) {
	if resp == nil || len(allowedTools) == 0 {
		return
	}
	for i := range resp.Choices {
		msg := &resp.Choices[i].Message
		if len(msg.ToolCalls) > 0 || msg.FunctionCall != nil {
			if sanitizeExecutableToolCalls(msg, allowedTools) {
				resp.Choices[i].FinishReason = visualStudioFinishReason(resp.Choices[i].FinishReason)
			}
			if len(msg.ToolCalls) > 0 || msg.FunctionCall != nil {
				continue
			}
		}
		calls, cleaned := parseDSMLToolCalls(msg.Content, allowedTools)
		if len(calls) == 0 {
			continue
		}
		msg.Content = cleaned
		msg.ToolCalls = calls
		resp.Choices[i].FinishReason = "tool_calls"
	}
}

func sanitizeExecutableToolCalls(msg *provider.Message, allowedTools map[string]struct{}) bool {
	if msg == nil || len(allowedTools) == 0 {
		return false
	}
	removed := []string{}
	kept := msg.ToolCalls[:0]
	for _, call := range msg.ToolCalls {
		name := strings.TrimSpace(call.Function.Name)
		if isAllowedDSMLTool(name, allowedTools) {
			kept = append(kept, call)
			continue
		}
		if name == "" {
			name = "<empty>"
		}
		removed = append(removed, name)
	}
	msg.ToolCalls = kept
	if msg.FunctionCall != nil {
		name := strings.TrimSpace(msg.FunctionCall.Name)
		if !isAllowedDSMLTool(name, allowedTools) {
			if name == "" {
				name = "<empty>"
			}
			removed = append(removed, name)
			msg.FunctionCall = nil
		}
	}
	if len(removed) == 0 {
		return false
	}
	msg.Content = appendToolSanitizationNotice(msg.Content, removed)
	return true
}

func appendToolSanitizationNotice(content string, removed []string) string {
	unique := uniqueSortedToolNames(removed)
	notice := "[Proxy blocked undeclared tool calls: " + strings.Join(unique, ",") + "]"
	content = strings.TrimSpace(content)
	if content == "" {
		return notice
	}
	return content + "\n" + notice
}

func parseDSMLToolCalls(content string, allowedTools map[string]struct{}) ([]provider.ToolCall, string) {
	if len(content) == 0 || len(content) > maxDSMLContentBytes || len(allowedTools) == 0 {
		return nil, content
	}
	canonical := canonicalizeDSML(content)
	matches := dsmlToolCallsBlockPattern.FindAllStringSubmatch(canonical, -1)
	if len(matches) == 0 {
		return nil, content
	}
	calls := []provider.ToolCall{}
	for _, match := range matches {
		invokes := dsmlInvokePattern.FindAllStringSubmatch(match[1], -1)
		if len(invokes) == 0 || len(calls)+len(invokes) > maxDSMLToolCalls {
			return nil, content
		}
		for _, invoke := range invokes {
			name := strings.TrimSpace(invoke[1])
			if name == "" || !isAllowedDSMLTool(name, allowedTools) {
				return nil, content
			}
			arguments := map[string]any{}
			params := dsmlParameterPattern.FindAllStringSubmatch(invoke[2], -1)
			if len(params) > maxDSMLParameters {
				return nil, content
			}
			for _, param := range params {
				attrs := param[1]
				nameMatch := dsmlNameAttributePattern.FindStringSubmatch(attrs)
				if len(nameMatch) != 2 {
					return nil, content
				}
				paramName := strings.TrimSpace(nameMatch[1])
				if paramName == "" {
					return nil, content
				}
				if _, duplicate := arguments[paramName]; duplicate {
					return nil, content
				}
				stringValue := true
				if stringMatch := dsmlStringAttributePattern.FindStringSubmatch(attrs); len(stringMatch) == 2 {
					stringValue = !strings.EqualFold(strings.TrimSpace(stringMatch[1]), "false")
				}
				arguments[paramName] = parseDSMLParameterValue(strings.TrimSpace(param[2]), stringValue)
			}
			argumentBytes, err := json.Marshal(arguments)
			if err != nil || len(argumentBytes) > maxDSMLArgumentsBytes {
				return nil, content
			}
			calls = append(calls, provider.ToolCall{
				ID:   dsmlToolCallID(canonical, len(calls)+1),
				Type: "function",
				Function: provider.FunctionCall{
					Name:      name,
					Arguments: string(argumentBytes),
				},
			})
		}
	}
	if len(calls) == 0 {
		return nil, content
	}
	cleaned := strings.TrimSpace(dsmlToolCallsBlockPattern.ReplaceAllString(canonical, ""))
	return calls, cleaned
}

func canonicalizeDSML(content string) string {
	return strings.NewReplacer(
		"<|DSML|", "<｜DSML｜",
		"</|DSML|", "</｜DSML｜",
	).Replace(content)
}

func isAllowedDSMLTool(name string, allowedTools map[string]struct{}) bool {
	if len(allowedTools) == 0 {
		return false
	}
	_, ok := allowedTools[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func parseDSMLParameterValue(value string, stringValue bool) any {
	decoded := html.UnescapeString(value)
	if stringValue {
		return decoded
	}
	if parsed, err := strconv.ParseInt(decoded, 10, 64); err == nil {
		return parsed
	}
	if parsed, err := strconv.ParseFloat(decoded, 64); err == nil {
		return parsed
	}
	if parsed, err := strconv.ParseBool(decoded); err == nil {
		return parsed
	}
	if strings.EqualFold(decoded, "null") {
		return nil
	}
	return decoded
}

func dsmlToolCallID(content string, index int) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("dsml_%s_%d", hex.EncodeToString(sum[:6]), index)
}

func allowedToolNames(req *provider.ChatRequest) map[string]struct{} {
	allowed := map[string]struct{}{}
	if req == nil {
		return allowed
	}
	for _, tool := range req.Tools {
		name := strings.ToLower(strings.TrimSpace(tool.Function.Name))
		if name != "" {
			allowed[name] = struct{}{}
		}
	}
	if raw := req.Extra["functions"]; len(raw) > 0 {
		var functions []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &functions) == nil {
			for _, function := range functions {
				name := strings.ToLower(strings.TrimSpace(function.Name))
				if name != "" {
					allowed[name] = struct{}{}
				}
			}
		}
	}
	return allowed
}

func normalizeDSMLToolCallsInOllamaJSON(body []byte, allowedTools map[string]struct{}) []byte {
	if len(allowedTools) == 0 {
		return body
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	message, _ := root["message"].(map[string]any)
	if message == nil {
		return body
	}
	if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
		return body
	}
	content, _ := message["content"].(string)
	toolCalls, cleaned := parseDSMLToolCalls(content, allowedTools)
	if len(toolCalls) == 0 {
		return body
	}
	message["content"] = cleaned
	message["tool_calls"] = toolCalls
	root["done_reason"] = "tool_calls"
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}
