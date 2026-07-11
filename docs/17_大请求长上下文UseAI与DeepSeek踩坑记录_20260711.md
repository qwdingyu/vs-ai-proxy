# 大请求长上下文 UseAI 与 DeepSeek 踩坑记录（2026-07-11）

## 1. 背景与事故现象

本轮问题发生在 Visual Studio / Copilot 真实使用场景中，核心表现不是普通聊天失败，而是带有大量上下文和工具声明的 `POST /v1/chat/completions` 请求在不同 provider 上行为不一致。

用户反馈的典型现象：

1. 同一个项目、同一个 VS/Copilot 任务，第一次选择官方 `deepseek` provider 的 `deepseek-v4-flash` 可以成功。
2. 随后切换到 `useai` provider 的同名 `deepseek-v4-flash`，出现 `413`、`502`、`upstream_payload_too_large`、`client_gone` 或上游异常。
3. 管理端测试页的小请求可以成功，但 VS/Copilot 真实环境仍失败。
4. 之前排障过程中多次把现象误归因到模型不可见、nginx、new-api、apikey、Windows 环境或工具调用本身，导致分析不够闭环。

必须强调：本问题不能只用 Web 测试页判断。Web 测试页发送的是极小请求，通常没有 VS/Copilot 的大上下文、工具声明和文件内容；它只能证明“模型小请求可通”，不能证明“真实 VS/Copilot 大工具请求可通”。

## 2. 关键配置事实

本轮需要特别注意官方 DeepSeek provider 的配置：

```json
{
  "id": "deepseek",
  "name": "deepseek",
  "display_name": "deepseek",
  "api_key": "sk-xxxx",
  "base_url": "https://api.deepseek.com",
  "type": "openai",
  "enabled": true,
  "priority": 1
}
```

注意事项：

- 官方 DeepSeek 的 `base_url` 是 `https://api.deepseek.com`，不是 `https://api.deepseek.com/v1`。
- 代理内部根据 provider capability 拼接官方 DeepSeek 的 chat path；不能在诊断中随意假设 `/v1/chat/completions`。
- `useai` 的 `base_url` 是 `https://api.eforge.xyz/v1`，属于 OpenAI-compatible 聚合网关路径。
- `apikey` / group 会影响 `useai` 的 `/models` 可见列表。之前旧 key 下 `deepseek-v4-flash` 返回 `model_not_found`，用户修正 key 后 `/models` 已可见该模型。
- 不能把“config.json 没显式绑定模型”直接等同为“上游没有模型”；`/models` 动态发现也是合法模型来源。

## 3. 已确认的根因结论

### 3.1 不是单纯模型不可见

修正后的 `useai` key 可通过：

```bash
curl -H 'User-Agent: VS-AI-Proxy-Diagnostic/1.0' \
  -H 'Authorization: Bearer <useai_api_key>' \
  https://api.eforge.xyz/v1/models
```

确认包含：

- `deepseek-v4-flash`
- `step-3.5-flash`
- `step-3.7-flash`
- `step-router-v1`

所以后续 `useai + deepseek-v4-flash` 失败不再是“模型不可见”的问题。

### 3.2 是大请求体 / 长上下文阈值强相关问题

使用同一份 VS/Copilot 风格请求体（包含工具声明、stream=true、大量上下文）进行矩阵测试，结果如下：

| Provider | Model | Target Bytes | Direct | Proxy | 结论 |
| --- | --- | ---: | --- | --- | --- |
| `deepseek` | `deepseek-v4-flash` | `50KB` | 200 | 200 | 成功 |
| `useai` | `deepseek-v4-flash` | `50KB` | 200 | 200 | 成功 |
| `useai` | `deepseek-v4-flash` | `200KB` | 200 | 200 | 成功 |
| `useai` | `deepseek-v4-flash` | `500KB` | 200 | 200 | 成功 |
| `useai` | `deepseek-v4-flash` | `800KB` | 200 | 200 | 成功 |
| `deepseek` | `deepseek-v4-flash` | `1.06MB` | 200 | 200 | 成功 |
| `useai` | `deepseek-v4-flash` | `1.06MB` | 413/502 | 502 | 失败，分类为 `upstream_payload_too_large` 或上游异常 |

关键观察：

- 官方 `deepseek` 同体积 `1.06MB` 请求可以成功。
- `useai` 同模型同体积失败。
- `useai` 在 `800KB` 左右仍可成功，`1.06MB` 失败，说明存在大小阈值或上游渠道限制。
- 对 `1.09MB` 无工具请求做过额外对照，仍然返回 413，说明不是工具 schema 单独导致，而是请求体/上下文本身大小触发限制。

### 3.3 代理没有异常放大请求体

本轮新增日志字段 `upstream_bytes`，用于对比：

- 客户端进入代理的 `request_bytes`
- 代理序列化后发给上游的 `upstream_bytes`
- 差值 `delta_bytes = upstream_bytes - request_bytes`

代表性结果：

| Provider | Model | request_bytes | upstream_bytes | delta_bytes | 状态 |
| --- | --- | ---: | ---: | ---: | --- |
| `deepseek` | `deepseek-v4-flash` | 1,077,092 | 1,077,127 | 35 | 200 |
| `useai` | `deepseek-v4-flash` | 1,077,089 | 1,077,099 | 10 | 502 / payload too large |

结论：代理只增加极少量 JSON 字段差异，并没有把请求从小体积异常膨胀到大体积。因此本轮 `useai` 的 1MB 失败不是代理体积膨胀导致。

### 3.4 官方 deepseek 路由曾有一个真实代理 bug

排查过程中发现一个独立代理问题：

- config 中同时可能存在：
  - `deepseek/deepseek-v4-flash`
  - `deepseek-v4-flash`
- 请求模型是 VS 展示名：`deepseek - deepseek-v4-flash`
- 旧逻辑在同 provider 内可能优先通过 basename 匹配到 `deepseek/deepseek-v4-flash`
- 官方 DeepSeek 上游实际要求模型名是 `deepseek-v4-flash`
- 结果会报：

```text
The supported API model names are deepseek-v4-pro or deepseek-v4-flash,
but you passed deepseek/deepseek-v4-flash.
```

修复策略：

1. 同 provider 内先做精确模型名匹配。
2. 精确匹配失败后，再做 basename 匹配。
3. 保证官方 DeepSeek 的 `deepseek - deepseek-v4-flash` 最终 upstream 是 `deepseek-v4-flash`。

修复后验证：

- 官方 `deepseek` 直连 `1.06MB`：200
- 官方 `deepseek` 经代理 `1.06MB`：200
- 日志 upstream：`deepseek-v4-flash`

## 4. 本轮代码层面的对策与优化

### 4.1 VS 展示名前缀固定 provider

问题：VS/Copilot 可能发送：

```text
UseAI - deepseek-v4-flash
UseCpa - step-3.7-flash:latest
DeepSeek - deepseek-v4-flash
```

这类模型名已经表达了 provider 选择意图。旧逻辑中如果 provider 的本地模型缓存尚未刷新，可能无法路由，或者在某些情况下走到其他 provider。

当前策略：

- 如果模型名包含 provider 展示前缀，则先解析 provider。
- 找到 provider 后，优先匹配该 provider 的模型。
- 如果本地模型缓存没有该模型，也只向该 provider 透传展示名前缀后的模型名。
- 禁止因为本地未缓存而跨 provider fallback。

意义：

- `/models` 动态发现延迟不会导致 VS 已选模型直接不可路由。
- 不会把用户明确选择的 `UseAI - xxx` 悄悄转给 `deepseek` 或其他 provider。
- 上游是否支持该模型交给被选中的 provider 给出权威结果。

### 4.2 `/models` 发现结果只补充，不覆盖配置种子模型

问题：某些 OpenAI-compatible 网关会根据：

- API key
- group
- User-Agent
- WAF
- 渠道状态
- 临时错误

返回不同的 `/models` 列表。如果刷新时直接覆盖启动时从 config 中注入的模型，可能导致已导入/已绑定模型在运行中突然丢失。

当前策略：

- 启动时 config.json 中绑定的模型作为路由种子。
- 后台 `/models` 发现结果用 `MergeModels` 合并。
- 发现结果可以补充模型，但不应抹掉配置中已有模型。

注意：这不是把 config 当唯一事实源，而是同时保留：

1. 用户显式导入/绑定的模型。
2. 上游 `/models` 动态发现的模型。

### 4.3 聚合网关不再隐式注入 cached reasoning

问题：之前 transformer 会尝试把缓存的 `reasoning_content` 注入后续请求。该能力对部分直连推理 provider 有意义，但对 `useai`、`new-api`、`sub2api` 这类聚合网关存在风险：

- 聚合网关背后不同渠道的上下文/请求体限制不同。
- 隐式注入会让请求体变大，而且用户和日志不容易发现。
- 对 VS/Copilot 的大工具请求尤其危险，可能更早触发 413。

当前策略：

- 仅对 direct reasoning provider 注入 cached reasoning。
- 对聚合网关不注入。
- 减少不可见请求体增长。

### 4.4 增加 `upstream_bytes` 观测字段

日志中新增：

```json
{
  "request_bytes": 1077089,
  "upstream_bytes": 1077099,
  "error_code": "upstream_payload_too_large"
}
```

用途：

- 判断代理是否异常放大请求体。
- 判断失败是代理处理问题，还是上游链路限制。
- 对比不同 provider 的同体积请求差异。
- 后续排查 `client_gone`、`context canceled`、`413` 时必须首先看该字段。

### 4.5 错误提示改为当前 provider 语义

当候选只有一个 provider 时，错误文案从“所有候选提供商请求均失败”改为更准确的“当前提供商请求失败”。

意义：

- 防止用户误以为系统尝试了多个 provider。
- 符合当前防御策略：带 provider 前缀时固定 provider，不跨 provider fallback。

## 5. 固化的测试脚本

### 5.1 单次诊断脚本

路径：

```bash
tests/useai_large_request_diagnostic.sh
```

用途：

- 生成一个 VS/Copilot 风格的大请求体。
- 包含工具声明，如 `create_file`、`apply_patch`、`git`、`powershell`、`get_file` 等。
- 同时测试：
  1. 直连上游 provider。
  2. 经过本地 vs-ai-proxy。
- 输出：
  - direct status
  - proxy status
  - request_bytes
  - upstream_bytes
  - delta_bytes
  - last_proxy_log

常用命令：

```bash
INJECT_MODEL_BINDING=0 \
MODEL=deepseek-v4-flash \
PROVIDER_ID=deepseek \
DISPLAY_PROVIDER=deepseek \
TARGET_BYTES=1060000 \
tests/useai_large_request_diagnostic.sh
```

```bash
INJECT_MODEL_BINDING=0 \
MODEL=deepseek-v4-flash \
PROVIDER_ID=useai \
DISPLAY_PROVIDER=UseAI \
TARGET_BYTES=1060000 \
tests/useai_large_request_diagnostic.sh
```

参数说明：

| 参数 | 说明 |
| --- | --- |
| `CONFIG_SOURCE` | 默认 `~/.config/vs-ai-proxy/config.json` |
| `PROVIDER_ID` | provider 实例 ID，例如 `deepseek`、`useai`、`useai2` |
| `DISPLAY_PROVIDER` | VS 展示名前缀，例如 `deepseek`、`UseAI`、`UseAI2` |
| `MODEL` | 上游模型名，例如 `deepseek-v4-flash` |
| `TARGET_BYTES` | 目标请求体大小 |
| `INJECT_MODEL_BINDING` | 是否只在临时 config 中注入模型绑定，默认可用于诊断；正式对照建议设为 `0` |
| `DIRECT_TIMEOUT_SECONDS` | 直连上游超时 |
| `PROXY_TIMEOUT_SECONDS` | 代理请求超时 |

注意：脚本会复制配置到 `.bin/useai-large-diagnostic/config.json`，不会修改用户原始 config。

### 5.2 批量矩阵诊断脚本

路径：

```bash
tests/large_request_matrix_diagnostic.sh
```

用途：

- 复用单次诊断脚本。
- 不再每个 provider/model 临时写一遍测试命令。
- 适合比较多个 provider、多个模型、多个请求体大小。

默认矩阵：

```text
deepseek|deepseek|deepseek-v4-flash
useai|UseAI|deepseek-v4-flash
```

默认大小：

```text
50000 200000 500000 800000 1060000
```

运行默认矩阵：

```bash
tests/large_request_matrix_diagnostic.sh
```

只跑关键 1.06MB：

```bash
SIZES='1060000' tests/large_request_matrix_diagnostic.sh
```

自定义 provider/model：

```bash
CASES=$'deepseek|deepseek|deepseek-v4-flash\nuseai|UseAI|deepseek-v4-flash\nuseai2|UseAI2|gpt-5.5' \
SIZES='50000 800000 1060000' \
tests/large_request_matrix_diagnostic.sh
```

结果文件：

```bash
.bin/large-request-matrix/results.jsonl
.bin/large-request-matrix/summary.json
```

终端摘要示例：

```text
deepseek  deepseek-v4-flash  1060000  direct=200  proxy=200  code=-                         req=1077092  up=1077127  delta=35
useai     deepseek-v4-flash  1060000  direct=502  proxy=502  code=upstream_payload_too_large req=1077089  up=1077099  delta=10
```

## 6. 本轮测试结果记录

### 6.1 小请求对照

命令：

```bash
SIZES='50000' \
CASES=$'deepseek|deepseek|deepseek-v4-flash\nuseai|UseAI|deepseek-v4-flash' \
tests/large_request_matrix_diagnostic.sh
```

结果：

| Provider | Model | Target | Direct | Proxy | request_bytes | upstream_bytes | delta |
| --- | --- | ---: | --- | --- | ---: | ---: | ---: |
| `deepseek` | `deepseek-v4-flash` | 50KB | 200 | 200 | 50,496 | 50,531 | 35 |
| `useai` | `deepseek-v4-flash` | 50KB | 200 | 200 | 50,493 | 50,503 | 10 |

结论：小请求两边都成功，说明模型、key、基础路由都可用。

### 6.2 关键大请求对照

命令：

```bash
SIZES='1060000' \
CASES=$'deepseek|deepseek|deepseek-v4-flash\nuseai|UseAI|deepseek-v4-flash' \
DIRECT_TIMEOUT_SECONDS=140 \
PROXY_TIMEOUT_SECONDS=160 \
tests/large_request_matrix_diagnostic.sh
```

结果：

| Provider | Model | Target | Direct | Proxy | error_code | request_bytes | upstream_bytes | delta |
| --- | --- | ---: | --- | --- | --- | ---: | ---: | ---: |
| `deepseek` | `deepseek-v4-flash` | 1.06MB | 200 | 200 | - | 1,077,092 | 1,077,127 | 35 |
| `useai` | `deepseek-v4-flash` | 1.06MB | 502/413 | 502 | `upstream_payload_too_large` 或上游异常 | 1,077,089 | 1,077,099 | 10 |

结论：同体积同模型，官方 DeepSeek 成功，UseAI 失败；这不是代理体积膨胀问题，而是 UseAI 链路对该大请求的限制或上游渠道异常。

## 7. 后续配置建议

### 7.1 UseAI / new-api / sub2api 侧

如果希望 `useai + deepseek-v4-flash` 支持 VS/Copilot 真实大上下文，需要检查：

1. new-api / sub2api 的请求体大小限制。
2. 上游反代 nginx / Caddy / Cloudflare 的 body limit。
3. new-api 内部渠道对该模型的 context limit。
4. 该模型绑定到哪个渠道组，是否所有渠道都支持大上下文。
5. 渠道是否有独立超时、请求大小、上下文大小限制。
6. API key 所属 group 是否和 `/models` 可见模型一致。
7. 大请求是否命中特定渠道，该渠道是否返回 413 或 5xx。

本轮证据显示：`useai` 的 1.06MB 请求 direct 也失败，所以只改代理不能让该链路无限制支持大请求。

### 7.2 vs-ai-proxy 侧

代理侧可以继续增强“防御”，但必须清楚边界：

- 可以做：
  - 更早识别大请求风险。
  - 在日志中明确 request/upstream bytes。
  - 给出 provider/channel 级别提示。
  - 支持可配置阈值，超过阈值提示用户切换 provider 或减少上下文。
  - 未来可做可选的上下文裁剪策略。
- 不应做：
  - 静默跨 provider fallback。
  - 静默丢弃工具声明。
  - 静默裁剪用户上下文导致工具调用缺失。
  - 把 413 伪装成普通上游 500。

## 8. 后续观测重点

遇到类似问题时，必须按以下顺序看日志：

1. `provider`：是否是用户选择的 provider。
2. `model`：是否仍是 VS 展示名。
3. `upstream`：是否是上游真实模型名。
4. `request_bytes`：客户端进入代理的请求大小。
5. `upstream_bytes`：代理发给上游的请求大小。
6. `delta_bytes`：代理是否异常放大。
7. `request_tools`：工具声明是否存在，数量是否异常。
8. `response_tools`：模型是否真的返回工具调用。
9. `error_code`：特别关注：
   - `upstream_payload_too_large`
   - `client_deadline_reached`
   - `client_gone`
   - `upstream_server_error`
   - `upstream_rate_limited`
10. `elapsed_ms`：是否接近 VS/Copilot 等待上限。

判断规则：

- `request_bytes` 小、`upstream_bytes` 大很多：优先查代理 transform / reasoning 注入 / 参数扩展。
- `request_bytes ≈ upstream_bytes` 且直连也失败：优先查上游链路限制。
- 官方 provider 成功、聚合 provider 失败：优先查聚合网关渠道限制、body/context limit、group、WAF。
- 小请求成功、大请求失败：不要再用 Web 测试页证明“模型没问题”，必须用大请求脚本。

## 9. 避免再次踩坑的规则

1. 不要用 Web 测试页的小请求代表 VS/Copilot 真实大工具请求。
2. 不要把 `/models` 可见等同于“任意大小上下文都可用”。
3. 不要把 `config.json` 未显式绑定等同于“模型不存在”。
4. 不要在没有 `request_bytes/upstream_bytes` 对比前断言代理放大或没有放大。
5. 不要把官方 DeepSeek 的 base_url 当成 `/v1` 形式。
6. 不要让 provider 展示名前缀模型跨 provider fallback。
7. 不要在聚合网关上隐式注入 cached reasoning。
8. 不要对 `413` 做无意义跨 provider 重试；它通常代表请求体/上下文/渠道限制。
9. 不要每次临时写一次 curl 或 Python；必须复用 `tests/large_request_matrix_diagnostic.sh`。
10. 不要只测单一模型；至少覆盖官方 provider、聚合 provider、小请求、大请求四个维度。

## 10. 本轮验证命令

代码回归：

```bash
go test ./... -count=1
```

Race 核心包：

```bash
go test -race ./cmd/server ./internal/proxy ./internal/provider ./internal/config -count=1
```

流式协议：

```bash
bash tests/streaming_test.sh
bash tests/streaming_ollama_test.sh
```

大请求矩阵：

```bash
SIZES='1060000' \
CASES=$'deepseek|deepseek|deepseek-v4-flash\nuseai|UseAI|deepseek-v4-flash' \
DIRECT_TIMEOUT_SECONDS=140 \
PROXY_TIMEOUT_SECONDS=160 \
tests/large_request_matrix_diagnostic.sh
```

## 11. 当前状态

代理侧已完成：

- provider 展示名前缀固定路由。
- `/models` 动态发现与 config 种子模型合并。
- 官方 DeepSeek 精确模型优先匹配。
- 聚合网关禁用隐式 cached reasoning 注入。
- 日志记录 `upstream_bytes`。
- `upstream_payload_too_large` 更明确诊断提示。
- 单次与批量大请求诊断脚本固化。

仍需外部/配置侧处理：

- `useai + deepseek-v4-flash` 在 1MB 级 VS/Copilot 请求下的上游限制。
- new-api/sub2api/反代/渠道的 body/context/timeout 配置。
- 如果希望 UseAI 与官方 DeepSeek 一样承载 1MB+ VS/Copilot 请求，需要在 UseAI 链路上扩大限制或把该模型路由到支持大上下文的渠道。

## 12. 结论

本轮问题的最终定论是：

- 官方 `deepseek` provider 使用 `https://api.deepseek.com` 配置时，`deepseek-v4-flash` 可以承载本轮构造的 1.06MB VS/Copilot 工具大请求。
- `useai` provider 在同模型、同体积请求下失败，且代理出站体积几乎没有增加，因此不是代理把请求放大导致。
- UseAI 失败与大请求/长上下文阈值强相关，不是单纯模型不可见，也不是单纯工具名 `create_file` 的问题。
- 后续不要再靠临时脚本和零散 curl 排查，应统一使用固化矩阵脚本复现、比较、归档。

## 13. 模型名“张冠李戴”专项排查补充

用户再次指出：官方 DeepSeek 曾被代理错误路由为 `deepseek/deepseek-v4-flash`，不能只局部修一个 case，必须全面排查类似问题。本节记录专项审查结果。

### 13.1 排查范围

本次排查覆盖了 registry 中所有可能把模型 A 误映射为模型 B 的路径：

1. VS 展示名路径：`Provider - model[:latest]`。
2. provider hint 路径：`provider/model`。
3. provider-qualified 路径：`model@provider`。
4. namespaced upstream 路径：`vendor/model`。
5. catalog 映射路径：`ModelCatalog.syncRegistryMappings` 写入的 `modelToProvider` / `modelToUpstream`。
6. 动态 `/models` 刷新路径：`SetModels` / `MergeModels`。
7. fallback 路径：模型未命中时按 provider priority 透传。
8. basename 映射路径：`glm-5.2` -> `z-ai/glm-5.2` 这类 VS 展示短名适配。

### 13.2 发现的同类风险

#### 风险 A：同 provider 内“精确模型”和“namespaced 模型”同时存在

示例：

```text
deepseek-v4-flash
deepseek/deepseek-v4-flash
```

旧风险：

- 请求是 `deepseek - deepseek-v4-flash`。
- registry 可能通过 basename 匹配命中 `deepseek/deepseek-v4-flash`。
- 官方 DeepSeek 实际只接受 `deepseek-v4-flash`。
- 结果就是“张冠李戴”：展示名看起来是官方 DeepSeek，但 upstream 变成了不被官方接受的 namespaced 模型。

修复：

- `candidateForEntryModelLocked` 分阶段匹配：
  1. 先匹配 entry.Models 中的精确 upstream。
  2. 再匹配 entry.Models 中的 basename。
  3. 再匹配 catalog/modelToUpstream 中的精确模型或精确 upstream。
  4. 最后才匹配 catalog/modelToUpstream 的 basename。

#### 风险 B：provider hint `deepseek/deepseek-v4-flash` 优先级错误

示例：

```text
请求: deepseek/deepseek-v4-flash
provider hint: deepseek
bare model: deepseek-v4-flash
```

旧风险：

- 先检查完整 `deepseek/deepseek-v4-flash` 可能命中 namespaced 模型。
- 但如果同 provider 存在裸模型 `deepseek-v4-flash`，对官方 DeepSeek 来说裸模型才是正确 upstream。

修复：

- `resolveProviderHintLocked` 改为先检查 bare model，再检查完整 clean model。
- 这样 `deepseek/deepseek-v4-flash` 会优先解释为“provider=deepseek，model=deepseek-v4-flash”，而不是强行传 upstream=`deepseek/deepseek-v4-flash`。

注意：这只在 provider hint 能匹配到 provider 时生效。对于真正的 vendor namespace，例如 `z-ai/glm-5.2`，如果 `z-ai` 不是 provider alias，则仍按 namespaced model 处理。

#### 风险 C：catalog 映射重放 basename 覆盖精确意图

旧风险：

- `ModelCatalog` 可能把配置模型和发现模型都同步到 registry。
- 如果 catalog 中同时存在精确裸模型和 namespaced 模型，后续 `candidateForEntryModelLocked` 遍历 `modelToProvider` 时可能通过 basename 抢先命中错误 upstream。

修复：

- catalog 映射参与 candidate 选择时也分两轮：先精确，再 basename。
- 新增测试覆盖 catalog 同时存在：
  - `deepseek-v4-flash -> deepseek-v4-flash`
  - `deepseek/deepseek-v4-flash -> deepseek/deepseek-v4-flash`
- 期望 `deepseek - deepseek-v4-flash` 最终 upstream 仍是 `deepseek-v4-flash`。

#### 风险 D：未知 VS 展示名前缀被剥掉后误路由

示例：

```text
UnknownProvider - deepseek-v4-flash
```

旧风险：

- 如果 `UnknownProvider` 没有匹配到 provider，旧逻辑可能继续走普通 display suffix 处理。
- 结果会剥掉 `UnknownProvider - `，把请求误路由到已有的 `deepseek` 或其他 provider。

修复：

- 只要请求形态像 VS 展示名，并且 provider 前缀无法匹配，就视为已匹配 display 语义但候选为空。
- 不再继续 fallback 到其他 provider。
- 新增集成测试：`UnknownProvider - deepseek-v4-flash:latest` 不应调用任何已有 provider。

### 13.3 当前模型匹配优先级规范

最终规范如下：

1. `model@provider`：强绑定 provider。
2. `ProviderDisplay - model[:latest]`：强绑定展示名前缀对应 provider。
3. `provider/model` 且 `provider` 是已知 provider/alias：解释为 provider hint，先匹配 bare model。
4. 精确模型名匹配优先于 basename 匹配。
5. 同 provider 内，裸模型精确匹配优先于 namespaced basename。
6. basename 只用于兼容 VS 展示短名，例如 `glm-5.2` -> `z-ai/glm-5.2`，且遇到多个 namespaced 候选必须拒绝自动路由。
7. provider 前缀不存在时，不允许剥前缀后跨 provider fallback。
8. fallback 只用于没有显式 provider 意图的普通模型名。

### 13.4 新增/强化的回归测试

新增或强化的测试包括：

- `TestRegistryDisplayNamePrefersExactProviderModelBeforeBasename`
  - 覆盖 `deepseek - deepseek-v4-flash` 不应命中 `deepseek/deepseek-v4-flash`。
- `TestRegistryProviderHintPrefersExactBareModelBeforeNamespacedModel`
  - 覆盖 `deepseek/deepseek-v4-flash` provider hint 应优先解释为裸模型。
- `TestRegistryCatalogMappingPrefersExactUpstreamBeforeBasename`
  - 覆盖 catalog 映射中精确 upstream 优先级。
- `TestRegistryUnknownDisplayNamePrefixDoesNotFallbackToOtherProvider`
  - 覆盖未知 display provider 不允许 fallback。
- `TestChatCompletionsRejectsUnknownVisualStudioDisplayProvider`
  - 集成层确认未知展示名前缀不会真实调用 provider。
- 原有 `TestRegistryRejectsAmbiguousNamespacedBasename`
  - 保证 basename 有多个 namespaced 候选时拒绝自动路由。
- 原有 `TestRegistryPrefersDirectOfficialGLMOverNamespacedBasename`
  - 保证官方裸模型优先于聚合 namespaced 模型。

### 13.5 真实配置矩阵验证

使用真实本地配置执行小请求矩阵：

```bash
SIZES='50000' \
CASES=$'deepseek|deepseek|deepseek-v4-flash\nuseai|UseAI|deepseek-v4-flash\nusecpa|UseCpa|step-3.7-flash\nuseai2|UseAI2|gpt-5.5' \
tests/large_request_matrix_diagnostic.sh
```

验证结果：

| Provider | Display | Model | Direct | Proxy | Upstream | 结论 |
| --- | --- | --- | --- | --- | --- | --- |
| `deepseek` | `deepseek` | `deepseek-v4-flash` | 200 | 200 | `deepseek-v4-flash` | 正确 |
| `useai` | `UseAI` | `deepseek-v4-flash` | 200 | 200 | `deepseek-v4-flash` | 正确 |
| `usecpa` | `UseCpa` | `step-3.7-flash` | 200 | 200 | `step-3.7-flash` | 正确 |
| `useai2` | `UseAI2` | `gpt-5.5` | 200 | 200 | `gpt-5.5` | 正确 |

没有发现上述真实配置在小请求下出现 provider 或 upstream 张冠李戴。

### 13.6 后续注意事项

1. 新增 provider alias 时必须确认不会和 vendor namespace 冲突。
2. 如果某 provider 同时暴露裸模型和 namespaced 模型，必须用精确优先策略。
3. 不要随意恢复“未知展示名前缀 fallback”，这会重新引入误路由。
4. basename 兼容只能作为最后手段，且必须做歧义检测。
5. 任何涉及 `ResolveCandidates`、`ResolveModel`、`ModelBasename`、`DisplayNameParts` 的改动，都必须跑 registry 全量测试和真实配置矩阵脚本。

## 14. `context canceled` / `use of closed network connection` 专项优化补充

### 14.1 现象

真实日志示例：

```text
模型 gpt-5.5 在提供商 useai2 流式失败: openai stream error: 请求失败: Post "https://api.eforge.xyz/v1/chat/completions": context canceled
POST /v1/chat/completions - 499 (100043 ms)
client_deadline_reached 499 97918 ms
```

另一个网络断连示例：

```text
write tcp 192.168.1.11:57874->104.21.57.81:443: use of closed network connection
```

### 14.2 结论

这两类问题和 `413 Request Entity Too Large` 不同：

- `413` 是明确的请求体/上下文过大，通常由 nginx、CDN、new-api 或渠道限制返回。
- `client_deadline_reached` 是客户端接近等待上限时取消请求，典型耗时约 98-100 秒。
- `use of closed network connection` 是连接生命周期异常，可能发生在本机网络、Cloudflare/CDN、反代、源站或上游任意一层。

`104.21.57.81` 这类 IP 通常是 Cloudflare 边缘节点 IP，不是 new-api 源站 IP。看到该 IP 只能说明客户端当前连接到了 CDN/边缘节点，不能说明源站 IP 暴露或 new-api 机器就是这个地址。

### 14.3 本轮优化

为避免继续把这些错误混成普通 502，本轮增加了两类日志字段：

```json
{
  "error_code": "client_deadline_reached",
  "cancel_reason": "client_deadline_reached",
  "network_peer": "104.21.57.81:443"
}
```

字段含义：

| 字段 | 说明 |
| --- | --- |
| `cancel_reason` | 细化 499/取消类请求原因，例如 `client_deadline_reached`、`client_canceled`、`server_timeout` |
| `network_peer` | 从 Go 网络错误中提取的远端 `IP:port`，用于识别 CDN/边缘节点或直连源站 |

同时增强错误提示：

- `client_deadline_reached` 会提示检查 new-api/sub2api 渠道首 token、单渠道超时和重试策略。
- `network_error` 会提示检查 DNS/CDN、代理网络、防火墙、连接重置，并在能解析时展示远端 peer。
- 诊断脚本会额外记录 `direct_elapsed_ms` 和 `proxy_elapsed_ms`，用于判断直连与代理路径耗时是否同时接近 VS/Copilot 等待上限。

### 14.4 判断规则

| 现象 | 优先判断 | 建议 |
| --- | --- | --- |
| `499` 且 `elapsed_ms≈100000` | VS/Copilot 客户端等待上限 | 检查上游首 token、渠道排队、new-api 单渠道超时 |
| `context canceled` 且耗时很短 | 用户取消、客户端断开或网络瞬断 | 看是否可重试成功，结合本地网络和客户端状态 |
| `use of closed network connection` | 连接写入或流式过程中被关闭 | 看 `network_peer`，若为 Cloudflare IP，检查 CDN/源站链路 |
| 小请求快、大请求 100 秒 | 大上下文导致上游慢或排队 | 降低上下文、减少文件内容或切换更稳 provider |
| direct 和 proxy 都慢/断 | 上游链路问题概率高 | 优先查 provider/channel，而不是代理转换 |
| direct 快、proxy 慢很多 | 代理或本机网络才是优先嫌疑 | 保留 result.json，重点看日志与本机资源 |

### 14.5 不做的事

本轮没有加入“流式中途自动重试”。原因：

- 流式响应一旦已经向 VS/Copilot 写出 chunk，自动重试会破坏协议顺序。
- 工具调用请求可能不是幂等的，盲目重试会让上游重复计费或重复生成工具调用。
- 当前更安全的做法是增强分类和观测，让用户/运维能定位是客户端 deadline、CDN 断连还是上游限制。

如后续要做重试，只能限制在“尚未向客户端写任何响应之前”，且必须受防御开关控制，并记录 `fallback_mode`。

## 15. VS/Copilot 100 秒等待上限与代理有效超时预算

### 15.1 新现象

真实日志：

```text
模型 step-router-v1 在提供商 useai 流式失败: openai stream error: 请求失败: Post "https://api.eforge.xyz/v1/chat/completions": context canceled
POST /v1/chat/completions - 499 (100011 ms)
```

这类日志和工具调用格式、模型路由、`max_output_tokens` 参数错误、`413` 都不是同一个问题。它的关键特征是：

- HTTP 状态是 `499`。
- `elapsed_ms` 接近 `100000`。
- Go 错误通常表现为 `context canceled`。
- 发生在 VS/Copilot 真实流式请求里，尤其是大上下文、多工具声明或上游首 token 波动时。

### 15.2 根因判断

这是客户端等待窗口先耗尽：VS/Copilot 在约 100 秒仍没有得到可接受结果时取消请求。代理之前允许模型/profile 配置使用 `180`、`240`、`300` 秒超时，这对普通 HTTP 客户端可能合理，但对 VS/Copilot 没有意义：

- 代理还在等待上游时，VS/Copilot 已经断开。
- 最终日志只能记录 `client_deadline_reached` / `499`。
- 用户看到的是“卡很久后失败”，而不是明确的上游超时。
- new-api/sub2api 如果单渠道超时过长，也来不及在客户端窗口内完成渠道轮换。

因此，这不是简单把超时调大能解决的问题；对 VS/Copilot 场景，超时必须小于客户端等待上限。

### 15.3 本轮代码策略

新增可配置的 `defense.client_timeout_budget_seconds`，并将 `modelTimeoutSeconds` 的返回值裁剪到客户端安全预算内：

- 配置或模型 profile 小于当前客户端预算：保留用户更短的超时。
- 配置或模型 profile 大于当前客户端预算：有效请求超时裁剪到预算值，默认 90 秒。
- 默认 180 秒不再直接用于 VS/Copilot 上游请求。

这样做的目标不是“让慢上游变快”，而是避免代理继续等待到 VS/Copilot 必然取消的 100 秒，让失败在客户端断开前转为明确的 `timeout` 诊断。

### 15.4 为什么不是继续调大超时

调大超时只能让代理在客户端断开后继续消耗资源，不能让 VS/Copilot 接收结果。最佳实践是分层预算：

| 层级 | 建议预算 | 说明 |
| --- | --- | --- |
| new-api/sub2api 单渠道 | 20-30 秒 | 给内部渠道轮换留时间 |
| vs-ai-proxy 有效上游请求 | 默认 90 秒，可配置 15-95 秒 | 低于 VS/Copilot 约 100 秒等待上限 |
| VS/Copilot 客户端 | 约 100 秒 | 客户端行为，不由代理控制 |
| 服务端 WriteTimeout | 大于代理预算 | 避免服务端提前关闭正常流 |

如果 new-api 内部某个渠道首 token 接近或超过客户端等待窗口，代理无法把它变成可用体验；应在 new-api 内部缩短单渠道超时、启用健康度/优先级/失败切换，或者把该模型路由到更稳定的渠道组。

### 15.5 本轮验证

新增/调整测试：

- `TestModelTimeoutSecondsUsesSafeDefaultBudget`：默认有效超时为 90 秒，而不是 180 秒。
- `TestModelTimeoutSecondsCapsLongProfileBeforeClientDeadline`：300 秒 profile 会被裁剪到 90 秒。
- `TestModelTimeoutSecondsPreservesShortModelOverride`：25 秒模型级配置不会被放大。
- `TestLoggingMiddlewareCapturesToolDiagnosticsWithoutArguments`：日志记录 `stream_state`、`network_peer`，并确认不会泄露工具参数。

已执行验证：

```bash
go test ./internal/proxy -count=1
go test ./... -count=1
go test -race ./cmd/server ./internal/proxy ./internal/provider ./internal/config -count=1
bash tests/streaming_test.sh
bash tests/streaming_ollama_test.sh
bash -n tests/useai_large_request_diagnostic.sh
bash -n tests/large_request_matrix_diagnostic.sh
make build
./vs-ai-proxy --version
git diff --check
```

### 15.6 后续观测重点

上线后重点看这些字段：

| 字段/现象 | 期望变化 |
| --- | --- |
| `499` + `elapsed_ms≈100000` | 应显著减少 |
| `error_code=timeout` + `elapsed_ms≈90000` | 说明代理在客户端断开前主动结束慢上游 |
| `stream_state=upstream_connecting` | 上游连接/首 token 前超时，优先查 new-api 渠道首 token |
| `stream_state=upstream_connected` | 已连上但读取 chunk 慢或中途断，优先查上游流式稳定性/CDN |
| `network_peer=104.21.x.x` / `172.67.x.x` | Cloudflare/CDN 边缘节点，不是源站 IP |

### 15.7 注意事项

1. 不要把 `timeout_seconds` 盲目调到 180/300 秒来“解决” VS/Copilot 超时；这会重新制造 100 秒 499。
2. 不要在已经向下游写出流式 chunk 后自动重试；这会破坏 Copilot 流式协议，并可能重复工具调用。
3. 大请求、工具声明很多时，优先减少上下文或优化上游渠道组，而不是在代理层无限重试。
4. 如果确实需要更长请求，应确认客户端不是 VS/Copilot，或者未来增加明确的客户端类型/预算配置，而不是全局放开。

## 16. 客户端超时预算配置化与可观测性补充

### 16.1 为什么继续优化

仅把 VS/Copilot 安全预算写死为 90 秒虽然能避免多数 `499 (100000ms)`，但从顶层设计看仍有两个不足：

1. 不同客户端或用户环境可能存在不同等待上限，硬编码不利于灰度和排障。
2. 日志只有最终错误，不知道模型/profile 原始配置是多少、代理实际使用多少，容易误以为 `timeout_seconds=300` 没生效。

因此本轮把“客户端预算”升级为配置项和日志观测项。

### 16.2 新配置

`config.json` 新增：

```json
{
  "defense": {
    "enabled": true,
    "client_timeout_budget_seconds": 90
  }
}
```

约束：

| 配置项 | 默认 | 最小 | 最大 | 说明 |
| --- | --- | --- | --- | --- |
| `defense.client_timeout_budget_seconds` | 90 | 15 | 95 | VS/Copilot 客户端预算；更长的模型/profile 超时会被裁剪到该值 |

最大值限制为 95 秒，是为了避免重新逼近 VS/Copilot 约 100 秒客户端取消窗口。低于该值的模型级 `timeout_seconds` 仍会保留，不会被放大。

### 16.3 新日志字段

请求日志新增：

```json
{
  "configured_timeout_seconds": 300,
  "effective_timeout_seconds": 90
}
```

含义：

| 字段 | 说明 |
| --- | --- |
| `configured_timeout_seconds` | 模型配置、profile 或默认值计算出的原始上游超时 |
| `effective_timeout_seconds` | 实际传入 provider 请求 context 的有效超时，已考虑客户端预算裁剪 |

后台日志 tooltip 也会显示“有效超时 / 配置超时”，便于直接判断是否发生裁剪。

### 16.4 Web 后台

`/admin#/config` 增加“客户端超时预算（秒）”输入框：

- 默认 90。
- 建议 15-95。
- 保存后热更新，端口仍需重启。
- 该配置属于防御策略的一部分，但即使防御开关关闭，也不建议让 VS/Copilot 请求超过客户端等待窗口；否则问题会退化为 100 秒 499。

### 16.5 验证点

新增/强化测试：

- 旧配置自动补 `client_timeout_budget_seconds=90`。
- 过高预算裁剪到 95，过低预算裁剪到 15。
- `modelTimeoutSeconds` 返回 configured/effective 两个值。
- 300 秒 profile 在默认预算下实际为 90 秒。
- 25 秒模型配置仍保持 25 秒。
- 自定义预算 60 秒时，默认 180 秒会裁剪为 60 秒。
- 日志记录 `configured_timeout_seconds` 与 `effective_timeout_seconds`。

## 17. `client_timeout_budget_seconds=90` 与 499 的匹配关系决策

### 17.1 先明确 499 的含义

在本项目日志里，下面这种形态通常不是代理主动失败：

```text
POST /v1/chat/completions - 499 (100011 ms)
openai stream error: ... context canceled
```

它表示 VS/Copilot 客户端在接近自身等待上限时取消了请求。代理继续等待上游不会改变结果，因为客户端已经不再接收响应。

因此，`defense.client_timeout_budget_seconds` 的目标不是让慢上游变快，而是让代理在客户端取消前给出明确的 `timeout` 诊断，避免用户等到 `499`。

### 17.2 分层预算模型

行业最佳实践是分层 timeout budget，而不是所有层都设置为同一个值：

| 层级 | 推荐值 | 目标 |
| --- | --- | --- |
| new-api/sub2api 单渠道超时 | 20-30 秒 | 让上游网关有机会在客户端窗口内轮换渠道 |
| vs-ai-proxy 有效上游请求预算 | 默认 90 秒 | 在 VS/Copilot 约 100 秒取消前主动失败并记录诊断 |
| vs-ai-proxy 最大可配置预算 | 95 秒 | 给高延迟环境留少量空间，但避免贴近 100 秒临界点 |
| VS/Copilot 客户端等待上限 | 约 98-100 秒 | 客户端行为，代理无法控制 |
| HTTP server WriteTimeout | 大于代理预算 | 避免服务端先于代理预算关闭正常流 |

关键逻辑：

```text
new-api 单渠道 timeout < new-api 总体切换窗口 < proxy effective timeout < VS/Copilot client deadline
```

如果 proxy timeout 大于或等于 VS/Copilot deadline，最终会退化为 `499 client_deadline_reached`。如果 proxy timeout 明显小于 deadline，则能得到可解释的 `timeout`，日志也能显示 `configured_timeout_seconds` 与 `effective_timeout_seconds`。

### 17.3 为什么默认选 90 秒

默认值经历过三个候选：

| 候选 | 优点 | 问题 | 结论 |
| --- | --- | --- | --- |
| 85 秒 | 距离 100 秒更安全，几乎不会撞客户端 deadline | 对本来 86-90 秒可成功的慢响应过早失败，用户体感偏保守 | 可作为网络差环境的手动配置，不适合默认 |
| 90 秒 | 距离 100 秒仍有约 8-10 秒安全余量，同时减少过早失败 | 极差网络或客户端更短 deadline 时仍可能接近边界 | 当前默认最优折中 |
| 95 秒 | 最大化等待上游返回 | 安全余量过小，遇到网络抖动、调度延迟、日志 flush、上游最后一跳慢时容易重新变 499 | 仅作为高级可配上限，不作为默认 |
| 100 秒及以上 | 看似给上游更多时间 | 与 VS/Copilot 客户端 deadline 冲突，代理无法把结果交给已取消的客户端 | 禁止作为默认，不建议配置 |

因此，默认 `90` 是为了在“成功机会”和“避免 499”之间取平衡；`95` 是最大上限，不是推荐值。

### 17.4 为什么范围是 15-95 秒

- **最小 15 秒**：避免用户误填 1-5 秒导致所有请求大量误超时。真实模型首 token、工具请求、大上下文请求不应被过短预算杀死。
- **默认 90 秒**：贴合 VS/Copilot 约 100 秒等待窗口，保留约 8-10 秒安全余量。
- **最大 95 秒**：允许特殊网络/模型场景略微放宽，但仍低于客户端取消窗口。

不允许超过 95 秒，是为了防止用户把模型/profile 的 `180`、`240`、`300` 秒重新带回 VS/Copilot 路径，制造 `499 (100000ms)`。

### 17.5 与日志字段的关系

新日志字段用于判断预算是否按预期生效：

```json
{
  "configured_timeout_seconds": 300,
  "effective_timeout_seconds": 90,
  "error_code": "timeout",
  "elapsed_ms": 90000
}
```

判断规则：

| 日志形态 | 含义 | 处理建议 |
| --- | --- | --- |
| `499` + `elapsed_ms≈98000-100000` | 客户端先取消，proxy 没来得及主动结束 | 检查预算是否被设置过高、是否出现客户端更短 deadline |
| `timeout` + `elapsed_ms≈effective_timeout_seconds*1000` | proxy 在客户端取消前主动结束慢上游 | 检查上游首 token、new-api 渠道超时/轮换策略 |
| `configured_timeout_seconds > effective_timeout_seconds` | 发生预算裁剪 | 正常，说明模型/profile 原始超时被客户端预算保护 |
| `effective_timeout_seconds < configured_timeout_seconds` 且请求仍 499 | 客户端 deadline 可能小于预期，或下游连接提前断开 | 可把预算临时下调到 80-85 并观察 |
| 小请求成功、大请求 timeout/499 | 大上下文、工具声明或文件内容导致上游慢 | 优化上下文、减少附件/历史、切换更稳渠道 |

### 17.6 推荐配置

默认推荐：

```json
{
  "defense": {
    "enabled": true,
    "client_timeout_budget_seconds": 90
  }
}
```

不同场景建议：

| 场景 | 建议值 | 原因 |
| --- | --- | --- |
| 普通 VS/Copilot Windows 用户 | 90 | 默认最佳折中 |
| 网络质量差、经常 499 | 80-85 | 提前失败，避免撞客户端 deadline |
| 上游较慢但偶尔 90 秒内能成功 | 90 | 保持默认，不要过早失败 |
| 明确知道客户端等待窗口更长 | 95 | 只能作为高级调优，仍不建议超过 95 |
| 想解决慢上游 | 不建议调高 proxy 预算 | 应调整 new-api/sub2api 单渠道超时和渠道健康策略 |

### 17.7 最终建议

当前最佳实践配置是：

- 默认值：`90`
- 可配置范围：`15-95`
- 常规用户不需要修改。
- 如果仍出现 `499≈100s`，优先把预算降到 `85` 观察，而不是升高。
- 如果出现 `timeout≈90s`，说明代理已正确工作；下一步应治理上游渠道首 token、排队、单渠道超时和上下文大小。

这套方案的核心价值是把“客户端无意义取消”转化为“代理可解释超时”，并通过日志告诉用户：原始模型超时是多少、实际执行预算是多少、问题发生在哪一层。
