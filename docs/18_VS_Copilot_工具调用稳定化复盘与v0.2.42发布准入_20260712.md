# VS Copilot 工具调用稳定化复盘与 v0.2.42 发布准入（2026-07-12）

## 背景

0.2.16 之后我们围绕 VS Copilot、GPT/GLM/DeepSeek、new-api/sub2api 兼容网关做了多轮增强，目标是解决：

- GPT/GLM 在 VS Copilot 中返回工具调用后，客户端提示“无法运行 create_file / get_file / run_tests”等问题。
- 部分上游返回 OpenAI 标准 `tool_calls`，部分返回 legacy `function_call`。
- 部分上游在 `stream=false` 时仍返回 SSE，或在流式路径中分片返回工具参数。
- VS/Copilot 对 `finish_reason`、工具名、工具参数结构比 Web 测试页更敏感。

用户反馈 v0.2.41 仍出现“无法运行 get_file/create_file”，说明之前的测试还没有覆盖到所有协议路径，也没有真正测到“下游可执行”的层级。

## 核心目标

本次修复的核心目标不是继续堆补丁，而是把工具调用链路重新收敛为一个清晰、可验证的设计：

1. 代理不应误删、误降级或改坏模型已经返回的标准工具调用。
2. 代理只做必要兼容归一化，例如常见别名映射、`finish_reason` 修正、SSE 聚合。
3. 代理不伪造客户端没有声明的工具，不扩大权限边界。
4. 测试必须证明下游能解析并执行工具调用，而不只是 HTTP 不报错。

## 根因复盘

### 已修过但仍漏掉的分支

v0.2.41 前后已经修复了多条路径：

- OpenAI 标准非流式 `tool_calls` 默认透传。
- OpenAI 流式 `tool_calls` 默认透传。
- OpenAI 流式 legacy `function_call` 默认透传。
- `finish_reason:"tool_calls"` 不再因为代理没有观察到工具名而降级成 `stop`。

但本次复查发现一个关键漏点：

- raw JSON 非流式路径里的 legacy `message.function_call` 仍保留了旧严格逻辑。
- 当 `function_call.name` 未出现在当前请求声明的 tools 中时，旧逻辑可能删除该 `function_call`，并在 content 中注入 `Proxy blocked undeclared tool calls`。
- GPT/GLM/兼容网关如果刚好在非流式响应里使用 legacy `function_call:create_file` 或 `function_call:get_file`，VS 就会看不到可执行工具调用，表现为“无法运行 create_file/get_file”。

这不是上游网络问题，也不是 new-api 本身的问题，而是代理内部协议矩阵缺口。

### 为什么之前测试没有抓住

之前很多测试证明的是：

- 响应 HTTP 状态是 200。
- 响应里包含某些字符串。
- 工具调用没有在某个单一路径被删除。

这些测试有价值，但不够。它们没有证明：

- VS 客户端能把响应解析成结构化工具调用。
- 工具参数仍是可执行 JSON。
- 流式增量参数能被正确合并。
- `create_file` 创建后的文件能被 `get_file` 读取到。

因此测试层级必须从“格式存在”提高到“执行语义成立”。

## 顶层设计取舍

### 稳定透传是默认策略

对 OpenAI 标准工具协议，代理默认稳定透传：

- 非流式 `message.tool_calls`：保留。
- 非流式 legacy `message.function_call`：只要 name 非空就保留。
- 流式 `delta.tool_calls`：保留分片。
- 流式 legacy `delta.function_call`：保留分片。

这条策略的理由：

- VS Copilot 是真正执行工具的一方，代理不应在缺少完整上下文时替客户端做安全裁决。
- 以前“未声明就删除”的逻辑在兼容网关和模型方言漂移下误伤太大。
- 删除工具调用会把模型输出从“可执行动作”变成普通文本或 stop，用户侧表现就是“无法运行工具”。

### 只做安全的别名映射

工具别名映射仍保留，但必须遵守约束：

- 只把模型返回的常见语义别名映射到客户端已经声明的目标工具。
- 不向请求或响应中新增客户端没有声明的工具。
- 不把高风险破坏性别名泛化映射到文件编辑工具。

例如：

- 如果 VS 声明了 `powershell`，`run_tests` 可以归一为 `powershell`。
- 如果 VS 没有声明 `powershell` 或 `terminal`，`run_tests` 不会被代理强行映射。

### DSML 文本方言保持安全边界

DSML 是文本转工具调用，不是 OpenAI 原生工具协议。因此它保持更严格的边界：

- 只有目标工具已声明时才转换为可执行工具调用。
- 如果 DSML 中出现未声明工具，保持原文本，不伪造可执行工具。

这是为了避免模型在普通文本中输出疑似工具片段时，代理错误转成真实执行动作。

## 本次代码调整

### 修复 legacy `function_call`

`internal/proxy/openai_chat_response.go` 中 raw JSON 非流式 legacy `function_call` 改为：

- `name` 非空：透传。
- `name` 为空：删除并提示 `空工具名`，避免客户端执行无名工具。

### 删除旧严格分支残留

复查时发现流式 sanitizer 中还有一些旧状态字段和不可达严格分支，例如：

- blocked tool index 状态。
- legacy function_call block 状态。
- 根据是否观察到允许工具来把 `finish_reason:"tool_calls"` 改成 `stop` 的逻辑。

这些逻辑虽然当前测试不再触发，但保留在代码中会带来两个风险：

- 后续维护者误以为还有“严格拦截”默认策略。
- 未来小改动可能让不可达分支重新生效，复活同类问题。

因此本次直接删除，保证实现和设计一致：OpenAI 标准工具调用默认透传。

### 修正误导性测试名

原测试名 `TestOpenAIStreamBlocksLegacyFunctionCallContinuationAtHandler` 与当前稳定透传策略相反，已改为：

- `TestOpenAIStreamPassesThroughLegacyFunctionCallContinuationAtHandler`

测试名必须表达真实行为，避免下一次审查时误判策略方向。

## 新增测试准入

### 协议矩阵测试

新增/强化 `internal/proxy/openai_chat_response_test.go`：

- JSON `tool_calls:create_file`
- JSON `tool_calls:get_file`
- JSON legacy `function_call:create_file`
- JSON legacy `function_call:get_file`
- SSE `tool_calls:create_file`
- SSE `tool_calls:get_file`
- SSE legacy `function_call:create_file`
- SSE legacy `function_call:get_file`

该矩阵用于证明代理不会在任何核心协议路径中误删、误降级、误注入 block notice。

### 执行语义测试

新增 `internal/proxy/tool_execution_e2e_test.go`，这是本次最关键的准入补强。

它不是只检查字符串，而是完整走 `/v1/chat/completions` handler，然后模拟 VS 客户端做三件事：

1. 解析非流式 JSON 或流式 SSE。
2. 提取结构化工具名和 JSON 参数。
3. 在最小内存文件系统中实际执行 `create_file` / `get_file`。

覆盖场景：

- 非流式 OpenAI `tool_calls`：先 `create_file`，再 `get_file`，校验读回内容。
- 非流式 legacy `function_call`：分别执行 `create_file` 与 `get_file`。
- 流式 OpenAI `tool_calls`：解析 SSE 后执行创建和读取。
- 流式 legacy `function_call`：解析 SSE 后执行旧格式工具调用。

这组测试能真实测出以下问题：

- 工具调用被代理删除。
- `finish_reason` 被错误降级。
- 工具参数被流式分片合并坏。
- legacy `function_call` 被丢弃。
- 下游无法解析参数 JSON。
- `create_file/get_file` 到执行层语义不成立。

## 验证结果

本次发版前已执行：

```bash
go test ./internal/proxy -run 'VisualStudioToolExecutionE2E|OpenAIToolCallCompatibilityMatrix|LegacyFunctionCall' -count=1
git diff --check
go test ./... -count=1
go test -race ./cmd/server ./internal/proxy ./internal/provider ./internal/config -count=1
bash tests/streaming_test.sh
bash tests/streaming_ollama_test.sh
bash -n tests/useai_large_request_diagnostic.sh
bash -n tests/large_request_matrix_diagnostic.sh
make build
./vs-ai-proxy --version
```

结果全部通过。

## 已知边界

这次修复不能承诺“任何模型永远不会出现无法运行工具”。准确边界如下：

- 可以保证：代理已知 OpenAI 工具协议路径不会再因为本地未声明判断而误删 `create_file/get_file`。
- 可以保证：当前测试已覆盖流式/非流式、标准/legacy、解析/执行语义。
- 不能保证：模型不会返回 VS 根本不支持的工具名。
- 不能保证：上游超时、413、499、503 时工具调用一定能产生，因为那时模型响应可能还没完成。
- 不能保证：真实 Visual Studio 内部工具注册表与测试模拟器完全一致，因此发布后仍需做 Windows VS smoke test。

## 后续排障优先级

如果发布后仍看到“无法运行 get_file/create_file/run_tests”：

1. 先看请求日志 `request_tools`，确认 VS 当前请求声明了哪些工具。
2. 再看响应日志 `response_tools`，确认模型实际返回了哪些工具。
3. 如果 `response_tools` 为空但上游成功，优先怀疑模型没有返回工具调用，而不是代理误删。
4. 如果 `response_tools` 有工具但 VS 报无法运行，抓取响应片段确认工具名是否在 VS 声明列表中。
5. 如果出现新别名，必须按四类协议矩阵和执行语义层补测试后再修。
6. 如果是 413/499/503/timeout，按大请求和上游渠道排障文档处理，不要混同为工具解析问题。

## 发版判断

v0.2.42 的发版判断：

- 没有新增无关功能。
- 没有新增依赖。
- 删除了旧严格拦截残留，代码更简单。
- 增强的是测试和文档，而不是继续堆兼容分支。
- 当前修改没有偏离核心目标：让 VS Copilot 下游稳定拿到并执行工具调用。

因此本次具备发布条件。

## 2026-07-13 补充：工具目录化治理与执行级发布门禁

### 为什么不能再按单个工具打补丁

`create_file`、`get_file`、`apply_patch`、`run_tests`、`git`、`powershell`、`grep_search` 等问题属于同一类：模型返回的工具名、协议形态、参数 JSON、`finish_reason`、流式分片任一环节被代理改坏，VS/Copilot 都可能提示“无法运行工具”。因此不能看到一个失败工具就补一个 `if`，必须统一治理。

### 当前统一策略

- OpenAI 标准 `tool_calls`、legacy `function_call`、流式 `delta.tool_calls`、流式 `delta.function_call` 默认稳定透传。
- 兼容性归一化只做安全别名映射，并且只映射到当前请求已经声明的工具。
- DSML 文本方言只有在目标工具已声明时才转换为可执行工具调用。
- 文件语义工具不降级到 shell：例如 `delete_file/remove_file` 只映射到 `delete_files`，`ls/find_file/glob` 不映射到 `powershell`，避免参数 schema 不匹配造成更隐蔽的执行失败。
- 当前工具目录基于本项目真实日志、用户反馈和本地测试样本沉淀，不宣称是 VS/Copilot 官方公开完整列表。发现新工具时必须先补目录和测试，再发布。

### 已覆盖工具族

- 计划/澄清：`adapt_plan`、`ask_question`、`clarify_requirements`、`detect_memories` 及 `update_plan/plan/clarify/ask_user/detect_memorie` 等别名。
- 终端/测试/构建：`powershell`、`terminal`、`run_in_terminal` 及 `run_tests/run_command/bash/cmd/dotnet_test/npm_test/run_lint` 等别名。
- Git：`git` 及 `git_status/git_diff/git_log/git_command`。
- 文件写入：`create_file`、`edit_file`、`apply_patch` 及 `write_file/create_new_file/append_file/apply_diff/apply_changes` 等别名。
- 文件删除：`delete_files` 及 `delete_file/remove_file`。
- 文件读取/列表：`get_file`、`file_search`、`list_files` 及 `read_file/open_file/view_file/ls/find_file/glob` 等别名。
- 搜索/符号：`code_search`、`grep_search`、`file_search`、`find_symbol` 及 `rg/grep/ripgrep/search_symbol` 等别名。

### 本次新增的执行级验证

发布门禁不再只验证 HTTP 200 或响应中有字符串，而是验证“VS/Copilot 下游是否能拿到并执行结构化工具调用”：

- 非流式 `tool_calls`：`create_file -> get_file`。
- 非流式 `tool_calls`：`create_file -> apply_patch -> get_file`。
- 非流式 legacy `function_call`：`create_file`、`get_file`。
- 流式 `tool_calls`：`create_file -> get_file`。
- 流式 `tool_calls`：`create_file -> apply_patch -> get_file`。
- 流式别名：`apply_diff -> apply_patch`，并验证文件内容确实被修改。
- 流式常见工具族：`run_tests -> powershell`、`git_status -> git`、`rg -> grep_search`、`read_file -> get_file`，并通过模拟 VS 执行器验证参数 JSON 可执行。
- 流式 legacy `function_call`：`create_file`、`get_file`。
- DSML：真实 `get_file` 样本、`run_tests`、常见别名、未声明工具拒绝转换。
- Catalog 属性测试：别名无重复、别名目标必须存在、canonical 不被改写、所有 alias 在非流式 JSON / OpenAI raw JSON / OpenAI SSE / DSML 中均可按声明工具归一化。

### 新增工具的准入流程

1. 先保存真实 `request_tools`、`response_tools`、provider、model、stream 状态、错误码和响应片段。
2. 判断失败工具是 canonical、alias，还是模型幻觉出的未声明工具。
3. canonical 工具必须加入目录并补 pass-through 测试。
4. alias 工具必须补非流式 JSON、raw OpenAI JSON、OpenAI SSE、DSML 四类归一化测试。
5. 会触发真实执行的工具必须补执行级 E2E，证明参数 JSON 可执行。
6. 参数 schema 不兼容时禁止映射，宁可保留原始输出并增强诊断，也不能映射到错误工具。
7. 发布前至少通过 `make tool-check`、`go test ./...`、`go test -race` 重点包、`go vet ./...`、前端脚本语法检查和 Windows 交叉构建。

### 对 `无法运行 apply_patch` 样本的审慎解释

样本中同时出现 `client_gone`、`499`、`stream_state=downstream_started`、约 44 秒耗时，以及后续另一次 `200`。这说明代理曾经向 VS/Copilot 写出过下游数据，随后客户端断开或取消；44 秒也不是典型约 100 秒等待上限。它不能单独证明是工具解析失败，也不能单独证明是上游超时。但由于用户侧明确看到“无法运行 apply_patch”，本次仍按工具调用协议矩阵补强，确保代理自身不再因为工具名、finish_reason、流式分片或参数 JSON 改写造成工具不可执行。
