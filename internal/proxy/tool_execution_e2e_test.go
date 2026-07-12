package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

type visualStudioToolRuntime struct {
	files map[string]string
}

func newVisualStudioToolRuntime() *visualStudioToolRuntime {
	return &visualStudioToolRuntime{files: map[string]string{}}
}

func (r *visualStudioToolRuntime) execute(t *testing.T, calls []provider.ToolCall) []string {
	t.Helper()
	results := make([]string, 0, len(calls))
	for _, call := range calls {
		switch call.Function.Name {
		case "create_file":
			var args struct {
				Path     string `json:"path"`
				Filename string `json:"filename"`
				Content  string `json:"content"`
			}
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				t.Fatalf("create_file arguments are not executable JSON: %v; args=%q", err, call.Function.Arguments)
			}
			path := firstNonEmptyString(args.Path, args.Filename)
			if path == "" {
				t.Fatalf("create_file missing path/filename: %#v", args)
			}
			r.files[path] = args.Content
			results = append(results, "created:"+path)
		case "get_file":
			var args struct {
				Path     string `json:"path"`
				Filename string `json:"filename"`
			}
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				t.Fatalf("get_file arguments are not executable JSON: %v; args=%q", err, call.Function.Arguments)
			}
			path := firstNonEmptyString(args.Path, args.Filename)
			content, ok := r.files[path]
			if !ok {
				t.Fatalf("get_file target does not exist after tool execution: %q", path)
			}
			results = append(results, "read:"+path+":"+content)
		default:
			t.Fatalf("unexpected tool reached VS executor: %q", call.Function.Name)
		}
	}
	return results
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func TestVisualStudioToolExecutionE2EOpenAINonStreamToolCalls(t *testing.T) {
	rawBody := []byte(`{"id":"chatcmpl-tools","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"docs/e2e.md\",\"content\":\"ok\"}"}},{"id":"call_get","type":"function","function":{"name":"get_file","arguments":"{\"filename\":\"docs/e2e.md\"}"}}]},"finish_reason":"tool_calls"}]}`)
	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, "")
	prov.rawBody = rawBody

	resp := postOpenAIChatForToolExecution(t, prov, false)
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 2 || resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("VS client did not receive executable tool_calls: %#v", resp.Choices[0])
	}

	results := newVisualStudioToolRuntime().execute(t, calls)
	if strings.Join(results, ",") != "created:docs/e2e.md,read:docs/e2e.md:ok" {
		t.Fatalf("unexpected execution results: %#v", results)
	}
}

func TestVisualStudioToolExecutionE2EOpenAINonStreamLegacyFunctionCall(t *testing.T) {
	tests := []struct {
		name    string
		rawBody []byte
		seed    map[string]string
		want    string
	}{
		{
			name:    "create_file",
			rawBody: []byte(`{"id":"chatcmpl-create","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"","function_call":{"name":"create_file","arguments":"{\"path\":\"docs/legacy-create.md\",\"content\":\"legacy ok\"}"}},"finish_reason":"function_call"}]}`),
			want:    "created:docs/legacy-create.md",
		},
		{
			name:    "get_file",
			rawBody: []byte(`{"id":"chatcmpl-get","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"","function_call":{"name":"get_file","arguments":"{\"filename\":\"docs/legacy-get.md\"}"}},"finish_reason":"function_call"}]}`),
			seed:    map[string]string{"docs/legacy-get.md": "seeded"},
			want:    "read:docs/legacy-get.md:seeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, "")
			prov.rawBody = tt.rawBody
			resp := postOpenAIChatForToolExecution(t, prov, false)
			call := resp.Choices[0].Message.FunctionCall
			if call == nil || call.Name != tt.name || resp.Choices[0].FinishReason != "function_call" {
				t.Fatalf("VS client did not receive executable legacy function_call: %#v", resp.Choices[0])
			}
			runtime := newVisualStudioToolRuntime()
			for path, content := range tt.seed {
				runtime.files[path] = content
			}
			results := runtime.execute(t, []provider.ToolCall{{ID: "function_call", Type: "function", Function: *call}})
			if strings.Join(results, ",") != tt.want {
				t.Fatalf("unexpected execution results: %#v", results)
			}
		})
	}
}

func TestVisualStudioToolExecutionE2EOpenAIStreamToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"docs/stream.md\",\"content\":\"stream ok\"}"}},{"index":1,"id":"call_get","type":"function","function":{"name":"get_file","arguments":"{\"filename\":\"docs/stream.md\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, stream)

	calls, finishReason := postOpenAIStreamForToolExecution(t, prov)
	if len(calls) != 2 || finishReason != "tool_calls" {
		t.Fatalf("VS stream client did not receive executable tool_calls: calls=%#v finish=%q", calls, finishReason)
	}
	results := newVisualStudioToolRuntime().execute(t, calls)
	if strings.Join(results, ",") != "created:docs/stream.md,read:docs/stream.md:stream ok" {
		t.Fatalf("unexpected execution results: %#v", results)
	}
}

func TestVisualStudioToolExecutionE2EOpenAIStreamLegacyFunctionCall(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		seed   map[string]string
		want   string
	}{
		{
			name: "create_file",
			stream: strings.Join([]string{
				`data: {"choices":[{"delta":{"function_call":{"name":"create_file","arguments":"{\"path\":\"docs/stream-legacy-create.md\",\"content\":\"stream legacy ok\"}"}},"finish_reason":null}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
				``,
			}, "\n"),
			want: "created:docs/stream-legacy-create.md",
		},
		{
			name: "get_file",
			stream: strings.Join([]string{
				`data: {"choices":[{"delta":{"function_call":{"name":"get_file","arguments":"{\"filename\":\"docs/stream-legacy-get.md\"}"}},"finish_reason":null}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
				``,
			}, "\n"),
			seed: map[string]string{"docs/stream-legacy-get.md": "stream seeded"},
			want: "read:docs/stream-legacy-get.md:stream seeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, tt.stream)
			calls, finishReason := postOpenAIStreamForToolExecution(t, prov)
			if len(calls) != 1 || calls[0].Function.Name != tt.name || finishReason != "tool_calls" {
				t.Fatalf("VS stream client did not receive executable legacy function_call: calls=%#v finish=%q", calls, finishReason)
			}
			runtime := newVisualStudioToolRuntime()
			for path, content := range tt.seed {
				runtime.files[path] = content
			}
			results := runtime.execute(t, calls)
			if strings.Join(results, ",") != tt.want {
				t.Fatalf("unexpected execution results: %#v", results)
			}
		})
	}
}

func postOpenAIChatForToolExecution(t *testing.T, prov *fakeProvider, stream bool) provider.ChatResponse {
	t.Helper()
	rec := postOpenAIChatCompletion(t, prov, stream)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp provider.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("VS client could not parse non-stream JSON response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}

func postOpenAIStreamForToolExecution(t *testing.T, prov *fakeProvider) ([]provider.ToolCall, string) {
	t.Helper()
	rec := postOpenAIChatCompletion(t, prov, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	toolChunks := map[int]map[string]any{}
	finishReason := ""
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		chunk, err := parseOpenAIStreamPayload(payload)
		if err != nil {
			t.Fatalf("VS stream client could not parse SSE payload: %v; payload=%s", err, payload)
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		mergeOpenAIStreamToolCalls(toolChunks, chunk.ToolCalls)
	}
	return buildProviderToolCalls(toolChunks), finishReason
}

func postOpenAIChatCompletion(t *testing.T, prov *fakeProvider, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})
	body := `{
		"model":"gpt-test",
		"messages":[{"role":"user","content":"please use tools"}],
		"tools":[
			{"type":"function","function":{"name":"create_file","description":"Create file","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"get_file","description":"Read file","parameters":{"type":"object"}}}
		],
		"stream":` + boolLiteral(stream) + `
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func boolLiteral(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
