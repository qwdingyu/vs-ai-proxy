package proxy

import (
	"encoding/json"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

var toolAliasTargets = copilotToolAliasTargets()

func canonicalToolName(name string, allowedTools map[string]struct{}) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || len(allowedTools) == 0 {
		return trimmed
	}
	key := strings.ToLower(trimmed)
	if declared, ok := declaredToolName(key, allowedTools); ok {
		return declared
	}
	for _, target := range toolAliasTargets[key] {
		if declared, ok := declaredToolName(target, allowedTools); ok {
			return declared
		}
	}
	return trimmed
}

// declaredToolName 使用大小写不敏感匹配，但返回请求中声明的原始拼写。
// Visual Studio 会用声明表按精确名称查找工具，返回全小写名称可能导致工具查找失败。
func declaredToolName(name string, allowedTools map[string]struct{}) (string, bool) {
	for declared := range allowedTools {
		if strings.EqualFold(strings.TrimSpace(declared), strings.TrimSpace(name)) {
			return strings.TrimSpace(declared), true
		}
	}
	return "", false
}

func canonicalizeFunctionCallName(function *provider.FunctionCall, allowedTools map[string]struct{}) bool {
	if function == nil {
		return false
	}
	canonical := canonicalToolName(function.Name, allowedTools)
	if strings.TrimSpace(function.Name) == strings.TrimSpace(canonical) {
		return false
	}
	function.Name = canonical
	return true
}

func canonicalizeProviderToolCallNames(calls []provider.ToolCall, allowedTools map[string]struct{}) bool {
	changed := false
	for i := range calls {
		if canonicalizeFunctionCallName(&calls[i].Function, allowedTools) {
			changed = true
		}
	}
	return changed
}

func canonicalizeRawFunctionName(function map[string]any, allowedTools map[string]struct{}) bool {
	if function == nil {
		return false
	}
	name, _ := function["name"].(string)
	canonical := canonicalToolName(name, allowedTools)
	if strings.TrimSpace(name) == strings.TrimSpace(canonical) {
		return false
	}
	function["name"] = canonical
	return true
}

func canonicalizeRawToolCallNames(calls []any, allowedTools map[string]struct{}) bool {
	changed := false
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		function, _ := call["function"].(map[string]any)
		if canonicalizeRawFunctionName(function, allowedTools) {
			changed = true
		}
	}
	return changed
}

func canonicalizeRawLegacyFunctionCall(functionCall map[string]any, allowedTools map[string]struct{}) bool {
	return canonicalizeRawFunctionName(functionCall, allowedTools)
}

func canonicalizeToolNameInJSONRawMessage(raw json.RawMessage, allowedTools map[string]struct{}) (json.RawMessage, bool) {
	var function map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &function) != nil {
		return raw, false
	}
	if !canonicalizeRawFunctionName(function, allowedTools) {
		return raw, false
	}
	out, err := json.Marshal(function)
	if err != nil {
		return raw, false
	}
	return out, true
}
