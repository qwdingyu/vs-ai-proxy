# 配置架构分析：Web/config.json 单入口与 model-selection fallback

> 日期: 2026-07-02
> 状态: 已修正方案 (REVISED)
> 涉及范围: config, provider, model_catalog, model-selection

---

## 1. 当前架构全貌

### 1.1 配置层一览

| 层 | 存储位置 | 创建时机 | 可编辑性 |
|---|---|---|---|
| `.env` | 项目根目录 `.env` | 手动创建 | 用户手动编辑 |
| `config.json` | `~/.config/vs-ai-proxy/config.json` | 首次启动自动生成 | UI 编辑 + 手动编辑 |
| `model-selection/*.json` | `internal/provider/model-selection/` (11 个文件) | 编译时 embed 到二进制 | 不可编辑（需改源码重编译） |
| `logs.json` | `~/.config/vs-ai-proxy/logs.json` | 运行时写入 | 自动管理 |

### 1.2 各层职责逐层追踪

#### ① `.env` → 运行环境配置

```
PORT=12345
HOST=127.0.0.1
CONFIG_PATH=~/.config/vs-ai-proxy/config.json
STORE_PATH=~/.config/vs-ai-proxy/logs.json
```

**消费方**：启动流程与运行环境。

**修正后的定位**：`.env` 只保留单端口监听、配置文件路径、日志路径、Docker/部署相关配置。大模型 provider、base_url、api_key、模型参数都应通过 Web 面板写入 `config.json`，避免用户在多个地方配置同一件事。

---

#### ② config.json.providers → Registry.Add()

```json
{
  "name": "UseAI",
  "base_url": "https://api.eforge.xyz/v1",
  "type": "openai",
  "enabled": true,
  "priority": 0
}
```

**消费方**：
- `NewServer()` → `registry.Add(entry)` — 每个 provider 注册到 Registry
- Registry 启动异步 `refreshModels()` → `provider.ListModels()` — 从上游发现可用模型

**特点**：唯一权威来源，无歧义。定义"怎么连、连哪里"。

---

#### ③ config.json.models → findModelConfig()

```json
{
  "name": "deepseek-v4-pro",
  "provider": "deepseek",
  "context_length": 1000000,
  "max_output_tokens": 384000,
  "supports_tools": true,
  "enabled": true
}
```

**消费方（两处）**：

| 代码路径 | 用途 | 优先级 |
|---|---|---|
| `findModelConfig()` → `applyExecutionDefaults()` | 请求执行参数 fallback (temperature, max_tokens 等) | **第一层（最弱）** |
| `buildOllamaShowBody()` | Ollama `/api/show` 响应 (context_length, supports_tools 等) | **第一层** |

`findModelConfig` 搜索逻辑：
```go
// 第一遍：匹配模型名 + provider 一致
for _, m := range cfg.Models {
    if !m.Enabled || !modelConfigNameMatches(m, ...) { continue }
    if providerName != "" && m.Provider == providerName { return m, true }
}
// 第二遍：匹配模型名，不计 provider
for _, m := range cfg.Models {
    if !m.Enabled || !modelConfigNameMatches(m, ...) { continue }
    return m, true // 只要模型名匹配就算
}
```

---

#### ④ model-selection/*.json → ModelCatalog → catalog.Profile()

```json
{
  "provider": "deepseek",
  "models": [
    {
      "match": "deepseek-v4-flash",
      "priority": 3,
      "enabled": true,
      "execution": {
        "context_length": 1048576,
        "max_output_tokens": 131072,
        "temperature": 0.2,
        "max_tokens": 4096,
        "reasoning_effort": "medium",
        "timeout_seconds": 90
      }
    }
  ]
}
```

**加载方式**：`NewModelCatalog()` 调用两条路径：
1. `loadEmbeddedModelSelections()` — 从 embed FS `model-selection/` 目录读取
2. `loadModelSelections(configDir)` — 从用户配置目录读取；目录不存在时无效果

**消费方（三处）**：

| 代码路径 | 用途 | 优先级 |
|---|---|---|
| `catalog.Profile()` → `applyProfileDefaults()` | **覆盖** `applyExecutionDefaults` 已设置的参数 | **第二层（最终生效）** |
| `catalog.Profile()` → `buildOllamaShowBody()` | **覆盖** `findModelConfig` 已读取的能力参数 | **第二层** |
| `catalog.rebuildLocked()` → `syncRegistryMappings()` | 构建运行时模型路由表（model→ProviderEntry） | **唯一来源** |

`catalog.Profile()` 搜索逻辑：
```go
// 先在 entries（model-selection + discovered 合并结果）中搜索
// 1. 最长模型名匹配 + provider 一致
// 2. 精确 key 匹配 (model@provider)
```

`rebuildLocked()` 合并逻辑：
```go
// 第一遍：model-selection 条目（需 providerActiveLocked 检查）
for _, sel := range c.configs {
    if !c.providerActiveLocked(sel.Provider) { continue }  // ← 跳过 disabled provider
    for _, profile := range sel.Models {
        entries[key] = entry  // 加入 entries
    }
}
// 第二遍：discovered 条目（仅当 entries 中无同名 key 时才加入）
for _, discovered := range c.discoveredEntries() {
    key := catalogKey(discovered.Model, discovered.Provider)
    if _, exists := entries[key]; exists { continue }  // ← model-selection 优先
    entries[key] = discovered
}
```

---

#### ⑤ 运行时 provider ListModels → Registry.discoveredEntries()

```go
type ProviderEntry struct {
    Provider Provider
    Models   []string  // ← 异步从上游 API 拉取
    Priority int
}
```

**消费方**：
- `registry.SetModels()` → `rebuildModelMappingsLocked()` — 刷新内部 model→provider 映射
- `catalog.discoveredEntries()` → `rebuildLocked()` — 向 catalog 补充无静态配置的模型

**特点**：纯内存，不持久化。但 `UpdateModelMappingsWithUpstream()` 通过 catalog 的 `syncRegistryMappings()` 将语义注入 Registry 的最终路由表。

---

### 1.3 参数覆盖优先级链（当前）

以 `deepseek-v4-flash` 在 `UseAI` provider 上为例：

```
客户端请求                     temperature=nil, max_tokens=nil
       │
       ▼
applyExecutionDefaults() →   findModelConfig()
                                  │
                                  ├── cfg.Models 中有？ → 无（默认 config.json 无 deepseek-v4-flash）
                                  │
                                  ▼
                              applyGlobalDefaults() → temperature=0.7, max_tokens=4096
       │
       ▼
applyProfileDefaults() →     catalog.Profile("deepseek-v4-flash", "UseAI")
                                  │
                                  ├── model-selection 中有？ → "deepseek-v4-flash" provider=deepseek
                                  │   但 deepseek provider disabled → providerActiveLocked = false → 跳过
                                  │
                                  ├── discovered 中有？ → "deepseek-v4-flash@UseAI"
                                  │   Configured=false, Profile 空（无 temperature, max_tokens 等）
                                  │
                                  ▼
                              命中但 profile 为空 → 不覆盖
       │
       ▼
最终请求                     temperature=0.7, max_tokens=4096
```

**问题**：`applyProfileDefaults` 覆盖或不覆盖取决于 catalog 查到的 profile 是否有值。对于 ListModels 发现的模型，profile 是空的（没有能力参数），所以 `applyExecutionDefaults` 的默认值反而成了最终值。

---

## 2. 当前架构的问题

### 2.1 问题一：同一字段定义在两个文件，model-selection 总有更高优先级

| 字段 | config.json.models | model-selection/*.json | 哪个生效 |
|---|---|---|---|
| temperature | ✅ | ✅ | model-selection（作为 profile 被 applyProfileDefaults 覆盖） |
| max_tokens | ✅ | ✅ | model-selection |
| context_length | ✅ | ✅ | model-selection（在 buildOllamaShowBody 中覆盖） |
| max_output_tokens | ✅ | ✅ | model-selection |
| supports_tools | ✅ | ✅ | model-selection |

用户在 UI 上编辑模型参数 → 保存到 config.json → 参数不生效（被 embed 覆盖）。**这是最大的混淆来源。**

### 2.2 问题二：model-selection 中 provider 字段的角色不清晰

`deepseek.json` 中：
```json
{ "provider": "deepseek", "models": [...] }
```

这个 `provider` 字段在 `rebuildLocked()` 中做了两件事：
1. `providerActiveLocked("deepseek")` — 检查 deepseek provider 是否启用。若 disabled，**整个文件被跳过**
2. 定义模型→provider 映射，写入 entries 供路由使用

但对于当前场景：UseAI（指向 token.sensenova.cn）通过 ListModels 返回了 `deepseek-v4-flash`，这个模型实际由 UseAI 服务。但 model-selection 硬编码为 deepseek provider。**静态定义与运行时实际不符。**

### 2.3 问题三：config.json.models 的 provider 字段也造成了假象

`config.json` 默认有：
```json
{ "name": "deepseek-v4-pro", "provider": "deepseek", ... }
```

用户看到这个会认为"deepseek-v4-pro 属于 deepseek provider"。但实际上请求的路由由 Registry 的 `upstreamToProviders` 决定，provider 字段只用于 `findModelConfig()` 的第一遍精确匹配。

**provider 字段在两个文件中都存在，但都不是路由的决定性因素**——路由最终由 `syncRegistryMappings()` 根据 catalog entries 的 Provider 字段决定。

### 2.4 问题四：三层配置，消费方零散

```
config.json.models         →   findModelConfig (两处消费)
model-selection/*.json     →   catalog.Profile (三处消费)
ListModels 发现             →   discoveredEntries + registry.rebuild (两处消费)
```

同一个能力参数（如 context_length）在三个地方定义，在五处代码中被读取，覆盖顺序靠代码先后顺序隐式决定——不是显式设计。

---

## 3. 修正后的建议方案：Web/config.json 作为唯一用户配置入口

### 3.1 设计原则

1. **单一用户入口**：普通用户只在 Web 面板配置大模型 provider、API key 和模型参数，最终落到 `config.json`。
2. **渐进复杂度**：默认只展示 provider 列表和模型列表；多 API key、多账号、多模型通过“新增 provider 实例”自然表达，不引入单独 routing_rules。
3. **已有能力不推倒**：保留当前 Registry、ModelCatalog、流式矩阵、failover、Visual Studio model alias、request transformer，只调整配置合并优先级。
4. **用户配置优先**：`config.json` 中用户显式配置的 provider/model 永远优先于内置默认 profile。
5. **内置 profile 只做 fallback**：`model-selection` 不再作为用户不可见的最终覆盖层，而是作为默认能力库；用户一旦在 Web 里修改同名模型，就覆盖内置 profile。
6. **.env 退回运维角色**：`.env` 不再是大模型 provider 的主配置位置，只保留单端口监听相关的 `PORT`、`HOST`、Docker 发布相关的 `BIND_HOST`、`CONFIG_PATH`、`STORE_PATH`、可选 `PROXY_API_KEY` / `ADMIN_API_KEY`。旧版 `PROXY_PORT` 仅作为兼容 fallback，不再推荐；`MANAGEMENT_PORT` / `MANAGEMENT_HOST` 已废弃。

### 3.2 config.json 新结构

```jsonc
{
  "port": 12345,
  "default_model": "deepseek-v4-flash",

  "providers": [
    {
      "id": "useai",
      "name": "UseAI",
      "display_name": "UseAI",
      "base_url": "https://api.eforge.xyz/v1",
      "type": "openai",
      "api_key": "",
      "enabled": true,
      "priority": 0
    },
    {
      "id": "useai-paid",
      "name": "UseAI Paid",
      "display_name": "UseAI Paid",
      "base_url": "https://api.eforge.xyz/v1",
      "type": "openai",
        "api_key": "<api-key>",
      "enabled": true,
      "priority": 1
    },
    {
      "id": "ollama-local",
      "name": "ollama",
      "display_name": "Local Ollama",
      "base_url": "http://localhost:11434",
      "type": "ollama",
      "api_key": "",
      "enabled": true,
      "priority": 2
    }
  ],

  "models": [
    {
      "name": "deepseek-v4-flash",
      "provider_id": "",
      "context_length": 1048576,
      "max_output_tokens": 131072,
      "supports_tools": true,
      "supports_vision": false,
      "temperature": 0.2,
      "max_tokens": 4096,
      "reasoning_effort": "medium",
      "timeout_seconds": 90,
      "override_client_params": false,
      "enabled": true
    },
    {
      "name": "deepseek-v4-pro",
      "provider_id": "useai-paid",
      "context_length": 1048576,
      "max_output_tokens": 384000,
      "supports_tools": true,
      "supports_vision": false,
      "temperature": 0.2,
      "max_tokens": 8192,
      "reasoning_effort": "high",
      "timeout_seconds": 180,
      "override_client_params": false,
      "enabled": true
    }
  ]
}
```

**关键变化**：

| 字段 | 变化 | 原因 |
|---|---|---|
| `providers[].id` | 新增，作为稳定内部 ID | 支持同一 provider 类型配置多个 API key，例如 `useai`、`useai-paid`、`useai-team-a` |
| `providers[].name` | 保留，作为展示名兼兼容旧配置 | 旧配置仍可迁移；UI 可以直接展示 |
| `providers[].api_key` | 进入 config.json，由 Web 管理 | 避免 `.env` 和 Web 两处配置同一 provider |
| `models[].provider_id` | 新增但可为空 | 为空表示按 provider priority 自动选择；有值表示该模型参数只绑定某个 provider 实例 |
| `models[].provider` | 兼容保留一段时间，迁移为 provider_id | 避免一次性破坏旧 config 和旧测试 |
| `execution` 嵌套 | 暂不强制引入 | 当前 Go 结构体已是扁平字段；为降低迁移成本，先保留扁平格式 |

### 3.3 数据流变化

**当前（3 层并行，互相覆盖）：**
```
config.json.Models ──→ findModelConfig() ──→ 请求参数第一层
                          ↓ 被覆盖
model-selection/*  ──→  catalog.Profile() → applyProfileDefaults() → 请求参数第二层（最终）
                          ↓
                       rebuildLocked() → syncRegistryMappings() → 路由表
                          ↑
ListModels          ──→  discoveredEntries（被 model-selection 阻塞）
```

**修正后（用户配置优先 + 内置 profile fallback + 自动发现补充）：**
```
config.json.providers ──→ Registry.Add(provider instance)
config.json.models    ──→ 用户模型参数 override
内置 model profiles    ──→ 默认能力 fallback
ListModels            ──→ 运行时发现实际可用模型

EffectiveModelResolver:
  1. 先匹配 config.json.models 中 provider_id 精确绑定的配置
  2. 再匹配 config.json.models 中 provider_id 为空的通用配置
  3. 再匹配内置 model profile
  4. 最后使用全局默认值

Registry:
  1. 路由仍按 provider priority + ListModels 发现结果生成候选
  2. 请求模型为 model@provider_id 时，固定到指定 provider instance
  3. 请求 bare model 时，按 priority/failover 自动选择
```

### 3.4 变化清单

#### 不建议删除

| 文件/目录 | 原因 |
|---|---|
| `internal/provider/model-selection/` | 暂时保留为内置 profile fallback，避免把大量默认模型写入用户 config |
| `ModelCatalog` | 保留，但调整为从“用户配置 + 内置 profile + 发现模型”合成有效模型视图 |
| `applyProfileDefaults()` | 暂时保留或改名为 `applyEffectiveModelDefaults()`，但优先级必须低于用户 config |
| `UpdateModelMappingsWithUpstream()` | 保留，Visual Studio 模型列表和 failover 仍需要它 |

#### 修改

| 文件 | 改动 |
|---|---|
| `config.go` → `ProviderConfig` | 增加 `ID string json:"id"` 和 `DisplayName string json:"display_name"`；保留 `Name` 兼容旧配置 |
| `config.go` → `ModelConfig` | 增加 `ProviderID string json:"provider_id"`；保留 `Provider` 兼容旧配置 |
| `config.go` → 迁移逻辑 | 旧 provider 无 `id` 时用规范化 `name` 生成；旧 model 的 `provider` 迁移到 `provider_id` |
| `api.go` / Web UI | 所有 provider、API key、base_url、模型参数都通过 Web 保存到 `config.json` |
| `.env.example` | 移除 `PROVIDER_*_API_KEY`、`PROVIDER_*_BASE_URL`，只保留运行端口、路径和可选代理认证 |
| `model_catalog.go` | 调整合并优先级：用户 config 覆盖内置 profile；内置 profile 覆盖 discovered 空 profile |
| `request_transformer.go` | 请求参数只读取“effective model config”，不再让内置 profile 覆盖用户 config |
| `model_endpoints.go` / `ollama_show.go` | 展示 effective model config，确保 UI 保存后 Visual Studio 立即看到变化 |

#### 保留不变

| 逻辑 | 保持原因 |
|---|---|
| `Registry` 的 ResolveCandidates 逻辑 | 路由仍按 provider priority + ListModels 发现自动决定 |
| `Registry.Add()` | provider 注册逻辑不变 |
| `refreshModels()` | ListModels 异步发现逻辑不变 |
| `UpdateModelMappingsWithUpstream()` | catalog 仍将映射注入 Registry |
| OpenAI/Ollama 双协议与流式矩阵 | 这是 Visual Studio 兼容核心能力，不能因配置改造破坏 |
| reasoning cache / request transformer / failover | 这是参考 C# 项目的核心行为迁移，继续保留 |

### 3.5 迁移风险与回退

| 风险场景 | 缓解措施 |
|---|---|
| 现有 config.json 没有 `id` / `provider_id` | 启动时自动补齐，保存后写回新字段；旧字段继续读取 |
| 用户已有 `.env` 中的 provider key | 首次启动可读取一次并迁移到 `config.json`，之后 UI 成为主入口；文档标记旧环境变量为 deprecated |
| 同名模型由多个 provider 提供 | bare model 继续按 priority/failover；`model@provider_id` 固定到指定 provider instance |
| 一个 provider 多个 API key | 通过多个 provider instance 表达，例如 `useai`、`useai-paid`，不引入 key 数组 |
| 用户模型参数被内置 profile 覆盖 | 明确优先级：用户 config 永远高于内置 profile |
| config.json 保存密钥的安全担忧 | 本地桌面代理可接受；后续可接系统 Keychain/Credential Manager，但不作为当前 P0 |

### 3.6 预期效果

- **用户心智简化**：大模型相关配置只有 Web/config.json 一个入口。
- **开发改动可控**：不删除 Registry、ModelCatalog、流式转换、failover 等已完成能力，只调整配置合并优先级和字段兼容。
- **多 key 支持自然**：同一 provider 类型可以新增多个 provider instance，不需要复杂 routing_rules。
- **多模型支持自然**：ListModels 发现模型，用户只对需要调参的模型保存 override。
- **Visual Studio 兼容保留**：继续支持 bare model、`model@provider_id`、`/v1/models`、`/api/tags`。

---

## 4. 明确决策

1. 大模型 provider 配置、API key、base_url、模型参数统一由 Web 面板写入 `config.json`。
2. `.env` 不再作为 provider 配置主入口，只保留运行环境配置。
3. 不删除 `provider` 概念；新增 `provider_id`，并保留旧 `provider` 字段作为兼容输入。
4. 不把全部 `model-selection` 内容写入 `DefaultConfig()`；内置 profile 继续做 fallback。
5. 不引入显式 `routing_rules` 作为第一阶段产品概念。
6. 一个 provider 多 API key 用多个 provider instance 表达。
7. 一个 provider 多模型由 `ListModels` 自动发现，用户只保存模型 override。
8. 保存 config 后必须热更新 Registry/Catalog，使 Web 修改立即影响 Visual Studio 路由和模型展示。

## 5. 分阶段实施建议

### Phase 1：低风险兼容迁移

1. `ProviderConfig` 增加 `id`、`display_name`，保留 `name`。
2. `ModelConfig` 增加 `provider_id`，保留 `provider`。
3. 配置加载时自动补齐 `id` / `provider_id`。
4. Web provider 表单改为保存 API key/base_url 到 `config.json`。
5. `.env.example` 移除 `PROVIDER_*` 示例，只保留运维项。

### Phase 2：统一 effective config

1. 引入内部 `EffectiveModelConfig` 合并结果。
2. 合并优先级固定为：用户 provider_id 精确配置 > 用户通用模型配置 > 内置 profile > discovered profile > global defaults。
3. `request_transformer`、`/api/show`、`/api/tags`、`/v1/models` 都读取 effective config。

### Phase 3：产品化增强

1. Web UI 支持“复制 provider”，用于同一 provider 多 API key。
2. Web UI 显示模型来源：discovered / user override / built-in fallback。
3. Web UI 支持“将发现模型固定到配置”。
4. 后续再评估系统 Keychain/Credential Manager，不作为当前阻塞项。

## 6. 2026-07-14 请求默认值修正

历史分析中的 `applyGlobalDefaults() -> temperature=0.7, max_tokens=4096` 已不再适用于 Visual Studio Copilot 工具请求。当前实现仅对声明了 modern `tools` 或 legacy `functions` 的请求跳过这两个猜测默认；普通聊天继续保留原行为，避免扩大兼容变更。工具请求仍可使用客户端值、用户模型配置和正式 profile。

原因是 `max_tokens=4096` 会截断较大的 `create_file`、补丁和代码生成工具参数，并且会阻止 profile 中更大 `MaxTokens` 在后续合并阶段生效。该历史段落保留用于说明当时决策，但当前行为以 `internal/proxy/request_transformer.go` 和通用工具矩阵测试为准。
