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
