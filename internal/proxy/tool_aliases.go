package proxy

import (
	"encoding/json"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

var toolAliasTargets = map[string][]string{
	"run_tests":        {"powershell", "terminal", "run_in_terminal"},
	"run_test":         {"powershell", "terminal", "run_in_terminal"},
	"test":             {"powershell", "terminal", "run_in_terminal"},
	"run_command":      {"powershell", "terminal", "run_in_terminal"},
	"execute_command":  {"powershell", "terminal", "run_in_terminal"},
	"exec":             {"powershell", "terminal", "run_in_terminal"},
	"shell":            {"powershell", "terminal", "run_in_terminal"},
	"bash":             {"powershell", "terminal", "run_in_terminal"},
	"cmd":              {"powershell", "terminal", "run_in_terminal"},
	"command_prompt":   {"powershell", "terminal", "run_in_terminal"},
	"execute_shell":    {"powershell", "terminal", "run_in_terminal"},
	"terminal_command": {"powershell", "terminal", "run_in_terminal"},
	"run_terminal_cmd": {"powershell", "terminal", "run_in_terminal"},
	"install_package":  {"powershell", "terminal", "run_in_terminal"},
	"build_project":    {"powershell", "terminal", "run_in_terminal"},
	"run_build":        {"powershell", "terminal", "run_in_terminal"},
	"dotnet_build":     {"powershell", "terminal", "run_in_terminal"},
	"dotnet_test":      {"powershell", "terminal", "run_in_terminal"},
	"npm_install":      {"powershell", "terminal", "run_in_terminal"},
	"npm_test":         {"powershell", "terminal", "run_in_terminal"},
	"npm_run":          {"powershell", "terminal", "run_in_terminal"},
	"run_lint":         {"powershell", "terminal", "run_in_terminal"},
	"run_formatter":    {"powershell", "terminal", "run_in_terminal"},
	"format_code":      {"powershell", "terminal", "run_in_terminal"},
	"git_command":      {"git", "powershell", "terminal", "run_in_terminal"},
	"git_status":       {"git", "powershell", "terminal", "run_in_terminal"},
	"git_diff":         {"git", "powershell", "terminal", "run_in_terminal"},
	"git_log":          {"git", "powershell", "terminal", "run_in_terminal"},
	"write_file":       {"create_file", "edit_file", "apply_patch"},
	"save_file":        {"create_file", "edit_file", "apply_patch"},
	"replace_file":     {"edit_file", "apply_patch"},
	"modify_file":      {"edit_file", "apply_patch"},
	"update_file":      {"edit_file", "apply_patch"},
	"patch_file":       {"apply_patch", "edit_file"},
	"read_file":        {"get_file", "file_search"},
	"open_file":        {"get_file", "file_search"},
	"view_file":        {"get_file", "file_search"},
	"cat_file":         {"get_file", "file_search"},
	"list_files":       {"file_search", "grep_search", "code_search"},
	"find_file":        {"file_search", "grep_search", "code_search"},
	"search_code":      {"code_search", "grep_search", "file_search"},
	"search_files":     {"file_search", "grep_search", "code_search"},
	"grep":             {"grep_search", "code_search", "file_search"},
	"ripgrep":          {"grep_search", "code_search", "file_search"},
}

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
