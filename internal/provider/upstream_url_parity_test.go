package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ResolveUpstreamURL 是 config doctor 展示"实际上游地址"的唯一事实源。
// 本测试通过真实发起 ListModels 请求，断言它预测的路径与真实转发的路径一致，
// 防止 doctor 与真实行为漂移后给出误导性诊断。
func TestResolveUpstreamURLMatchesRealRequest(t *testing.T) {
	cases := []struct {
		name      string
		basePath  string
		chatPath  string
		modelPath string
	}{
		{"useai_带v1与显式transport", "/v1", "chat/completions", "models"},
		{"裸域名_无transport", "", "", ""},
		{"gemini风格全路径", "/v1beta/openai", "", ""},
		{"deepseek显式v1路径", "", "v1/chat/completions", "v1/models"},
		{"非版本自定义路径", "/api", "", ""},
		{"版本化自定义路径", "/api/paas/v4", "chat/completions", "models"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
			}))
			defer srv.Close()

			baseURL := srv.URL + c.basePath
			p := NewOpenAIProviderWithTransport("x", "x", "k", baseURL, c.chatPath, c.modelPath, true, 0)
			_, _ = p.ListModels(context.Background())

			want := ResolveUpstreamURL("x", "x", baseURL, "", c.chatPath, c.modelPath, "models")
			wantPath := want[len(srv.URL):]
			if gotPath != wantPath {
				t.Fatalf("doctor 预测与真实请求不一致:\n 实际请求 = %s\n 预测     = %s", gotPath, wantPath)
			}
			if gotPath == "" {
				t.Fatal("未捕获到请求路径")
			}
		})
	}
}
