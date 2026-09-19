package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 鲁棒性：畸形/极端输入不得 panic。
func TestDoctorRobustness(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"空对象", `{}`},
		{"空数组", `{"providers":[],"models":[]}`},
		{"null providers", `{"providers":null,"models":null}`},
		{"base 只有 scheme 分隔符", `{"providers":[{"id":"a","base_url":"://","enabled":true}]}`},
		{"base 含 unicode", `{"providers":[{"id":"中文","base_url":"https://例子.com/v1","enabled":true}]}`},
		{"超长 base", `{"providers":[{"id":"a","base_url":"https://` + string(make([]byte, 0)) + `h.com/` + repeatStr("x", 5000) + `","enabled":true}]}`},
		{"模型名为空", `{"providers":[],"models":[{"name":"","provider_id":"x"}]}`},
		{"provider 无 id/name", `{"providers":[{"type":"openai","base_url":"https://h"}],"models":[]}`},
		{"负数与零值", `{"config_version":-5,"port":-1,"providers":[],"models":[]}`},
		{"未知字段", `{"providers":[{"id":"a","base_url":"https://h","totally_unknown":123}],"models":[],"extra":{}}`},
		{"chat_path 是绝对路径", `{"providers":[{"id":"a","base_url":"https://h","transport":{"chat_path":"/v1/chat/completions","models_path":"/v1/models"}}],"models":[]}`},
		{"chat_path 含重复斜杠", `{"providers":[{"id":"a","base_url":"https://h//v1//","transport":{"chat_path":"//chat//completions"}}],"models":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "config.json")
			if err := os.WriteFile(p, []byte(c.body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := LoadForDoctor(p)
			if err != nil {
				t.Logf("解析失败(可接受): %v", err)
				return
			}
			report := CheckConfig(cfg, p)
			out := RenderDoctorReport(report)
			if len(out) == 0 {
				t.Error("渲染输出为空")
			}
			t.Logf("error=%d warn=%d", report.CountBySeverity()[DoctorError], report.CountBySeverity()[DoctorWarn])
		})
	}
}

// nil 配置不得 panic（注释声称支持）。
func TestDoctorNilConfigDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CheckConfig(nil) panic: %v", r)
		}
	}()
	report := CheckConfig(nil, "x.json")
	_ = RenderDoctorReport(report)
}

// 输出必须确定性：同一配置多次渲染结果完全一致（避免 map 迭代顺序泄漏）。
func TestDoctorRenderDeterministic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{"providers":[
		{"id":"z","base_url":"https://h1","enabled":true},
		{"id":"a","base_url":"https://h2","enabled":true},
		{"id":"m","base_url":"https://h3","enabled":true}],"models":[]}`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadForDoctor(p)
	if err != nil {
		t.Fatal(err)
	}
	first := RenderDoctorReport(CheckConfig(cfg, p))
	for i := 0; i < 20; i++ {
		again := RenderDoctorReport(CheckConfig(cfg, p))
		if again != first {
			t.Fatalf("第 %d 次渲染与首次不一致（输出不确定）", i+1)
		}
	}
}

func repeatStr(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
