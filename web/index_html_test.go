package web

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestIndexHTMLHasNoDuplicateIDsAndScriptReferencesExist(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	idRe := regexp.MustCompile(`\bid="([^"]+)"`)
	ids := map[string]int{}
	for _, match := range idRe.FindAllStringSubmatch(html, -1) {
		ids[match[1]]++
	}
	for id, count := range ids {
		if count > 1 {
			t.Fatalf("duplicate id %q appears %d times", id, count)
		}
	}

	refRe := regexp.MustCompile(`\$\('([^']+)'\)`)
	for _, match := range refRe.FindAllStringSubmatch(html, -1) {
		if ids[match[1]] == 0 {
			t.Fatalf("script references missing id %q", match[1])
		}
	}
}

func TestLogsFilterInputsAreWiredToQueryPayload(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	filters := map[string]string{
		"q":            "logFilterSearch",
		"provider":     "logFilterProvider",
		"model":        "logFilterModel",
		"error_code":   "logFilterErrorCode",
		"error_reason": "logFilterErrorReason",
		"request_id":   "logFilterRequestID",
		"status_code":  "logFilterStatusCode",
	}
	for queryName, id := range filters {
		t.Run(queryName, func(t *testing.T) {
			if !strings.Contains(html, `id="`+id+`"`) {
				t.Fatalf("missing filter input %s", id)
			}
			if !strings.Contains(html, queryName+": $('"+id+"').value") {
				t.Fatalf("filter %s is not read into query payload", queryName)
			}
			if !strings.Contains(html, "$('"+id+"').value = filters."+queryName+" || ''") {
				t.Fatalf("filter %s is not reset from filter state", queryName)
			}
		})
	}
}

func TestNavigationKeepsBeginnerFlowFirst(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	wantOrder := []string{
		`data-page="providers">提供商`,
		`data-page="test">测试`,
		`data-page="logs">日志`,
		`data-page="faq">FAQ`,
		`data-page="contact">联系我们`,
		`data-page="advanced">高级`,
	}
	last := -1
	for _, marker := range wantOrder {
		idx := strings.Index(html, marker)
		if idx < 0 {
			t.Fatalf("missing nav marker %q", marker)
		}
		if idx <= last {
			t.Fatalf("nav marker %q appears out of order", marker)
		}
		last = idx
	}
	if !strings.Contains(html, `id="page-advanced"`) {
		t.Fatalf("missing advanced landing page")
	}
}

func TestProviderProbeImportsModelsWithoutBeginnerConfirmation(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, "探测并导入模型") {
		t.Fatalf("provider probe button should explain automatic import")
	}
	if strings.Contains(html, "是否自动加入模型配置") {
		t.Fatalf("provider probe import should not ask beginners to understand model import confirmation")
	}
	if !strings.Contains(html, "已为 ${providerId} 自动导入") {
		t.Fatalf("missing automatic import success message")
	}
	if !strings.Contains(html, "item.name === editingModelName && String(item.provider_id || item.provider || '') === editingModelProvider") {
		t.Fatalf("model edit should match name plus provider to avoid touching same-name models")
	}
	if !strings.Contains(html, "x.name !== name || modelProviderKey(x) !== modelProvider") {
		t.Fatalf("model delete should preserve same-name models bound to other providers")
	}
}

func TestProviderModalUsesWiderLayout(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, ".provider-modal { width: min(560px, calc(100vw - 32px)); }") {
		t.Fatalf("provider modal should be wider than the generic modal")
	}
	if !strings.Contains(html, `<div class="modal provider-modal">`) {
		t.Fatalf("provider modal should use provider-modal class")
	}
}

func TestDeleteProviderExplainsBoundModelsAndRefreshesDependentPages(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	checks := []string{
		"deleteProviderWithBoundModelPrompt",
		"该提供商仍有模型绑定，直接删除会造成无效配置",
		"是否同时删除这些模型并删除提供商",
		"const modelRes = await fetchJSON('/models');",
		"if (boundModels.length)",
		"await loadProviders();",
		"await loadModels();",
		"await loadTestLab();",
	}
	for _, check := range checks {
		if !strings.Contains(html, check) {
			t.Fatalf("provider delete flow missing %q", check)
		}
	}
	if strings.Contains(html, "loadTestProviders") {
		t.Fatalf("provider delete flow should not call undefined loadTestProviders")
	}
}

func TestProviderAPIKeyIsPasswordWithVisibilityToggle(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	checks := []string{
		`<input id="pKey" type="password" autocomplete="off" />`,
		`id="btnToggleProviderKey"`,
		"$('btnToggleProviderKey').addEventListener('click'",
		"$('pKey').type = show ? 'text' : 'password'",
		"$('pKey').type = 'password';",
		"显示 API Key",
		"隐藏 API Key",
	}
	for _, check := range checks {
		if !strings.Contains(html, check) {
			t.Fatalf("provider API key visibility flow missing %q", check)
		}
	}
}

func TestProviderListShowsBoundModelCount(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	checks := []string{
		"<th>模型</th>",
		"const countModelsByProvider = (models) =>",
		"const modelCounts = countModelsByProvider(modelRes.models || [])",
		"<td>${escapeHTML(modelCounts.get(key.toLowerCase()) || 0)}</td>",
		`<tr><td colspan="8" class="empty">加载中...</td></tr>`,
		`colspan="8"`,
	}
	for _, check := range checks {
		if !strings.Contains(html, check) {
			t.Fatalf("provider bound model count UI missing %q", check)
		}
	}
}

func TestLogToolDiagnosticsExplainsMissingResponseTools(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, "响应 无工具调用") {
		t.Fatalf("tool diagnostics should explicitly show when request declared tools but response did not include tool calls")
	}
}

func TestLogRowsCanCopyDiagnosticSummary(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	checks := []string{
		`<th>操作</th>`,
		`<tr><td colspan="10" class="empty">加载中...</td></tr>`,
		`<tr><td colspan="10" class="empty">暂无日志</td></tr>`,
		`.logs-table .col-action { width: 86px; }`,
		`tbody.innerHTML = '<tr><td colspan="10" class="empty">暂无日志</td></tr>';`,
		`data-action="copy-log-diagnostic"`,
		"formatLogDiagnosticCopyText",
		"currentLogRows = logs",
		"await copyText('请求诊断'",
	}
	for _, check := range checks {
		if !strings.Contains(html, check) {
			t.Fatalf("log diagnostic copy flow missing %q", check)
		}
	}
}

func TestAdvancedChildPagesKeepAdvancedNavActive(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, "const activePage = ['config', 'models', 'monitor'].includes(currentPage) ? 'advanced' : currentPage") {
		t.Fatalf("advanced child pages should keep the advanced nav item active")
	}
}

func TestDynamicAdminTablesEscapeUserControlledValues(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	want := []string{
		"<td>${escapeHTML(key)}</td>",
		"<td>${escapeHTML(item.display_name || item.name)}</td>",
		"data-name=\"${escapeAttr(key)}\"",
		"<option value=\"${escapeAttr(providerKey(p))}\">${escapeHTML(providerLabel(p))} (${escapeHTML(p.type)})</option>",
		"<option value=\"${escapeAttr(name)}\">${escapeHTML(name)}</option>",
		"<td>${escapeHTML(item.name)}</td>",
		"<td>${escapeHTML(modelProvider || '-')}</td>",
		"data-provider=\"${escapeAttr(modelProvider)}\"",
		"<td>${escapeHTML(item.provider)}</td>",
	}
	for _, marker := range want {
		if !strings.Contains(html, marker) {
			t.Fatalf("missing escaped table marker %q", marker)
		}
	}
}

func TestReleaseBuildDoesNotInstallWindowsResourceTool(t *testing.T) {
	data, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefile := string(data)
	if !strings.Contains(makefile, "build-all: ensure-windows-res") {
		t.Fatalf("build-all should use checked-in Windows resource instead of regenerating it during release")
	}
	if strings.Contains(makefile, "GOPROXY=$${GOPROXY:-https://goproxy.cn,direct} go install") {
		t.Fatalf("release build should not install go-winres from the network")
	}
}

func TestMakefileReleaseCheckIncludesCoreGates(t *testing.T) {
	data, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefile := string(data)
	checks := []string{
		"release-check: tool-check",
		"go test ./... -count=1",
		"go test -race ./cmd/server ./internal/proxy ./internal/provider",
		"go vet ./...",
		"node --check",
		"git diff --check",
		"GOOS=windows GOARCH=amd64 go build",
		"RELEASE_CHECK_OK",
	}
	for _, check := range checks {
		if !strings.Contains(makefile, check) {
			t.Fatalf("release-check missing %q", check)
		}
	}
}
