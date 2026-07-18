# Kimi provider 路径、消息与 VS Copilot 兼容踩坑记录（2026-07-18）

本文只记录当前仓库代码和测试已经覆盖的事实，避免把 Kimi 问题与 MiMo 参数方言混在一起。

## 1. 当前结论

Kimi 的主要特殊性不是 MiMo 那类输出预算字段方言，而是：

1. 默认 coding endpoint 已经带版本路径：`https://api.kimi.com/coding/v1`。
2. provider capability 中的 `ChatPath` / `ModelsPath` 必须分别是 `chat/completions` 和 `models`，不能再额外加 `v1/`。
3. Kimi 对空 assistant 历史更严格；VS/Copilot 多轮上下文里出现空 assistant 或只有 reasoning 的占位消息时，代理需要在请求转换层清理。
4. Kimi 仍按默认 OpenAI-compatible 输出预算字段 `max_tokens` 处理；不要套用 MiMo 的 `max_completion_tokens` 结论。

## 2. 与 MiMo 的区别

| 项目 | Kimi | MiMo |
| --- | --- | --- |
| provider id | `kimi` | `xiaomimimo` |
| 默认 base URL | `https://api.kimi.com/coding/v1` | `https://api.xiaomimimo.com/v1` |
| chat path | `chat/completions` | `v1/chat/completions` |
| models path | `models` | `v1/models` |
| 输出预算字段 | `max_tokens` | `max_completion_tokens` |
| 已知核心问题 | versioned base URL、空 assistant 历史 | 输出预算字段语义、reasoning_content 消耗预算 |

因此不要把 MiMo 的 `max_completion_tokens` 规则复制给 Kimi，也不要把 Kimi 的空 assistant 问题当成 MiMo 的根因。

## 3. 代码事实源

### 3.1 Provider capability

事实源：`internal/provider/capabilities.go`

Kimi entry：

- `Category: direct`
- `ApiFormat: openai`
- `SupportsReasoningEffort: false`
- `SupportsTopK: false`
- `ChatPath: chat/completions`
- `ModelsPath: models`
- `DefaultBaseUrl: https://api.kimi.com/coding/v1`
- `EnvPrefix: KIMI`

验证：

- `internal/provider/capabilities_test.go`
  - `TestCompatibilityProfileForKimiVersionedBaseURL`

### 3.2 管理页与 API 展示

`/api/providers` 不自己推断 Kimi path，而是通过：

- `providerCompatibilityProfileFromConfig()`
- `provider.CompatibilityProfileFor()`

这样管理页展示、路径治理、参数治理共用同一份 provider capability，避免 Web/API/Provider 三处事实漂移。

### 3.3 请求转换

事实源：`internal/proxy/request_transformer.go`

关键行为：

- `transformRequest()` 先按 provider 判断是否需要 reasoning cache，再清理空 assistant 占位。
- 对非 direct reasoning provider，纯 reasoning assistant 会按空占位处理；Kimi 属于这类 provider。
- 工具请求声明了 `tools` 或 legacy `functions` 时，不自动注入全局 `max_tokens=4096`，避免截断大工具参数。
- `top_k` 和 `reasoning_effort` 只在 provider capability 支持时透传。

验证：

- `internal/proxy/request_transformer_test.go`
  - `TestTransformRequestDropsSemanticallyEmptyAssistantPlaceholderForKimi`
  - `TestApplyProfileDefaultsEnforcesFixedTemperature`
  - `TestApplyExecutionDefaultsOverrideClientParams`

## 4. 日志解读

Kimi 相关失败也必须按 `request_id` 聚合，不要只按时间相邻判断。

正确排查顺序：

1. 看最终 `POST /v1/chat/completions - <status>`。
2. 看同一 `request_id` 的 `provider`、`requested_model`、`upstream`。
3. 如果只有某个 attempt warning，但最终是 `200`，先判断为可恢复候选失败。
4. 如果最终是 `4xx/5xx`，再看 `error_code`、`reason`、`attempts`。

## 5. 禁止事项

1. 不要在 `/v1/chat/completions` handler 中写 Kimi 模型名特判。
2. 不要给 Kimi 套用 MiMo 的 `max_completion_tokens`。
3. 不要为了通过 Kimi，把所有 provider 的 assistant reasoning 历史都删除；direct reasoning provider 仍需要保留 reasoning cache。
4. 不要在工具请求上无条件注入 `max_tokens=4096`。
5. 不要把 Kimi 未返回用量的请求按 0 Token 统计。

## 6. 发布前必测

修改 Kimi/provider/request transformer 相关逻辑时，至少运行：

```bash
go test ./internal/provider ./internal/proxy ./internal/api -run 'Kimi|Compatibility|Transform|Provider|Management' -count=1
go test ./... -count=1
go vet ./...
```

如果同时改了 Web 或统计：

```bash
go test ./web ./internal/store -count=1
make release-check
```

## 7. 当前剩余边界

当前仓库没有把真实 Kimi 上游 API key 放进 CI，因此真实线上 Kimi SLA、返回速度、偶发网关错误不在自动测试覆盖范围内。仓库测试覆盖的是代理自身应保证的行为：路径不拼错、请求转换不破坏消息、provider capability 不漂移、日志和统计口径一致。
