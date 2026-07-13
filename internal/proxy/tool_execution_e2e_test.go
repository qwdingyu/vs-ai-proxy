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
		case "apply_patch":
			var args struct {
				Patch string `json:"patch"`
				Diff  string `json:"diff"`
			}
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				t.Fatalf("apply_patch arguments are not executable JSON: %v; args=%q", err, call.Function.Arguments)
			}
			patch := firstNonEmptyString(args.Patch, args.Diff)
			path, before, after := parseSingleFilePatch(t, patch)
			content, ok := r.files[path]
			if !ok {
				t.Fatalf("apply_patch target does not exist: %q", path)
			}
			if !strings.Contains(content, before) {
				t.Fatalf("apply_patch before text %q not found in %q content %q", before, path, content)
			}
			r.files[path] = strings.Replace(content, before, after, 1)
			results = append(results, "patched:"+path)
		case "powershell", "terminal", "run_in_terminal", "run_command_in_terminal":
			var args struct {
				Command string `json:"command"`
				Cmd     string `json:"cmd"`
				Script  string `json:"script"`
			}
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				t.Fatalf("%s arguments are not executable JSON: %v; args=%q", call.Function.Name, err, call.Function.Arguments)
			}
			command := firstNonEmptyString(args.Command, args.Cmd, args.Script)
			if command == "" {
				t.Fatalf("%s missing command/cmd/script: %#v", call.Function.Name, args)
			}
			results = append(results, call.Function.Name+":"+command)
		case "git":
			var args struct {
				Args    []string `json:"args"`
				Command string   `json:"command"`
			}
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				t.Fatalf("git arguments are not executable JSON: %v; args=%q", err, call.Function.Arguments)
			}
			command := firstNonEmptyString(strings.Join(args.Args, " "), args.Command)
			if command == "" {
				t.Fatalf("git missing args/command: %#v", args)
			}
			results = append(results, "git:"+command)
		case "code_search", "grep_search", "file_search", "find_symbol", "list_files":
			var args struct {
				Query   string `json:"query"`
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				t.Fatalf("%s arguments are not executable JSON: %v; args=%q", call.Function.Name, err, call.Function.Arguments)
			}
			query := firstNonEmptyString(args.Query, args.Pattern, args.Path)
			if query == "" {
				t.Fatalf("%s missing query/pattern/path: %#v", call.Function.Name, args)
			}
			results = append(results, call.Function.Name+":"+query)
		default:
			t.Fatalf("unexpected tool reached VS executor: %q", call.Function.Name)
		}
	}
	return results
}

func parseSingleFilePatch(t *testing.T, patch string) (path string, before string, after string) {
	t.Helper()
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "*** Update File: "):
			path = strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			before = strings.TrimPrefix(line, "-")
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			after = strings.TrimPrefix(line, "+")
		}
	}
	if path == "" || before == "" || after == "" {
		t.Fatalf("unsupported apply_patch payload: %q", patch)
	}
	return path, before, after
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

func TestVisualStudioToolExecutionE2EOpenAINonStreamApplyPatch(t *testing.T) {
	rawBody := []byte(`{"id":"chatcmpl-patch","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"docs/patch.md\",\"content\":\"old\"}"}},{"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":"{\"patch\":\"*** Begin Patch\\n*** Update File: docs/patch.md\\n@@\\n-old\\n+new\\n*** End Patch\"}"}},{"id":"call_get","type":"function","function":{"name":"get_file","arguments":"{\"filename\":\"docs/patch.md\"}"}}]},"finish_reason":"tool_calls"}]}`)
	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, "")
	prov.rawBody = rawBody

	resp := postOpenAIChatForToolExecution(t, prov, false)
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 3 || calls[1].Function.Name != "apply_patch" || resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("VS client did not receive executable apply_patch tool_calls: %#v", resp.Choices[0])
	}
	results := newVisualStudioToolRuntime().execute(t, calls)
	if strings.Join(results, ",") != "created:docs/patch.md,patched:docs/patch.md,read:docs/patch.md:new" {
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

func TestVisualStudioToolExecutionE2EOpenAIStreamApplyPatch(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_create","type":"function","function":{"name":"create_file","arguments":"{\"path\":\"docs/stream-patch.md\",\"content\":\"old\"}"}},{"index":1,"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":"{\"patch\":\"*** Begin Patch\\n*** Update File: docs/stream-patch.md\\n@@\\n-old\\n+new\\n*** End Patch\"}"}},{"index":2,"id":"call_get","type":"function","function":{"name":"get_file","arguments":"{\"filename\":\"docs/stream-patch.md\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, stream)

	calls, finishReason := postOpenAIStreamForToolExecution(t, prov)
	if len(calls) != 3 || calls[1].Function.Name != "apply_patch" || finishReason != "tool_calls" {
		t.Fatalf("VS stream client did not receive executable apply_patch tool_calls: calls=%#v finish=%q", calls, finishReason)
	}
	results := newVisualStudioToolRuntime().execute(t, calls)
	if strings.Join(results, ",") != "created:docs/stream-patch.md,patched:docs/stream-patch.md,read:docs/stream-patch.md:new" {
		t.Fatalf("unexpected execution results: %#v", results)
	}
}

func TestVisualStudioToolExecutionE2EOpenAIStreamApplyDiffAlias(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"apply_diff","arguments":"{\"patch\":\"*** Begin Patch\\n*** Update File: docs/alias-patch.md\\n@@\\n-old\\n+new\\n*** End Patch\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, stream)

	calls, finishReason := postOpenAIStreamForToolExecution(t, prov)
	if len(calls) != 1 || calls[0].Function.Name != "apply_patch" || finishReason != "tool_calls" {
		t.Fatalf("VS stream client did not canonicalize apply_diff alias: calls=%#v finish=%q", calls, finishReason)
	}
	runtime := newVisualStudioToolRuntime()
	runtime.files["docs/alias-patch.md"] = "old"
	results := runtime.execute(t, calls)
	if strings.Join(results, ",") != "patched:docs/alias-patch.md" || runtime.files["docs/alias-patch.md"] != "new" {
		t.Fatalf("unexpected execution results: %#v files=%#v", results, runtime.files)
	}
}

func TestVisualStudioToolExecutionE2EOpenAIStreamCommonCopilotToolFamilies(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_tests","type":"function","function":{"name":"run_tests","arguments":"{\"command\":\"go test ./...\"}"}},{"index":1,"id":"call_git","type":"function","function":{"name":"git_status","arguments":"{\"args\":[\"status\",\"--short\"]}"}},{"index":2,"id":"call_search","type":"function","function":{"name":"rg","arguments":"{\"query\":\"create_file\"}"}},{"index":3,"id":"call_read","type":"function","function":{"name":"read_file","arguments":"{\"filename\":\"docs/common.md\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, stream)

	calls, finishReason := postOpenAIStreamForToolExecution(t, prov)
	if len(calls) != 4 || finishReason != "tool_calls" {
		t.Fatalf("VS stream client did not receive common tool family calls: calls=%#v finish=%q", calls, finishReason)
	}
	gotNames := []string{calls[0].Function.Name, calls[1].Function.Name, calls[2].Function.Name, calls[3].Function.Name}
	wantNames := []string{"powershell", "git", "grep_search", "get_file"}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Fatalf("tool %d canonicalized to %q, want %q; calls=%#v", i, gotNames[i], wantNames[i], calls)
		}
	}
	runtime := newVisualStudioToolRuntime()
	runtime.files["docs/common.md"] = "common ok"
	results := runtime.execute(t, calls)
	if strings.Join(results, ",") != "powershell:go test ./...,git:status --short,grep_search:create_file,read:docs/common.md:common ok" {
		t.Fatalf("unexpected execution results: %#v", results)
	}
}

func TestVisualStudioToolExecutionE2EOpenAIStreamRunCommandInTerminalAlias(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_terminal","type":"function","function":{"name":"run_command_in_terminal","arguments":"{\"command\":\"dotnet test\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, stream)

	calls, finishReason := postOpenAIStreamForToolExecution(t, prov)
	if len(calls) != 1 || finishReason != "tool_calls" {
		t.Fatalf("VS stream client did not receive terminal tool call: calls=%#v finish=%q", calls, finishReason)
	}
	if calls[0].Function.Name != "run_command_in_terminal" {
		t.Fatalf("tool canonicalized to %q, want run_command_in_terminal; calls=%#v", calls[0].Function.Name, calls)
	}
	results := newVisualStudioToolRuntime().execute(t, calls)
	if strings.Join(results, ",") != "run_command_in_terminal:dotnet test" {
		t.Fatalf("unexpected execution results: %#v", results)
	}
}

func TestVisualStudioToolExecutionE2EOpenAIStreamRunCommandInTerminalFallsBackToDeclaredPowershell(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_terminal","type":"function","function":{"name":"run_command_in_terminal","arguments":"{\"command\":\"dotnet test\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, stream)

	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})
	body := `{
		"model":"gpt-test",
		"messages":[{"role":"user","content":"please run tests"}],
		"tools":[{"type":"function","function":{"name":"powershell","description":"Run command","parameters":{"type":"object"}}}],
		"stream":true
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	calls, finishReason := parseOpenAIStreamToolCalls(t, rec.Body.String())
	if len(calls) != 1 || finishReason != "tool_calls" || calls[0].Function.Name != "powershell" {
		t.Fatalf("terminal alias should map to declared powershell: calls=%#v finish=%q body=%s", calls, finishReason, rec.Body.String())
	}
	results := newVisualStudioToolRuntime().execute(t, calls)
	if strings.Join(results, ",") != "powershell:dotnet test" {
		t.Fatalf("unexpected execution results: %#v", results)
	}
}

func TestVisualStudioToolExecutionE2EOpenAIStreamCodeSearchFallsBackToDeclaredGrepSearch(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_search","type":"function","function":{"name":"code_search","arguments":"{\"query\":\"grep_search\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	prov := newFakeProvider("useai", true, []string{"gpt-test"}, nil, stream)

	server := newOpenServer(prov)
	handler := withMux(server, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)
	})
	body := `{
		"model":"gpt-test",
		"messages":[{"role":"user","content":"please search"}],
		"tools":[{"type":"function","function":{"name":"grep_search","description":"Search","parameters":{"type":"object"}}}],
		"stream":true
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	calls, finishReason := parseOpenAIStreamToolCalls(t, rec.Body.String())
	if len(calls) != 1 || finishReason != "tool_calls" || calls[0].Function.Name != "grep_search" {
		t.Fatalf("code_search should map to declared grep_search: calls=%#v finish=%q body=%s", calls, finishReason, rec.Body.String())
	}
	results := newVisualStudioToolRuntime().execute(t, calls)
	if strings.Join(results, ",") != "grep_search:grep_search" {
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
	return parseOpenAIStreamToolCalls(t, rec.Body.String())
}

func parseOpenAIStreamToolCalls(t *testing.T, body string) ([]provider.ToolCall, string) {
	t.Helper()
	toolChunks := map[int]map[string]any{}
	finishReason := ""
	for _, line := range strings.Split(body, "\n") {
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
			{"type":"function","function":{"name":"apply_patch","description":"Apply patch","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"get_file","description":"Read file","parameters":{"type":"object"}}},
				{"type":"function","function":{"name":"powershell","description":"Run PowerShell command","parameters":{"type":"object"}}},
				{"type":"function","function":{"name":"run_command_in_terminal","description":"Run terminal command","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"git","description":"Run Git command","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"grep_search","description":"Search text","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"code_search","description":"Search code","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"file_search","description":"Search files","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"find_symbol","description":"Find symbol","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"list_files","description":"List files","parameters":{"type":"object"}}}
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
