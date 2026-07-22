# MiMo provider 参数方言与 VS Copilot 兼容踩坑记录（2026-07-18）

本文记录 `xiaomimimo / mimo-v2.5` 在 Visual Studio Copilot BYOM 场景中的一次排查结论。目标是把事实、排除项和修复边界写清楚，避免后续把它误归类为 Kimi 同类问题，或继续在 handler 层无止境打补丁。

## 1. 现场现象

用户在本地配置 `/Users/dingyuwang/.config/vs-ai-proxy/config.json` 中启用了：

```text
provider_id: xiaomimimo
type: openai
base_url: https://api.xiaomimimo.com/v1
model: mimo-v2.5
```

现场日志：

```text
[15:36:29 INFO] POST /v1/chat/completions - 200 (11220 ms)
[15:36:39 WARN] 模型 mimo-v2.5（xiaomimimo）失败: 上游响应无法识别
[15:36:39 INFO] POST /v1/chat/completions - 200 (8422 ms)
```

`上游响应无法识别` 对应本项目诊断码 `proxy_parse_error`，通常表示上游 HTTP 状态可能是 200，但响应体或响应契约不满足代理给 VS/Copilot 做的 OpenAI-compatible 校验。

## 2. 已验证事实

### 2.1 不是 URL 路径问题

本地配置的 `base_url` 是：

```text
https://api.xiaomimimo.com/v1
```

项目的 `joinURLPath()` 会去重重叠 path：

```text
base_url /v1 + fallback path v1/chat/completions
=> https://api.xiaomimimo.com/v1/chat/completions
```

因此 MiMo 这次不是智谱曾经遇到的 `/v4/v1/chat/completions` 类路径拼接问题。

### 2.2 MiMo 的 SSE 基本符合 OpenAI-compatible 形态

真实流式请求 `stream=true + max_completion_tokens=32` 返回标准 `data: ...` 事件，并以：

```text
data: [DONE]
```

结束。中间存在一个 `choices: []` 且只携带 `usage` 的 chunk。现有 `CollectOpenAIChatSSE()` 会忽略空 choices，并保留 usage；这不是本次主要根因。

### 2.3 MiMo 能返回标准 tool_calls

真实工具调用请求返回：

```json
{
  "finish_reason": "tool_calls",
  "message": {
    "content": "",
    "role": "assistant",
    "tool_calls": [
      {
        "id": "call_...",
        "type": "function",
        "function": {
          "name": "get_weather",
          "arguments": "{\"city\": \"上海\"}"
        }
      }
    ],
    "reasoning_content": "..."
  }
}
```

这说明 `mimo-v2.5` 至少在基础工具调用响应结构上可用，不应简单标记为“不支持工具”。

### 2.4 MiMo 不像 Kimi 那样严格拒绝空 assistant 历史

实测两个反例：

1. assistant tool_calls 历史去掉 `reasoning_content`，MiMo 仍返回 HTTP 200。
2. 普通 assistant 历史 `content:""`，MiMo 仍返回 HTTP 200。

因此不能把 MiMo 的失败直接等同于 Kimi 的：

```text
the message at position ... with role 'assistant' must not be empty
```

Kimi 的核心问题是“会话历史消息合法性更严格”；MiMo 当前第一根因不是这个。

## 3. 根因

MiMo 的关键特殊性是：它虽然提供 OpenAI-compatible `/chat/completions` 接口，但输出 token 预算字段应优先使用：

```text
max_completion_tokens
```

而不是本项目原来对所有 OpenAI-compatible provider 统一收敛的：

```text
max_tokens
```

真实对比：

| 请求字段 | 值 | 结果 |
| --- | ---: | --- |
| `max_tokens` | 32 | HTTP 200，但 `finish_reason=length`，`content=""`，预算几乎耗在 `reasoning_content` |
| `max_completion_tokens` | 32 | HTTP 200，`finish_reason=stop`，`content="OK"` |

这说明在 MiMo 上，`max_tokens` 与 `max_completion_tokens` 不是可随意互换的别名。原先 `normalizeOpenAIChatCompletionsRequestBody()` 把 `max_completion_tokens` / `max_output_tokens` 统一改写成 `max_tokens`，对多数 OpenAI-compatible 网关是稳妥的，但会破坏 MiMo 的实际行为。

## 4. 为什么会表现成“上游响应无法识别”

MiMo 默认会输出 `reasoning_content`，并且推理 token 会占用输出预算。

当代理把用户或客户端的输出预算改写成 `max_tokens` 后，小预算请求可能先耗尽在推理阶段，产生：

```json
{
  "finish_reason": "length",
  "message": {
    "content": "",
    "reasoning_content": "..."
  }
}
```

如果后续请求中客户端没有完整保留 `reasoning_content`，或某些上游返回了截断的工具参数 / 非完整 chunk，代理的严格契约校验就可能把它归为 `proxy_parse_error`。这个错误文案本身是准确的，但旧文案不够清楚，没有告诉用户“可能是 provider 参数方言导致输出预算被错误消耗”。

### 4.1 它不是网络错误的典型形态

现场同时出现的错误大致分两类：

| 日志 reason | 主要含义 | 常见方向 |
| --- | --- | --- |
| `上游响应无法识别` | 已经拿到上游响应，但响应体、SSE 事件、工具参数或终态契约没有通过代理校验 | 参数方言、输出预算耗尽、截断、非标准 JSON/SSE、工具调用不完整 |
| `无法连接上游` | 尚未成功建立可用上游响应，或连接/等待响应阶段失败 | DNS、TLS、网络、网关、WAF、超时、上游不可达 |

所以 `mimo-v2.5（xiaomimimo）失败: 上游响应无法识别` 不能优先按网络问题处理。它更像“上游返回了东西，但这份东西不能安全转交给 VS/Copilot”。

### 4.2 为什么 MiMo 会话中可能看到 step-router 错误

如果控制台同时出现：

```text
模型 mimo-v2.5（xiaomimimo）失败: 上游响应无法识别
模型 step-router-v1（useai）失败: 无法连接上游
```

不能只凭肉眼把它理解为“同一个请求明明固定 MiMo，却又偷偷切到了 step-router”。需要先看同一行或相邻日志里的 `request_id`、`provider`、`requested_model`、`upstream` 和 `attempts`。

可能情况：

1. 同一时间 Visual Studio/Copilot 发起了多个并发请求，其中一部分仍使用默认模型或旧会话模型。
2. 请求模型没有强绑定 provider，registry 按候选优先级尝试了其它 provider。
3. 前端或客户端展示的“当前模型”与实际请求体里的 `model` 不一致。
4. 旧版控制台成功日志只显示 `POST /v1/chat/completions - 200`，没有 provider/model，上下文不足，容易把不相干请求串在一起。

因此本次日志侧修复要求：

- 成功和失败的控制台请求日志都带 `request_id`、`provider`、`requested_model`、`upstream`。
- provider attempt warning 也带同一组路由字段。
- 排查时以 `request_id` 为主键，不再只按时间相邻推断。

### 4.3 为什么会出现 WARN 后同一 request_id 最终 200

例如：

```text
[21:45:40 WARN] 模型 mimo-v2.5-pro（xiaomimimo）候选尝试失败: request_id=... reason=上游响应格式不兼容
[21:45:40 INFO] POST /v1/chat/completions - 200 (...) request_id=... provider=xiaomimimo requested_model="xiaomimimo - mimo-v2.5-pro" upstream=mimo-v2.5-pro
```

这类日志不能直接理解为“客户端收到失败”。proxy 里有两个层次：

1. `WARN 模型 ... 候选尝试失败`：单个候选 provider 或备用聊天模式的一次 attempt 失败。
2. `INFO POST ... - 200`：同一个客户端请求最终已经成功返回。

因此排查顺序应为：

1. 先按 `request_id` 聚合日志。
2. 如果同一 `request_id` 最终是 `200`，优先判断为一次可恢复 attempt 失败，观察是否影响 VS 实际输出。
3. 如果最终是 `4xx/5xx`，再按 `error_code`、`reason`、`attempts` 判断是网络、鉴权、额度、参数还是响应契约问题。
4. 如果频繁出现 `proxy_parse_error` 但最终仍 200，说明备用路径正在兜底，后续应收集上游原始 DEBUG 日志评估是否需要补 provider capability，而不是在 handler 中按模型名继续打补丁。

## 5. 修复方案

本次采用 provider capability 方案，而不是在 handler 里写 `if model == mimo-v2.5`：

1. `ProviderCapabilities` 增加 `OutputTokenParam`。
2. 默认 provider 继续使用 `max_tokens`，保持既有兼容。
3. 新增 `xiaomimimo` 能力档案：
   - `ChatPath: v1/chat/completions`
   - `ModelsPath: v1/models`
   - `OutputTokenParam: max_completion_tokens`
4. `OpenAIProvider` 序列化请求时按 provider capability 选择输出预算字段。
5. `/api/providers` 的 `compatibility_profile` 暴露 `output_token_param`，管理页 title 只读展示。

这种做法符合项目现有原则：

- 路径差异放在 provider capability。
- 参数差异也放在 provider capability。
- handler 只管路由和诊断，不散落单模型特判。
- 未注册 provider 默认行为不变。

## 6. 顶层设计判断

这次修复不是继续在 handler 层打补丁，但它暴露了一个必须长期坚持的设计边界：

> OpenAI-compatible 只能说明“协议入口形似 OpenAI”，不能说明“所有参数语义等价 OpenAI”。

因此系统应该把 provider 差异分成三层处理：

| 层级 | 事实源 | 职责 |
| --- | --- | --- |
| provider capability | `internal/provider/capabilities.go` | path、API format、输出 token 参数名、reasoning/top_k 等 provider 级方言 |
| model profile | `internal/provider/model_catalog.go` 与 `model-selection/*.json` | 上下文长度、输出上限、模型级 execution 参数和能力 |
| proxy handler | `internal/proxy` | 路由、fallback、诊断、VS/Copilot 协议兼容；不得承担单 provider 参数知识 |

用 Module 深度判断：

- `ProviderCapabilities` 的 Interface 很小，但隐藏了 path、参数方言和展示兼容档案，具备 Leverage。
- 如果删除 `OutputTokenParam`，MiMo 规则会重新散落到请求序列化、handler、API/Web 展示和测试中，Locality 变差。
- `OpenAIProvider` 只消费能力档案，不知道 MiMo 的业务故事；这是正确的 seam。
- API/Web 只展示 `compatibility_profile`，不重新推断方言；避免多个事实源漂移。

后续新增类似 provider 时，应按同一模式补 capability 字段或 model profile 字段。只有当差异无法表达为 provider/model 档案时，才考虑引入新的 Adapter；不要直接在 `/v1/chat/completions` handler 里追加模型名判断。

## 7. 风险评估

| 影响面 | 结论 |
| --- | --- |
| OpenAI / DeepSeek / Kimi / UseAI 等既有 provider | 默认仍输出 `max_tokens`，不改变行为 |
| 自定义 OpenAI-compatible provider | 未识别时仍走 `custom + max_tokens` |
| `xiaomimimo` | 输出预算字段改为 `max_completion_tokens` |
| `/api/providers` | 只增加只读字段 `output_token_param`，不改变配置结构 |
| Web 管理页 | 只在兼容档案 title 增加说明，不新增编辑项 |

## 8. 验证记录

真实上游最小验证：

1. `mimo-v2.5 + max_tokens=32`：
   - HTTP 200
   - `finish_reason=length`
   - `content=""`
   - `reasoning_tokens=31`
2. `mimo-v2.5 + max_completion_tokens=32`：
   - HTTP 200
   - `finish_reason=stop`
   - `content="OK"`
3. `mimo-v2.5 + stream=true + max_completion_tokens=32`：
   - 返回标准 SSE
   - 以 `[DONE]` 结束
   - 尾部 usage chunk `choices=[]` 可由现有解析器处理
4. `mimo-v2.5 + tools`：
   - 返回标准 `tool_calls`
   - `finish_reason=tool_calls`
5. MiMo 空 assistant 历史反例：
   - 普通空 assistant 历史返回 200
   - 去掉 `reasoning_content` 的 tool_calls 历史也返回 200

本地回归测试：

```text
go test ./internal/provider -count=1
go test ./internal/api -run 'Test.*Provider' -count=1
go test ./web -count=1
```

## 9. 后续注意事项

1. 不要再把所有 OpenAI-compatible provider 的输出 token 字段硬编码成 `max_tokens`。
2. 新 provider 如果官方文档或实测表明 token 字段语义不同，应补 provider capability，而不是改 handler。
3. `reasoning_content` 是 MiMo 正常响应的一部分，不应在 raw passthrough 或 SSE 聚合中丢失。
4. `content="" + reasoning_content 非空` 不是空响应；但如果客户端后续历史只保留了空 `content`，可能引发多轮质量或兼容问题。
5. `proxy_parse_error` 文案可继续优化，但不要把参数方言、截断、工具契约失败都混成“上游坏了”。
6. 字段方言修好之后，仍可能出现「budget 绝对值不够 reasoning + tool_calls」：HTTP 200、`finish_reason=length`、无结构化 `tool_calls`，客户端误报为 git/工具失败。完整 live 复现矩阵与全局对策见 `37_MiMo_git工具调用失败根因分析与对策_20260722.md`（该文归档时未改代码）。
