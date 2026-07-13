package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

func declaredAllCatalogCanonicalTools() map[string]struct{} {
	declared := map[string]struct{}{}
	for _, family := range copilotToolCatalog {
		for _, name := range family.Canonical {
			declared[name] = struct{}{}
		}
	}
	return declared
}

func firstDeclaredTarget(targets []string, declared map[string]struct{}) string {
	for _, target := range targets {
		if _, ok := declared[target]; ok {
			return target
		}
	}
	return ""
}

func TestCopilotToolCatalogHasNoDuplicateAliasDefinitions(t *testing.T) {
	seen := map[string]string{}
	for familyName, family := range copilotToolCatalog {
		for alias := range family.Aliases {
			if previous, ok := seen[alias]; ok {
				t.Fatalf("alias %q is defined in both %s and %s", alias, previous, familyName)
			}
			seen[alias] = familyName
		}
	}
}

func TestCopilotToolCatalogTargetsAreKnownCanonicalTools(t *testing.T) {
	canonical := declaredAllCatalogCanonicalTools()
	for familyName, family := range copilotToolCatalog {
		for alias, targets := range family.Aliases {
			t.Run(familyName+"/"+alias, func(t *testing.T) {
				for _, target := range targets {
					if _, ok := canonical[target]; !ok {
						t.Fatalf("alias %q target %q is not a known canonical tool", alias, target)
					}
				}
			})
		}
	}
}

func TestCopilotToolCatalogNonStreamCanonicalToolsPassThrough(t *testing.T) {
	allowed := declaredAllCatalogCanonicalTools()
	for toolName := range allowed {
		t.Run(toolName, func(t *testing.T) {
			resp := &provider.ChatResponse{Choices: []provider.Choice{{
				Message:      provider.Message{ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.FunctionCall{Name: toolName, Arguments: `{"value":"ok"}`}}}},
				FinishReason: "tool_calls",
			}}}
			normalizeProviderSpecificToolCalls(resp, allowed)
			call := resp.Choices[0].Message.ToolCalls[0]
			if call.Function.Name != toolName || call.Function.Arguments != `{"value":"ok"}` || resp.Choices[0].FinishReason != "tool_calls" {
				t.Fatalf("canonical tool changed unexpectedly: %#v", resp.Choices[0])
			}
		})
	}
}

func TestCopilotToolCatalogNonStreamAliasesCanonicalize(t *testing.T) {
	declared := declaredAllCatalogCanonicalTools()
	for familyName, family := range copilotToolCatalog {
		for alias, targets := range family.Aliases {
			want := firstDeclaredTarget(targets, declared)
			if want == "" {
				t.Fatalf("alias %q has no declared target in all-catalog declared set", alias)
			}
			t.Run(familyName+"/"+alias, func(t *testing.T) {
				resp := &provider.ChatResponse{Choices: []provider.Choice{{
					Message:      provider.Message{ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.FunctionCall{Name: alias, Arguments: `{"value":"ok"}`}}}},
					FinishReason: "tool_calls",
				}}}
				normalizeProviderSpecificToolCalls(resp, declared)
				call := resp.Choices[0].Message.ToolCalls[0]
				if call.Function.Name != want || call.Function.Arguments != `{"value":"ok"}` || resp.Choices[0].FinishReason != "tool_calls" {
					t.Fatalf("alias %q canonicalized to unexpected response: %#v", alias, resp.Choices[0])
				}
			})
		}
	}
}

func marshalOpenAIToolResponseForCatalogTest(t *testing.T, name string) []byte {
	t.Helper()
	body := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": `{"value":"ok"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return data
}

func marshalOpenAIStreamToolChunkForCatalogTest(t *testing.T, name string) []byte {
	t.Helper()
	body := map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    "call_1",
					"type":  "function",
					"function": map[string]any{
						"name":      name,
						"arguments": `{"value":"ok"}`,
					},
				}},
			},
			"finish_reason": nil,
		}},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal stream chunk: %v", err)
	}
	return data
}

func TestCopilotToolCatalogOpenAIJSONAliasesCanonicalize(t *testing.T) {
	declared := declaredAllCatalogCanonicalTools()
	for familyName, family := range copilotToolCatalog {
		for alias, targets := range family.Aliases {
			want := firstDeclaredTarget(targets, declared)
			t.Run(familyName+"/"+alias, func(t *testing.T) {
				body := marshalOpenAIToolResponseForCatalogTest(t, alias)
				normalized := normalizeProviderSpecificToolCallsInOpenAIJSON(body, declared)
				var resp provider.ChatResponse
				if err := json.Unmarshal(normalized, &resp); err != nil {
					t.Fatalf("normalized JSON invalid: %v; body=%s", err, string(normalized))
				}
				call := resp.Choices[0].Message.ToolCalls[0]
				if call.Function.Name != want || resp.Choices[0].FinishReason != "tool_calls" {
					t.Fatalf("alias %q JSON canonicalized to unexpected response: %#v", alias, resp.Choices[0])
				}
			})
		}
	}
}

func TestCopilotToolCatalogOpenAIStreamAliasesCanonicalize(t *testing.T) {
	declared := declaredAllCatalogCanonicalTools()
	sanitizer := newOpenAIStreamToolSanitizer(declared)
	for familyName, family := range copilotToolCatalog {
		for alias, targets := range family.Aliases {
			want := firstDeclaredTarget(targets, declared)
			t.Run(familyName+"/"+alias, func(t *testing.T) {
				line := "data: " + string(marshalOpenAIStreamToolChunkForCatalogTest(t, alias))
				normalized := sanitizer.normalizeLine(line)
				payload := strings.TrimSpace(strings.TrimPrefix(normalized, "data:"))
				var root struct {
					Choices []struct {
						Delta struct {
							ToolCalls []provider.ToolCall `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(payload), &root); err != nil {
					t.Fatalf("normalized stream JSON invalid: %v; line=%s", err, normalized)
				}
				if got := root.Choices[0].Delta.ToolCalls[0].Function.Name; got != want {
					t.Fatalf("stream alias %q canonicalized to %q, want %q; line=%s", alias, got, want, normalized)
				}
			})
		}
	}
}

func TestCopilotToolCatalogDSMLAliasesCanonicalize(t *testing.T) {
	declared := declaredAllCatalogCanonicalTools()
	for familyName, family := range copilotToolCatalog {
		for alias, targets := range family.Aliases {
			want := firstDeclaredTarget(targets, declared)
			t.Run(familyName+"/"+alias, func(t *testing.T) {
				content := `<｜DSML｜tool_calls><｜DSML｜invoke name="` + alias + `"><｜DSML｜parameter name="value">ok</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>`
				calls, cleaned := parseDSMLToolCalls(content, declared)
				if len(calls) != 1 || calls[0].Function.Name != want {
					t.Fatalf("DSML alias %q calls=%#v want %q", alias, calls, want)
				}
				if strings.Contains(cleaned, "DSML") {
					t.Fatalf("converted DSML should be removed: %q", cleaned)
				}
			})
		}
	}
}
