# VS Copilot 稳定化顶层设计与 v0.2.37 推进方案（2026-07-10）

## 背景

`v0.2.16` 是当前用户使用最多的版本。从 `v0.2.16` 之后，项目围绕 Visual Studio / Copilot BYOM 真实场景连续增强了 provider/model 绑定、测试页模型下拉、OpenAI-compatible 网关兼容、GPT/GLM/DeepSeek 工具调用、DSML 方言、流式 tool_calls、token 参数归一化、超时和 fallback。

这些能力本身有价值，但它们被逐步叠加到主链路中，部分默认策略过于激进，导致出现以下现象：

- 工具调用被代理误拦，表现为 `无法运行 create_file`、PowerShell/Git 工具调用失败。
- 流式工具调用的 name/arguments 分片被误判为 `<empty>` 或未声明工具。
- 长请求被 VS/Copilot 客户端取消，代理日志仍可能显示 200，排障被误导。
- `413` 请求体过大被误判为普通上游错误，甚至继续 fallback。
- 跨 provider fallback 掩盖了当前 provider 的真实错误。

因此，不能继续局部打补丁，也不能简单回滚到 `v0.2.16`。正确方案是：保留已有有效资产，重新定义主链路默认策略，把高风险能力变成可控能力。

## 目标

v0.2.37 的目标不是继续增加新功能，而是恢复并稳定 Visual Studio / Copilot 默认可用性：

1. 默认不误拦工具调用。
2. 默认不跨 provider 隐式 fallback。
3. 默认准确记录客户端取消、请求过大、上游失败。
4. 保留已有协议兼容能力，例如 SSE 转 JSON、finish_reason 归一化、token 参数治理、DSML 解析。
5. 让增强能力可配置、可观测、可回退。

## 总原则

- 默认先保证用户能用。
- 工具调用链路最高优先级是保留语义、保持协议、避免误杀。
- 代理只做协议兼容和诊断，不默认做安全裁判。
- 所有会产生第二次上游请求的行为必须可见、可控。
- 400/401/403/404/413/429 这类明确错误不应被盲目 fallback 掩盖。
- `context canceled` 首先按客户端取消处理，不应当作 provider 普通失败。

## 已有价值资产

以下能力应保留：

- `stream=false` 返回 SSE 的兼容。
- `finish_reason` 归一化，适配 VS OpenAI .NET SDK 严格枚举。
- `max_output_tokens` / `max_completion_tokens` 到 `max_tokens` 的安全归一化。
- DSML 方言识别和转换。
- 流式 tool_calls 参数分片合并。
- legacy `function_call` 兼容。
- `strict`、`tool_choice`、`parallel_tool_calls`、工具扩展字段保留。
- 测试页按 provider 官方 `/models` 构建模型下拉。
- provider/model 绑定防混淆。
- `Retry-After` 冷却。
- 防御模式和 provider 健康排序，但默认不能过度干预。

## 必须调整的高风险能力

### 1. 默认拦截未声明工具

这是最危险的回归点。VS/Copilot 工具声明格式可能与代理解析不完全一致。如果 allowed tools 为空，不能默认认为客户端没有声明工具。

调整后：

- 默认工具策略为 `warn` 或 `passthrough`。
- 未识别或未声明工具默认透传，只记录诊断。
- 只有高级用户显式开启 `strict` 时才阻断。
- 禁止把 `Proxy blocked undeclared tool calls` 这类文本写入 assistant content 作为默认行为。

### 2. `<empty>` 工具名误判

OpenAI 流式 tool_calls 可能把 name 和 arguments 拆在不同 chunk。某个 chunk 没有 name 不代表工具非法。

调整后：

- name 为空但有 arguments 的 chunk 默认透传。
- 按 choice index + tool index 跟踪状态。
- 只有明确给出非法 name 且策略为 strict 时才阻断。

### 3. 跨 provider fallback

跨 provider fallback 可能把 `useai2` 的真实错误掩盖成其他 provider 的成功或失败。

调整后：

- 默认不跨 provider fallback。
- 明确模型绑定 provider 时，只走绑定 provider。
- 只有用户显式开启时才允许跨 provider。

### 4. 413 请求过大

413 是请求体或上下文过大，不是普通 5xx。继续 fallback 往往只会拖慢并导致客户端取消。

调整后：

- 413 分类为 `upstream_payload_too_large`。
- 默认不 retry、不 fallback。
- 日志提示减少历史上下文、附件、文件内容或切换大上下文 provider。

### 5. 客户端取消日志

流式请求一旦写出响应头，HTTP 状态会是 200。但如果后续 VS/Copilot 取消，不能在日志里继续记为成功。

调整后：

- 客户端取消记录为 `499 client_gone`。
- 不再把 `context canceled` 当作 provider 健康失败。
- 客户端已取消后，不再 retry 或 fallback。

## 推荐默认模式：VS Stable

新增或内化一个默认兼容模式：`vs_stable`。

默认行为：

- 工具调用：`warn` 或 `passthrough`，不默认阻断。
- DSML：仅当无标准 `tool_calls/function_call` 时启用。
- token 参数：安全归一化。
- finish_reason：归一化。
- SSE-on-nonstream：转换。
- provider fallback：不跨 provider。
- 400/401/403/404/413/429：不 fallback。
- 5xx/EOF/连接重置：同 provider 短重试。
- 超时：尊重客户端取消。
- 日志：记录工具摘要、fallback 摘要、请求大小摘要。

## 推荐配置方向

长期配置可演进为：

```json
{
  "compatibility_mode": "vs_stable",
  "tool_policy": "warn",
  "defense": {
    "stable_user_agent": true,
    "retry_5xx": true,
    "stream_nonstream_fallback": true,
    "cross_provider_fallback": false,
    "respect_retry_after": true
  },
  "timeouts": {
    "connect_seconds": 30,
    "first_byte_seconds": 60,
    "stream_idle_seconds": 60,
    "total_seconds": 0
  },
  "diagnostics": {
    "log_request_size": true,
    "log_tool_summary": true,
    "log_fallback_attempts": true
  }
}
```

v0.2.37 不一定一次实现所有配置项，但默认行为必须按该方向收敛。

## v0.2.37 第一刀范围

第一刀只做稳定化，不加新功能：

1. 工具调用默认透传或 warn，不默认阻断。
2. 流式 `<empty>` 工具片段不误杀。
3. 413 不 retry、不 fallback，并给出明确诊断。
4. 客户端取消记录为 `499 client_gone`。
5. 客户端取消不计入 provider 失败，不触发 provider fallback。
6. 防御关闭或默认稳定模式下，不跨 provider fallback。

## 验收标准

必须满足：

- `create_file` 不再被代理误拦。
- PowerShell/Git 工具调用不再被 `<empty>` 误杀。
- 标准 `tool_calls` 能透传。
- legacy `function_call` 能透传。
- 流式 tool_calls 分片能透传。
- DSML 能在无标准工具调用时转成标准 `tool_calls`。
- `gpt-5.5` 慢或取消时日志准确显示 `client_gone`，不是假 200。
- `deepseek-v4-flash` 413 时明确提示请求体过大，不盲目 fallback。
- 默认不会从已绑定 provider 悄悄 fallback 到其他 provider。

## 发布门禁

发布前必须运行：

```bash
go test ./... -count=1
go test -race ./cmd/server ./internal/proxy ./internal/provider ./internal/config -count=1
bash tests/streaming_test.sh
bash tests/streaming_ollama_test.sh
GOOS=windows GOARCH=amd64 go build -o .bin/vs-ai-proxy-windows-amd64-test.exe ./cmd/server
go build -o .bin/vs-ai-proxy-local-test ./cmd/server
git diff --check
```

还应进行 Windows + Visual Studio/Copilot 实机冒烟：

- 创建文件。
- PowerShell 命令。
- Git 命令。
- 读取文件。
- 标准流式输出。
- 非流式测试。
- `gpt-5.5`。
- `deepseek-v4-flash`。

## 后续原则

- 不再一天内频繁发多个补丁版本。
- 每个默认策略变化都必须有 fixture/replay 测试。
- 任何可能产生第二次上游请求的能力，都必须说明触发条件和日志字段。
- 任何工具调用拦截逻辑都必须证明不会误拦 VS/Copilot 合法工具。
- 协议修复默认开启；安全拦截默认关闭或 warn；跨 provider fallback 默认关闭。
