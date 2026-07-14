# Visual Studio Copilot 通用工具调用契约与矩阵测试（2026-07-14）

## 1. 背景

项目过去围绕 `create_file`、`get_file`、`powershell` 等具体工具反复修复，虽然每次都能解决一个已知样本，但不能证明 Visual Studio Copilot 当前和未来声明的其他工具同样可用。工具调用的真正公共边界不是工具名，而是：

```text
客户端声明和历史
  -> 代理请求解析/克隆/参数治理
  -> OpenAI/Ollama 上游
  -> JSON/SSE/legacy/DSML 响应归一化
  -> Visual Studio 可执行 tool_calls/function_call
  -> 后续 tool message 按 call id 回传
```

本次目标是把这条链定义成可验证的协议契约。生产实现对任意合法声明工具使用同一条链；常见工具名只用于测试覆盖、可选安全别名和诊断展示，不能成为主流程分支。

## 2. 通用契约

### 2.1 请求契约

代理转发到上游时必须保留：

- 任意合法工具名，不要求在内置目录中登记。
- `tools[].type/function/name/description/parameters`。
- `strict`、provider options 等工具和 function 扩展字段。
- 顶层 `tool_choice`、`parallel_tool_calls` 和其他未知字段。
- 历史 `assistant.tool_calls`、legacy `function_call`。
- `tool_call_id` 和 tool role 结果消息。
- multimodal content、provider state 等嵌套扩展字段。

代理可以治理已知不兼容参数，但不得因为无法识别工具名而删除工具或历史。

工具请求未提供 `temperature`/`max_tokens` 时，代理不再凭空注入全局值。只有客户端、用户模型配置或匹配的正式 profile 明确提供时才发送这些参数，避免 4096 输出上限截断文件工具参数，也让大模型 profile 的更大上限真正生效。普通聊天保留原默认，控制本次兼容改动范围。

### 2.2 modern 响应契约

每个 OpenAI `tool_calls[]` 在提交给 Visual Studio 前必须满足：

```text
id            非空且在当前响应中稳定
type          精确为 function
function.name 非空；优先匹配请求声明的精确拼写
arguments     包含合法 JSON 的字符串
finish_reason tool_calls
```

部分 Ollama/OpenAI-compatible 上游省略 `id/type`。代理会补充 `call_proxy_*` ID 和 `type=function`，使后续 tool message 可以关联；已有合法 ID 保持不变。

缺失 type 或大小写变体可以安全归一化为 `function`；其他非标准 type 不能用内部默认值掩盖，必须在 provider/direct SSE/Ollama 边界明确失败。

### 2.3 legacy 响应契约

legacy `function_call` 不要求 tool call ID，但必须保留完整 `name/arguments`，并使用 `finish_reason=function_call`。流式和非流式 fallback 都必须保真，不能只返回结束原因而遗漏调用正文。

### 2.4 流式契约

- SSE 按逻辑 event 解析，不按物理行猜 JSON。
- `name`、`id` 在代理聚合边界按 delta/累计 identity 语义合并；`arguments` 按标准 delta 追加并在终态校验为完整 JSON，不能把累计参数重复追加。
- 面向 Visual Studio 的压力测试使用标准形态：首个 chunk 给出完整 `id`，后续 chunk 省略 `id`；provider 聚合器另有 ID 分片兼容测试，不把 VS SDK 是否拼接 ID 当成已证明事实。
- 多个并行工具按 `choice index + tool index` 隔离状态。
- 工具尾部先缓存，完整性和终态验证通过后再提交。
- OpenAI/Ollama 双向流转换复用同一逻辑 SSE 事件处理器，不能按物理行直接把工具块写给下游。
- `[DONE]` 只在响应契约验证通过后写给客户端。
- 空流、无终态 EOF、错误 event、残缺 JSON、残缺工具参数不得伪装为成功。
- 单个 SSE event 与完整工具尾部统一使用 64 MiB 上限；Scanner 按需扩容，不为普通请求预分配大内存。

### 2.5 截断契约

`length/content_filter` 是跨协议不变量：无论工具来自 modern、legacy、DSML 还是 Ollama，都不得向客户端暴露可执行调用。代理不能猜测被截断的剩余参数。

### 2.6 别名边界

主路径优先原样保留上游返回的任意工具名。只有模型返回已知安全别名、且当前请求实际声明了目标工具时，才允许映射到声明的精确名称。

别名不是通用支持的基础，也不能凭空注册工具。文件/搜索工具不得降级成 shell；参数 schema 不确定时不得发明字段。

## 3. 本次发现的系统性问题

### 3.1 缺少 OpenAI envelope

raw JSON 和 Ollama 流可能只有 `function`，没有 `id/type`。旧代码会返回 HTTP 200，但 Visual Studio 无法可靠关联后续 tool result。

### 3.2 identity 分片被当成冲突

旧 SSE collector 允许 arguments 分片，却把 name/id 的第二个非空 delta 当成冲突。任意工具名一旦被拆成 `mcp_workspace_` + `symbol` 就会失败。

### 3.3 direct SSE 内部状态仍按物理行解析

转发器已经支持标准多行 SSE event，但诊断 accumulator 把归一化结果再次按物理行拆开，导致合法多行 JSON 被误判成空响应。

### 3.4 空响应和无终态 EOF 被记成成功

只有 role + `[DONE]`、或者有部分内容但没有 finish/[DONE] 的断流，旧路径仍返回成功。客户端最终只表现为“没有工具调用”或无响应，掩盖了上游协议错误。

### 3.5 单事件上限不一致

工具尾部总上限是 64 MiB，但 direct Scanner 单行只有 4 MiB。较大的 `create_file`/代码生成 arguments 会在进入正式校验前失败。

### 3.6 Ollama 成功终态提交过早

Ollama 转 OpenAI 时，旧路径收到 `done=true` 就立即写出 OpenAI `finish_reason`，循环结束后再无条件补 `[DONE]`。只有空终块、或者有部分内容但没有 `done=true/[DONE]` 的 EOF，也可能因此被客户端看成正常完成。

此外，raw JSON 若异常地同时携带 modern `tool_calls` 和 legacy `function_call`，旧截断分支只删除 modern 调用。`length/content_filter` 下仍可能残留 legacy 可执行调用。

### 3.7 双向流转换绕过逻辑事件门禁

Ollama 转 OpenAI 原先会立即写出非终态工具块，直到 `done=true` 后才检查流是否结束；后续 `length/content_filter` 或残缺参数无法撤回已经暴露的工具。OpenAI 转 Ollama 则按 Scanner 物理行直接解析，sanitizer 在 `[DONE]` 前补出的 synthetic finish 与标准多行 SSE 会被误当成一段 JSON。

同一次 choice 若跨事件混用 modern `tool_calls` 和 legacy `function_call`，旧状态机也没有拒绝，可能造成参数合并歧义。typed fallback 只检查工具字段，不检查空 `stop` payload，仍可合成 role + stop + `[DONE]` 的空成功。

## 4. 解决方案

1. 在共享 raw/typed normalizer 中统一补齐并验证 modern tool envelope。
2. provider collector 与 proxy direct stream 使用同一 identity delta 合并规则：空值设置、重复值忽略、累计值替换、纯 delta 追加。
3. Ollama 流在转换为 OpenAI SSE 前维护稳定的 tool index -> call ID 映射。
4. raw JSON 必须有非空 choices、message、合法终态和实际文本/推理/工具；明确的 `length/content_filter` 空结果除外。
5. direct SSE 把 `[DONE]` 延迟到完整响应验证后提交，拒绝空成功和无终态 EOF。
6. Ollama 转 OpenAI、OpenAI 转 Ollama 都先缓存逻辑终态和工具尾部，校验内容、工具和终态后再写 `finish_reason/[DONE]` 或 `done=true`；禁止无终态 EOF 生成成功终态。
7. `length/content_filter` 同时清除 modern 与 legacy 调用，异常混合响应也不能绕过截断规则。
8. 将 Scanner 单事件上限与 64 MiB 工具尾部总上限统一。
9. 对声明 modern `tools` 或 legacy `functions` 的请求跳过全局 `temperature=0.7`、`max_tokens=4096` 猜测默认；普通聊天保持不变。
10. raw、typed、direct SSE 统一拒绝同一 choice 混用 modern/legacy；typed fallback 同样拒绝空 `stop`，但保留明确空 `length/content_filter` 终态。

没有新增外部依赖，没有改变 provider 路由、90 秒客户端预算或跨 provider 重试策略。

## 5. 正式矩阵测试

新增 `internal/proxy/copilot_tool_contract_matrix_test.go`，不是临时脚本。

### 5.1 工具名维度

同一套断言覆盖：

- 文件创建、编辑、补丁、读取、搜索、列表、删除。
- terminal、PowerShell、Git。
- symbol、plan、question、memory。
- 不在内置目录中的 `mcp_workspace_symbol_42` 等任意扩展工具。

该名称表只存在于测试，生产协议代码不读取它。

### 5.2 协议维度

| 场景 | 核心断言 |
| --- | --- |
| non-stream raw JSON | 任意名称、schema、history、tool_choice、扩展字段保真 |
| 缺失/非法 id/type | 缺失值补齐；未知 type 明确失败，不能让内部校验与下游输出分叉 |
| direct SSE | 缺失 id/type 修复；name/arguments 分片后完全一致 |
| parallel tools | 多 index 交错分片不串线 |
| randomized | 固定种子 100 轮，每轮 3 个任意并行工具 |
| large event | 5 MiB 单事件可通过且参数字节完全一致 |
| Ollama stream | object arguments 转 JSON string，补齐 id/type，空流和无终态 EOF 不提交成功终态 |
| raw terminal | 空 choices/空 stop 拒绝；混合截断删除两种调用，非截断混用拒绝 |
| direct terminal | 空流、无终态 EOF、错误 event 不得发送成功终态 |
| bidirectional adapters | 多行 SSE、synthetic finish、截断/残缺工具、空 typed fallback 不得绕过逻辑事件门禁 |
| provider collector | modern/legacy name 分片和兼容性 ID 分片可聚合 |

### 5.3 客户端可执行性断言

测试不再只搜索响应字符串，而是按 Visual Studio 客户端需要的结构重新解析 SSE，合并调用，并逐项验证：

- call 数量和顺序；
- 非空 ID；
- `type=function`；
- 精确工具名；
- arguments 字节一致且是合法 JSON；
- modern/legacy 对应的 finish reason。

## 6. 发布门禁

`make tool-check` 必须包含：

```bash
go test ./internal/provider ./internal/converter -count=1 \
  -run 'TestToolProtocolContract|Test.*Tool|Test.*FunctionCall|TestCollectOpenAIChatSSEAggregatesFragmented'

go test ./internal/proxy -count=1 \
  -run 'TestCopilot|TestToolProtocolContract|TestVisualStudioToolExecutionE2E|...'
```

完整发布仍需：

```bash
make release-check
```

并显式构建 Windows x64、macOS x64、macOS arm64。项目不使用 Docker，不能以容器测试替代桌面产物。

## 7. 排查建议

升级后遇到工具问题，先按协议阶段判断：

1. `request_tools` 没有目标工具：检查客户端本次请求声明，不改代理别名。
2. `upstream_connecting` 超时：尚未收到上游响应，检查上下文和渠道首响应。
3. `upstream_connected` 后协议错误：保存脱敏 SSE，正式 fixture 应进入矩阵测试。
4. `response_tools` 有目标工具但 VS 不执行：检查 ID/type/name/arguments/finish reason 和客户端 schema 错误。
5. HTTP 200 但没有 content/reasoning/tools：新版本应拒绝；如果仍出现，先核对运行版本。

## 8. 剩余边界

- 代理能保证中转协议保真，不能迫使模型选择工具，也不能补造缺失的必填业务参数。
- 上游在 90 秒内没有返回 HTTP 响应头时，没有响应可供归一化。
- DSML 是非标准方言，仍采用 8 KiB 前缀探测与 64 MiB 聚合上限之间的延迟权衡；真实晚发样本必须脱敏沉淀后再调整。
- Windows 真实付费 provider 的最终验证仍需使用包含本提交的 exe 进行小 session/大 session 对照。
