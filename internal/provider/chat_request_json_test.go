package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatRequestPreservesUnknownTopLevelFields(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"test-model",
		"messages":[{"role":"user","content":"hi"}],
		"provider_routing":{"allow_fallbacks":true},
		"metadata":{"trace_id":"abc"}
	}`), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	if len(req.Extra) != 2 {
		t.Fatalf("extra len = %d, want 2", len(req.Extra))
	}

	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("decode marshalled request: %v", err)
	}
	if _, ok := raw["provider_routing"]; !ok {
		t.Fatalf("provider_routing was not preserved: %s", string(out))
	}
	if _, ok := raw["metadata"]; !ok {
		t.Fatalf("metadata was not preserved: %s", string(out))
	}
}

func TestChatRequestKnownFieldsOverrideExtraFields(t *testing.T) {
	req := ChatRequest{
		Model: "canonical",
		Extra: map[string]json.RawMessage{
			"model":            []byte(`"stale"`),
			"provider_option":  []byte(`true`),
			"reasoning_effort": []byte(`"low"`),
		},
	}

	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("decode marshalled request: %v", err)
	}
	if raw["model"] != "canonical" {
		t.Fatalf("model = %#v, want canonical", raw["model"])
	}
	if _, ok := raw["provider_option"]; !ok {
		t.Fatalf("provider_option was not preserved: %s", string(out))
	}
	if raw["reasoning_effort"] == "low" {
		t.Fatalf("known reasoning_effort leaked from extra: %s", string(out))
	}
}

func TestMessagePreservesUnknownFields(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{
		"role":"assistant",
		"content":"answer",
		"cache_control":{"type":"ephemeral"},
		"audio":{"id":"voice"}
	}`), &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}

	if len(msg.Extra) != 2 {
		t.Fatalf("extra len = %d, want 2", len(msg.Extra))
	}

	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("decode marshalled message: %v", err)
	}
	if _, ok := raw["cache_control"]; !ok {
		t.Fatalf("cache_control was not preserved: %s", string(out))
	}
	if _, ok := raw["audio"]; !ok {
		t.Fatalf("audio was not preserved: %s", string(out))
	}
}

func TestMessagePreservesRawMultimodalContent(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{
		"role":"user",
		"content":[
			{"type":"text","text":"Describe"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}
		]
	}`), &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}

	if msg.Content != "" {
		t.Fatalf("content = %q, want empty string with raw content", msg.Content)
	}
	if len(msg.ContentRaw) == 0 {
		t.Fatalf("content raw should be preserved")
	}

	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("decode marshalled message: %v", err)
	}
	content, ok := raw["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content = %#v, want multimodal array: %s", raw["content"], string(out))
	}
}

func TestToolPreservesUnknownNestedFields(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"test-model",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"strict":true,
			"function":{
				"name":"lookup",
				"description":"Lookup",
				"parameters":{"type":"object"},
				"cache_control":{"type":"ephemeral"}
			}
		}]
	}`), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("decode marshalled request: %v", err)
	}
	tools := raw["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["strict"] != true {
		t.Fatalf("tool strict extension was not preserved: %s", string(out))
	}
	fn := tool["function"].(map[string]any)
	if _, ok := fn["cache_control"]; !ok {
		t.Fatalf("function cache_control extension was not preserved: %s", string(out))
	}
}

func TestToolCallPreservesUnknownNestedFields(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{
		"role":"assistant",
		"content":"",
		"tool_calls":[{
			"id":"call_1",
			"type":"function",
			"index":0,
			"function":{
				"name":"lookup",
				"arguments":"{}",
				"provider_state":{"chunk":1}
			}
		}]
	}`), &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}

	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("decode marshalled message: %v", err)
	}
	calls := raw["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["index"] != float64(0) {
		t.Fatalf("tool call index extension was not preserved: %s", string(out))
	}
	fn := call["function"].(map[string]any)
	if _, ok := fn["provider_state"]; !ok {
		t.Fatalf("function provider_state extension was not preserved: %s", string(out))
	}
}

func TestFunctionCallAcceptsObjectArguments(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{
		"role":"assistant",
		"content":"",
		"tool_calls":[{
			"id":"call_1",
			"type":"function",
			"function":{"name":"create_file","arguments":{"path":"a.txt","content":"ok"}}
		}]
	}`), &msg); err != nil {
		t.Fatalf("unmarshal message with object arguments: %v", err)
	}

	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(msg.ToolCalls))
	}
	var arguments map[string]string
	if err := json.Unmarshal([]byte(msg.ToolCalls[0].Function.Arguments), &arguments); err != nil {
		t.Fatalf("arguments should be JSON object string: %q", msg.ToolCalls[0].Function.Arguments)
	}
	if arguments["path"] != "a.txt" || arguments["content"] != "ok" {
		t.Fatalf("arguments = %#v", arguments)
	}
}

func TestChatRequestPreservesLegacyFunctionFields(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-test",
		"messages":[{"role":"user","content":"run powershell"}],
		"functions":[{"name":"powershell","description":"Run PowerShell","parameters":{"type":"object"}}],
		"function_call":"auto"
	}`), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("decode marshalled request: %v", err)
	}
	if _, ok := raw["functions"]; !ok {
		t.Fatalf("legacy functions were not preserved: %s", string(out))
	}
	if raw["function_call"] != "auto" {
		t.Fatalf("function_call = %#v, want auto", raw["function_call"])
	}
}

func TestMessagePreservesLegacyFunctionCall(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{
		"role":"assistant",
		"content":"",
		"function_call":{"name":"powershell","arguments":{"command":"Get-ChildItem"}}
	}`), &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if msg.FunctionCall == nil || msg.FunctionCall.Name != "powershell" {
		t.Fatalf("function_call not parsed: %#v", msg.FunctionCall)
	}
	var arguments map[string]string
	if err := json.Unmarshal([]byte(msg.FunctionCall.Arguments), &arguments); err != nil {
		t.Fatalf("function_call arguments should be JSON object string: %q", msg.FunctionCall.Arguments)
	}
	if arguments["command"] != "Get-ChildItem" {
		t.Fatalf("arguments = %#v", arguments)
	}

	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if !strings.Contains(string(out), `"function_call"`) {
		t.Fatalf("function_call was not preserved: %s", string(out))
	}
}
