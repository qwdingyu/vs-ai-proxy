package proxy

import (
	"encoding/json"
	"testing"
)

const dsmlAdvisorSample = `[Advisor consultation #1] [Advisor review]
<｜DSML｜tool_calls> <｜DSML｜invoke name="get_file"> <｜DSML｜parameter name="endLine" string="false">59</｜DSML｜parameter> <｜DSML｜parameter name="filename" string="true">src\Runner.Console\Health\DeviceHealthCheck.cs</｜DSML｜parameter> <｜DSML｜parameter name="startLine" string="false">1</｜DSML｜parameter> </｜DSML｜invoke> <｜DSML｜invoke name="get_file"> <｜DSML｜parameter name="endLine" string="false">63</｜DSML｜parameter> <｜DSML｜parameter name="filename" string="true">src\Runner.Console\Health\QueueHealthCheck.cs</｜DSML｜parameter> <｜DSML｜parameter name="startLine" string="false">1</｜DSML｜parameter> </｜DSML｜invoke> <｜DSML｜invoke name="get_file"> <｜DSML｜parameter name="endLine" string="false">52</｜DSML｜parameter> <｜DSML｜parameter name="filename" string="true">src\Runner.Console\Health\StorageHealthCheck.cs</｜DSML｜parameter> <｜DSML｜parameter name="startLine" string="false">1</｜DSML｜parameter> </｜DSML｜invoke> <｜DSML｜invoke name="get_file"> <｜DSML｜parameter name="endLine" string="false">139</｜DSML｜parameter> <｜DSML｜parameter name="filename" string="true">src\Runner.Console\Observability\MetricsCollector.cs</｜DSML｜parameter> <｜DSML｜parameter name="startLine" string="false">1</｜DSML｜parameter> </｜DSML｜invoke> </｜DSML｜tool_calls> [End of advisor consultation #1]`

func TestParseDSMLToolCallsAdvisorSample(t *testing.T) {
	calls, cleaned := parseDSMLToolCalls(dsmlAdvisorSample)
	if len(calls) != 4 {
		t.Fatalf("calls len = %d, want 4", len(calls))
	}
	if calls[0].Function.Name != "get_file" {
		t.Fatalf("tool name = %q, want get_file", calls[0].Function.Name)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments should be JSON: %v", err)
	}
	if args["filename"] != `src\Runner.Console\Health\DeviceHealthCheck.cs` || args["startLine"] != "1" || args["endLine"] != "59" {
		t.Fatalf("args = %#v", args)
	}
	if cleaned != "[Advisor consultation #1] [Advisor review]\n [End of advisor consultation #1]" {
		t.Fatalf("cleaned content = %q", cleaned)
	}
}
