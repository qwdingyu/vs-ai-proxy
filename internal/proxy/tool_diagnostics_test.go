package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

func TestToolDiagnosticsSummarizeNamesOnly(t *testing.T) {
	var req provider.ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-test",
		"messages":[{"role":"user","content":"run commands"}],
		"tools":[{"type":"function","function":{"name":"powershell"}},{"type":"function","function":{"name":"git"}}],
		"functions":[{"name":"legacy_shell"}]
	}`), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	summary := requestToolSummary(&req)
	for _, want := range []string{"git", "powershell", "legacy_shell"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q missing %q", summary, want)
		}
	}

	resp := &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.Message{ToolCalls: []provider.ToolCall{{Function: provider.FunctionCall{Name: "powershell", Arguments: `{"command":"Get-ChildItem -Force"}`}}}}}}}
	responseSummary := responseToolSummaryFromChatResponse(resp)
	if responseSummary != "returned: powershell" {
		t.Fatalf("response summary = %q, want only tool name", responseSummary)
	}
	if strings.Contains(responseSummary, "Get-ChildItem") {
		t.Fatalf("response summary leaked command arguments: %q", responseSummary)
	}
}

func TestToolDiagnosticsSummarizeStreamToolNames(t *testing.T) {
	acc := newStreamReasoningAccumulator()
	acc.consumeOpenAIChunk(openAIStreamChunk{ToolCalls: []any{map[string]any{
		"id": "call_1",
		"function": map[string]any{
			"name":      "git",
			"arguments": `{"args":["status","--short"]}`,
		},
	}}})

	summary := toolSummaryFromAccumulator(acc)
	if summary != "streamed: git" {
		t.Fatalf("stream summary = %q, want streamed: git", summary)
	}
}
