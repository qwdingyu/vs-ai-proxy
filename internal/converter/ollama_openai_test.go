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

// 回归：llama.context_length 必须**无条件**存在，与 family 取值无关。
//
// 原测试只用 family="llama" 断言该键——恰好 architecture+"."+context_length
// 就等于 llama.context_length，于是"通过"了；真实链路里 family 是 deepseek /
// qwen 这类具体架构名，该键就消失，只剩 deepseek.context_length。
//
// 注意（事实核对 2026-09）：llama.context_length 只是**兼容性冗余键**。
// Ollama 官方只返回 <arch>.context_length；VS Code llama.vscode 的解析规则
// （language-model-token-limits.ts）读的是顶层 context_length / input_token_limit /
// max_output_tokens，候选对象不含 model_info，字段表也不含 llama.context_length。
// 保留该键是因为第三方代理实现以此为约定，且多一个键无害；但**主路径依赖的是
// 顶层字段**，那条链路由 TestFinalTopLevelTokenBudgetFields 单独覆盖。
func TestBuildOllamaModelInfoAlwaysExposesLlamaContextLength(t *testing.T) {
	for _, family := range []string{"llama", "deepseek", "qwen", "glm", "kimi", "api", ""} {
		info := BuildOllamaModelInfo("some-model", 1_048_576, 1_048_576, 131072, family, true, false)
		got, ok := info["llama.context_length"]
		if !ok {
			t.Errorf("family=%q 时缺少 llama.context_length（客户端会退到 ~100K）", family)
			continue
		}
		if got != 1_048_576 {
			t.Errorf("family=%q 时 llama.context_length = %#v, want 1048576", family, got)
		}
		if info["general.context_length"] != 1_048_576 {
			t.Errorf("family=%q 时 general.context_length 缺失或错误", family)
		}
	}
}

// 非 llama 架构时，架构限定键与 llama 键必须同时存在且一致。
func TestBuildOllamaModelInfoKeepsArchitectureAndLlamaKeys(t *testing.T) {
	info := BuildOllamaModelInfo("m", 262144, 262144, 8192, "deepseek", true, false)
	if info["deepseek.context_length"] != 262144 {
		t.Errorf("deepseek.context_length = %#v, want 262144", info["deepseek.context_length"])
	}
	if info["llama.context_length"] != 262144 {
		t.Errorf("llama.context_length = %#v, want 262144（必须与架构键一致）", info["llama.context_length"])
	}
}

// /api/show 与 /api/tags 暴露的 model_info 都必须带上该键（端到端）。
func TestBuildOllamaShowResponseExposesLlamaContextLengthForRealFamily(t *testing.T) {
	out, err := BuildOllamaShowResponse("deepseek-v4-flash", "deepseek-v4-flash",
		1_048_576, 1_048_576, 131072, "deepseek", true, false, nil)
	if err != nil {
		t.Fatalf("BuildOllamaShowResponse 失败: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	modelInfo, ok := body["model_info"].(map[string]any)
	if !ok {
		t.Fatalf("model_info 不是对象: %#v", body["model_info"])
	}
	if modelInfo["llama.context_length"] != float64(1_048_576) {
		t.Errorf("真实 family 下 llama.context_length = %#v, want 1048576",
			modelInfo["llama.context_length"])
	}
}

// 主路径：VS Code llama.vscode 的真实解析规则（language-model-token-limits.ts）下，
// 我们的 /api/tags 顶层字段必须能被解析出输入/输出预算，不能退到 8192/4096。
//
// 该规则：
//
//	候选对象 = [顶层, meta, metadata, limits, top_provider]（不含 model_info）
//	输入字段 = max_input_tokens|input_token_limit|prompt_token_limit|max_prompt_tokens|
//	          context_length|context_window|max_context_length|max_sequence_length|
//	          max_position_embeddings|n_ctx|n_ctx_train
//	输出字段 = max_output_tokens|output_token_limit|completion_token_limit|
//	          max_completion_tokens|max_generated_tokens
func TestFinalTopLevelTokenBudgetFields(t *testing.T) {
	info := BuildOllamaModelInfo("deepseek-v4-flash", 1_048_576, 1_048_576, 131_072, "deepseek", true, false)

	inputFields := []string{"max_input_tokens", "input_token_limit", "prompt_token_limit",
		"max_prompt_tokens", "context_length", "context_window", "max_context_length",
		"max_sequence_length", "max_position_embeddings", "n_ctx", "n_ctx_train"}
	outputFields := []string{"max_output_tokens", "output_token_limit", "completion_token_limit",
		"max_completion_tokens", "max_generated_tokens"}

	pick := func(fields []string) (string, float64) {
		for _, f := range fields {
			if v, ok := info[f].(int); ok && v > 0 {
				return f, float64(v)
			}
		}
		return "", 0
	}

	inField, inVal := pick(inputFields)
	if inField == "" {
		t.Fatal("顶层无可解析的输入预算字段：VS Code 会退到 8192")
	}
	if inVal != 1_048_576 {
		t.Errorf("%s = %v, want 1048576", inField, inVal)
	}
	outField, outVal := pick(outputFields)
	if outField == "" {
		t.Fatal("顶层无可解析的输出预算字段：VS Code 会退到 4096")
	}
	if outVal != 131_072 {
		t.Errorf("%s = %v, want 131072", outField, outVal)
	}
	t.Logf("VS Code 规则解析：输入 ← %s=%v，输出 ← %s=%v", inField, inVal, outField, outVal)
}
