package web

import (
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestIndexHTMLHasNoDuplicateIDsAndScriptReferencesExist(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	idRe := regexp.MustCompile(`\bid=("[^"]+"|'[^']+')`)
	ids := map[string]int{}
	for _, match := range idRe.FindAllStringSubmatch(html, -1) {
		id := match[1]
		if len(id) >= 2 && ((id[0] == '"' && id[len(id)-1] == '"') || (id[0] == '\'' && id[len(id)-1] == '\'')) {
			id = id[1 : len(id)-1]
		}
		ids[id]++
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

func TestDashboardTokenUsageIsWiredWithoutTreatingUnknownAsZero(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	for _, marker := range []string{
		`id="promptTokens"`,
		`id="completionTokens"`,
		`id="totalTokens"`,
		`id="usageCoverage"`,
		`id="cachedTokens"`,
		`id="reasoningTokens"`,
		`id="modelTokenTable"`,
		`id="todayTokens"`,
		`id="todayRange"`,
		`id="todayMissingUsage"`,
		`id="weekTokens"`,
		`id="weekRange"`,
		`id="weekMissingUsage"`,
		`id="monthTokens"`,
		`id="monthRange"`,
		`id="monthMissingUsage"`,
		`id="dailyTokenTrend"`,
		`if (!log || !log.usage) return '-'`,
		`formatUsageCoverage(stats.usage_reported_count, stats.token_usage_requests)`,
		`const periods = stats.period_usage || {}`,
		`const currentPeriods = stats.current_periods || {}`,
		`currentPeriodKey(currentPeriods, 'daily', formatLocalDateKey(today))`,
		`currentPeriodKey(currentPeriods, 'weekly', formatLocalISOWeekKey(today))`,
		`currentPeriodKey(currentPeriods, 'monthly', formatLocalMonthKey(today))`,
		`currentPeriodRange(currentPeriods, 'weekly', currentWeekRange(today))`,
		`const formatLocalISOWeekKey = (date) => {`,
		`const formatPeriodTokens = (period) => hasReportedPeriodUsage(period) ? formatTokenCount(period.total_tokens || 0) : '-'`,
		`formatPeriodRange(period, fallbackRange)`,
		`formatMissingUsage(period)`,
		`renderDailyTokenTrend(periods.daily || [])`,
		`const modelUsage = stats.model_usage || []`,
		`<td class="col-tokens">${formatTokenUsage(log)}</td>`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("token usage UI missing %q", marker)
		}
	}
	if strings.Contains(html, "cached_tokens + reasoning_tokens") {
		t.Fatal("cached/reasoning token subsets must not be added to total")
	}
}

func TestDashboardShowsVerifiedModelCoverageWithoutClaimingEveryModelIsTested(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	for _, marker := range []string{
		`class="supported-models"`,
		`href="#/test" data-i18n="dashboard.supported.testBtn"`,
		`data-i18n="dashboard.supported.desc"`,
		`data-i18n="dashboard.supported.note"`,
		`deepseek-v4-flash`,
		`step-router-v1 / step-3.7-flash`,
		`gpt-5.5,5.6等`,
		`glm-5.2 / z-ai/glm-5.2`,
		`kimi-for-coding`,
		`mimo-v2.5 / mimo-v2.5-pro`,
		`“系列兼容”不等于每个型号均已逐一实测`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("verified model coverage missing %q", marker)
		}
	}
	if strings.Contains(html, "step_route_v1") {
		t.Fatal("dashboard should use the upstream model ID step-router-v1")
	}
}

func TestVerifiedModelCoverageMatchesREADMEAndHeroSummary(t *testing.T) {
	htmlData, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	readmeData, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	readmeCoverage := readREADMEModelCoverage(string(readmeData))
	webCoverage := readWebModelCoverage(string(htmlData))
	if len(readmeCoverage) != len(webCoverage) {
		t.Fatalf("model family count differs: README=%d Web=%d", len(readmeCoverage), len(webCoverage))
	}
	for family, readmeModels := range readmeCoverage {
		webModels, ok := webCoverage[family]
		if !ok {
			t.Errorf("Web model coverage missing README family %q", family)
			continue
		}
		if webModels != readmeModels {
			t.Errorf("model IDs differ for %s: README=%q Web=%q", family, readmeModels, webModels)
		}
	}

	summary := readI18nCatalog(t, "i18n/zh.js")["dashboard.hero.models"]
	if summary == "" {
		t.Fatal("zh catalog missing dashboard.hero.models")
	}
	for family := range readmeCoverage {
		if !strings.Contains(summary, family) {
			t.Errorf("hero model summary missing README family %q", family)
		}
	}
}

func readREADMEModelCoverage(content string) map[string]string {
	rowPattern := regexp.MustCompile(`(?m)^\|\s*([^|\n]+?)\s*\|\s*([^|\n]+?)\s*\|`)
	return collectModelCoverage(rowPattern.FindAllStringSubmatch(content, -1))
}

func readWebModelCoverage(content string) map[string]string {
	rowPattern := regexp.MustCompile(`(?s)<td class="model-family"[^>]*>([^<]+)</td>\s*<td class="model-ids"[^>]*>([^<]+)</td>`)
	return collectModelCoverage(rowPattern.FindAllStringSubmatch(content, -1))
}

func collectModelCoverage(rows [][]string) map[string]string {
	modelIDPattern := regexp.MustCompile(`[a-z0-9]+(?:[-./][a-z0-9]+)+`)
	coverage := make(map[string]string)
	for _, row := range rows {
		if len(row) < 3 {
			continue
		}
		family := strings.TrimSpace(row[1])
		modelIDs := modelIDPattern.FindAllString(strings.ToLower(row[2]), -1)
		if family == "模型系列" || strings.HasPrefix(family, "---") || len(modelIDs) == 0 {
			continue
		}
		coverage[family] = strings.Join(modelIDs, ",")
	}
	return coverage
}

func TestDashboardTokenUsageExplainsCoverageAndSubsets(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	for _, marker := range []string{
		`data-i18n="dashboard.tokens.note"`,
		`模型用量`,
		`有用量数据`,
		`未返回的不估算、不按 0 计算`,
		`缓存命中的输入`,
		`模型内部思考用量`,
		`已包含在总 Token 中，不重复相加`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("token usage explanation missing %q", marker)
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
		`data-page="providers" data-i18n="nav.providers">提供商`,
		`data-page="test" data-i18n="nav.test">测试`,
		`data-page="logs" data-i18n="nav.logs">日志`,
		`data-page="faq" data-i18n="nav.faq">FAQ`,
		`data-page="contact" data-i18n="nav.contact">联系我们`,
		`data-page="advanced" data-i18n="nav.advanced">高级`,
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
	if !strings.Contains(html, `data-i18n="models.import.success"`) && !strings.Contains(html, `t('models.import.success'`) {
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
		`t('providers.delete.confirm'`,
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
		`t('providers.form.showKey')`,
		`t('providers.form.hideKey')`,
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
		`data-i18n="table.header.model"`,
		`data-i18n="providers.form.compatibilityProfile"`,
		"const providerCompatibilityProfile = (provider) => provider?.compatibility_profile || {};",
		"const providerCompatibilityLabel = (provider) => {",
		"const countModelsByProvider = (models) =>",
		"const modelCounts = countModelsByProvider(modelRes.models || [])",
		"<td title=\"${escapeAttr(providerCompatibilityTitle(item))}\">${escapeHTML(providerCompatibilityLabel(item))}</td>",
		"<td>${escapeHTML(modelCounts.get(key.toLowerCase()) || 0)}</td>",
		`<tr><td colspan="9" class="empty" data-i18n="table.loading">加载中...</td></tr>`,
		`colspan="9"`,
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
	if !strings.Contains(html, `data-i18n="logs.toolDiagnostics.noResponseTools"`) && !strings.Contains(html, `t('logs.toolDiagnostics.noResponseTools'`) {
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
		`data-i18n="table.header.action"`,
		`<tr><td colspan="11" class="empty" data-i18n="table.loading">加载中...</td></tr>`,
		`<tr><td colspan="11" class="empty" data-i18n="table.empty.logs">暂无日志</td></tr>`,
		`.logs-table .col-action { width: 86px; }`,
		`tbody.innerHTML = '<tr><td colspan="11" class="empty">' + t('table.empty.logs') + '</td></tr>';`,
		`data-action="copy-log-diagnostic"`,
		"formatLogDiagnosticCopyText",
		"currentLogRows = logs",
		`t('action.copyDiagnostic')`,
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
		"release-check: tool-check vuln-check i18n-check",
		"govulncheck@v1.6.0",
		"go test ./... -count=1",
		"go test -race ./... -count=1",
		"go vet ./...",
		"node --check",
		"node tests/i18n_runtime_test.js",
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

func TestI18nScriptsUseAdminStaticAssetPaths(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	for _, path := range []string{
		"/admin/i18n/index.js",
		"/admin/i18n/zh.js",
		"/admin/i18n/en.js",
	} {
		if !strings.Contains(html, `src="`+path+`"`) {
			t.Errorf("index.html should load i18n asset from %q", path)
		}
	}
}

func TestI18nMarkersDoNotOwnInteractiveChildren(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	unsafeLabel := regexp.MustCompile(`(?i)<label[^>]*\bdata-i18n\s*=`)
	if match := unsafeLabel.FindString(html); match != "" {
		t.Fatalf("data-i18n must be placed on a leaf text node, found unsafe label %q", match)
	}

	runtime, err := fs.ReadFile(MustSubFS(), "i18n/index.js")
	if err != nil {
		t.Fatalf("read dist/i18n/index.js: %v", err)
	}
	checks := []string{
		"if (el.childElementCount > 0)",
		"document.documentElement.lang =",
	}
	for _, check := range checks {
		if !strings.Contains(string(runtime), check) {
			t.Errorf("i18n runtime missing safety contract %q", check)
		}
	}
}

func TestI18nCatalogsCoverReferencesWithoutDuplicateKeys(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	references := collectI18nReferences(html)

	zh := readI18nCatalog(t, "i18n/zh.js")
	en := readI18nCatalog(t, "i18n/en.js")
	for key := range references {
		if _, ok := zh[key]; !ok {
			t.Errorf("zh catalog missing referenced key %q", key)
		}
		if _, ok := en[key]; !ok {
			t.Errorf("en catalog missing referenced key %q", key)
		}
	}
	for key, zhValue := range zh {
		enValue, ok := en[key]
		if !ok {
			t.Errorf("en catalog missing zh key %q", key)
			continue
		}
		if got, want := placeholders(enValue), placeholders(zhValue); got != want {
			t.Errorf("placeholder mismatch for %q: en=%q zh=%q", key, got, want)
		}
	}
	for key := range en {
		if _, ok := zh[key]; !ok {
			t.Errorf("zh catalog missing en key %q", key)
		}
	}
}

func TestEnglishI18nCatalogContainsNoChineseCopy(t *testing.T) {
	en := readI18nCatalog(t, "i18n/en.js")
	han := regexp.MustCompile(`\p{Han}`)
	for key, value := range en {
		if key == "lang.toggle" {
			continue
		}
		if han.MatchString(value) {
			t.Errorf("English catalog key %q contains Chinese text %q", key, value)
		}
	}
}

func TestDynamicUserFacingTextUsesI18n(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	checks := []string{
		`${t('action.copyDiagnostic')}</button>`,
		`withButtonLoading('btnTestConnection', t('dynamic.testing')`,
		`withButtonLoading('btnTestChat', t('dynamic.chatting')`,
		`test.message || test.error || t('error.unknown')`,
		`t('dynamic.metadataCapabilities', capabilities)`,
	}
	for _, check := range checks {
		if !strings.Contains(html, check) {
			t.Errorf("dynamic i18n flow missing %q", check)
		}
	}
}

func collectI18nReferences(html string) map[string]struct{} {
	references := map[string]struct{}{}
	attributePattern := regexp.MustCompile(`data-i18n(?:-[a-z-]+)?=["']([^"']+)["']`)
	for _, match := range attributePattern.FindAllStringSubmatch(html, -1) {
		references[match[1]] = struct{}{}
	}
	callPattern := regexp.MustCompile(`\bt\(\s*["']([^"']+)["']`)
	for _, match := range callPattern.FindAllStringSubmatch(html, -1) {
		references[match[1]] = struct{}{}
	}
	return references
}

func readI18nCatalog(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := fs.ReadFile(MustSubFS(), path)
	if err != nil {
		t.Fatalf("read dist/%s: %v", path, err)
	}
	entryPattern := regexp.MustCompile(`(?m)^\s*'([^']+)'\s*:\s*'((?:\\.|[^'])*)',?\s*$`)
	catalog := map[string]string{}
	for _, match := range entryPattern.FindAllStringSubmatch(string(data), -1) {
		key := match[1]
		if _, exists := catalog[key]; exists {
			t.Errorf("%s contains duplicate key %q", path, key)
			continue
		}
		catalog[key] = match[2]
	}
	if len(catalog) == 0 {
		t.Fatalf("dist/%s contains no i18n entries", path)
	}
	return catalog
}

func placeholders(value string) string {
	pattern := regexp.MustCompile(`\{[0-9]+\}`)
	matches := pattern.FindAllString(value, -1)
	sort.Strings(matches)
	return strings.Join(matches, ",")
}
