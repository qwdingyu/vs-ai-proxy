package proxy

// copilotToolCatalog 是 VS/Copilot 常见工具名与安全别名目录。
//
// 设计原则：
// 1. 标准 OpenAI tool_calls / legacy function_call 默认 stable 透传；本目录只用于别名归一化和 DSML 文本方言转换。
// 2. 只把别名映射到“当前请求已声明”的目标工具，绝不凭空发明工具。
// 3. 只做语义兼容映射：文件语义工具不会降级到 shell/powershell，避免参数 schema 不匹配导致“工具已调用但无法运行”。
var copilotToolCatalog = map[string]copilotToolFamily{
	"planning": {
		Canonical: []string{
			"adapt_plan",
			"ask_question",
			"clarify_requirements",
			"detect_memories",
			"finish_plan",
			"record_observation",
			"signal_plan_ready",
			"update_plan_progress",
		},
		Aliases: map[string][]string{
			"update_plan":        {"adapt_plan"},
			"plan":               {"adapt_plan"},
			"clarify":            {"clarify_requirements", "ask_question"},
			"ask_user":           {"ask_question", "clarify_requirements"},
			"detect_memory":      {"detect_memories"},
			"detect_memorie":     {"detect_memories"},
			"detect_memories_v2": {"detect_memories"},
		},
	},
	"terminal": {
		Canonical: []string{"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
		Aliases: map[string][]string{
			"run_tests":               {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"run_test":                {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"test":                    {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"run_command":             {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"run_command_in_terminal": {"run_command_in_terminal", "powershell", "terminal", "run_in_terminal"},
			"execute_command":         {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"exec":                    {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"shell":                   {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"bash":                    {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"cmd":                     {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"command_prompt":          {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"execute_shell":           {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"terminal_command":        {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"run_terminal_cmd":        {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"install_package":         {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"build_project":           {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"run_build":               {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"dotnet_build":            {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"dotnet_test":             {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"npm_install":             {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"npm_test":                {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"npm_run":                 {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"run_lint":                {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"run_formatter":           {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
			"format_code":             {"powershell", "terminal", "run_in_terminal", "run_command_in_terminal"},
		},
	},
	"git": {
		Canonical: []string{"git"},
		Aliases: map[string][]string{
			"git_command": {"git", "powershell", "terminal", "run_in_terminal"},
			"git_status":  {"git", "powershell", "terminal", "run_in_terminal"},
			"git_diff":    {"git", "powershell", "terminal", "run_in_terminal"},
			"git_log":     {"git", "powershell", "terminal", "run_in_terminal"},
		},
	},
	"file_write": {
		Canonical: []string{"create_file", "edit_file", "edit_files", "apply_patch"},
		Aliases: map[string][]string{
			"write_file":      {"create_file", "edit_file", "apply_patch"},
			"save_file":       {"create_file", "edit_file", "apply_patch"},
			"create_new_file": {"create_file", "edit_file", "apply_patch"},
			"new_file":        {"create_file", "edit_file", "apply_patch"},
			"make_file":       {"create_file", "edit_file", "apply_patch"},
			"replace_file":    {"edit_file", "apply_patch"},
			"modify_file":     {"edit_file", "apply_patch"},
			"update_file":     {"edit_file", "apply_patch"},
			"append_file":     {"edit_file", "apply_patch", "create_file"},
			"insert_file":     {"edit_file", "apply_patch"},
			"patch_file":      {"apply_patch", "edit_file"},
			"apply_diff":      {"apply_patch", "edit_file"},
			"apply_changes":   {"apply_patch", "edit_file"},
		},
	},
	"file_delete": {
		Canonical: []string{"delete_files"},
		Aliases: map[string][]string{
			"delete_file": {"delete_files"},
			"remove_file": {"delete_files"},
		},
	},
	"file_read": {
		Canonical: []string{"get_file", "file_search", "list_files"},
		Aliases: map[string][]string{
			"read_file":  {"get_file", "file_search"},
			"open_file":  {"get_file", "file_search"},
			"view_file":  {"get_file", "file_search"},
			"cat_file":   {"get_file", "file_search"},
			"get_files":  {"file_search"},
			"read_files": {"file_search"},
			"list_file":  {"list_files", "file_search"},
			"list_files": {"list_files", "file_search", "grep_search", "code_search"},
			"ls":         {"list_files", "file_search", "grep_search", "code_search"},
			"find_file":  {"file_search", "grep_search", "code_search"},
			"find_files": {"file_search", "grep_search", "code_search"},
			"glob":       {"file_search", "grep_search", "code_search"},
		},
	},
	"search": {
		Canonical: []string{"code_search", "grep_search", "file_search", "find_symbol"},
		Aliases: map[string][]string{
			"code_search":   {"code_search", "grep_search", "file_search"},
			"grep_search":   {"grep_search", "code_search", "file_search"},
			"file_search":   {"file_search", "grep_search", "code_search"},
			"search_code":   {"code_search", "grep_search", "file_search"},
			"search_files":  {"file_search", "grep_search", "code_search"},
			"grep":          {"grep_search", "code_search", "file_search"},
			"ripgrep":       {"grep_search", "code_search", "file_search"},
			"rg":            {"grep_search", "code_search", "file_search"},
			"find_symbol":   {"find_symbol", "code_search", "grep_search"},
			"search_symbol": {"find_symbol", "code_search", "grep_search"},
		},
	},
	"vs_workspace": {
		Canonical: []string{
			"get_background_terminal_output",
			"get_errors",
			"get_files_in_project",
			"get_output_window_logs",
			"get_projects_in_solution",
			"get_tests",
			"get_web_pages",
		},
		Aliases: map[string][]string{},
	},
	"vs_agents": {
		Canonical: []string{
			"profiler_agent",
			"search_agent",
			"start_modernization",
		},
		Aliases: map[string][]string{},
	},
	"nuget": {
		Canonical: []string{
			"fix_vulnerable_packages",
			"get_latest_package_version",
			"get_package_context",
			"review_supply_chain_security",
			"update_package_version",
			"upgrade_packages_to_latest",
		},
		Aliases: map[string][]string{},
	},
}

type copilotToolFamily struct {
	Canonical []string
	Aliases   map[string][]string
}

func copilotToolAliasTargets() map[string][]string {
	aliases := map[string][]string{}
	for _, family := range copilotToolCatalog {
		for alias, targets := range family.Aliases {
			aliases[alias] = append([]string(nil), targets...)
		}
	}
	return aliases
}

func knownCopilotToolNames() map[string]struct{} {
	known := map[string]struct{}{}
	for _, family := range copilotToolCatalog {
		for _, name := range family.Canonical {
			known[name] = struct{}{}
		}
		for alias := range family.Aliases {
			known[alias] = struct{}{}
		}
	}
	return known
}
