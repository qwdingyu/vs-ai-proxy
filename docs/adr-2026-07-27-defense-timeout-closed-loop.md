# ADR: 防御超时预算闭环修复

## 决策日期

2026-07-27

## 状态

已实施

## 背景

用户发现"启用上游防御模式"开关关闭后，95 秒超时预算仍然生效。
经追溯，`defense.enabled` 只控制短重试、UA 稳定、429 冷却和协议兜底，
但 `client_timeout_budget_seconds` 的运行时裁剪和配置钳位完全独立于 `defense.enabled`，
形成了"开关没有形成闭环"的问题。

## 问题根因

`ClientTimeoutBudgetSeconds` 的消费链共 3 个点，全部不受 `Defense.Enabled` 守卫：

| 层级 | 位置 | 原行为 | 影响 |
|------|------|--------|------|
| 配置归一化 | `config.go:NormalizeForRuntime()` | 无条件钳位 [15, 95] | 用户设 999 被截成 95 |
| 运行时裁剪 | `server.go:effectiveClientBoundTimeoutSeconds()` | 无条件 cap 到 budget | 模型 180s 被裁到 90/95s |
| 前端限制 | `web/dist/index.html:<input max="95">` | HTML 硬限制 | UI 无法输入 >95 |

下游 `requestContextWithTimeout` → `providerOperationContext` → `doChatHTTPRequest`
自动继承父 context 的 deadline，不需要逐层修改。

## 决策

修复方案：让 `defense.enabled` 同时控制超时预算裁剪，形成完整闭环。

### 具体修改

**1. `config.go` — `NormalizeForRuntime()`**

防御关闭时跳过 budget 钳位 [15, 95]，保留用户原始值
（运行时 `effectiveClientBoundTimeoutSeconds` 会直接透传，原始值不受影响）。

```go
if cfg.Defense.Enabled == nil || *cfg.Defense.Enabled {
    // 只有防御开启（或未设置=默认开启）时才钳位 [15, 95]
}
```

**2. `server.go` — `effectiveClientBoundTimeoutSeconds()`**

防御关闭时直接返回 `configuredTimeout`，不做 budget 裁剪。

```go
if cfg != nil && !proxyDefenseEnabled(cfg) {
    return configuredTimeout
}
```

**3. `web/dist/index.html`**

移除 `max="95"` 硬限制，更新提示文案说明超时预算仅在防御开启时生效。

### 修复后行为矩阵

| 防御开关 | `client_timeout_budget_seconds` | 模型 `timeout_seconds` | 实际有效超时 |
|---------|-------------------------------|----------------------|------------|
| 🔴 关 | 任意值 | 未设置 → 默认 180s | **180s** |
| 🔴 关 | 任意值 | 显式设置 300s | **300s** |
| 🟢 开 | 未设置 → 默认 90 | 未设置 → 180s | 90s（裁剪） |
| 🟢 开 | 设置 120 | 显式设置 300s | 95s（钳位到上限） |
| 🟢 开 | 设置 30 | 显式设置 60s | 30s（budget 更短） |

### 边界条件

- `cfg` 为 nil：`effectiveClientBoundTimeoutSeconds` 中 `cfg != nil` 为 false，跳过防御检查，使用默认 budget=90（原行为）
- `Defense.Enabled` 为 nil：`proxyDefenseEnabled` 返回 true（默认开启），安全阀生效（原行为）
- 热更新：保存配置后 `NormalizeForRuntime` 重新归一化，`Reconfigure` 刷新运行时快照，下次请求使用新配置

## 影响范围

- 修改文件：5 个（config.go, server.go, index.html, zh.js, en.js）
- 新增测试：2 个（config_test.go, server_test.go）
- 全项目 12 个包测试通过，零回归

## 遗留问题

`applyDefenseCandidatePolicy` 函数目前不检查 `Defense.Enabled`，始终限制候选 provider 为 1 个。
这是一个独立的设计决策（注释说明"不再代表跨 provider fallback"），不在本次修复范围内。

## 参考资料

- `internal/config/config.go` — `DefenseConfig` 结构体，`NormalizeForRuntime` 函数
- `internal/proxy/server.go` — `proxyDefenseEnabled`，`effectiveClientBoundTimeoutSeconds`，`modelTimeoutSeconds`
- `web/dist/index.html` — 配置表单前端代码