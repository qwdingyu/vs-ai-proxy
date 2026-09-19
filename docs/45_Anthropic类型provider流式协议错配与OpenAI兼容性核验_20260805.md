# VS Copilot 无法使用与 Anthropic 类型 provider 流式协议错配复盘

用户反馈"升级后管理测试页可以测试，但 Visual Studio Copilot 用不了"。本次排查确认：
问题**只影响 `type=anthropic` 的 provider**，OpenAI 兼容路径没有被破坏。下面记录根因、
为何 OpenAI 也"看起来"出问题、以及排查方法。

## 结论速览

| 项目 | 结论 |
| --- | --- |
| 受影响范围 | 仅 `type=anthropic` 的 provider（本次 8 个模型） |
| OpenAI 兼容路径 | **未被破坏**，本次改动为纯新增（0 行修改既有代码） |
| 是否是回归 | **不是**。该缺陷从 anthropic 支持引入起就存在，从未在流式下工作过 |
| 测试页为何正常 | 测试页固定 `stream=false`，走的是另一条已正确的分支 |

## 根因

`handleChatCompletions` 里流式分支在 anthropic 判断**之前**就 `return` 了：

```go
if req.Stream {
    // ... 直接走 handleStream，按 provider.ResolveApiFormat 分流
    return
}
// anthropic 判断在这里，流式请求永远到不了
providerType := getProviderTypeFromConfig(cfg, prov.Name())
```

而 anthropic 类型 provider 在 `providerFromConfig` 中被构造成 `*OpenAIProvider`，
`ResolveApiFormat` 因此返回 `ApiFormatOpenAi`。结果同时错两头：

1. **上游收到 OpenAI 线格式**：没有顶层 `system`、没有 `anthropic-version`、没有 `x-api-key`
2. **下游收到 Anthropic 原生 SSE**：`dsml_stream.go` 的 `flushEvent` 会把非 `data:` 行原样重发，
   于是 VS 收到 `event: message_start` / `content_block_delta`，**没有 `choices`、没有 `[DONE]`**

Visual Studio Copilot 的 BYOM 固定使用 `stream: true`，因此必然命中这条错误路径；
管理测试页用 `stream=false`，走的是已正确的非流式 anthropic 分支——这就是
"能测试、不能用"的全部原因。

## 为什么 OpenAI 兼容模式也会"看起来"出问题

需要区分三件不同的事，避免误判：

1. **OpenAI 分流逻辑没有被改。** 自 anthropic 工作开始（`313d239` 起）到本次修复，
   `handleStream` / `streamOpenAI` / `dsml_stream.go` / `openai_chat_response.go`
   从未被修改。可用下面命令复核：

   ```bash
   git log --oneline 313d239..HEAD -- internal/proxy/dsml_stream.go internal/proxy/openai_chat_response.go
   ```

2. **`83c5220` 确实改了 OpenAI 路径上的一个错误文案**（`tool_response_normalizer.go`）：
   把 `解析响应失败: upstream returned error object: ...` 改成 `API 错误 200: ...`。
   该改动只影响**诊断分类标签**，不影响行为——两个分类都不在
   `shouldStopCandidateFallback` 的提前终止列表中，故障转移决策完全相同。
   已用测试验证：新旧文案 → `upstream_api_error` / `proxy_parse_error` → 停止故障转移均为 `false`。

3. **真正的混淆来源是 provider 配置**。若 VS 里选择的模型绑定在 anthropic 类型 provider 上，
   即使它"看起来是普通模型名"（如 LongCat / claude-*），也会走 anthropic 分支。
   反过来，`default_model` 若是 openai 类型，则不受影响。

## 一个需要留意的历史改动（本次确认对当前配置无影响）

`a7bf3d0` 把 provider 构造从"能力表推导路径"改为"使用实例 transport"：

```go
provider.NewOpenAIProviderWithTransport(
    id, capability, p.APIKey, p.BaseURL,
    p.Transport.ChatPath, p.Transport.ModelsPath, p.Enabled, timeout,
)
```

这意味着 `transport.chat_path` 为空时会退回能力默认值，可能改变上游 URL。
**当前用户配置里所有 provider 的 `chat_path` / `models_path` 均已显式填好**，
因此不构成问题。若未来出现"升级后所有模型 404"，这是第一个要查的地方。

## 排查顺序（下次遇到"VS 不能用"）

1. 看管理页日志里该请求的 `provider` 字段，确认它是 openai 还是 anthropic 类型
2. 用 `curl` 直接打 `/v1/chat/completions` 并带 `stream: true`，检查响应里是否有
   `"object":"chat.completion.chunk"`、`choices[].delta`、结尾 `data: [DONE]`
3. 若出现 `event:` 行或 `content_block_delta`，说明 Anthropic SSE 泄漏到了下游
4. 对比测试页（`stream=false`）是否正常——若正常而流式异常，优先怀疑流式分支的协议分流

## 本次修复要点

- 在流式分支**最前面**加 anthropic 判断，与非流式保持同一语义
- 复用非流式已验证的转换 helper，避免出现第二套只在流式下生效的语义
- 读取上游 Anthropic SSE 时：多行 `data:` 需拼接；`message_stop` 作为应用层终态即结束读取
  （不能等 EOF，也不能依赖结尾换行符）；解析错误必须回传，不能被吞成"缺少内容"
- 工具参数按**协议 index** 而非切片位置索引（上游 index 不保证从 0 连续）

## 未验证项

- 没有做真实 Visual Studio / Copilot UI 复现（此环境无法完成）
- 结论基于 HTTP/SSE 协议层验证 + 真实二进制端到端测试

## 相关文档

- `08_VisualStudio_Copilot_流式_finish_reason_踩坑记录_20260703.md` — 流式终态契约
- `22_VS_Copilot工具调用响应协议统一与测试盲区_20260714.md` — 工具协议与测试盲区
- `26_Windows真实超时与流式终态发布阻断复盘_20260714.md` — 流式终态阻断
- `43_近期提交专家审查与协议转发稳定性评估_20260727.md` — 协议转发稳定性评估
