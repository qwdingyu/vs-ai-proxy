# UseAI GPT-5.5 与 Visual Studio Copilot 工具调用踩坑记录（2026-07-10）

## 背景

本次问题集中在管理端测试页、Provider 模型列表校验、UseAI `gpt-5.5`、Visual Studio Copilot 下游兼容，以及工具调用协议适配。

用户侧典型现象：

- `usecpa + deepseek-v4-flash` 在测试页被误判为“未绑定当前 provider”。
- `useai + gpt-5.5` `/v1/models` 能返回模型，但 `/v1/chat/completions` 非流式请求报错：
  - `500 do_request_failed`
  - `Empty reply from server`
  - `503 Service temporarily unavailable`
  - 或 `stream=false` 却返回 `data: ...` SSE chunk，导致 JSON 解析失败。
- 真实 Visual Studio Copilot 环境中，`/v1/chat/completions stream=true` 也出现过：
  - `openai stream error: API 错误 503: Service temporarily unavailable`
  - `context canceled`
  - 最终对 VS 表现为 `upstream_server_error`。
- Visual Studio Copilot 工具调用中，部分模型/provider 对 `tool_calls`、legacy `function_call`、object arguments、流式工具参数分片的兼容性不同。
- `step-router-v1` 等模型可能不返回标准 OpenAI `tool_calls`，而是把工具调用编码在文本中的 DSML 片段，例如 `<｜DSML｜tool_calls>` / `<｜DSML｜invoke name="get_file">`。
- 管理页面按钮文案曾使用“连接测试”，容易让用户误以为是网络连通性测试，而实际只是读取 provider 官方模型列表。

## 关键结论

1. `/v1/models` 成功只代表 **Base URL/API Key 可以读取官方模型列表**，不代表每个模型都能完成一次对话。
2. `useai/gpt-5.5` 当前不是“模型不存在”，而是上游 New API 渠道/协议链路存在不稳定：非流式可能失败，流式通常可用。
3. New API 虽然可以配置多个渠道，但实测某些单渠道失败会直接透出到客户端，因此代理侧必须做有限兜底。
4. VS Copilot 下游比普通 Web/curl 更严格：
   - 非流式必须返回 JSON，不能返回 `data:` SSE。
   - `finish_reason` 不能是空字符串或未知值。
   - 工具调用字段必须尽量原样保留，不能丢 `strict`、`tool_choice`、`parallel_tool_calls`、嵌套扩展字段。
5. 模型输出的非标准工具调用方言不能当作普通文本透传；需要在代理边界统一归一化为 OpenAI `tool_calls`。

## 已落地修复位置

### 1. Provider 类型只保留 OpenAI

文件：`web/dist/index.html`

新增/编辑 provider 的类型下拉只保留 `openai`，`ollama/custom` 已在前端注释隐藏，避免用户误选后产生大量沟通成本。

注意：后端 `validateProviderConfig` 仍保留 `ollama/custom` 兼容，避免旧配置无法加载或迁移失败。

### 2. 测试页模型下拉来自官方 `/models`

文件：`web/dist/index.html`

测试页切换 provider 后，会调用 `/admin/api/test/connection`，读取所选 provider 官方模型列表，并用返回结果构建模型下拉框。

这解决了：

- 官方智谱返回 `glm-5.2`，第三方 provider 返回 `z-ai/glm-5.2`，二者不能混用。
- 用户不应手工输入模型名，否则容易选错 provider 的模型。

### 3. 测试页 stale binding 兜底

文件：`internal/api/api.go`

测试页仍会阻止“已知模型绑定到别的 provider”的错误选择，但新增规则：如果当前 provider 实时 `/models` 明确包含该模型，则允许测试通过。

对应场景：

- 本地配置里 `deepseek-v4-flash` 仍绑定 `deepseek`。
- 用户选择 `usecpa`，而 `usecpa /v1/models` 实时返回 `deepseek-v4-flash`。
- 此时不应被旧本地绑定拦截。

### 4. UseAI / GPT-5.5 非流式异常兼容

文件：

- `internal/provider/provider.go`
- `internal/proxy/openai_chat_response.go`
- `internal/proxy/server.go`
- `internal/api/api.go`

修复点：

- OpenAI provider 对瞬态错误做最多 3 次短重试：`429/5xx/EOF/empty reply/connection reset/timeout`。
- 不重试 `400/401/403/404`，避免放大参数、鉴权、模型不存在等确定性错误。
- 如果 `stream=false` 返回 `data:` SSE，provider 层可聚合成普通 `ChatResponse`。
- 真实代理 raw OpenAI 透传路径也会在写给下游前识别 SSE，并转换为标准 JSON `chat.completion`。
- 管理测试页非流式失败后，会自动尝试流式兜底，并返回：
  - `fallback_mode: "stream"`
  - `warning: "非流式测试失败，已自动使用流式测试兜底..."`
- 真实 `/v1/chat/completions stream=true` 路径如果流式上游失败且尚未写出响应，会反向尝试非流式请求；成功后合成为 OpenAI SSE 返回给 VS。

### 5. VS Copilot finish_reason 兼容

文件：`internal/proxy/openai_chat_response.go`

VS 对 `finish_reason` 枚举严格，空字符串、`unknown`、provider 私有值会导致客户端解析失败。

代理统一归一化：

- `""` / `"null"` / `"unknown"` / 未知值 → `"stop"`
- 保留标准值：`stop`、`length`、`tool_calls`、`content_filter`、`function_call`

### 6. 工具调用协议兼容

文件：

- `internal/provider/chat_request_json.go`
- `internal/proxy/server.go`
- `internal/converter/ollama_openai.go`

已覆盖：

- OpenAI modern `tools` / `tool_calls`
- legacy `functions` / `function_call`
- `function.arguments` 为 string 或 object
- `tool_choice`
- `parallel_tool_calls`
- `strict`
- tool/function 嵌套扩展字段
- 流式 `tool_calls` 参数分片
- 常见 VS 工具：`create_file`、`powershell`、`run_in_terminal`、`apply_patch`、`read_file`
- DSML 文本工具调用：`<｜DSML｜tool_calls>`、`<｜DSML｜invoke name="...">`、`<｜DSML｜parameter name="...">...` 会统一转换为标准 OpenAI `tool_calls`。

### 7. 管理页面按钮语义修正

文件：`web/dist/index.html`

文案已调整：

- Provider 列表：`测试` → `校验模型列表`
- 测试台：`测试连接` → `读取模型列表`
- Provider 弹窗：`探测` → `探测模型接口`

语义说明：

- `校验模型列表` / `读取模型列表`：只验证 provider 官方 `/models` 是否可读取。
- `测试对话`：验证指定模型能否完成真实 chat。
- 不能再把 `/models` 读取叫作“网络连通性测试”。

## 踩坑过程复盘

### 坑 1：把 `/models` 成功误当作模型可对话成功

`useai/gpt-5.5` 的 `/models` 返回过 `gpt-5.5`，但 chat 非流式仍失败。

最佳实践：

- `/models` 用于构建下拉框和校验 provider/API key。
- `/chat/completions` 才能验证模型真实可用。
- UI 文案必须区分“模型列表校验”和“测试对话”。

### 坑 2：New API 多渠道不等于永不透出失败

即使 New API 配了多个渠道，实测仍可能把某个渠道的 `503`、`do_request_failed`、空响应直接透出。

最佳实践：

- 代理侧对瞬态失败做短重试。
- 保持重试克制，最多 3 次，且不重试 4xx。
- 错误信息必须带 body preview，方便定位到底是上游 JSON、HTML、纯文本还是 SSE。

### 坑 3：非流式请求可能返回 SSE

`stream=false` 不一定保证上游返回 JSON。某些 OpenAI-compatible 网关会返回：

```text
data: {...}

data: [DONE]
```

如果直接透给非流式下游，VS 或 OpenAI SDK 会报 JSON 解析失败。

最佳实践：

- provider 层 `Chat` 尽力把 SSE 聚合成 `ChatResponse`。
- proxy raw OpenAI 透传路径也必须做同样处理，不能只修管理测试页。

### 坑 4：只修测试页会产生“测试成功、下游失败”

最初测试页增加了流式兜底，能显示 `success: true`，但真实代理 raw OpenAI 路径仍可能直接返回 SSE。

最佳实践：

- 测试页修复只能证明诊断体验改善。
- 必须同步检查真实下游路径：`/v1/chat/completions`、`/api/chat`、stream 和 non-stream。
- 每个测试页兜底都要问：真实代理路径是否也具备同等能力？

### 坑 4.1：只修非流式不够，VS 真实路径可能是 stream=true

真实 VS Copilot 会走 `/v1/chat/completions stream=true`。如果只处理 `stream=false` 返回 SSE、或只处理非流式失败后的流式兜底，仍会出现：

```text
模型 gpt-5.5 在提供商 useai2 流式失败: openai stream error: API 错误 503
POST /v1/chat/completions - 502
```

最佳实践：

- OpenAI 流式路径失败且尚未写出响应时，可以尝试非流式请求。
- 非流式成功后必须合成为标准 OpenAI SSE：`data: chunk` + `data: [DONE]`。
- 不能在已经写出部分 SSE 后再切换协议；只能在 response 尚未开始时兜底。
- 工具调用也要能从 fallback 聚合结果中保留 `tool_calls`，避免 VS 工具执行失败。

### 坑 5：VS 对 finish_reason 比浏览器更严格

Web 面板可显示的响应，VS 可能因 `finish_reason:""` 失败。

最佳实践：

- 写给 VS 前归一化 finish reason。
- 流式 chunk 和非流式 JSON 都要处理。

### 坑 6：工具调用字段不能只按“标准字段”重建

VS/Copilot 和不同 provider 会携带扩展字段；如果 Marshal/Unmarshal 丢字段，工具调用就可能表现为“create_file 无法运行”“powershell 无法调用”。

最佳实践：

- 未知字段必须保留在 `Extra` 中。
- object arguments 必须转换成 JSON string，兼容 OpenAI 工具调用规范。
- 流式工具参数分片不能丢。

### 坑 7：部分模型会把工具调用放在文本方言里

`step-router-v1` 返回过类似内容：

```text
<｜DSML｜tool_calls>
  <｜DSML｜invoke name="get_file">
    <｜DSML｜parameter name="filename">src\Runner.Console\Health\DeviceHealthCheck.cs</｜DSML｜parameter>
  </｜DSML｜invoke>
</｜DSML｜tool_calls>
```

这不是模型不可用，也不是上游配置错误，而是模型/provider 的工具调用方言。如果原样透传给 VS，VS 只会看到普通 assistant 文本，不会执行工具。

最佳实践：

- 在代理边界把 provider-specific 方言统一转换成标准 OpenAI `tool_calls`。
- 不要按模型名写死；应按响应格式识别 DSML block。
- OpenAI 非流式、OpenAI 流式聚合、Ollama JSON 输出都要走同一套归一化。
- DSML block 从 `content` 移除，保留 block 外的说明文本。
- `finish_reason` / `done_reason` 应设置为 `tool_calls`，让下游明确进入工具执行阶段。

## 验证命令

每次改动 VS / provider / tool calling 相关代码后，至少运行：

```bash
go test ./internal/provider ./internal/proxy -count=1 -run 'Test(ChatRequestPreservesLegacyFunctionFields|MessagePreservesLegacyFunctionCall|FunctionCallAcceptsObjectArguments|OpenAIProviderChatRawPreservesCommonToolMatrix|OpenAIProviderChatRawRetriesTransientServerErrors|OpenAIProviderChatStreamRetriesTransientServerErrors|OpenAIProviderChatAcceptsSSEBodyForNonStreamRequest|OpenAIChatForwardsToolRequestFields|OllamaChatForwardsToolSchemaExtensionsToOpenAIProvider|OpenAIChatConvertsNonStreamSSEBodyToJSON|StreamOpenAIToOllamaPreservesToolCallArgumentChunks|StreamOpenAINormalizesBlankFinishReasonForVisualStudio|OpenAIStreamBodyToChatResponseAggregatesSSE)'
```

最终提交前运行：

```bash
go test ./... -count=1
git diff --check
```

## 当前已覆盖的关键测试

- `internal/provider/provider_test.go`
  - 非 JSON 响应带 `body_preview`
  - 非流式返回 SSE 可聚合
  - 非流式 5xx 可重试
  - 流式 5xx 可重试
  - 4xx 不重试
  - common tool matrix 保留
- `internal/provider/chat_request_json_test.go`
  - object arguments
  - legacy `functions/function_call`
  - unknown nested fields
- `internal/proxy/integration_test.go`
  - OpenAI 工具字段转发
  - Ollama → OpenAI 工具 schema 扩展保留
  - 非流式 SSE 转 JSON，不把 `data:` 泄漏给下游
  - DSML 文本工具调用转标准 OpenAI/Ollama tool_calls
- `internal/proxy/server_test.go`
  - OpenAI SSE → Ollama NDJSON
  - 流式工具调用参数分片
  - VS finish_reason 归一化
- `internal/api/api_test.go`
  - 测试页空模型拦截
  - stale binding 但官方实时模型存在时允许
  - 非流式失败后流式兜底

## 后续维护注意事项

1. 不要把“模型列表读取成功”写成“连接成功”或“模型可用”。
2. 不要把 `glm-5.2` 和 `z-ai/glm-5.2` 当作同一个模型。
3. 不要只看管理测试页成功，必须验证真实代理入口。
4. 不要删除 unknown field preservation，否则工具调用会回归。
5. 不要取消 `finish_reason` 归一化，VS 会比 Web 更容易失败。
6. 不要对所有错误无限重试；只重试瞬态错误，避免放大 4xx 和账单风险。
7. 不要把 DSML 工具调用当成模型异常；这是 provider/tool-call 方言，必须转换为标准 `tool_calls`。
8. `web/dist/assets/images/qrcode_qq.png` 当前是未跟踪文件，非本次修复范围，不要误加入提交。

## 相关文件速查

- Provider OpenAI 请求、重试、SSE 兼容：`internal/provider/provider.go`
- Chat request/tool calling JSON 保真：`internal/provider/chat_request_json.go`
- 真实代理 OpenAI 响应归一化：`internal/proxy/openai_chat_response.go`
- 真实代理路由与 raw OpenAI 透传：`internal/proxy/server.go`
- 管理测试接口：`internal/api/api.go`
- 管理页面文案、按钮防抖、模型下拉：`web/dist/index.html`

## 2026-07-10 追加：生产级审查后的全局加固

### 问题 8：重试、模式兜底和 provider fallback 叠加会放大请求

真实日志出现过 85-100 秒请求，例如：

```text
模型 gpt-5.5 在提供商 useai2 流式失败: context canceled
POST /v1/chat/completions - 502 (100001 ms)
```

根因不是单个模型名，而是恢复策略叠加：

1. OpenAI provider 对瞬态 5xx 做短重试。
2. 代理层在非流式失败时会尝试流式聚合，流式失败时会尝试非流式合成 SSE。
3. 注册表还可能继续尝试同模型的其他 provider 候选。
4. 如果每一层都各自使用新的超时预算，请求会被放大，用户看到长时间卡住，服务端也会增加上游压力和计费风险。

最佳实践已经固化到代码：

- `timeout_seconds` 现在表示单个 provider candidate 的总预算，而不是每次 HTTP attempt 的预算。
- 未配置模型超时时默认使用 60 秒总预算，避免无界等待。
- provider 内部 HTTP 请求不再用第二套 `http.Client.Timeout` 截断模型级 deadline，防止 `timeout_seconds=180` 仍被默认 60 秒提前杀掉。
- 流式/非流式互相兜底只在可恢复错误发生时触发：5xx、网络瞬断、协议不兼容。
- 4xx、429、`context canceled`、`context deadline exceeded` 不触发模式兜底。
- 429 在没有明确 `Retry-After` 支持前不盲目重试，避免放大限流和账单风险。

排障信号：

- `X-Proxy-Fallback-Mode: nonstream-to-stream` 表示非流式失败后，代理用流式聚合成 JSON 成功。
- `X-Proxy-Fallback-Mode: stream-to-nonstream` 表示流式失败后，代理用非流式响应合成 SSE 成功。
- 没有该 header 时，不应把成功误判为 fallback 成功。

### 问题 9：DSML 工具调用必须是协议边界转换，不是模型名特判

`step-router-v1` 返回 DSML 文本工具调用是 provider/model 方言问题，不应写死模型名，也不应只对某个 provider 打补丁。

当前规则：

- 只转换客户端请求中声明过的 `tools` / legacy `functions`。
- DSML 中任意工具未声明、参数重复、参数缺少 name、超过资源限制时，整组 DSML 原样保留为文本，不执行任何工具。
- 这是原子策略：不能“合法的执行、非法的删除”，否则会造成模型意图被代理层篡改。
- `string="false"` 的参数会尽力保留数字、布尔、null 类型；`string="true"` 保持字符串。
- 工具调用 ID 使用内容哈希生成，避免简单递增 ID 在重放/缓存场景下不稳定。
- 支持 `<｜DSML｜...>` 和 `<|DSML|...>` 两类 marker。
- 资源上限：DSML 内容 1MiB、工具调用 32 个、单调用参数 64 个、参数 JSON 256KiB。

流式 DSML 的权衡：

- 代理只探测开头少量 SSE 行，以保护普通流式首 token 延迟。
- 检测到 DSML 后才聚合完整流并输出标准 OpenAI SSE `tool_calls`。
- 普通 SSE 必须逐行原样转发，不能因为 DSML 探测被重复、改写或延迟到全量完成。
- `X-Proxy-Tool-Call-Normalization: dsml` 表示本次确实发生了 DSML 到标准 `tool_calls` 的转换。

### 问题 10：测试页成功不等于 VS Copilot 真实路径成功

管理测试页通常是短 prompt、单接口、人工触发；VS Copilot 真实路径会覆盖：

- `/v1/chat/completions`
- `stream=true` 和 `stream=false`
- 标准 `tool_calls`
- legacy `functions/function_call`
- 流式工具参数分片
- 模型输出 DSML 方言
- 上游返回 SSE 但客户端请求非流式
- 上游 5xx、网关 `do_request_failed`、客户端取消

因此回归测试必须覆盖真实代理入口，不能只看 `/api/test/chat`。

### 新增必须保持的回归测试

关键测试包含：

- provider 重试共享同一个 operation timeout，不允许每次 attempt 获得新的完整预算。
- 模型级长 timeout 可以覆盖 provider 默认短 timeout。
- 4xx、429、取消、超时不触发流/非流互相兜底。
- fallback 成功时响应头必须显示 `X-Proxy-Fallback-Mode`。
- 普通 SSE 在 DSML 探测后仍保持原样。
- 未声明 DSML 工具必须原样保留，不得转换或删除。
- 混合合法/非法 DSML 工具调用必须整组拒绝。
- 重复 DSML 参数必须整组拒绝。

### 后续禁止回退的实践

1. 不要按 `gpt-5.5`、`step-router-v1`、`glm-5.2` 等模型名写兼容分支；应识别协议形态。
2. 不要在代理层和 provider 层分别设置互相竞争的超时语义。
3. 不要对 4xx、429、用户取消做兜底重试。
4. 不要在流式已写出字节后切换 provider，否则下游会收到混合流。
5. 不要把 DSML 文本无条件转换为工具调用，必须校验声明工具和资源限制。
6. 不要把“测试页可用”宣传为“VS Copilot 工具调用全链路可用”，除非真实 `/v1/chat/completions` 流/非流都通过。
7. 不要删除 `X-Proxy-Fallback-Mode` / `X-Proxy-Tool-Call-Normalization`，它们是线上定位“上游成功”还是“代理恢复成功”的关键证据。

### 2026-07-10 追加：`client_gone` 与上游 500 必须分开判断

真实日志中可能同时看到：

```text
流状态: 错误
client_gone
openai_stream_error
API 错误 500: upstream error: do request failed
```

这类日志容易误判为“GPT 模型自身持续不可用”。实际需要拆成两件事：

1. `API 错误 500 / do_request_failed`：上游网关或上游渠道失败，属于 provider/server 侧错误。
2. `client_gone` / `context canceled`：下游客户端已经取消或断开，属于客户端生命周期事件。

最佳实践：

- 一旦请求 context 已取消，代理不应继续流/非流兜底，也不应切换到其他 provider。
- `client_gone` 不应记为 `upstream_server_error`，否则会误导排障，把用户取消当成模型故障。
- 如果没有 `client_gone`，单纯 `API 错误 500 / do_request_failed` 仍可按瞬态上游错误处理，并允许在未写出响应前做一次安全兜底。
- 如果已经写出流式字节，不允许切换 provider；下游会收到混合 SSE，风险更高。

当前回归测试已覆盖：

- wrapped `context.Canceled + API 错误 500` 分类为 `client_gone`。
- 流式请求在 `client_gone` 后不会继续请求 secondary provider。

### 2026-07-10 追加：工具调用日志必须可诊断但不能泄露命令参数

客户反馈中出现“有些修改能看到痕迹，有些改完看不到痕迹”。在 VS Copilot 场景里，这通常需要区分：

1. 编辑器/补丁类工具：例如文件编辑、apply patch，通常更容易看到 diff。
2. 命令类工具：例如 `powershell`、`git`、终端命令，可能绕过编辑器缓冲区，用户只能看到最终文件状态。

代理不能执行这些工具，但必须把协议层工具调用记录清楚，方便判断当次请求到底发生了什么。

当前日志增强策略：

- `request_tools`：只记录客户端声明过哪些工具，例如 `declared: git,powershell`。
- `response_tools`：只记录模型实际返回了哪些工具，例如 `returned: powershell` 或 `streamed: git`。
- `fallback_mode`：记录是否发生流/非流恢复，例如 `stream-to-nonstream`。
- `normalization`：记录是否发生 provider-specific 归一化，例如 `dsml`。
- 不记录完整工具参数，不记录 PowerShell/Git 命令正文，避免把用户本机路径、命令、secret 写进日志。

排障建议：

- 如果 `request_tools` 里没有某个工具名，模型/DSML 中出现该工具时不应转换执行，这是安全边界。
- 如果 `response_tools` 为空，说明模型没有返回标准工具调用，可能只是输出了普通文本建议。
- 如果 `response_tools` 出现 `powershell` / `git`，但用户看不到编辑器 diff，应优先怀疑是命令类工具修改了工作区，而不是代理直接改文件。
- 如果出现 `normalization: dsml`，说明代理把 provider 方言转换成标准 `tool_calls`，应继续检查工具名是否在声明列表中。

### 2026-07-10 追加：标准 `tool_calls` 也必须受声明工具边界约束

此前 DSML 方言已经要求工具名必须出现在客户端请求声明的 `tools` / legacy `functions` 中。但进一步审查发现，标准 OpenAI `tool_calls`、legacy `function_call`、流式 `tool_calls` 也必须遵守同一条安全边界，否则模型可能凭空返回 `powershell`、`git` 等命令类工具调用，导致下游工具宿主尝试执行未授权工具。

当前统一规则：

- 请求没有声明工具时，代理不主动把文本或方言转换成工具调用。
- 请求声明了工具时，响应中的标准 `tool_calls` / `function_call` 只保留声明过的工具名。
- 未声明工具调用会被移除，并转成可见文本提示：`Proxy blocked undeclared tool calls: ...`。
- raw OpenAI JSON、typed ChatResponse、流式 SSE、流式聚合 fallback、DSML 方言都走同一条边界。
- 日志只记录工具名摘要，不记录命令参数。

这条规则的目的不是限制正常工具调用，而是防止模型或上游 provider 越过 VS/Copilot 客户端声明的工具权限边界。

## 2026-07-10 追加：v0.2.21 到 v0.2.24 工具调用回归与最终修复边界

### 现象

`v0.2.21` 引入声明工具边界后，真实 VS Copilot 流式工具调用出现了更严重的失败：

```text
[Proxy blocked undeclared tool calls: <empty>]
无法运行 get_file / grep_search / create_file
```

这不是 VS 工具本身没有声明，而是代理把 OpenAI SSE 中合法的参数续片误判成空工具名。

### 根因

OpenAI 流式工具调用是增量协议：

1. 首个 chunk 通常包含 `tool_calls[0].function.name`。
2. 后续 chunk 经常只包含 `index` 和 `function.arguments` 的片段。
3. 这些参数续片不能独立判断工具名，也不能按 `<empty>` 工具拦截。

错误做法是逐行无状态调用完整响应的工具过滤器。完整响应里缺少工具名可以视为非法；但流式参数续片缺少工具名是正常协议形态。

### 修复版本

用户侧遇到工具调用异常时，应升级到 `v0.2.24` 或更新版本。

关键修复链路：

- `v0.2.22`：允许合法 OpenAI SSE 参数续片通过，不再把续片显示成 `<empty>`。
- `v0.2.23`：直通 SSE 改成有状态过滤；非法工具首片被拦截后，同 index 的后续参数片静默丢弃，避免孤儿参数泄露。
- `v0.2.24`：当所有工具都被拦截后，`finish_reason` 改为 `stop`；当仍有合法工具保留时才继续使用 `tool_calls`。`/api/chat` 的 OpenAI→Ollama 流转换也接入同一过滤策略。

### 当前必须保持的协议规则

1. 非流式完整响应可以按完整工具名严格校验。
2. 流式 `tool_calls` / legacy `function_call` 必须按 chunk 状态处理。
3. 无 `name` 的参数续片：
   - 如果对应工具 index 已确认合法，放行。
   - 如果对应工具 index 已被拦截，静默丢弃。
   - 如果还没有看到名称，不得按 `<empty>` 提示给用户。
4. 明确带非法 `name` 的 chunk 必须拦截，并输出可见诊断：`Proxy blocked undeclared tool calls: <name>`。
5. 拦截后不能让客户端继续看到 `finish_reason:"tool_calls"`，除非仍有合法工具调用保留。
6. `/v1/chat/completions` 与 `/api/chat` 必须使用同一工具边界，不能只修 VS 路径。

### 已固化的回归测试

必须保留以下测试类型：

- helper 层：合法参数续片不被 `<empty>` 拦截。
- helper 层：非法工具首片被拦后，后续参数续片不泄露。
- handler 层：真实 `/v1/chat/completions` 流式路径覆盖 modern `tool_calls`。
- handler 层：真实 `/v1/chat/completions` 流式路径覆盖 legacy `function_call`。
- handler 层：raw 非流式 JSON 中未声明 `tool_calls` 被移除且 `finish_reason=stop`。
- `/api/chat`：OpenAI SSE 转 Ollama NDJSON 时同样执行声明工具边界。
- Windows CI：必须运行 `internal/proxy` 测试，不能只测命令行和更新模块。

### 发布校验要求

以后涉及工具调用、流式、模型测试页、fallback 的改动，发布前至少执行：

```bash
go test ./internal/proxy -count=1
go test ./... -count=1
go test -race ./internal/proxy -count=1
git diff --check
```

CI 中 Linux 必须跑 `go test ./...`，Windows smoke 必须包含 `./internal/proxy`，因为用户主要使用 Windows + Visual Studio。

## 2026-07-10 追加：v0.2.25 发布后二进制黑盒验收

为避免只依赖源码测试，本次对 GitHub Release 中已经发布的 `v0.2.25` 二进制做了黑盒验收。

### 验收对象

- Release：`https://github.com/qwdingyu/vs-ai-proxy/releases/tag/v0.2.25`
- 本机验收资产：`vs-ai-proxy-v0.2.25-macos-x64.tar.gz`
- Windows 资产也已单独下载验收：`vs-ai-proxy-v0.2.25-windows-x64.exe.zip`

二进制元数据确认：

- `main.version=v0.2.25`
- `vcs.revision=9e83f2b0a1be1f80fd6107b5a9057038a8d914e7`
- `vcs.modified=false`

### 黑盒上游模拟

启动一个本地 OpenAI-compatible 假上游：

- `GET /v1/models` 返回 `gpt-test`
- `POST /v1/chat/completions` 根据 prompt 返回三类 SSE：
  - 合法 modern `tool_calls`：首片带 `name=grep_search`，续片只带 `arguments`
  - 非法 modern `tool_calls`：首片带 `name=powershell`，续片只带危险参数
  - 非法 legacy `function_call`：首片带 `name=powershell`，续片只带危险参数

代理使用独立临时配置启动，避免污染用户真实配置。

### `/v1/chat/completions` 验收结论

合法工具流：

- 输出保留 `name:"grep_search"`
- 输出保留续片参数 `"needle"}`
- 没有出现 `Proxy blocked` 或 `<empty>`
- 最终 `finish_reason:"tool_calls"`

非法 modern 工具流：

- 首片被改写为可见提示：`[Proxy blocked undeclared tool calls: powershell]`
- 后续 `Remove-Item` 参数片未泄露
- 没有出现 `<empty>`
- 最终 `finish_reason:"stop"`

非法 legacy `function_call` 流：

- 首片被改写为可见提示：`[Proxy blocked undeclared tool calls: powershell]`
- 后续 `Remove-Item` 参数片未泄露
- 没有出现 `function_call` 或 `<empty>`
- 最终 `finish_reason:"stop"`

### `/api/chat` 验收结论

OpenAI SSE 转 Ollama NDJSON 路径也做了黑盒验证：

- 非法 `powershell` 被提示为普通 assistant content
- 后续危险参数未泄露
- 没有出现 `<empty>` 或 `tool_calls`
- 最终 NDJSON 为 `done_reason:"stop"`

### 结论

`v0.2.25` 发布资产本身已经通过黑盒验收。当前建议用户直接升级到 `v0.2.25`，不是只依赖源码测试或本地未打包版本判断。

## 2026-07-10 追加：v0.2.27 / v0.2.28 全局工具边界收紧

### 为什么不能继续局部打补丁

VS Copilot 的真实请求可能同时覆盖以下形态：

- 标准 OpenAI `tool_calls`
- legacy `function_call`
- 流式 `tool_calls` 参数分片
- 流式 legacy `function_call` 参数分片
- 多 `choice.index` 并发流
- DSML 文本工具方言
- OpenAI SSE 转 Ollama NDJSON 的 `/api/chat` 路径
- 上游返回 raw JSON、typed `ChatResponse` 或 fallback 聚合结果

如果只在某一个 handler、某一种模型名、某一种工具名上修复，会继续出现“测试页成功、VS 真实路径失败”或“某个模型正常、另一个模型工具全被拦”的问题。

### 当前最终策略

当前实现采用统一的稳定策略：**标准 OpenAI `tool_calls` / legacy `function_call` 默认透传，不再因为代理识别不到声明而删除工具；DSML 文本方言和工具别名归一化只会转换到当前请求已声明的真实工具。**

允许工具来源只有两个：

1. OpenAI `tools[].function.name`
2. legacy `functions[].name`

如果请求没有声明任何工具，标准 `tool_calls` / legacy `function_call` 仍保持原样透传，避免代理误删；但 DSML 文本方言不会被转换为可执行工具，工具别名也不会被映射到不存在的目标工具。

### v0.2.27 修复点

流式工具状态从只按 `tool.index` 记录，升级为按 `choice.index + tool.index` 记录。

原因：OpenAI SSE 允许多个 choice 同时流式返回；不同 choice 可以同时出现 `tool_calls[0]`。如果只按工具 index 记录，choice 0 的合法工具状态可能污染 choice 1，导致未声明或非法工具被误放行，或合法工具参数续片被误拦截。

### v0.2.28 修复点

请求未声明工具时，不再把响应工具调用视为“无需过滤”。这包括：

- raw OpenAI JSON 里的 `message.tool_calls`
- raw OpenAI JSON 里的 `message.function_call`
- typed `ChatResponse` 里的工具调用
- OpenAI SSE 里的流式 `tool_calls`
- OpenAI SSE 里的流式 legacy `function_call`
- OpenAI SSE 转 Ollama NDJSON 路径

用户可见的空工具名提示也从 `<empty>` 改为 `空工具名`，避免误以为代理内部异常。

### 必须继续保持的发布门禁

工具调用相关改动发布前，必须至少覆盖：

```bash
go test ./internal/proxy -count=1
go test ./... -count=1
go test -race ./internal/proxy -count=1
git diff --check
```

同时需要确认 GitHub Release 中 Windows 包存在，并验证 `vs-ai-proxy.exe --version` 输出为当前 tag。

### 当前推荐版本

Windows + Visual Studio Copilot 用户当前建议升级到 `v0.2.28` 或更新版本。

不要再推荐 `v0.2.24` / `v0.2.25` 作为最终修复版本；这些版本是修复链路中的阶段性版本，不包含后续“多 choice 状态隔离”和“请求无声明工具时阻断所有工具调用”的最终边界收紧。

## 2026-07-10 追加：Windows 启动自动更新超时处理

### 现象

Windows 启动时出现：

```text
启动自动更新检查失败，继续启动当前版本: context deadline exceeded
```

这类错误表示启动阶段访问 GitHub Release 超时。常见原因包括：Windows 网络代理未配置给当前进程、防火墙或安全软件拦截、`api.github.com` 访问慢、GitHub 连接被重置，或启动时直接下载 Release 资产导致超过启动窗口。

### 根因

原启动流程把“检查更新”和“下载/安装更新”放在同一个同步启动窗口中执行。启动超时时间只有 10 秒，而 Windows 用户下载 GitHub Release zip 经常超过 10 秒，所以日志会出现 `context deadline exceeded`。

### 当前策略

启动阶段只做快速 Release 检查；如果发现新版本，下载和安装转入后台执行，不阻塞代理服务启动。

- 检查失败：记录明确原因，继续启动当前版本。
- 检查无更新：正常启动。
- 检查有更新：后台下载并安装；Windows 下仍使用延迟替换脚本，等待当前进程退出后替换并重启。
- 手工 `--self-update` 保持同步行为，适合用户主动升级时使用。

### 用户排障建议

如果仍频繁看到 GitHub 超时，可先在 PowerShell 中验证：

```powershell
Invoke-WebRequest https://api.github.com/repos/qwdingyu/vs-ai-proxy/releases/latest -UseBasicParsing
```

如公司网络或代理环境无法访问 GitHub，可临时关闭启动自动更新：

```powershell
$env:VS_AI_PROXY_AUTO_UPDATE="0"
.\vs-ai-proxy.exe
```

关闭自动更新不会影响代理、模型测试或 VS Copilot 使用，只是不再启动时检查新版本。

## 2026-07-10 追加：`max_output_tokens` 导致工具调用未进入执行阶段

### 现象

真实 Visual Studio Copilot 环境中出现：

```text
POST /v1/chat/completions - 502
模型 gpt-5.5 在提供商 useai2 流式失败: openai stream error: API 错误 400: {"error":{"message":"Unsupported parameter: max_output_tokens","type":"invalid_request_error","param":"","code":null}}
请求 declared: adapt_plan,apply_patch,ask_question,clarify_requirements,code_search,create_file,...
```

用户看到的是“无法运行 `create_file`”，但这次不是工具声明丢失，也不是代理拦截了 `create_file`。日志已经显示请求里声明了工具；真正失败发生在模型响应之前：上游在 `/v1/chat/completions` 阶段拒绝了请求参数。

### 根因

`max_output_tokens` 是 Responses API 常见参数，而当前真实请求走的是 OpenAI Chat Completions 兼容接口：

```text
/v1/chat/completions
```

部分 OpenAI-compatible 网关会宽容忽略未知参数，但 New API、sub2api 或其他网关可能选择严格校验并返回 `400 Unsupported parameter`。一旦上游直接返回 400，模型不会生成任何 `tool_calls`，VS Copilot 就会表现为“工具无法执行”。

### 为什么这不是单个工具问题

这类错误会影响所有工具，不只影响 `create_file`：

- `create_file`
- `apply_patch`
- `powershell`
- `git`
- `code_search`
- `read_file`
- 其他 VS Copilot 声明的工具

只要请求在进入模型前被 400 拒绝，任何工具都没有机会被调用。

### 解决方案

在 OpenAI-compatible provider 的 `/v1/chat/completions` 出口统一做协议归一化：

- 如果请求包含 `max_output_tokens` 且没有 `max_tokens`，转换为 `max_tokens`。
- 删除原始 `max_output_tokens`，避免严格网关返回 400。
- 保留 `tools`、`tool_choice`、`parallel_tool_calls` 和 provider 扩展字段，不能因为参数清洗破坏工具声明。
- 非流式 `ChatRaw` 和流式 `ChatStream` 都必须走同一归一化，因为 VS Copilot 真实工具执行通常是 `stream=true`。

实现位置：

- `internal/provider/provider.go` 中 `marshalOpenAIChatCompletionsRequest`
- `internal/provider/provider.go` 中 `normalizeOpenAIChatCompletionsRequestBody`

回归测试：

- `TestOpenAIProviderChatRawConvertsMaxOutputTokensForChatCompletions`
- `TestOpenAIProviderChatStreamConvertsMaxOutputTokensForChatCompletions`

### 对 New API / sub2api / 其他 OpenAI-compatible 网关是否通用

这个方案是协议层通用方案，不是 New API 特判，也不是针对 `gpt-5.5` 或 `create_file` 的局部补丁。

通用原因：

1. 代理对外接收的是 VS Copilot 请求，对上游发送的是 `/v1/chat/completions`。
2. `/v1/chat/completions` 的通用 token 上限参数是 `max_tokens`；`max_output_tokens` 属于另一类 API 形态或网关扩展。
3. 把 `max_output_tokens` 映射为 `max_tokens`，比直接透传更符合 Chat Completions 兼容网关的最大公约数。
4. New API、sub2api、one-api、LiteLLM、OpenRouter 风格网关都可能对未知参数采取不同策略；在代理边界做规范化能减少网关差异。

边界说明：

- 如果某个上游同时支持 `max_output_tokens` 和 `max_tokens`，当前策略优先保留用户/客户端已经显式给出的 `max_tokens`，只在缺失时用 `max_output_tokens` 补齐。
- 如果未来出现新的严格错误，例如 `Unsupported parameter: parallel_tool_calls`、`reasoning_effort`、`store`、`metadata`，不能盲目删除所有未知字段；应按“Chat Completions 标准 + VS Copilot 必需字段 + provider 已知兼容字段”的原则逐项治理。
- 不能把所有 `Extra` 全删掉，因为 `tool_choice`、`parallel_tool_calls`、provider 路由字段和部分 Copilot 扩展可能依赖未知字段保真。

### 后续排障标准

遇到“工具无法执行”时，不要先按工具名判断；先看上游错误阶段：

1. 如果日志是 `Unsupported parameter: xxx`，说明请求在模型前被网关拒绝，是参数兼容问题。
2. 如果日志显示 `declared:` 中没有目标工具，说明客户端没有声明工具，代理阻断是正确的安全行为。
3. 如果 `declared:` 有目标工具、上游无 4xx、响应也有 `tool_calls`，但工具仍未执行，再排查流式工具分片和 `finish_reason`。
4. 如果上游返回 5xx / `do_request_failed`，属于上游渠道健康或网关路由问题，代理可短重试但不能保证所有渠道一定成功。

### 当前结论

针对这次 `max_output_tokens` 造成的 gpt-5.5 / VS Copilot 工具不可用问题，当前修复是通用协议兼容方案，适用于 New API，也适用于 sub2api 等严格校验 OpenAI-compatible 网关。

## 2026-07-10 追加：Windows 自更新 `.new` / `.ps1` 长期残留

### 现象

Windows 后台自动更新日志显示：

```text
后台已安装新版本: v0.2.30 -> v0.2.31
旧版本备份文件: ...\vs-ai-proxy.exe.bak-20260710091223
已启动后台替换脚本，当前进程退出后会完成替换并重启。
```

但用户目录中长期保留：

- `vs-ai-proxy.exe.new`
- `vs-ai-proxy-self-update.ps1`

同时旧的 `vs-ai-proxy.exe` 仍然是旧版本，`.bak-*` 也没有生成。

### 根因

Windows 不能覆盖正在运行的 exe。原设计让 PowerShell 脚本等待当前进程退出后再执行：

```powershell
while (Get-Process -Id $pidToWait -ErrorAction SilentlyContinue) { Start-Sleep -Milliseconds 200 }
```

但后台自动更新路径启动脚本后，主进程继续运行并继续占用 `vs-ai-proxy.exe`。这导致脚本一直等待 PID 退出，永远走不到：

```powershell
Move-Item $exe $backup
Move-Item $stage $exe
```

因此 `.new` 和 `.ps1` 长期存在，旧 exe 不变。这是自更新流程问题，不是用户误解。

### 修复策略

Windows 后台替换脚本启动成功后，主进程必须主动退出，让脚本解除文件占用并完成替换。

当前策略：

1. 后台下载并暂存新版为 `vs-ai-proxy.exe.new`。
2. 生成 `vs-ai-proxy-self-update.ps1`。
3. 成功启动替换脚本后，通知主进程退出。
4. 主进程优雅关闭 HTTP 服务和请求日志。
5. PowerShell 脚本检测到旧进程退出后：
   - 把旧 exe 移动为 `.bak-*`
   - 把 `.new` 移动为正式 `vs-ai-proxy.exe`
   - 重新启动新版进程
6. 脚本写入 `vs-ai-proxy-self-update.log`，便于排查替换失败原因。

### 排障标准

如果仍看到 `.new` / `.ps1` 残留：

1. 先看 `vs-ai-proxy-self-update.log` 是否有 ERROR。
2. 检查旧进程是否仍在运行或被杀毒软件/Windows Defender 锁定。
3. 检查目录是否有写权限。
4. 手工退出所有 `vs-ai-proxy.exe` 进程后，再查看是否完成 `.bak-*` 和新版 exe 替换。

### 注意事项

- `.bak-*` 只有替换真正发生后才会生成。
- `.new` 存在只代表新版已暂存，不代表替换已完成。
- 自动更新不应只提示“当前进程退出后替换”，而必须在自动更新路径主动触发退出，否则脚本会一直等待。

## 2026-07-11 追加：`run_tests` 等非 VS 原生工具名的兼容策略

### 现象

用户反馈 VS Studio Copilot 开发过程中提示“无法运行 `run_tests`”。这类问题与之前 `create_file`、`powershell`、`git` 不完全相同：

- `create_file`、`powershell`、`git` 通常是 VS/Copilot 请求里已经声明的真实工具。
- `run_tests` 更像模型从其他 Agent/IDE 方言中学到的“语义工具名”，VS/Copilot 真实工具列表里未必存在同名工具。
- 如果代理盲目把 `run_tests` 加入请求工具声明，等于伪造客户端能力，VS 端仍可能无法执行，甚至造成更难排查的问题。

### 最佳实践

不能把所有常见命令名都直接补进 `tools`。代理应遵循三条原则：

1. **不扩权**：只使用客户端请求中已经声明的工具，不能凭空新增可执行工具。
2. **语义别名**：当模型返回 `run_tests`、`shell`、`write_file` 等非 VS 原生名字时，只在目标工具已声明时归一化到真实工具。
3. **稳定透传**：如果没有可匹配的真实工具，保留原始 tool call，不在代理层删除或改成 `<empty>`，让日志和客户端能看到真实问题。

### 当前实现

新增安全工具别名层：

| 模型可能返回 | 仅当已声明以下工具时才映射 |
| --- | --- |
| `run_tests` / `run_test` / `test` | `powershell`、`terminal`、`run_in_terminal` |
| `run_command` / `execute_command` / `exec` | `powershell`、`terminal`、`run_in_terminal` |
| `shell` / `bash` / `cmd` / `command_prompt` / `execute_shell` | `powershell`、`terminal`、`run_in_terminal` |
| `terminal_command` / `run_terminal_cmd` | `powershell`、`terminal`、`run_in_terminal` |
| `install_package` / `build_project` / `run_build` | `powershell`、`terminal`、`run_in_terminal` |
| `dotnet_build` / `dotnet_test` | `powershell`、`terminal`、`run_in_terminal` |
| `npm_install` / `npm_test` / `npm_run` | `powershell`、`terminal`、`run_in_terminal` |
| `run_lint` / `run_formatter` / `format_code` | `powershell`、`terminal`、`run_in_terminal` |
| `git_command` / `git_status` / `git_diff` / `git_log` | `git`、`powershell`、`terminal`、`run_in_terminal` |
| `write_file` / `save_file` | `create_file`、`edit_file`、`apply_patch` |
| `replace_file` / `modify_file` / `update_file` | `edit_file`、`apply_patch` |
| `patch_file` | `apply_patch`、`edit_file` |
| `read_file` / `open_file` / `view_file` / `cat_file` | `get_file`、`file_search` |
| `list_files` / `find_file` | `file_search`、`grep_search`、`code_search` |
| `search_code` / `search_files` / `grep` / `ripgrep` | `code_search`、`grep_search`、`file_search` |

覆盖路径：

- 标准 OpenAI `tool_calls`
- legacy `function_call`
- 流式增量 `tool_calls`
- DSML 方言 `<｜DSML｜invoke name="...">`

### 关键取舍

`run_tests` 不被作为独立工具加入 VS 请求。原因：代理不知道 VS 当前会话是否真的提供该工具，也不知道它的参数 schema。最安全的做法是：

- 如果 VS 声明了 `powershell`，则把 `run_tests` 归一为 `powershell`，保留原参数。
- 如果 VS 声明了 `terminal`，则把 `run_tests` 归一为 `terminal`。
- 如果只声明了 `get_file` / `code_search` 等非执行类工具，则不改名。

这样既能修复“模型误叫常见语义工具名”的问题，又不会扩大工具权限。

刻意不做的映射：

- 不把 `delete_file`、`remove_file`、`rm` 这类高风险破坏性别名泛化映射到文件编辑工具。
- 不把真实工具名 `find_symbol` 降级映射到 `code_search`，避免语义变弱后误导 VS 执行错误工具。
- 不在请求里新增客户端没有声明的工具，只修正模型返回的工具名。

### 新增验证

新增/强化测试覆盖：

- OpenAI JSON 响应中 `run_tests -> powershell`。
- OpenAI 流式 chunk 中 `run_tests -> powershell`。
- legacy `function_call` 中 `run_tests -> powershell`。
- DSML 方言中 `run_tests -> powershell`。
- 未声明 `powershell` / `terminal` 时，不把 `run_tests` 错误映射到不存在的工具。

### 后续排障标准

如果仍提示无法运行某个工具：

1. 先看请求日志 `request_tools`，确认 VS 当前请求到底声明了哪些工具。
2. 再看 `response_tools`，确认模型实际返回了什么工具名。
3. 如果 `response_tools` 里是未声明工具名，优先判断为模型方言漂移，而不是代理拦截。
4. 如发现新的稳定别名，应加入别名表并补四类测试：标准、legacy、流式、DSML。

## 2026-07-11 追加：legacy `function_call:create_file` 仍可能导致“无法运行 create_file”

### 现象

用户反馈 GPT 准备创建文档：

```text
开始写入新的事实版文档：docs/08_当前程序架构与运行流程事实梳理.md
无法运行 create_file
```

这类问题需要区分两种格式：

- modern OpenAI `tool_calls`：当前稳定策略已经默认透传，不会因为未声明而删除 `create_file`。
- legacy `function_call`：部分 GPT / GLM / 兼容网关可能仍返回旧格式 `function_call`，而不是 `tool_calls`。

### 复盘结论

复查代码后发现一个边界问题：

- 标准 `tool_calls` 已按稳定策略透传。
- 流式 `function_call` 已按稳定策略透传。
- 但 raw JSON 非流式路径里的 legacy `message.function_call` 仍保留了旧逻辑：如果工具名未在当前请求声明中匹配，就可能把它删除并写入 `Proxy blocked undeclared tool calls`。

这会造成某些 GPT 响应返回 legacy `function_call:create_file` 时，VS 端看起来像“无法运行 create_file”。

### 修复策略

现在统一为：

1. legacy `function_call` 只在工具名为空时清理，避免空工具名导致 VS 执行异常。
2. 只要 legacy `function_call.name` 非空，例如 `create_file`、`powershell`、`git`，默认稳定透传。
3. 工具别名仍只映射到已声明目标工具，不扩权。
4. DSML 文本方言仍要求目标工具已声明后才转换为可执行工具。

### 新增回归

新增测试覆盖：

- raw JSON 非流式 legacy `function_call:create_file` 即使未声明，也必须透传。
- raw JSON 非流式 legacy `function_call:get_file` 即使未声明，也必须透传。
- OpenAI 标准 `tool_calls`、legacy `function_call`、SSE `tool_calls`、SSE legacy `function_call` 四类协议路径均覆盖 `create_file` / `get_file`。
- 空 name 的 legacy `function_call` 仍会被清理并提示 `空工具名`，避免无效工具调用。

### 2026-07-12 追加：测试不能只停留在“不报错”

本次复盘后明确：工具调用兼容的发版准入不能只验证 HTTP 200、JSON 能解析，或响应字符串中包含 `tool_calls`。
这类测试只能证明“代理没有崩”，不能证明 VS/Copilot 下游真的可以执行工具。

新的准入标准分三层：

1. **代理透传层**：确认代理不会删除、降级或改坏 `tool_calls` / `function_call`。
2. **客户端解析层**：模拟 VS 客户端解析非流式 JSON 与流式 SSE，确保能拿到结构化工具名和参数。
3. **执行语义层**：用最小工具运行时实际执行 `create_file` / `get_file`：先创建内存文件，再读取同一路径并校验内容。

新增 `internal/proxy/tool_execution_e2e_test.go` 专门覆盖执行语义层：

- 非流式 OpenAI `tool_calls`：`create_file` 后接 `get_file`，校验读取内容。
- 非流式 legacy `function_call`：分别执行 `create_file` 与 `get_file`。
- 流式 OpenAI `tool_calls`：解析 SSE 增量后执行创建和读取。
- 流式 legacy `function_call`：解析 SSE 后执行旧格式工具调用。

这组测试的目标不是替代真实 Visual Studio，而是把我们可控边界推到“下游拿到工具后能执行”的层级。
如果这组测试失败，说明不是网络、模型质量或 VS UI 问题，而是代理输出的工具调用已经不具备可执行语义。

### 排障建议

如果后续仍看到“无法运行 create_file”：

1. 查看 `request_tools` 是否包含 `create_file`。
2. 查看 `response_tools` 是否返回了 `create_file`。
3. 如果响应是 `function_call` 而不是 `tool_calls`，确认版本是否包含本修复。
4. 如果代理日志显示 4xx/5xx/timeout，则问题发生在模型响应前，不是 `create_file` 执行阶段。
