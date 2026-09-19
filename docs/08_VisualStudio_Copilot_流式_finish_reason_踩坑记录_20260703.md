# Visual Studio Copilot 流式 `finish_reason` 踩坑记录

> 日期：2026-07-03  
> 关联版本：`0.2.4`、`0.2.5`、`0.2.6`  
> 关联提交：`67e44eb`、`8580f4b`、`d8d870c`  
> 结论：Visual Studio Copilot 的真实调用链路是严格的 OpenAI .NET SDK 流式解析。Web 面板返回 200 只能证明上游可用，不能证明 VS Copilot 客户端可以成功反序列化流式响应。
> 后续：模型身份和 `:latest` 展示策略已在 `docs/09_VisualStudio_Copilot_模型身份与展示名顶层设计_20260703.md` 中收敛；`PROVIDER - model:latest` 是本记录发生时的旧展示形态。

## 1. 本轮真实现象

### 1.1 第一阶段：VS 请求返回 502/503

Visual Studio Copilot 实测日志中出现过类似请求：

```text
POST /v1/chat/completions usecpa DEEPSEEK - deepseek-v4-flash:latest -> DEEPSEEK - deepseek-v4-flash 502
```

同时 VS 日志显示客户端发送的模型名是：

```text
DEEPSEEK - deepseek-v4-flash:latest
```

这不是上游真实模型 ID，而是 Visual Studio 从模型列表里拿到的展示名。此前代理把它当成真实 upstream model 转发，导致上游无法识别。

### 1.2 第二阶段：Web 测试成功，但 VS 仍失败

修复展示名路由后，Web 端测试模型可以正常返回，代理日志也能看到 200：

```text
2026/7/3 16:22:53 POST /v1/chat/completions useai DEEPSEEK - deepseek-v4-flash:latest -> deepseek-v4-flash 200 4112 ms
```

但 Visual Studio Copilot 客户端仍报错：

```text
OpenAI:
CopilotFailure
exception_type    System.ArgumentOutOfRangeException
exception_message Unknown ChatFinishReason value. (Parameter 'value')
Actual value was .
```

后续更完整的堆栈显示失败发生在 OpenAI .NET SDK 的流式响应反序列化阶段：

```text
OpenAI.Chat.ChatFinishReasonExtensions.ToChatFinishReason(String value)
OpenAI.Chat.InternalCreateChatCompletionStreamResponseChoice.DeserializeInternalCreateChatCompletionStreamResponseChoice(...)
OpenAI.Chat.StreamingChatCompletionUpdate.DeserializeStreamingChatCompletionUpdate(...)
OpenAI.AsyncSseUpdateCollection...
Microsoft.Extensions.AI.OpenAIChatClient.FromOpenAIStreamingChatCompletionAsync(...)
Microsoft.VisualStudio.Conversations.CopilotClient.BaseExternChatClient.ChatAsync(...)
```

这个堆栈非常关键：问题不是 HTTP 失败，也不是 provider 没返回内容，而是 VS 客户端在解析 OpenAI SSE chunk 时遇到了非法 `finish_reason`。

## 2. 关键误判点

### 2.1 误判：Web 面板能回复，所以 VS 应该也能用

这是错误的。Web 面板测试只证明：

- provider API key、base URL、模型名大体可用；
- 代理可以完成一次普通请求；
- 上游至少能返回内容。

它不能证明：

- VS Copilot 实际选择的模型名能被正确还原；
- VS 走的是非流式还是流式；
- SSE 每个 `data:` chunk 都符合 OpenAI .NET SDK 的严格枚举；
- VS 客户端能接受所有 provider 私有字段和异常值。

本次就是典型案例：Web 日志显示 200，但 VS 在客户端流式解析阶段失败。

### 2.2 误判：修了非流式 JSON 就等于修了 VS

`0.2.5` 修复了非流式 raw JSON 响应里的空 `finish_reason`：

```json
{
  "choices": [
    {
      "finish_reason": ""
    }
  ]
}
```

该修复对普通 `stream=false` 请求有效，但 VS Copilot 实际走的是流式路径。OpenAI SSE 的响应形态是：

```text
data: {"choices":[{"delta":{},"finish_reason":""}]}

data: [DONE]
```

OpenAI .NET SDK 会逐个解析 `data:` chunk。只要任意 chunk 中出现 `finish_reason:""`，VS 就会在客户端直接抛 `Unknown ChatFinishReason value`。因此只修最终 JSON 不够。

## 3. 根因拆解

### 3.1 根因一：Visual Studio 会回传展示模型名

Visual Studio 模型列表里展示的是类似：

```text
DEEPSEEK - deepseek-v4-flash:latest
```

这个展示名来自 `/api/tags` 或模型发现结果，目的是让用户能区分 provider。但 VS 后续请求可能把这个展示名原样作为 `model` 发回 `/v1/chat/completions`。

代理必须把它解析回真实 upstream model：

```text
DEEPSEEK - deepseek-v4-flash:latest
=> deepseek-v4-flash
```

相关修复：

- 提交：`67e44eb Handle Visual Studio display model names`
- tag：`0.2.4`
- 主要文件：`internal/provider/registry.go`

### 3.2 根因二：OpenAI-compatible provider 可能返回空 `finish_reason`

部分 OpenAI-compatible 上游会返回：

```json
"finish_reason": ""
```

浏览器、curl、Web 面板通常不会因为这个字段失败；但 Visual Studio Copilot 使用 OpenAI .NET SDK，`ChatFinishReason` 是严格枚举。空字符串不是合法枚举值，所以会抛：

```text
Unknown ChatFinishReason value. (Parameter 'value')
Actual value was .
```

相关修复：

- 提交：`8580f4b Normalize OpenAI finish reasons for Visual Studio`
- tag：`0.2.5`
- 主要文件：`internal/proxy/openai_chat_response.go`

### 3.3 根因三：流式 SSE 路径仍然直通了空 `finish_reason`

`0.2.5` 只覆盖了非流式 JSON。真实 VS 堆栈证明客户端失败点在：

```text
StreamingChatCompletionUpdate.DeserializeStreamingChatCompletionUpdate
AsyncSseUpdateCollection
FromOpenAIStreamingChatCompletionAsync
```

这说明 VS 正在解析 SSE 流，而不是普通 JSON。

此前 `streamOpenAI` 的行为是逐行直通上游 SSE：

```text
上游 data: {"choices":[{"finish_reason":""}]}
代理 data: {"choices":[{"finish_reason":""}]}
VS   解析失败
```

最终修复为写出前逐行规范化：

```text
上游 data: {"choices":[{"finish_reason":""}]}
代理 data: {"choices":[{"finish_reason":"stop"}]}
VS   可解析
```

相关修复：

- 提交：`d8d870c Handle Visual Studio streaming finish reasons`
- tag：`0.2.6`
- 主要文件：
  - `internal/proxy/openai_chat_response.go`
  - `internal/proxy/server.go`
  - `internal/proxy/server_test.go`

## 4. 修复策略

### 4.1 `finish_reason` 规范化规则

VS 可接受 OpenAI 标准结束原因：

```text
stop
length
tool_calls
content_filter
function_call
```

代理会把以下值收敛为 `stop`：

```text
""
"null"
"unknown"
其他 provider 私有值
```

这样做的原因：

1. VS 客户端无法接受空字符串或未知枚举。
2. `stop` 是最保守的成功结束原因。
3. 只修正 `finish_reason`，不重建整个响应，避免丢失 `reasoning_content`、`tool_calls`、provider 扩展字段。

### 4.2 非流式路径

非流式 raw OpenAI 响应在写回前调用：

```go
normalizeOpenAIChatResponseForVisualStudio(body)
```

目的：

- 保留上游原始 JSON；
- 只修正 `choices[].finish_reason`；
- 避免 typed struct 重建导致扩展字段丢失。

### 4.3 流式路径

OpenAI SSE 直通路径在写出每一行之前调用：

```go
normalizeOpenAIStreamLineForVisualStudio(line)
```

处理范围：

- 只处理 `data:` 行；
- 跳过空行、注释行、`data: [DONE]`；
- 只修改 `data:` JSON 内的非法 `finish_reason`；
- 保留 SSE 协议外壳和原有流式分片。

这是本次真正解决 VS 报错的关键改动。

## 5. 为什么这不是只为 DeepSeek 做的特殊处理

这次现象发生在 `deepseek-v4-flash`，但修复不能绑定 DeepSeek，原因如下：

1. Visual Studio Copilot 的严格解析来自客户端 SDK，不来自 DeepSeek。
2. 空 `finish_reason` 可能出现在任何 OpenAI-compatible provider。
3. 展示模型名回传也不是 DeepSeek 独有问题，任何 provider 都可能显示为 `PROVIDER - model` 或历史版本中的 `PROVIDER - model:latest`。
4. 本项目目标是扩展 Visual Studio Copilot 支持更多 AI provider，因此兼容层必须做在协议边界，而不是写死某个模型。

所以本次修复位于代理通用 OpenAI 流式/非流式响应路径，而不是 DeepSeek provider 内部。

## 6. 已增加的回归测试

### 6.1 展示模型名解析

覆盖场景：

```text
DEEPSEEK - deepseek-v4-flash:latest
=> deepseek-v4-flash
```

相关测试：

- `internal/provider/registry_test.go`

### 6.2 非流式空 `finish_reason`

覆盖场景：

```json
"finish_reason": ""
```

期望：

```json
"finish_reason": "stop"
```

相关测试：

- `internal/proxy/openai_chat_response_test.go`
- `internal/proxy/integration_test.go`

### 6.3 流式空 `finish_reason`

新增测试：

```go
TestStreamOpenAINormalizesBlankFinishReasonForVisualStudio
```

测试输入：

```text
data: {"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":""}]}
data: [DONE]
```

修复前失败：

```text
blank finish_reason leaked to Visual Studio stream
```

修复后输出包含：

```text
"finish_reason":"stop"
```

## 7. 已执行验证

本地验证命令：

```bash
go test -count=1 ./internal/proxy -run 'TestStreamOpenAINormalizesBlankFinishReasonForVisualStudio|TestStreamOpenAIHandlesLargeSSELine|TestOpenAIChatResponseFinishReason'
go test -count=1 ./...
go build -o /tmp/vs-ai-proxy-server ./cmd/server
git diff --check
./.bin/release-all.sh
```

验证结果：

- 定向回归测试通过。
- 全量 Go 测试通过。
- 本地构建通过。
- diff 空白检查通过。
- 本地生成 `0.2.6` 全平台发布包成功。
- GitHub Actions `main` 构建成功。
- GitHub Actions `0.2.6` tag 构建成功。
- GitHub Release `0.2.6` 已生成。

Release 中确认存在 Windows 包：

```text
vs-ai-proxy-v0.2.6-windows-x64.exe.zip
```

## 8. 排障顺序建议

以后遇到 Visual Studio Copilot 调用失败，不要只看 Web 面板是否能回复。建议按以下顺序排查。

### 8.1 先确认运行版本

访问：

```text
GET /health
```

确认：

```json
{
  "version": "0.2.6"
}
```

如果版本不是预期版本，先换二进制。Windows 机器尤其容易继续运行旧 exe。

### 8.2 看代理日志状态码

- `503`：优先看候选 provider 是否为空、模型是否能路由。
- `502`：优先看上游错误、网络错误、模型不存在或响应格式不兼容。
- `200` 但 VS 报错：优先看客户端解析问题，尤其是 SSE chunk、`finish_reason`、工具调用字段、内容格式。

### 8.3 看诊断响应头

重点关注：

```text
X-Proxy-Requested-Model
X-Proxy-Resolved-Model
X-Proxy-Candidate-Count
X-Proxy-Primary-Provider
X-Proxy-Upstream-Model
```

这些字段用于区分：

- VS 回传的是展示名还是真实模型名；
- 代理解析后的模型名是什么；
- 是否有候选 provider；
- 最终转发到哪个 provider；
- 上游实际收到的模型名是什么。

### 8.4 看 VS 堆栈位置

如果堆栈包含：

```text
StreamingChatCompletionUpdate
AsyncSseUpdateCollection
FromOpenAIStreamingChatCompletionAsync
```

优先怀疑流式 SSE chunk 格式问题。

如果堆栈包含：

```text
ChatFinishReasonExtensions.ToChatFinishReason
Unknown ChatFinishReason value
```

优先检查 `finish_reason` 是否为空字符串、`unknown` 或 provider 私有值。

## 9. 本次上线教训

1. **Visual Studio 真机验收不可替代**  
   curl、Web 测试、Go 单测都不能完全替代 VS Copilot 客户端。VS 使用的 OpenAI .NET SDK 有自己的严格解析规则。

2. **HTTP 200 不等于客户端成功**  
   本次代理日志显示 200，但 VS 客户端仍失败。以后必须把“VS 对话窗口实际返回内容”作为验收条件之一。

3. **流式和非流式必须分别验收**  
   修了非流式 JSON，不代表流式 SSE 已修。Visual Studio Copilot 实际更依赖流式路径。

4. **兼容逻辑应放在协议边界**  
   不要为某个 provider 或模型写死特殊分支。`finish_reason` 规范化应在 OpenAI-compatible 响应边界统一处理。

5. **模型展示名和 upstream model 必须分离**  
   给用户看的名字可以是 `PROVIDER - model`，但发给上游的必须是 provider 真实模型 ID。历史版本曾在展示名后追加 `:latest`，后续已改为仅在 aliases 中保留。

6. **发布后必须确认 Windows 机器运行的是新版本**  
   远端 VS 测试机可能仍在运行旧二进制。`/health.version` 是必须检查项。

## 10. 当前结论

截至 `0.2.6`：

- Visual Studio 展示模型名回传已可解析。
- OpenAI 非流式响应中的空 `finish_reason` 已规范化。
- OpenAI 流式 SSE chunk 中的空 `finish_reason` 已规范化。
- 相关自动化测试、构建、GitHub Actions 和 Release 均已通过。

仍需继续注意：

- 真实 VS Copilot 端到端验收仍应覆盖多个 provider，不应只测 `deepseek-v4-flash`。
- 后续如果接入新 provider，应重点验证流式 SSE 的 `finish_reason`、`tool_calls`、`reasoning_content`、多模态 content part。
- Web 面板测试应继续增强为“VS 等价请求回放器”，否则仍可能出现 Web 成功但 VS 失败的盲区。
