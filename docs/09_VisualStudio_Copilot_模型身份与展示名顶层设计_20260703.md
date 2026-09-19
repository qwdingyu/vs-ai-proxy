# Visual Studio Copilot 模型身份与展示名顶层设计

> 日期：2026-07-03  
> 背景：连续出现 `DEEPSEEK - deepseek-v4-flash:latest`、`finish_reason`、`glm-5.2` vs `z-ai/glm-5.2` 等问题后，需要把“模型展示名”和“上游真实模型名”从架构上分离，而不是继续局部补丁。

## 1. 核心结论

本项目面向 Visual Studio Copilot 的核心目标不是“把某个模型跑通”，而是让 Visual Studio 可以稳定使用更多 OpenAI-compatible / Ollama-compatible provider。

因此模型名必须分为四层：

| 层级 | 用途 | 示例 |
|------|------|------|
| `Display` | 给 Visual Studio 用户看的名字 | `USECPA - glm-5.2` |
| `Alias` | 客户端可能回传的兼容名字 | `glm-5.2`、`glm-5.2:latest`、`z-ai/glm-5.2@usecpa:latest` |
| `Qualified` | 代理内部 canonical 路由 ID | `z-ai/glm-5.2@usecpa` |
| `Upstream` | 发给 provider 的真实模型 ID | `z-ai/glm-5.2` |

原则：

- 用户看到的名字不能直接发给上游。
- Visual Studio 回传任意 alias，都必须先解析到 `Qualified` / `Upstream`。
- 上游 provider 永远只接收 `Upstream`。
- provider 冲突通过 `model@provider_id` 解决。
- `:latest` 只作为兼容 alias 保留，不再出现在用户可见主展示字段里。
- 只把 `:latest` 当作 Ollama 兼容 tag；`qwen3-coder:480b`、`model:free` 这类 provider 真实模型 ID 不能被剥离。
- basename alias 只有在全局唯一时才允许自动路由；不唯一时返回明确诊断，要求用户选择完整模型或 `model@provider_id`。

## 2. 为什么不能继续局部补丁

此前问题表面不同：

1. VS 回传 `DEEPSEEK - deepseek-v4-flash:latest`，上游不认识。
2. VS 显示 200，但 OpenAI .NET SDK 因空 `finish_reason` 解析失败。
3. VS 回传 `glm-5.2`，但上游真实模型是 `z-ai/glm-5.2`。

这些问题的共同根因是：

- Visual Studio 的展示/回传行为和上游 provider 的模型 ID 不是同一套命名体系。
- 代理必须在协议边界做模型身份解析，不能假设客户端传来的 `model` 就是上游模型。

## 3. 当前实现收敛

新增统一模型身份层：

```text
internal/provider/model_identity.go
```

它负责：

- `NewModelIdentity(upstream, provider)`
- `NewModelIdentityWithDisplay(upstream, provider, providerDisplay)`
- `ModelAliases(upstream, provider)`
- `ModelBasename(model)`
- `DisplayNameModelSuffix(model)`
- `StripModelTag(model)`

`Registry`、`/api/tags` 和测试都改为复用这一层。

## 4. `/api/tags` 输出策略

新的主输出：

```json
{
  "name": "USECPA - glm-5.2",
  "model": "z-ai/glm-5.2@usecpa",
  "aliases": [
    "z-ai/glm-5.2",
    "z-ai/glm-5.2:latest",
    "z-ai/glm-5.2@usecpa",
    "z-ai/glm-5.2@usecpa:latest",
    "glm-5.2",
    "glm-5.2:latest"
  ]
}
```

设计理由：

- `name` 不再带 `:latest`，减少 Visual Studio 用户认知负担。
- `model` 不再带 `:latest`，作为代理 canonical ID 使用。
- `aliases` 继续保留 `:latest`，兼容 Ollama-compatible 客户端。
- namespaced 模型保留短名 alias，兼容 VS 可能回传 basename 的行为。
- provider 展示前缀优先使用 `config.json` 中的 `display_name`，而不是硬编码大写 provider id。

## 4.1 `/v1/models` 输出策略

`/v1/models` 继续保持 OpenAI-compatible 的 `id` 字段，避免破坏已有客户端；同时补充 identity metadata：

```json
{
  "id": "z-ai/glm-5.2",
  "object": "model",
  "owned_by": "usecpa",
  "display_name": "UseCpa Paid - glm-5.2",
  "upstream_model": "z-ai/glm-5.2",
  "canonical": "z-ai/glm-5.2@usecpa",
  "aliases": [
    "z-ai/glm-5.2",
    "z-ai/glm-5.2:latest",
    "z-ai/glm-5.2@usecpa",
    "z-ai/glm-5.2@usecpa:latest",
    "glm-5.2",
    "glm-5.2:latest"
  ]
}
```

这样 `/v1/models` 与 `/api/tags` 共用同一套身份投影，但仍兼容 OpenAI SDK 只读取 `id` 的行为。

## 5. 路由解析顺序

请求进入 `/v1/chat/completions` 或 `/api/chat` 后，模型名解析应按以下顺序：

1. 精确匹配 `model@provider`。
2. 精确匹配已知 upstream。
3. 解析 provider hint，例如 `nvidia/qwen3-coder`。
4. 解析 VS 展示名，例如 `USECPA - glm-5.2`。
5. 解析 namespaced 模型短名，例如 `glm-5.2` -> `z-ai/glm-5.2`，但仅在匹配唯一时自动解析。
6. 最后才进入 fallback candidates。

这样可以避免把 `glm-5.2` 盲目发给 `useai`、`deepseek`、`ollama`、`usecpa` 全部重试。

如果短名匹配多个上游模型，代理返回：

```json
{
  "error": {
    "code": "model_alias_ambiguous",
    "message": "模型短名匹配到多个上游模型，无法安全自动路由"
  }
}
```

这是稳定性优先的选择：宁可提示用户明确选择，也不要静默路由到错误 provider。

## 6. 配置闭环

Web 端保存的模型配置必须参与路由，而不是只影响展示。

已采用的原则：

- `config.json` 中启用且绑定 `provider_id` 的模型，会作为该 provider 的路由种子。
- 这可以覆盖 provider `/models` 不稳定、返回 403、启动时尚未刷新等情况。
- `provider_id` 必须是 provider 实例 ID，例如 `usecpa`，不能填模型名。

正确示例：

```json
{
  "name": "z-ai/glm-5.2",
  "provider_id": "usecpa",
  "provider": "usecpa",
  "enabled": true
}
```

错误示例：

```json
{
  "name": "z-ai/glm-5.2",
  "provider_id": "z-ai/glm-5.2",
  "provider": "z-ai/glm-5.2"
}
```

## 7. 验收标准

面向 Visual Studio Copilot，不能只用 Web 面板测试判断成功。

必须覆盖：

- `/api/tags` 展示名不带 `:latest`。
- `/api/tags` aliases 包含 `:latest` 兼容项。
- `qwen3-coder:480b`、`model:free` 等真实冒号模型不会被当作 tag 剥离。
- VS 回传 `USECPA - glm-5.2` 可路由到 `z-ai/glm-5.2`。
- VS 回传 `USECPA - glm-5.2:latest` 可路由到 `z-ai/glm-5.2`。
- VS 回传 `glm-5.2` 可路由到 `z-ai/glm-5.2`。
- 当 `glm-5.2` 同时匹配多个上游模型时，返回 `model_alias_ambiguous`，不请求任何 provider。
- VS 回传 `z-ai/glm-5.2@usecpa` 可精确路由到 `usecpa`。
- 上游 provider 实际收到的 `model` 是 `z-ai/glm-5.2`，不是展示名、短名或 tagged alias。

## 8. 当前边界

当前设计解决的是模型身份问题，不直接解决：

- provider 自身返回 502/503；
- API key 权限不足；
- provider `/models` 返回 403；
- Visual Studio OpenAI .NET SDK 对流式响应字段的严格解析。

这些问题仍然需要通过日志、诊断响应头、`/health.version` 和真实 VS 端到端测试排查。
