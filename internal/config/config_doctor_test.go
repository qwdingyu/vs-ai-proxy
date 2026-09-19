package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// config doctor 验收测试
//
// 验收标准来自 docs/46：doctor 必须能明确报告发现 #2/#3 的场景
//（非版本路径 base_url、升级后自动写回），并保持完全只读。
// ---------------------------------------------------------------------------

func writeTestConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	return path
}

func cleanBase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

// 场景 A（46 号发现 #2/#3）：base_url 是非版本路径 + transport 为空。
// v0.2.69 会推导出 .../api/v1/chat/completions，当前版本推导出 .../api/chat/completions，
// 且启动时会把这个推导结果写回磁盘。doctor 必须同时报告「URL」与「将写回」。
func TestDoctor_NonVersionBaseURLFlagsRewrite(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{
  "config_version": 2, "port": 9990,
  "providers": [{"id":"p1","name":"p1","type":"openai","api_key":"k","base_url":"https://host/api","enabled":true}],
  "models": []
}`)

	cfg, resolved, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	if resolved != path {
		t.Fatalf("返回路径 %q 与传入的不一致", resolved)
	}

	report := CheckConfig(cfg, resolved)

	if !report.RewriteNeeded {
		t.Error("期望 RewriteNeeded=true（transport 为空，启动会推导并写回）")
	}
	// 注意：非版本路径只是"路径会变化且被固化"，doctor 无法断定上游一定不接受，
	// 因此是 INFO 级而非 WARN/ERROR。这里断言的是可观测性，不是误报告警。

	// 必须能找到 p1 的报告，且 chat URL 是拼接后的真实地址
	var got *DoctorProviderReport
	for i := range report.Providers {
		if report.Providers[i].ID == "p1" {
			got = &report.Providers[i]
		}
	}
	if got == nil {
		t.Fatal("未找到 provider p1 的报告")
	}
	want := "https://host/api/chat/completions"
	if got.ChatURL != want {
		t.Fatalf("ChatURL = %s, want %s", got.ChatURL, want)
	}
	if !got.ChatPathDerived {
		t.Error("期望 ChatPathDerived=true（磁盘上 transport 为空，是推导出来的）")
	}
}

// 只读性是 doctor 的核心约束：诊断不能改变磁盘文件。
func TestDoctor_LoadForDoctorDoesNotWrite(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{"config_version":1,"providers":[{"id":"p1","name":"p1","type":"openai","base_url":"https://h/v1","enabled":true}],"models":[]}`)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}

	cfg, _, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	_ = CheckConfig(cfg, path)

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("再次读取失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("LoadForDoctor/CheckConfig 修改了磁盘文件！\nbefore=%s\nafter=%s", before, after)
	}
}

// config_version 为旧值时必须报告「将升级」。
func TestDoctor_ReportsConfigVersionMigration(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{"config_version":1,"providers":[],"models":[]}`)

	cfg, _, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, path)
	if !report.ConfigVersionMigrated {
		t.Error("期望 ConfigVersionMigrated=true")
	}
	if !report.RewriteNeeded {
		t.Error("期望 RewriteNeeded=true")
	}
}

// anthropic 类型的 chat_path 不是 Messages 端点 → ERROR。
func TestDoctor_AnthropicWrongChatPath(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{"config_version":2,"providers":[
		{"id":"a1","name":"a1","type":"anthropic","api_key":"k","base_url":"https://h","enabled":true,
		 "transport":{"chat_path":"chat/completions","models_path":"models"}}],"models":[]}`)

	cfg, _, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, path)

	found := false
	for _, f := range report.Findings {
		if f.Severity == DoctorError && f.Subject == "a1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未对 anthropic 类型的错误 chat_path 报告 ERROR: %+v", report.Findings)
	}
}

// 模型绑定到不存在的 provider_id → WARN。
func TestDoctor_ModelBoundToMissingProvider(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{"config_version":2,"providers":[
		{"id":"p1","name":"p1","type":"openai","api_key":"k","base_url":"https://h/v1","enabled":true,
		 "transport":{"chat_path":"chat/completions","models_path":"models"}}],
	 "models":[{"name":"m1","provider_id":"does-not-exist"}]}`)

	cfg, _, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, path)

	found := false
	for _, f := range report.Findings {
		if f.Severity == DoctorWarn && f.Subject == "m1" && f.Message != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未对悬空的 provider_id 绑定报告 WARN: %+v", report.Findings)
	}
	if !report.HasProblems() {
		t.Error("期望 HasProblems=true")
	}
}

// 同名模型绑定到不同 provider 是正常用法，不得报告为重复。
func TestDoctor_SameModelDifferentProvidersIsNotDuplicate(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{"config_version":2,"providers":[
		{"id":"a","name":"a","type":"openai","api_key":"k","base_url":"https://h/v1","enabled":true,
		 "transport":{"chat_path":"chat/completions","models_path":"models"}},
		{"id":"b","name":"b","type":"openai","api_key":"k","base_url":"https://h2/v1","enabled":true,
		 "transport":{"chat_path":"chat/completions","models_path":"models"}}],
	 "models":[{"name":"m1","provider_id":"a"},{"name":"m1","provider_id":"b"}]}`)

	cfg, _, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, path)

	for _, f := range report.Findings {
		if f.Severity == DoctorWarn || f.Severity == DoctorError {
			t.Errorf("同名不同 provider 不应产生告警: [%s] %s", f.Severity, f.Message)
		}
	}
	if report.HasProblems() {
		t.Error("期望 HasProblems=false")
	}
}

// 同名 + 同 provider_id 才是真正的重复。
func TestDoctor_TrueDuplicateModelIsReported(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{"config_version":2,"providers":[
		{"id":"a","name":"a","type":"openai","api_key":"k","base_url":"https://h/v1","enabled":true,
		 "transport":{"chat_path":"chat/completions","models_path":"models"}}],
	 "models":[{"name":"m1","provider_id":"a"},{"name":"m1","provider_id":"a"}]}`)

	cfg, _, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, path)

	found := false
	for _, f := range report.Findings {
		if f.Severity == DoctorWarn && f.Subject == "m1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未报告真正的重复绑定: %+v", report.Findings)
	}
}

// base_url 缺少 scheme → ERROR；为空 → WARN。
func TestDoctor_BadBaseURL(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{"config_version":2,"providers":[
		{"id":"noscheme","name":"noscheme","type":"openai","api_key":"k","base_url":"host/v1","enabled":true,
		 "transport":{"chat_path":"chat/completions","models_path":"models"}},
		{"id":"empty","name":"empty","type":"openai","api_key":"k","base_url":"","enabled":true,
		 "transport":{"chat_path":"chat/completions","models_path":"models"}}],"models":[]}`)

	cfg, _, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, path)

	sev := map[string]DoctorSeverity{}
	for _, f := range report.Findings {
		if _, ok := sev[f.Subject]; !ok {
			sev[f.Subject] = f.Severity
		}
	}
	if sev["noscheme"] != DoctorError {
		t.Errorf("noscheme 期望 ERROR, 得到 %q", sev["noscheme"])
	}
	if sev["empty"] != DoctorWarn {
		t.Errorf("empty 期望 WARN, 得到 %q", sev["empty"])
	}
}

// 重复的版本段（/v1/v1/）必须被抓住——这正是 46 号发现 #2 的失效形态。
// base_url 与 transport 都带版本前缀时，joinURLPath 会做重叠段去重，
// 不会产生 /v1/v1/。验证这一真实行为，避免 doctor 去防一个不会发生的故障。
func TestDoctor_OverlappingVersionSegmentsAreDeduped(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{"config_version":2,"providers":[
		{"id":"dup","name":"dup","type":"openai","api_key":"k","base_url":"https://h/v1","enabled":true,
		 "transport":{"chat_path":"v1/chat/completions","models_path":"v1/models"}}],"models":[]}`)

	cfg, _, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, path)

	var rep *DoctorProviderReport
	for i := range report.Providers {
		if report.Providers[i].ID == "dup" {
			rep = &report.Providers[i]
		}
	}
	if rep == nil {
		t.Fatal("未找到 provider dup")
	}
	if rep.ChatURL != "https://h/v1/chat/completions" {
		t.Fatalf("重叠版本段未被去重: %s", rep.ChatURL)
	}
	for _, f := range report.Findings {
		if f.Severity == DoctorError || f.Severity == DoctorWarn {
			t.Errorf("去重后的正常配置不应产生告警: [%s] %s", f.Severity, f.Message)
		}
	}
}

func TestDoctor_RenderContainsKeyInfo(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{"config_version":2,"providers":[
		{"id":"p1","name":"p1","type":"openai","api_key":"k","base_url":"https://host/api","enabled":true}],
	 "models":[]}`)

	cfg, _, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, path)
	out := RenderDoctorReport(report)

	for _, want := range []string{
		"只读诊断",
		"https://host/api/chat/completions",
		"由推导填充",
		"汇总",
	} {
		if !contains(out, want) {
			t.Errorf("渲染输出缺少关键内容 %q", want)
		}
	}
}

// 无法解析的配置必须返回错误，而不是 panic。
func TestDoctor_LoadForDoctorRejectsInvalidJSON(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{not valid json`)
	if _, _, err := LoadForDoctor(path); err == nil {
		t.Error("期望解析失败返回错误")
	}
}

// 不存在的配置文件必须返回错误。
func TestDoctor_LoadForDoctorMissingFile(t *testing.T) {
	dir := cleanBase(t)
	missing := filepath.Join(dir, "nope", "config.json")
	if _, _, err := LoadForDoctor(missing); err == nil {
		t.Error("期望文件不存在时返回错误")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return len(needle) == 0
}

// 升级回归形态（46 号发现 #2 的确定性判定）：未知能力（自定义网关）provider、
// base_url 带非版本路径、chat_path 由推导产生 —— 旧版实际请求 base + /v1/chat/completions，
// 新版为 base + /chat/completions，两者不等是可计算的事实，必须 WARN。
func TestDoctor_LegacyURLChangeWarns(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{
  "config_version": 2, "port": 9990,
  "providers": [{"id":"gw","name":"gw","type":"openai","api_key":"k","base_url":"https://host/api","enabled":true}],
  "models": []
}`)

	cfg, resolved, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, resolved)

	found := false
	for _, f := range report.Findings {
		if f.Severity == DoctorWarn && f.Subject == "gw" &&
			strings.Contains(f.Message, "https://host/api/v1/chat/completions") &&
			strings.Contains(f.Message, "https://host/api/chat/completions") {
			found = true
		}
	}
	if !found {
		t.Fatalf("期望对 gw 报告新旧 URL 变化的 WARN，实际 findings=%+v", report.Findings)
	}
}

// 常规形态不得误报：裸域名（旧版补 v1 = 新版保留 legacy）与 /v1 结尾
// （新旧拼接结果一致）的净 URL 不变，不应产生 WARN。
func TestDoctor_NoWarnWhenLegacyURLMatchesNew(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{
  "config_version": 2, "port": 9990,
  "providers": [
    {"id":"bare","name":"bare","type":"openai","api_key":"k","base_url":"https://a.example.com","enabled":true},
    {"id":"withv1","name":"withv1","type":"openai","api_key":"k","base_url":"https://b.example.com/v1","enabled":true}
  ],
  "models": []
}`)

	cfg, resolved, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, resolved)

	for _, f := range report.Findings {
		if f.Severity == DoctorWarn && strings.Contains(f.Message, "实际上游地址发生了变化") {
			t.Errorf("净 URL 未变化的 provider 不应产生升级变化 WARN: %+v", f)
		}
	}
}

// 已知能力 provider：注册表路径不再参与运行时 URL，推导值与注册表预设不一致时
// 给 INFO 说明（以报告中的实际 URL 为准），而不是猜测性 WARN。
func TestDoctor_KnownCapabilityPathMismatchInfo(t *testing.T) {
	dir := cleanBase(t)
	path := writeTestConfig(t, dir, `{
  "config_version": 2, "port": 9990,
  "providers": [{"id":"useai","name":"useai","type":"openai","api_key":"k","base_url":"https://gw.example.com","enabled":true}],
  "models": []
}`)

	cfg, resolved, err := LoadForDoctor(path)
	if err != nil {
		t.Fatalf("LoadForDoctor 失败: %v", err)
	}
	report := CheckConfig(cfg, resolved)

	found := false
	for _, f := range report.Findings {
		if f.Severity == DoctorInfo && f.Subject == "useai" && strings.Contains(f.Message, "能力注册表(useai)") {
			found = true
		}
		if f.Severity == DoctorWarn && f.Subject == "useai" && strings.Contains(f.Message, "实际上游地址发生了变化") {
			t.Errorf("已知能力 provider 不应触发未知能力专属的 WARN: %+v", f)
		}
	}
	if !found {
		t.Fatalf("期望对 useai 输出注册表路径 INFO，实际 findings=%+v", report.Findings)
	}
}

// ---------------------------------------------------------------------------
// RewriteNeeded 必须与 NewManager 的真实写回行为一致
//
// RewriteNeeded 是"启动会改什么"的预览，一旦与真实行为分叉就会误导用户。
// 曾经的分叉：doctor 拿原始 disk 当比较基准，而 NewManager 拿归一化前的克隆
// CloneAppConfig(disk)；后者会把 nil 切片规范成非 nil 空切片，于是"磁盘上
// 缺失 models 键"这种配置会被 doctor 误报为"会写回"，而 NewManager 实际不写。
//
// 断言方式刻意不硬编码期望值，而是直接对照 NewManager 是否真的改写了磁盘文件：
// 这样即使将来 NewManager 的判定逻辑变化，本用例也不会失效，只会继续守住"两者一致"。
// ---------------------------------------------------------------------------
func TestDoctor_RewriteNeededMatchesNewManagerWriteback(t *testing.T) {
	// assertParity：先取 doctor 的预览，再让 NewManager 真正跑一次，比较二者。
	assertParity := func(t *testing.T, path string) {
		t.Helper()

		disk, resolved, err := LoadForDoctor(path)
		if err != nil {
			t.Fatalf("LoadForDoctor 失败: %v", err)
		}
		report := CheckConfig(disk, resolved)

		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取配置失败: %v", err)
		}
		if _, err := NewManager(path); err != nil {
			t.Fatalf("NewManager 失败: %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("再次读取配置失败: %v", err)
		}
		actualRewrite := string(before) != string(after)

		if report.RewriteNeeded != actualRewrite {
			t.Fatalf("doctor.RewriteNeeded=%v 与 NewManager 实际写回=%v 不一致："+
				"doctor 是启动写回行为的预览，分叉即误导用户", report.RewriteNeeded, actualRewrite)
		}
	}

	// 核心回归形态：完全归一化的配置，只删掉 models 键。
	// 必须由 NewManager 先生成，保证"除 models 之外都已归一化"，
	// 否则其它字段的差异会掩盖 nil 切片这一个变量。
	t.Run("models 键缺失（nil 切片）", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(`{"config_version":2,"port":19999,"providers":[],"models":[]}`), 0600); err != nil {
			t.Fatalf("写入配置失败: %v", err)
		}
		if _, err := NewManager(path); err != nil {
			t.Fatalf("NewManager 失败: %v", err)
		}
		normalized, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取归一化配置失败: %v", err)
		}
		var asMap map[string]any
		if err := json.Unmarshal(normalized, &asMap); err != nil {
			t.Fatalf("解析归一化配置失败: %v", err)
		}
		delete(asMap, "models")
		stripped, err := json.Marshal(asMap)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		if err := os.WriteFile(path, stripped, 0600); err != nil {
			t.Fatalf("写入剥离后的配置失败: %v", err)
		}

		assertParity(t, path)
	})

	for _, c := range []struct {
		name string
		body string
	}{
		{
			name: "transport 为空（确实需要推导并写回）",
			body: `{"config_version":2,"providers":[{"id":"p1","name":"p1","type":"openai","api_key":"k","base_url":"https://host/api","enabled":true}],"models":[]}`,
		},
		{
			name: "config_version 为旧值",
			body: `{"config_version":1,"providers":[{"id":"p1","name":"p1","type":"openai","api_key":"k","base_url":"https://h/v1","enabled":true,"transport":{"chat_path":"chat/completions","models_path":"models"}}],"models":[]}`,
		},
		{
			name: "已完全归一化（不应写回）",
			body: `{"config_version":2,"defense":{"enabled":true,"client_timeout_budget_seconds":90},"providers":[{"id":"useai","name":"UseAI","display_name":"UseAI","api_key":"","base_url":"https://api.eforge.xyz/v1","type":"openai","transport":{"chat_path":"chat/completions","models_path":"models"},"enabled":true,"priority":0}],"models":[]}`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			assertParity(t, writeTestConfig(t, t.TempDir(), c.body))
		})
	}
}
