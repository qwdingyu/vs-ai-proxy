package proxy

import (
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

func TestBuildOllamaChatResponseConvertsLegacyFunctionCall(t *testing.T) {
	body := buildOllamaChatResponse("gpt-test", &provider.ChatResponse{
		Choices: []provider.Choice{{
			Message: provider.Message{
				Role:    "assistant",
				Content: "",
				FunctionCall: &provider.FunctionCall{
					Name:      "powershell",
					Arguments: `{"command":"pwd"}`,
				},
			},
		}},
	})
	message := body["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]map[string]any)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v, want one", message["tool_calls"])
	}
	fn := calls[0]["function"].(*provider.FunctionCall)
	if fn.Name != "powershell" {
		t.Fatalf("function = %#v", fn)
	}
}
