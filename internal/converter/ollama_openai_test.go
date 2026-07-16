package converter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseOllamaStreamChunkAcceptsNativeNDJSON(t *testing.T) {
	chunk, err := ParseOllamaStreamChunk(`{"model":"llama","message":{"role":"assistant","content":"hi"},"done":false}`)
	if err != nil {
		t.Fatalf("ParseOllamaStreamChunk returned error: %v", err)
	}
	if chunk["model"] != "llama" {
		t.Fatalf("unexpected model: %#v", chunk["model"])
	}
}

func TestParseOllamaStreamChunkAcceptsSSEDataLine(t *testing.T) {
	chunk, err := ParseOllamaStreamChunk(`data: {"model":"llama","message":{"role":"assistant","content":"hi"},"done":false}`)
	if err != nil {
		t.Fatalf("ParseOllamaStreamChunk returned error: %v", err)
	}
	if chunk["model"] != "llama" {
		t.Fatalf("unexpected model: %#v", chunk["model"])
	}
}

func TestConvertOllamaChunkToOpenAISSEWritesDataLine(t *testing.T) {
	out, err := ConvertOllamaChunkToOpenAISSE(map[string]any{
		"model": "llama",
		"message": map[string]any{
			"role":    "assistant",
			"content": "hi",
		},
		"done": false,
	}, "llama")
	if err != nil {
		t.Fatalf("ConvertOllamaChunkToOpenAISSE returned error: %v", err)
	}
	if !strings.HasPrefix(string(out), "data: {") || !strings.HasSuffix(string(out), "\n") {
		t.Fatalf("converted OpenAI stream chunk must be SSE data line, got %q", string(out))
	}
}

func TestConvertOllamaChunkToOpenAISSEConvertsObjectToolArgumentsToJSONString(t *testing.T) {
	chunk, err := ParseOllamaStreamChunk(`{
		"model":"llama",
		"message":{"role":"assistant","content":"","tool_calls":[{
			"id":"call_1",
			"type":"function",
			"function":{"name":"read_file","arguments":{"path":"a.txt"}}
		}]},
		"done":false
	}`)
	if err != nil {
		t.Fatalf("ParseOllamaStreamChunk returned error: %v", err)
	}
	out, err := ConvertOllamaChunkToOpenAISSE(chunk, "llama")
	if err != nil {
		t.Fatalf("ConvertOllamaChunkToOpenAISSE returned error: %v", err)
	}

	payload := strings.TrimSpace(strings.TrimPrefix(string(out), "data:"))
	var converted map[string]any
	if err := json.Unmarshal([]byte(payload), &converted); err != nil {
		t.Fatalf("decode converted chunk: %v", err)
	}
	choice := converted["choices"].([]any)[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	call := delta["tool_calls"].([]any)[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["arguments"] != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %#v, want JSON object string", function["arguments"])
	}
}

func TestConvertOllamaDoneChunkPreservesToolCallsAndRepairsFinish(t *testing.T) {
	chunk, err := ParseOllamaStreamChunk(`{
		"model":"llama",
		"message":{"role":"assistant","content":"","tool_calls":[{
			"id":"call_1","type":"function",
			"function":{"name":"read_file","arguments":{"path":"a.txt"}}
		}]},
		"done":true,
		"done_reason":"stop"
	}`)
	if err != nil {
		t.Fatalf("ParseOllamaStreamChunk returned error: %v", err)
	}
	out, err := ConvertOllamaChunkToOpenAISSE(chunk, "llama")
	if err != nil {
		t.Fatalf("ConvertOllamaChunkToOpenAISSE returned error: %v", err)
	}

	payload := strings.TrimSpace(strings.TrimPrefix(string(out), "data:"))
	var converted map[string]any
	if err := json.Unmarshal([]byte(payload), &converted); err != nil {
		t.Fatalf("decode converted chunk: %v", err)
	}
	choice := converted["choices"].([]any)[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || len(delta["tool_calls"].([]any)) != 1 {
		t.Fatalf("done tool chunk was lost: %s", string(out))
	}
}

func TestOllamaChatResponse2OpenAIReadsNestedMessageAndThinking(t *testing.T) {
	out, err := OllamaChatResponse2OpenAI([]byte(`{
		"model":"llama",
		"message":{"role":"assistant","content":"answer","thinking":"reason"},
		"done":true
	}`), "llama")
	if err != nil {
		t.Fatalf("OllamaChatResponse2OpenAI returned error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choices := resp["choices"].([]any)
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "answer" {
		t.Fatalf("content = %#v, want answer", message["content"])
	}
	if message["reasoning_content"] != "reason" {
		t.Fatalf("reasoning_content = %#v, want reason", message["reasoning_content"])
	}
}

func TestOllamaChatResponse2OpenAIReadsToolCalls(t *testing.T) {
	out, err := OllamaChatResponse2OpenAI([]byte(`{
		"model":"llama",
		"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}]},
		"done":true,
		"done_reason":"tool_calls"
	}`), "llama")
	if err != nil {
		t.Fatalf("OllamaChatResponse2OpenAI returned error: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v, want tool_calls", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool calls missing: %s", string(out))
	}
}

func TestOllamaChatResponse2OpenAIConvertsObjectToolArgumentsToJSONString(t *testing.T) {
	out, err := OllamaChatResponse2OpenAI([]byte(`{
		"model":"llama",
		"message":{"role":"assistant","content":"","tool_calls":[{
			"id":"call_1",
			"type":"function",
			"function":{"name":"read_file","arguments":{"path":"a.txt"}}
		}]},
		"done":true,
		"done_reason":"tool_calls"
	}`), "llama")
	if err != nil {
		t.Fatalf("OllamaChatResponse2OpenAI returned error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	call := message["tool_calls"].([]any)[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["arguments"] != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %#v, want JSON object string", function["arguments"])
	}
}

func TestOpenAI2OllamaChatRequestPreservesToolsAndToolMessages(t *testing.T) {
	out, err := OpenAI2OllamaChatRequest([]byte(`{
		"model":"glm-5.2",
		"temperature":0.25,
		"max_tokens":2048,
		"messages":[
			{"role":"user","content":"create a file"},
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok","name":"create_file"}
		],
		"tools":[{"type":"function","strict":true,"function":{"name":"create_file","description":"Create file","parameters":{"type":"object"}}}],
		"tool_choice":"auto",
		"parallel_tool_calls":true,
		"stop":["END"]
	}`))
	if err != nil {
		t.Fatalf("OpenAI2OllamaChatRequest returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools not preserved: %s", string(out))
	}
	tool := tools[0].(map[string]any)
	if tool["strict"] != true {
		t.Fatalf("tool strict flag not preserved: %s", string(out))
	}
	if choice, ok := got["tool_choice"].(string); !ok || choice != "auto" {
		t.Fatalf("tool_choice = %#v, want auto", got["tool_choice"])
	}
	if parallel, ok := got["parallel_tool_calls"].(bool); !ok || !parallel {
		t.Fatalf("parallel_tool_calls = %#v, want true", got["parallel_tool_calls"])
	}
	options, ok := got["options"].(map[string]any)
	if !ok {
		t.Fatalf("options = %#v, want object", got["options"])
	}
	if stop, ok := options["stop"].([]any); !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("options.stop = %#v, want END", options["stop"])
	}
	if options["temperature"] != 0.25 {
		t.Fatalf("options.temperature = %#v, want 0.25", options["temperature"])
	}
	if options["num_predict"] != float64(2048) {
		t.Fatalf("options.num_predict = %#v, want 2048", options["num_predict"])
	}
	if _, leaked := options["max_tokens"]; leaked {
		t.Fatalf("OpenAI max_tokens leaked into Ollama options: %s", string(out))
	}
	if _, leaked := got["stop"]; leaked {
		t.Fatalf("OpenAI stop leaked to unsupported Ollama top-level field: %s", string(out))
	}
	messages := got["messages"].([]any)
	toolMessage := messages[2].(map[string]any)
	if toolMessage["tool_call_id"] != "call_1" || toolMessage["name"] != "create_file" {
		t.Fatalf("tool result message metadata not preserved: %s", string(out))
	}
}

func TestOpenAI2OllamaChatRequestPreservesLegacyFunctions(t *testing.T) {
	out, err := OpenAI2OllamaChatRequest([]byte(`{
		"model":"gpt-test",
		"messages":[{"role":"user","content":"run powershell"}],
		"functions":[{"name":"powershell","description":"Run PowerShell","parameters":{"type":"object"}}],
		"function_call":"auto"
	}`))
	if err != nil {
		t.Fatalf("OpenAI2OllamaChatRequest returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("legacy functions should be converted to tools: %s", string(out))
	}
	tool := tools[0].(map[string]any)
	fn := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn["name"] != "powershell" {
		t.Fatalf("unexpected converted tool: %#v", tool)
	}
}

func TestOpenAI2OllamaChatRequestUsesDeterministicTokenAliasPrecedence(t *testing.T) {
	out, err := OpenAI2OllamaChatRequest([]byte(`{
		"model":"llama",
		"options":{"max_tokens":111,"max_output_tokens":222,"num_predict":333}
	}`))
	if err != nil {
		t.Fatalf("OpenAI2OllamaChatRequest returned error: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	options, _ := body["options"].(map[string]any)
	if options["num_predict"] != float64(333) {
		t.Fatalf("num_predict = %#v, want explicit num_predict 333", options["num_predict"])
	}
	if _, ok := options["max_tokens"]; ok {
		t.Fatalf("max_tokens alias leaked: %#v", options)
	}
}

func TestOpenAI2OllamaChatRequestPreservesNonStringContent(t *testing.T) {
	out, err := OpenAI2OllamaChatRequest([]byte(`{
		"model":"vision-tool-model",
		"messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]
	}`))
	if err != nil {
		t.Fatalf("OpenAI2OllamaChatRequest returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	messages := got["messages"].([]any)
	message := messages[0].(map[string]any)
	content, ok := message["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("non-string content was not preserved: %s", string(out))
	}
}

func TestBuildOllamaShowResponsePublishesArchitectureContextLength(t *testing.T) {
	out, err := BuildOllamaShowResponse("llama", "llama", 8192, 7168, 1024, "llama", true, false, nil)
	if err != nil {
		t.Fatalf("BuildOllamaShowResponse returned error: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("decode show response: %v", err)
	}
	modelInfo, ok := body["model_info"].(map[string]any)
	if !ok {
		t.Fatalf("model_info = %#v, want object", body["model_info"])
	}
	if modelInfo["llama.context_length"] != float64(8192) {
		t.Fatalf("llama.context_length = %#v, want 8192", modelInfo["llama.context_length"])
	}
	if modelInfo["general.context_length"] != float64(8192) {
		t.Fatalf("general.context_length = %#v, want compatibility field", modelInfo["general.context_length"])
	}
	if modelInfo["input_token_limit"] != float64(7168) {
		t.Fatalf("input_token_limit = %#v, want 7168", modelInfo["input_token_limit"])
	}
}
