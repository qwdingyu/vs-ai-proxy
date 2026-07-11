package proxy

import "testing"

func TestCanonicalToolNameMapsCommonDevelopmentAliasesOnlyToDeclaredTargets(t *testing.T) {
	tests := []struct {
		name    string
		allowed map[string]struct{}
		want    string
	}{
		{name: "run_tests", allowed: map[string]struct{}{"powershell": {}}, want: "powershell"},
		{name: "build_project", allowed: map[string]struct{}{"terminal": {}}, want: "terminal"},
		{name: "dotnet_test", allowed: map[string]struct{}{"powershell": {}}, want: "powershell"},
		{name: "npm_install", allowed: map[string]struct{}{"terminal": {}}, want: "terminal"},
		{name: "run_lint", allowed: map[string]struct{}{"run_in_terminal": {}}, want: "run_in_terminal"},
		{name: "git_diff", allowed: map[string]struct{}{"git": {}}, want: "git"},
		{name: "write_file", allowed: map[string]struct{}{"create_file": {}}, want: "create_file"},
		{name: "patch_file", allowed: map[string]struct{}{"apply_patch": {}}, want: "apply_patch"},
		{name: "read_file", allowed: map[string]struct{}{"get_file": {}}, want: "get_file"},
		{name: "grep", allowed: map[string]struct{}{"grep_search": {}}, want: "grep_search"},
		{name: "search_files", allowed: map[string]struct{}{"file_search": {}}, want: "file_search"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalToolName(tt.name, tt.allowed); got != tt.want {
				t.Fatalf("canonicalToolName(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestCanonicalToolNameDoesNotInventUndeclaredTargets(t *testing.T) {
	for _, name := range []string{"run_tests", "write_file", "grep", "git_diff", "dotnet_test"} {
		if got := canonicalToolName(name, map[string]struct{}{"get_file": {}}); got != name {
			t.Fatalf("canonicalToolName(%q) = %q, must not map to unrelated declared tool", name, got)
		}
	}
	if got := canonicalToolName("read_file", map[string]struct{}{"get_file": {}}); got != "get_file" {
		t.Fatalf("read_file should map to declared get_file, got %q", got)
	}
	if got := canonicalToolName("find_symbol", map[string]struct{}{"code_search": {}}); got != "find_symbol" {
		t.Fatalf("find_symbol is a real tool name and must not be degraded to code_search, got %q", got)
	}
}
