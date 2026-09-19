package config

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// config doctor
//
// 背景（见 docs/46_新版本VS_Copilot对接故障改动源全面审查_20260919.md 发现 #2/#3）：
// 自 v0.2.70 起，加载配置时会执行归一化；一旦归一化结果与磁盘内容不同就立即写回。
// 归一化会推导 provider 的 transport.chat_path / models_path 并固化到 config.json。
// 如果推导规则对某个 base_url 形态给出了错误的路径，升级后第一次启动就会把这个
// 错误 URL 永久写进配置；此后即使修复了推导逻辑，配置里的显式值仍然优先，修复
// 无法自动生效。
//
// config doctor 的职责是**只读**地把这些风险提前暴露出来：
//   - 展示每个 provider 实际会被请求的上游 URL（base_url × transport 拼接结果）
//   - 标记出「由本次推导产生、而非用户显式填写」的路径
//   - 校验模型与 provider 的绑定是否指向不存在的 provider
//   - 预览归一化/迁移会写回哪些内容（dry-run，绝不落盘）
//
// 本文件所有函数都是纯函数或只读操作，不会修改传入的配置对象，更不会写磁盘。
// ---------------------------------------------------------------------------

// LoadForDoctor 只读地载入磁盘上的配置，**不做任何归一化与写回**。
//
// 与 NewManager 的关键区别：NewManager 在检测到归一化差异时会立即 save()，
// 那正是 doctor 需要预览的副作用。因此诊断路径必须绕开它，否则
// "运行 doctor 查看会写回什么"这件事本身就会先把配置改掉。
//
// 返回的配置是磁盘原始内容（未经归一化），调用方应交给 CheckConfig 处理。
func LoadForDoctor(configPath string) (*AppConfig, string, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = DefaultConfigPath()
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, configPath, err
	}

	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, configPath, fmt.Errorf("解析配置文件失败: %w", err)
	}
	return &cfg, configPath, nil
}

// DoctorSeverity 表示问题的严重程度。
type DoctorSeverity string

const (
	// DoctorInfo 表示值得注意但通常无害。
	DoctorInfo DoctorSeverity = "info"
	// DoctorWarn 表示很可能导致请求失败，建议人工确认。
	DoctorWarn DoctorSeverity = "warn"
	// DoctorError 表示几乎必然导致请求失败。
	DoctorError DoctorSeverity = "error"
)

// DoctorFinding 是一条诊断结论。
type DoctorFinding struct {
	Severity DoctorSeverity
	// Subject 是被诊断的对象（provider id / model 名）。
	Subject string
	// Message 是人类可读的结论。
	Message string
	// Hint 是可选的修复建议。
	Hint string
}

// DoctorProviderReport 描述单个 provider 的实际上游 URL。
type DoctorProviderReport struct {
	ID         string
	Type       string
	BaseURL    string
	Enabled    bool
	ChatPath   string
	ModelsPath string
	// ChatURL / ModelsURL 是拼接后的完整上游地址。
	ChatURL   string
	ModelsURL string
	// ChatPathDerived 表示该 chat_path 是本次归一化推导出来的，
	// 而不是用户在 config.json 里显式写下的。
	ChatPathDerived   bool
	ModelsPathDerived bool
	// ChatPathRewritten 表示磁盘上的显式值与归一化结果不同（会被改写）。
	ChatPathRewritten   bool
	ModelsPathRewritten bool
	// DiskChatPath 记录磁盘上原始的 chat_path，便于与改写结果对照展示。
	DiskChatPath string
}

// DoctorReport 是一次完整诊断的结果。
type DoctorReport struct {
	ConfigPath string
	Providers  []DoctorProviderReport
	Findings   []DoctorFinding
	// RewriteNeeded 表示按当前归一化逻辑启动会写回磁盘。
	RewriteNeeded bool
	// ConfigVersionMigrated 表示 config_version 会被升级。
	ConfigVersionMigrated bool
	// ModelCount / ProviderCount 用于输出概览。
	ModelCount    int
	ProviderCount int
}

// HasProblems 表示是否存在 warn/error 级别的问题。
func (r DoctorReport) HasProblems() bool {
	for _, f := range r.Findings {
		if f.Severity == DoctorWarn || f.Severity == DoctorError {
			return true
		}
	}
	return false
}

// CountBySeverity 统计各严重程度的条数。
func (r DoctorReport) CountBySeverity() map[DoctorSeverity]int {
	counts := map[DoctorSeverity]int{}
	for _, f := range r.Findings {
		counts[f.Severity]++
	}
	return counts
}

// CheckConfig 对已经载入的配置做只读诊断。
//
// disk 是磁盘上原始（未归一化）的配置，用于区分「用户显式填写」与「推导产生」。
// 传入 nil 时会退化为只做运行时视图检查。
func CheckConfig(disk *AppConfig, configPath string) DoctorReport {
	report := DoctorReport{ConfigPath: configPath}

	// baseline 是归一化**之前**的快照，专门用于"是否需要写回"的判定。
	// 它必须与 NewManager / Reload 同构：那两处比较的是 CloneAppConfig(cfg)
	// （归一化前的克隆），而不是磁盘上的原始对象。原因见下方 RewriteNeeded。
	baseline := CloneAppConfig(disk)
	// 在副本上执行归一化，绝不触碰调用方的对象。
	runtime := CloneAppConfig(disk)
	if runtime == nil {
		// CloneAppConfig(nil) 返回 nil；而本函数承诺"nil 时退化为只做运行时视图检查"，
		// 因此必须补一个非 nil 的空配置，否则后面 len(runtime.Providers) 会对
		// nil 指针取字段而 panic。
		runtime = &AppConfig{}
	}
	report.ConfigVersionMigrated = NormalizeForRuntime(runtime)

	// 用 baseline 而不是 disk 当比较基准，二者不等价：CloneAppConfig 会把 nil 切片
	// 规范成非 nil 空切片（见其 out.Models = make([]ModelConfig, len(...))）。
	// 若直接用 disk，当磁盘上缺失 models 键时 NewManager 判定"无需写回"，
	// 而 doctor 会得出"会写回"，给出与真实行为相反的预览——正是本文件要避免的漂移。
	if baseline != nil {
		report.RewriteNeeded = !reflect.DeepEqual(baseline, runtime)
	}

	report.ProviderCount = len(runtime.Providers)
	report.ModelCount = len(runtime.Models)

	diskProviders := map[string]ProviderConfig{}
	if disk != nil {
		for _, p := range disk.Providers {
			diskProviders[strings.ToLower(ProviderKey(p))] = p
		}
	}

	for _, p := range runtime.Providers {
		report.Providers = append(report.Providers, buildProviderReport(p, diskProviders))
	}
	sort.Slice(report.Providers, func(i, j int) bool {
		return report.Providers[i].ID < report.Providers[j].ID
	})

	report.Findings = append(report.Findings, checkProviders(runtime, diskProviders)...)
	report.Findings = append(report.Findings, checkModelBindings(runtime)...)
	report.Findings = append(report.Findings, checkRewrite(disk, runtime, report.RewriteNeeded)...)
	report.Findings = append(report.Findings, checkUpstreamURLChanges(runtime, report.Providers)...)

	return report
}

// checkUpstreamURLChanges 识别 docs/46 发现 #2 的升级回归形态：
// v0.2.72 把上游 URL 事实源从「能力注册表路径」改为「实例 transport 推导」，
// 对 base_url 带非版本路径的自定义网关，实际上游地址会在升级前后发生变化
// （旧版对未知能力 provider 无条件补 v1/chat/completions，新版改为 chat/completions）。
//
// 该判断是确定性计算而非猜测：对未知能力（自定义网关）provider，
// 旧版行为 = joinURLPath(base, "v1/chat/completions")（含重叠段去重），
// 用 ResolveUpstreamURL 显式传入旧路径即可精确复现，新旧不相等即为事实变化。
func checkUpstreamURLChanges(cfg *AppConfig, providers []DoctorProviderReport) []DoctorFinding {
	if cfg == nil {
		return nil
	}
	cfgByID := map[string]ProviderConfig{}
	for _, p := range cfg.Providers {
		cfgByID[strings.ToLower(ProviderKey(p))] = p
	}

	var findings []DoctorFinding
	for i := range providers {
		rep := providers[i]
		p, ok := cfgByID[strings.ToLower(rep.ID)]
		if !ok {
			continue
		}
		providerType := strings.ToLower(strings.TrimSpace(p.Type))
		// anthropic/ollama 的路径由类型固定推导，另有专项校验，不在此重复。
		if providerType != "openai" && providerType != "custom" && providerType != "" {
			continue
		}
		// 用户显式配置的路径是用户自己的决定，升级不改变它的含义。
		if !rep.ChatPathDerived {
			continue
		}

		capability := provider.InferCapabilityName(ProviderKey(p), p.Name, p.BaseURL, p.Type)
		if capability == "" {
			// 未知能力（自定义网关）：旧版行为可精确复现。
			legacyChatURL := provider.ResolveUpstreamURL(
				ProviderKey(p), p.Name, p.BaseURL, p.Type,
				"v1/chat/completions", "v1/models", "chat",
			)
			if legacyChatURL != rep.ChatURL {
				findings = append(findings, DoctorFinding{
					Severity: DoctorWarn,
					Subject:  rep.ID,
					Message: fmt.Sprintf("升级 v0.2.72 前后该 provider 的实际上游地址发生了变化：旧 %s → 新 %s（chat_path 由推导产生）",
						legacyChatURL, rep.ChatURL),
					Hint: "若该渠道升级后出现上游 404/无响应，把 transport.chat_path 显式配置为旧地址路径即可恢复；或核对上游网关文档确认正确路径",
				})
			}
			continue
		}
		// 已知能力 provider：这里**故意只给 INFO，不算旧 URL**。
		//
		// 原因：能力注册表自身的约定在 v0.2.69 之后变过。以 google 为例，
		// v0.2.69 是 ChatPath="v1beta/openai/chat/completions" +
		// DefaultBaseUrl="https://generativelanguage.googleapis.com"；
		// 现在改为 ChatPath="chat/completions" +
		// DefaultBaseUrl="https://generativelanguage.googleapis.com/v1beta/openai"，
		// 即"版本路径"从 ChatPath 挪进了 DefaultBaseUrl。
		// 因此用当前 caps.ChatPath 去推"旧版 URL"会得出一个两边都不对的地址，
		// 比不报更糟。已知能力的变化需要人工核对上游文档，不能靠推导断言。
		// （未知能力的旧行为是硬编码 fallback，可精确复现，见上面分支。）
		if caps := provider.GetCapabilities(capability); strings.Trim(caps.ChatPath, "/") != strings.Trim(p.Transport.ChatPath, "/") {
			findings = append(findings, DoctorFinding{
				Severity: DoctorInfo,
				Subject:  rep.ID,
				Message: fmt.Sprintf("运行时 chat_path 为推导值 %q，能力注册表(%s)预置路径为 %q；实际请求地址以上方 chat URL 为准",
					p.Transport.ChatPath, capability, caps.ChatPath),
			})
		}
	}
	return findings
}

func buildProviderReport(p ProviderConfig, diskProviders map[string]ProviderConfig) DoctorProviderReport {
	rep := DoctorProviderReport{
		ID:         ProviderKey(p),
		Type:       p.Type,
		BaseURL:    p.BaseURL,
		Enabled:    p.Enabled,
		ChatPath:   p.Transport.ChatPath,
		ModelsPath: p.Transport.ModelsPath,
		ChatURL:    resolveChatURL(p),
		ModelsURL:  resolveModelsURL(p),
	}

	diskP, onDisk := diskProviders[strings.ToLower(rep.ID)]
	if !onDisk {
		// provider 不在磁盘上 → 完全由 EnsureBuiltInProviders 等逻辑补出来的。
		rep.ChatPathDerived = true
		rep.ModelsPathDerived = true
		return rep
	}
	rep.DiskChatPath = diskP.Transport.ChatPath
	rep.ChatPathDerived = diskP.Transport.ChatPath == ""
	rep.ModelsPathDerived = diskP.Transport.ModelsPath == ""
	rep.ChatPathRewritten = diskP.Transport.ChatPath != p.Transport.ChatPath
	rep.ModelsPathRewritten = diskP.Transport.ModelsPath != p.Transport.ModelsPath
	return rep
}

func checkProviders(cfg *AppConfig, diskProviders map[string]ProviderConfig) []DoctorFinding {
	var findings []DoctorFinding
	seenID := map[string]bool{}

	for _, p := range cfg.Providers {
		id := ProviderKey(p)

		if strings.TrimSpace(p.BaseURL) == "" {
			findings = append(findings, DoctorFinding{
				Severity: DoctorWarn,
				Subject:  id,
				Message:  "base_url 为空，所有上游请求都会失败",
				Hint:     "在管理页或 config.json 中补全 base_url",
			})
		} else if !hasURLScheme(p.BaseURL) {
			findings = append(findings, DoctorFinding{
				Severity: DoctorError,
				Subject:  id,
				Message:  fmt.Sprintf("base_url 缺少 http(s):// 前缀: %q", p.BaseURL),
				Hint:     "补全为 https://" + strings.TrimLeft(p.BaseURL, "/"),
			})
		}

		if seenID[strings.ToLower(id)] {
			findings = append(findings, DoctorFinding{
				Severity: DoctorError,
				Subject:  id,
				Message:  "provider id 重复，会导致路由与日志无法区分",
				Hint:     "删除或重命名重复的 provider",
			})
		}
		seenID[strings.ToLower(id)] = true

		if strings.TrimSpace(p.Type) == "" {
			findings = append(findings, DoctorFinding{
				Severity: DoctorWarn,
				Subject:  id,
				Message:  "type 为空，将按 openai 兼容处理",
				Hint:     "显式设置为 openai / anthropic / ollama / custom",
			})
		}

		// anthropic 类型必须用 Messages API 路径，否则上游会 4xx。
		if strings.EqualFold(p.Type, "anthropic") {
			if !strings.HasSuffix(strings.Trim(p.Transport.ChatPath, "/"), "messages") {
				findings = append(findings, DoctorFinding{
					Severity: DoctorError,
					Subject:  id,
					Message:  fmt.Sprintf("anthropic 类型的 chat_path 不是 Messages 端点: %q", p.Transport.ChatPath),
					Hint:     "anthropic 类型应使用 v1/messages",
				})
			}
			if p.Transport.ChatPath == "" {
				findings = append(findings, DoctorFinding{
					Severity: DoctorError,
					Subject:  id,
					Message:  "anthropic 类型的 chat_path 为空",
					Hint:     "设置 transport.chat_path 为 v1/messages",
				})
			}
		}

		if _, onDisk := diskProviders[strings.ToLower(id)]; !onDisk && strings.TrimSpace(p.APIKey) == "" && p.Enabled {
			findings = append(findings, DoctorFinding{
				Severity: DoctorInfo,
				Subject:  id,
				Message:  "该 provider 不在磁盘配置中（由内置逻辑补出）且未配置 api_key",
				Hint:     "如不使用可在管理页禁用，避免候选排序被无凭据条目干扰",
			})
		}
	}

	return findings
}

func checkModelBindings(cfg *AppConfig) []DoctorFinding {
	var findings []DoctorFinding
	providerIDs := map[string]bool{}
	for _, p := range cfg.Providers {
		providerIDs[strings.ToLower(ProviderKey(p))] = true
	}

	// 同名模型绑定到不同 provider 是本产品的正常用法（同一模型多渠道兜底，
	// 例如 LongCat-2.0 同时由 longcat2 / longcat3 提供）。因此只在
	// 「同名 + 同 provider_id」时才报告，那才是真正的重复条目。
	type modelKey struct {
		name       string
		providerID string
	}
	seenModel := map[modelKey]bool{}
	namesByProvider := map[string][]string{}

	for _, m := range cfg.Models {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			findings = append(findings, DoctorFinding{
				Severity: DoctorWarn,
				Subject:  "(空模型名)",
				Message:  "存在 name 为空的模型条目",
				Hint:     "删除该条目或补全模型名",
			})
			continue
		}

		boundID := strings.TrimSpace(m.ProviderID)
		if boundID == "" {
			// 允许为空：由 model@provider_id 语法或默认模型在运行时决定。
			continue
		}
		if !providerIDs[strings.ToLower(boundID)] {
			findings = append(findings, DoctorFinding{
				Severity: DoctorWarn,
				Subject:  name,
				Message:  fmt.Sprintf("绑定的 provider_id %q 不存在", boundID),
				Hint:     "改为已存在的 provider id，或清空绑定让其回退到默认路由",
			})
		}

		key := modelKey{strings.ToLower(name), strings.ToLower(boundID)}
		if seenModel[key] {
			findings = append(findings, DoctorFinding{
				Severity: DoctorWarn,
				Subject:  name,
				Message:  fmt.Sprintf("模型名 %q 与 provider_id %q 完全重复", name, boundID),
				Hint:     "删除重复条目",
			})
		}
		seenModel[key] = true

		if boundID != "" {
			namesByProvider[strings.ToLower(boundID)] = append(namesByProvider[strings.ToLower(boundID)], name)
		}
	}
	return findings
}

func checkRewrite(disk, runtime *AppConfig, rewriteNeeded bool) []DoctorFinding {
	if disk == nil || !rewriteNeeded {
		return nil
	}

	var findings []DoctorFinding
	findings = append(findings, DoctorFinding{
		Severity: DoctorInfo,
		Subject:  "(配置)",
		Message:  "启动时会执行归一化并写回磁盘（dry-run 预览见下）",
		Hint:     "这是 v0.2.70 起的既有行为；如需保留原始文件请先备份",
	})

	if disk.ConfigVersion != runtime.ConfigVersion {
		findings = append(findings, DoctorFinding{
			Severity: DoctorInfo,
			Subject:  "config_version",
			Message:  fmt.Sprintf("将从 %d 升级为 %d", disk.ConfigVersion, runtime.ConfigVersion),
		})
	}

	diskProviders := map[string]ProviderConfig{}
	for _, p := range disk.Providers {
		diskProviders[strings.ToLower(ProviderKey(p))] = p
	}
	runtimeIDs := map[string]bool{}
	for _, p := range runtime.Providers {
		id := ProviderKey(p)
		runtimeIDs[strings.ToLower(id)] = true
		diskP, ok := diskProviders[strings.ToLower(id)]
		if !ok {
			findings = append(findings, DoctorFinding{
				Severity: DoctorInfo,
				Subject:  id,
				Message:  "该 provider 不在磁盘上，归一化会把它补入配置",
			})
			continue
		}
		if diskP.Transport.ChatPath != p.Transport.ChatPath {
			findings = append(findings, DoctorFinding{
				Severity: DoctorInfo,
				Subject:  id,
				Message: fmt.Sprintf("transport.chat_path 将被写为 %q（磁盘原值 %q）",
					p.Transport.ChatPath, diskP.Transport.ChatPath),
			})
		}
		if diskP.Transport.ModelsPath != p.Transport.ModelsPath {
			findings = append(findings, DoctorFinding{
				Severity: DoctorInfo,
				Subject:  id,
				Message: fmt.Sprintf("transport.models_path 将被写为 %q（磁盘原值 %q）",
					p.Transport.ModelsPath, diskP.Transport.ModelsPath),
			})
		}
	}
	for _, p := range disk.Providers {
		id := ProviderKey(p)
		if !runtimeIDs[strings.ToLower(id)] {
			findings = append(findings, DoctorFinding{
				Severity: DoctorWarn,
				Subject:  id,
				Message:  "磁盘上存在该 provider，但归一化后消失",
				Hint:     "请确认这是预期行为，否则可能丢失配置",
			})
		}
	}
	return findings
}

// hasURLScheme 判断 base_url 是否带有 http/https 前缀。
func hasURLScheme(baseURL string) bool {
	lower := strings.ToLower(strings.TrimSpace(baseURL))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// resolveChatURL / resolveModelsURL 直接复用 provider 包的 URL 解析，
// 保证 doctor 展示的地址与实际转发地址永远一致，不会出现"两套推导"。
func resolveChatURL(p ProviderConfig) string {
	return provider.ResolveUpstreamURL(
		ProviderKey(p), p.Name, p.BaseURL, p.Type,
		p.Transport.ChatPath, p.Transport.ModelsPath, "chat",
	)
}

func resolveModelsURL(p ProviderConfig) string {
	return provider.ResolveUpstreamURL(
		ProviderKey(p), p.Name, p.BaseURL, p.Type,
		p.Transport.ChatPath, p.Transport.ModelsPath, "models",
	)
}

// ---------------------------------------------------------------------------
// 文本渲染
// ---------------------------------------------------------------------------

// RenderDoctorReport 把诊断结果渲染成人类可读文本。
//
// 输出刻意包含"实际请求 URL"这一节：46 号审查显示，升级后最难排查的故障
// 是"配置里看起来正常、但拼接出来的上游地址是错的"，把最终 URL 打出来
// 可以直接对照上游文档判断。
func RenderDoctorReport(report DoctorReport) string {
	var b strings.Builder

	b.WriteString("config doctor —— 只读诊断（不会修改任何文件）\n")
	b.WriteString(strings.Repeat("=", 64) + "\n")
	fmt.Fprintf(&b, "配置文件: %s\n", report.ConfigPath)
	fmt.Fprintf(&b, "provider: %d 个，模型: %d 个\n", report.ProviderCount, report.ModelCount)

	if report.ConfigVersionMigrated {
		b.WriteString("config_version: 将被升级到当前版本\n")
	}
	if report.RewriteNeeded {
		b.WriteString("写回磁盘: 是（启动时会归一化并写回，见下方预览）\n")
	} else {
		b.WriteString("写回磁盘: 否（磁盘内容与归一化结果一致）\n")
	}

	// ── 实际上游地址 ──────────────────────────────────────────────
	b.WriteString("\n" + strings.Repeat("-", 64) + "\n")
	b.WriteString("实际上游地址（base_url × transport 拼接结果）\n")
	b.WriteString(strings.Repeat("-", 64) + "\n")
	for _, p := range report.Providers {
		state := "启用"
		if !p.Enabled {
			state = "停用"
		}
		fmt.Fprintf(&b, "\n[%s] type=%s %s\n", p.ID, p.Type, state)
		fmt.Fprintf(&b, "  base_url : %s\n", p.BaseURL)
		fmt.Fprintf(&b, "  chat     : %s%s\n", p.ChatURL, derivedMark(p.ChatPathDerived, p.ChatPathRewritten))
		if p.ModelsURL != "" {
			fmt.Fprintf(&b, "  models   : %s%s\n", p.ModelsURL, derivedMark(p.ModelsPathDerived, p.ModelsPathRewritten))
		}
		if p.ChatPathRewritten {
			fmt.Fprintf(&b, "  * chat_path 磁盘原值 %q 会被改写为 %q\n", p.DiskChatPath, p.ChatPath)
		}
	}

	// ── 发现项 ────────────────────────────────────────────────────
	b.WriteString("\n" + strings.Repeat("-", 64) + "\n")
	b.WriteString("诊断结论\n")
	b.WriteString(strings.Repeat("-", 64) + "\n")

	counts := report.CountBySeverity()
	if len(report.Findings) == 0 {
		b.WriteString("未发现问题。\n")
	} else {
		// 按严重程度排序输出；这里的三个级别即 CheckConfig 会产生的全集。
		for _, sev := range []DoctorSeverity{DoctorError, DoctorWarn, DoctorInfo} {
			for _, f := range report.Findings {
				if f.Severity != sev {
					continue
				}
				fmt.Fprintf(&b, "[%s] %s: %s\n", strings.ToUpper(string(sev)), f.Subject, f.Message)
				if f.Hint != "" {
					fmt.Fprintf(&b, "        建议: %s\n", f.Hint)
				}
			}
		}
	}

	// ── 汇总 ──────────────────────────────────────────────────────
	b.WriteString("\n" + strings.Repeat("-", 64) + "\n")
	fmt.Fprintf(&b, "汇总: error=%d warn=%d info=%d\n",
		counts[DoctorError], counts[DoctorWarn], counts[DoctorInfo])
	if report.HasProblems() {
		b.WriteString("结论: 发现需要人工确认的问题（见上方 WARN/ERROR）。\n")
	} else {
		b.WriteString("结论: 未发现会导致请求失败的问题。\n")
	}
	b.WriteString("提示: 本命令为只读诊断，不会修改配置文件。\n")

	return b.String()
}

func derivedMark(derived, rewritten bool) string {
	switch {
	case rewritten:
		return "   <- 将被改写"
	case derived:
		return "   <- 由推导填充"
	default:
		return ""
	}
}
