package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

const dsmlAdvisorSample = `[Advisor consultation #1] [Advisor review]
<｜DSML｜tool_calls> <｜DSML｜invoke name="get_file"> <｜DSML｜parameter name="endLine" string="false">59</｜DSML｜parameter> <｜DSML｜parameter name="filename" string="true">src\Runner.Console\Health\DeviceHealthCheck.cs</｜DSML｜parameter> <｜DSML｜parameter name="startLine" string="false">1</｜DSML｜parameter> </｜DSML｜invoke> <｜DSML｜invoke name="get_file"> <｜DSML｜parameter name="endLine" string="false">63</｜DSML｜parameter> <｜DSML｜parameter name="filename" string="true">src\Runner.Console\Health\QueueHealthCheck.cs</｜DSML｜parameter> <｜DSML｜parameter name="startLine" string="false">1</｜DSML｜parameter> </｜DSML｜invoke> <｜DSML｜invoke name="get_file"> <｜DSML｜parameter name="endLine" string="false">52</｜DSML｜parameter> <｜DSML｜parameter name="filename" string="true">src\Runner.Console\Health\StorageHealthCheck.cs</｜DSML｜parameter> <｜DSML｜parameter name="startLine" string="false">1</｜DSML｜parameter> </｜DSML｜invoke> <｜DSML｜invoke name="get_file"> <｜DSML｜parameter name="endLine" string="false">139</｜DSML｜parameter> <｜DSML｜parameter name="filename" string="true">src\Runner.Console\Observability\MetricsCollector.cs</｜DSML｜parameter> <｜DSML｜parameter name="startLine" string="false">1</｜DSML｜parameter> </｜DSML｜invoke> </｜DSML｜tool_calls> [End of advisor consultation #1]`

func TestParseDSMLToolCallsAdvisorSample(t *testing.T) {
	calls, cleaned := parseDSMLToolCalls(dsmlAdvisorSample, map[string]struct{}{"get_file": {}})
	if len(calls) != 4 {
		t.Fatalf("calls len = %d, want 4", len(calls))
	}
	if calls[0].Function.Name != "get_file" {
		t.Fatalf("tool name = %q, want get_file", calls[0].Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments should be JSON: %v", err)
	}
	if args["filename"] != `src\Runner.Console\Health\DeviceHealthCheck.cs` || args["startLine"] != float64(1) || args["endLine"] != float64(59) {
		t.Fatalf("args = %#v", args)
	}
	if cleaned != "[Advisor consultation #1] [Advisor review]\n [End of advisor consultation #1]" {
		t.Fatalf("cleaned content = %q", cleaned)
	}
}

func TestAllowedToolNamesIncludesLegacyFunctions(t *testing.T) {
	req := &provider.ChatRequest{
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunc{Name: "create_file"}}},
		Extra: map[string]json.RawMessage{
			"functions": []byte(`[{"name":"powershell"},{"name":"git"}]`),
		},
	}
	allowed := allowedToolNames(req)
	for _, name := range []string{"create_file", "powershell", "git"} {
		if _, ok := allowed[name]; !ok {
			t.Fatalf("allowed tools missing %q: %#v", name, allowed)
		}
	}
}

func TestParseDSMLToolCallsRejectsUndeclaredTools(t *testing.T) {
	calls, cleaned := parseDSMLToolCalls(dsmlAdvisorSample, map[string]struct{}{"create_file": {}})
	if len(calls) != 0 {
		t.Fatalf("undeclared get_file must not be converted: %#v", calls)
	}
	if cleaned != dsmlAdvisorSample {
		t.Fatalf("content should be unchanged when no declared tool matches")
	}
}

func TestParseDSMLToolCallsCanonicalizesRunTestsWhenTerminalToolDeclared(t *testing.T) {
	content := `<｜DSML｜tool_calls><｜DSML｜invoke name="run_tests"><｜DSML｜parameter name="command">go test ./...</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>`

	calls, cleaned := parseDSMLToolCalls(content, map[string]struct{}{"powershell": {}})

	if len(calls) != 1 || calls[0].Function.Name != "powershell" {
		t.Fatalf("run_tests should canonicalize to declared powershell tool: %#v", calls)
	}
	if strings.Contains(cleaned, "DSML") {
		t.Fatalf("DSML block should be removed after conversion: %q", cleaned)
	}
}

func TestParseDSMLToolCallsCanonicalizesCommonToolFamilies(t *testing.T) {
	content := `<｜DSML｜tool_calls>
<｜DSML｜invoke name="apply_diff"><｜DSML｜parameter name="patch">diff</｜DSML｜parameter></｜DSML｜invoke>
<｜DSML｜invoke name="run_tests"><｜DSML｜parameter name="command">go test ./...</｜DSML｜parameter></｜DSML｜invoke>
<｜DSML｜invoke name="search_symbol"><｜DSML｜parameter name="name">Handler</｜DSML｜parameter></｜DSML｜invoke>
<｜DSML｜invoke name="read_file"><｜DSML｜parameter name="filename">a.go</｜DSML｜parameter></｜DSML｜invoke>
</｜DSML｜tool_calls>`
	allowed := map[string]struct{}{
		"apply_patch": {},
		"find_symbol": {},
		"get_file":    {},
		"powershell":  {},
	}

	calls, cleaned := parseDSMLToolCalls(content, allowed)
	if len(calls) != 4 {
		t.Fatalf("calls len = %d, want 4: %#v", len(calls), calls)
	}
	wantNames := []string{"apply_patch", "powershell", "find_symbol", "get_file"}
	for i, want := range wantNames {
		if calls[i].Function.Name != want {
			t.Fatalf("call[%d].name = %q, want %q", i, calls[i].Function.Name, want)
		}
	}
	if strings.Contains(cleaned, "DSML") {
		t.Fatalf("converted DSML block should be removed: %q", cleaned)
	}
}

func TestParseDSMLToolCallsRejectsWholeBlockWhenAnyToolIsUndeclared(t *testing.T) {
	content := `<｜DSML｜tool_calls>
<｜DSML｜invoke name="get_file"><｜DSML｜parameter name="filename">a.cs</｜DSML｜parameter></｜DSML｜invoke>
<｜DSML｜invoke name="delete_file"><｜DSML｜parameter name="filename">a.cs</｜DSML｜parameter></｜DSML｜invoke>
</｜DSML｜tool_calls>`
	calls, cleaned := parseDSMLToolCalls(content, map[string]struct{}{"get_file": {}})
	if len(calls) != 0 {
		t.Fatalf("mixed-validity DSML must be atomic, got %#v", calls)
	}
	if cleaned != content {
		t.Fatal("rejected DSML must remain visible to the downstream client")
	}
}

func TestParseDSMLToolCallsRejectsDuplicateParameters(t *testing.T) {
	content := `<｜DSML｜tool_calls><｜DSML｜invoke name="get_file">
<｜DSML｜parameter name="filename">a.cs</｜DSML｜parameter>
<｜DSML｜parameter name="filename">b.cs</｜DSML｜parameter>
</｜DSML｜invoke></｜DSML｜tool_calls>`
	calls, cleaned := parseDSMLToolCalls(content, map[string]struct{}{"get_file": {}})
	if len(calls) != 0 || cleaned != content {
		t.Fatalf("ambiguous duplicate parameters must reject the whole block: calls=%#v cleaned=%q", calls, cleaned)
	}
}

func TestParseDSMLToolCallsPreservesParameterTypes(t *testing.T) {
	calls, _ := parseDSMLToolCalls(dsmlAdvisorSample, map[string]struct{}{"get_file": {}})
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments should be JSON: %v", err)
	}
	if args["startLine"] != float64(1) || args["endLine"] != float64(59) {
		t.Fatalf("numeric parameters lost type: %#v", args)
	}
}

func TestNormalizeProviderSpecificToolCallsPassesThroughUndeclaredStandardToolCalls(t *testing.T) {
	resp := &provider.ChatResponse{Choices: []provider.Choice{{
		Message:      provider.Message{ToolCalls: []provider.ToolCall{{ID: "call_ps", Type: "function", Function: provider.FunctionCall{Name: "powershell", Arguments: `{"command":"pwd"}`}}}},
		FinishReason: "tool_calls",
	}}}

	normalizeProviderSpecificToolCalls(resp, map[string]struct{}{"git": {}})
	message := resp.Choices[0].Message
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "powershell" {
		t.Fatalf("undeclared standard tool call should pass through in stable mode: %#v", message.ToolCalls)
	}
	if strings.Contains(message.Content, "Proxy blocked undeclared tool calls") {
		t.Fatalf("stable mode must not pollute assistant content: %#v", message)
	}
}
