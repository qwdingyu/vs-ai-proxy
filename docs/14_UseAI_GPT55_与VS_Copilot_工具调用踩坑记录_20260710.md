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
- 管理页面按钮文案曾使用“连接测试”，容易让用户误以为是网络连通性测试，而实际只是读取 provider 官方模型列表。

## 关键结论

1. `/v1/models` 成功只代表 **Base URL/API Key 可以读取官方模型列表**，不代表每个模型都能完成一次对话。
2. `useai/gpt-5.5` 当前不是“模型不存在”，而是上游 New API 渠道/协议链路存在不稳定：非流式可能失败，流式通常可用。
3. New API 虽然可以配置多个渠道，但实测某些单渠道失败会直接透出到客户端，因此代理侧必须做有限兜底。
4. VS Copilot 下游比普通 Web/curl 更严格：
   - 非流式必须返回 JSON，不能返回 `data:` SSE。
   - `finish_reason` 不能是空字符串或未知值。
   - 工具调用字段必须尽量原样保留，不能丢 `strict`、`tool_choice`、`parallel_tool_calls`、嵌套扩展字段。

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
7. `web/dist/assets/images/qrcode_qq.png` 当前是未跟踪文件，非本次修复范围，不要误加入提交。

## 相关文件速查

- Provider OpenAI 请求、重试、SSE 兼容：`internal/provider/provider.go`
- Chat request/tool calling JSON 保真：`internal/provider/chat_request_json.go`
- 真实代理 OpenAI 响应归一化：`internal/proxy/openai_chat_response.go`
- 真实代理路由与 raw OpenAI 透传：`internal/proxy/server.go`
- 管理测试接口：`internal/api/api.go`
- 管理页面文案、按钮防抖、模型下拉：`web/dist/index.html`
