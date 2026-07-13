package proxy

import "testing"

func TestCopilotToolCatalogAliasesMapOnlyToDeclaredTargets(t *testing.T) {
	for familyName, family := range copilotToolCatalog {
		for alias, targets := range family.Aliases {
			t.Run(familyName+"/"+alias, func(t *testing.T) {
				if len(targets) == 0 {
					t.Fatalf("alias %q has no targets", alias)
				}
				for _, target := range targets {
					allowed := map[string]struct{}{target: {}}
					if got := canonicalToolName(alias, allowed); got != target {
						t.Fatalf("canonicalToolName(%q, declared %q) = %q, want %q", alias, target, got, target)
					}
				}
			})
		}
	}
}

func TestCopilotToolCatalogCanonicalNamesRemainStable(t *testing.T) {
	for familyName, family := range copilotToolCatalog {
		for _, name := range family.Canonical {
			t.Run(familyName+"/"+name, func(t *testing.T) {
				allowed := map[string]struct{}{name: {}}
				if got := canonicalToolName(name, allowed); got != name {
					t.Fatalf("canonicalToolName(%q) = %q, want unchanged", name, got)
				}
			})
		}
	}
}

func TestCanonicalToolNameDoesNotInventUndeclaredTargets(t *testing.T) {
	allowed := map[string]struct{}{"get_file": {}}
	for alias := range copilotToolAliasTargets() {
		if alias == "read_file" || alias == "open_file" || alias == "view_file" || alias == "cat_file" {
			continue
		}
		if got := canonicalToolName(alias, allowed); got != alias {
			t.Fatalf("canonicalToolName(%q) = %q, must not map to unrelated declared get_file", alias, got)
		}
	}
}

func TestCanonicalToolNameDoesNotMapPluralFileReadAliasesToSingleFileTool(t *testing.T) {
	allowed := map[string]struct{}{"get_file": {}}
	for _, alias := range []string{"get_files", "read_files"} {
		if got := canonicalToolName(alias, allowed); got != alias {
			t.Fatalf("canonicalToolName(%q) = %q, plural read aliases must not map to single-file get_file", alias, got)
		}
	}
}

func TestCanonicalToolNameCoversCommonCopilotToolFamilies(t *testing.T) {
	declared := map[string]struct{}{
		"adapt_plan":           {},
		"apply_patch":          {},
		"ask_question":         {},
		"clarify_requirements": {},
		"code_search":          {},
		"create_file":          {},
		"delete_files":         {},
		"detect_memories":      {},
		"edit_file":            {},
		"file_search":          {},
		"find_symbol":          {},
		"get_file":             {},
		"git":                  {},
		"grep_search":          {},
		"list_files":           {},
		"powershell":           {},
		"run_in_terminal":      {},
	}

	tests := map[string]string{
		"apply_changes":   "apply_patch",
		"apply_diff":      "apply_patch",
		"bash":            "powershell",
		"build_project":   "powershell",
		"cat_file":        "get_file",
		"clarify":         "clarify_requirements",
		"create_new_file": "create_file",
		"delete_file":     "delete_files",
		"detect_memorie":  "detect_memories",
		"dotnet_test":     "powershell",
		"find_files":      "file_search",
		"git_status":      "git",
		"glob":            "file_search",
		"list_file":       "list_files",
		"new_file":        "create_file",
		"read_files":      "file_search",
		"rg":              "grep_search",
		"run_tests":       "powershell",
		"search_symbol":   "find_symbol",
		"update_plan":     "adapt_plan",
	}
	for alias, want := range tests {
		t.Run(alias, func(t *testing.T) {
			if got := canonicalToolName(alias, declared); got != want {
				t.Fatalf("canonicalToolName(%q) = %q, want %q", alias, got, want)
			}
		})
	}
}

func TestCanonicalToolNameDoesNotMapSemanticFileAliasesToShellTools(t *testing.T) {
	for _, name := range []string{"delete_file", "remove_file", "ls", "find_file", "glob"} {
		t.Run(name, func(t *testing.T) {
			allowed := map[string]struct{}{"powershell": {}, "terminal": {}, "run_in_terminal": {}}
			if got := canonicalToolName(name, allowed); got != name {
				t.Fatalf("canonicalToolName(%q) = %q, semantic file aliases must not be rewritten to shell tools with incompatible arguments", name, got)
			}
		})
	}
}

func TestKnownCopilotToolNamesIncludesObservedVSDeclaredTools(t *testing.T) {
	known := knownCopilotToolNames()
	observed := []string{
		"adapt_plan",
		"apply_patch",
		"ask_question",
		"clarify_requirements",
		"code_search",
		"create_file",
		"delete_files",
		"detect_memories",
		"edit_file",
		"file_search",
		"find_symbol",
		"get_file",
		"git",
		"grep_search",
		"list_files",
		"powershell",
	}
	for _, name := range observed {
		if _, ok := known[name]; !ok {
			t.Fatalf("observed VS/Copilot tool %q missing from catalog", name)
		}
	}
}
