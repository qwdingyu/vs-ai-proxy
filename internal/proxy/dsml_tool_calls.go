package proxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

var (
	dsmlToolCallsBlockPattern = regexp.MustCompile(`(?s)<｜DSML｜tool_calls>\s*(.*?)\s*</｜DSML｜tool_calls>`)
	dsmlInvokePattern         = regexp.MustCompile(`(?s)<｜DSML｜invoke\s+name="([^"]+)">\s*(.*?)\s*</｜DSML｜invoke>`)
	dsmlParameterPattern      = regexp.MustCompile(`(?s)<｜DSML｜parameter\s+name="([^"]+)"(?:\s+[^>]*)?>(.*?)</｜DSML｜parameter>`)
)

func normalizeDSMLToolCallsInChatResponse(resp *provider.ChatResponse) {
	if resp == nil {
		return
	}
	for i := range resp.Choices {
		msg := &resp.Choices[i].Message
		if len(msg.ToolCalls) > 0 {
			continue
		}
		calls, cleaned := parseDSMLToolCalls(msg.Content)
		if len(calls) == 0 {
			continue
		}
		msg.Content = cleaned
		msg.ToolCalls = calls
		resp.Choices[i].FinishReason = "tool_calls"
	}
}

func parseDSMLToolCalls(content string) ([]provider.ToolCall, string) {
	matches := dsmlToolCallsBlockPattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil, content
	}
	calls := []provider.ToolCall{}
	for _, match := range matches {
		for _, invoke := range dsmlInvokePattern.FindAllStringSubmatch(match[1], -1) {
			name := strings.TrimSpace(invoke[1])
			if name == "" {
				continue
			}
			arguments := map[string]string{}
			for _, param := range dsmlParameterPattern.FindAllStringSubmatch(invoke[2], -1) {
				paramName := strings.TrimSpace(param[1])
				if paramName == "" {
					continue
				}
				arguments[paramName] = decodeDSMLOutput(strings.TrimSpace(param[2]))
			}
			argumentBytes, err := json.Marshal(arguments)
			if err != nil {
				continue
			}
			calls = append(calls, provider.ToolCall{
				ID:   fmt.Sprintf("dsml_call_%d", len(calls)+1),
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
	cleaned := strings.TrimSpace(dsmlToolCallsBlockPattern.ReplaceAllString(content, ""))
	return calls, cleaned
}

func decodeDSMLOutput(value string) string {
	return strings.NewReplacer(
		"&quot;", `"`,
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
	).Replace(value)
}

func normalizeDSMLToolCallsInOllamaJSON(body []byte) []byte {
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
	toolCalls, cleaned := parseDSMLToolCalls(content)
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
