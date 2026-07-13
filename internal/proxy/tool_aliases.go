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
	if _, ok := allowedTools[key]; ok {
		return trimmed
	}
	for _, target := range toolAliasTargets[key] {
		if _, ok := allowedTools[target]; ok {
			return target
		}
	}
	return trimmed
}

func canonicalizeFunctionCallName(function *provider.FunctionCall, allowedTools map[string]struct{}) bool {
	if function == nil {
		return false
	}
	canonical := canonicalToolName(function.Name, allowedTools)
	if strings.EqualFold(strings.TrimSpace(function.Name), strings.TrimSpace(canonical)) {
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
	if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(canonical)) {
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
