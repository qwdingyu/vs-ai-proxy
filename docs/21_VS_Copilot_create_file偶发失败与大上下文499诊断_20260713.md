# VS Copilot `create_file` 偶发失败与大上下文 499 诊断记录（2026-07-13）

## 背景

用户反馈：VS Studio / Copilot 使用过程中，偶尔仍会出现“无法运行 `create_file`”。该现象不是每次必现；同一时间切换到其他模型后，文件创建又能正常完成并返回 200。

典型日志样本：

```text
[23:09:54 WARN] 模型 gpt-5.5 在提供商 useai2 流式失败: context canceled
[23:09:54 INFO] POST /v1/chat/completions - 499 (52649 ms) request_id=1783955342319450400 error_code=client_gone reason="客户端主动断开" action="若耗时很短，多为用户取消/窗口关闭；若接近 100 秒，按客户端等待上限排查。 本次请求体/上游体约 804.5 KB，属于大上下文；如果新建 session 后恢复，优先怀疑旧 session 历史膨胀、文件堆积或状态污染。" summary="客户端在响应完成前断开；耗时 52649ms；请求体 790.1 KB；上游体 804.5 KB；流状态 downstream_started；本次请求体/上游体约 804.5 KB，属于大上下文；如果新建 session 后恢复，优先怀疑旧 session 历史膨胀、文件堆积或状态污染。"
[23:10:53 INFO] POST /v1/chat/completions - 200 (41482 ms) request_id=1783955412469250500
```

## 关键定论

这类日志不能简单归因为“`create_file` 工具没有注册”或“代理拦截了 `create_file`”。本次样本显示的是：

1. 请求已经进入流式链路。
2. 代理已经开始向 VS/Copilot 下游写出数据（`stream_state=downstream_started`）。
3. 最终状态是 `499 client_gone`，表示客户端在响应完成前断开或取消。
4. 请求体约 790 KB、上游体约 804.5 KB，属于大上下文压力场景。
5. 切换模型后成功，说明代理全局工具调用链路并未完全失效；更可能是特定 provider/model/channel 在该上下文下响应过慢或工具调用未能完整返回。

因此，本次样本的最准确归类是：

> 大上下文 + 慢模型/慢渠道 + VS/Copilot 客户端提前断开，导致工具调用没有完整完成。用户看到的“无法运行 create_file”是结果，不是根因。

## 为什么会“偶发”

`create_file` 的完整链路有多条路径：

| 路径 | 状态 | 风险 |
| --- | --- | --- |
| 标准非流式 JSON `tool_calls` | 已覆盖 | 相对稳定 |
| legacy `function_call` | 已覆盖 | 需要继续保真 |
| 标准流式 SSE `tool_calls` 分片 | 已覆盖 | 对分片合并要求高 |
| DSML 文本方言 | 已覆盖 | 只在请求声明工具时转换 |
| `stream=false` 但上游返回 SSE | 历史上主要为文本兜底 | 如果 SSE 内含工具调用，可能丢失工具调用 |
| 499 / context canceled | 不是工具协议问题 | 客户端断开导致工具调用无法完成 |

偶发的根本原因在于：不同 provider/model/channel 对同一请求的响应模式和速度不同。某些上游会在 `stream=false` 时返回 SSE；某些渠道首 token 慢；某些模型在大上下文工具任务下生成工具调用更慢。只要 VS/Copilot 在工具调用完整返回前断开，下游就会表现为工具不可用。

## 对当前日志的专业判断

针对 23:09:54 样本：

- `499` + `context canceled`：客户端/下游断开，不是代理主动失败。
- `52649 ms`：已经偏慢，但没有到约 100 秒硬等待上限，可能是 VS 窗口、会话、请求生命周期或用户侧操作提前取消。
- `downstream_started`：代理已经向下游开始写流，不适合再切 provider；此时强行 fallback 会破坏 SSE 协议。
- `790 KB / 804.5 KB`：大上下文会放大首 token 延迟、工具参数生成耗时、网络中断概率和 VS 客户端等待不稳定性。
- 后续切换模型 200：说明应支持“模型稳定性差异”的诊断，而不是把所有问题归为工具缺失。

## 不能做的错误方案

1. **不要继续只补 `create_file` 别名。** `create_file` 是 canonical 工具，重复补别名解决不了 499 断流。
2. **不要在 `downstream_started` 后自动切换 provider/model。** 下游已经收到部分 SSE，切换会造成混流和协议损坏。
3. **不要把所有 499 都写成用户取消。** 大上下文工具任务下的 499 很多是“模型响应慢导致客户端生命周期结束”。
4. **不要把搜索/文件工具降级到 powershell。** 这会造成 schema 不匹配，反而产生新的“工具能调用但无法执行”。
5. **不要为了掩盖失败自动伪造 `create_file` 调用。** 工具调用必须来自模型标准输出或已声明方言转换。

## 最优方案

### 1. 诊断优先：把“工具未完成”一眼展示出来

当满足以下条件时：

- 请求声明了工具（尤其是 `create_file`、`apply_patch`、`edit_file`、`get_file`、`grep_search`、`run_command_in_terminal` 等）；
- 响应没有记录到 `response_tools`；
- 状态码为 499、timeout、client_deadline_reached，或上游流式 context canceled；
- 请求体/上游体超过 512 KB；

日志应明确提示：

> 本次请求声明了工具，但响应未完整返回工具调用；结合 499/context canceled 和大上下文，优先判断为模型/渠道响应过慢或客户端提前断开，而不是工具未注册。

### 2. 协议保真：补齐 `stream=false` 返回 SSE 的工具聚合

历史兼容逻辑会把 `stream=false` 却返回 SSE 的上游响应聚合成标准 JSON，以避免 VS 解析 `data:` 失败。该聚合必须和正式流式路径一致：

- 保留 `content`；
- 保留 `reasoning`；
- 合并 `tool_calls` 分片；
- 支持 legacy / DSML 方言；
- 设置 `finish_reason=tool_calls`；
- 执行工具名同族别名归一化。

否则上游实际返回了 `create_file`，代理却在兼容转换中丢掉它，会制造“偶发工具不可用”。

### 3. 策略建议：工具任务优先稳定模型

对于 VS/Copilot 工具调用任务，尤其是大上下文：

- 优先使用历史验证 `response_tools` 稳定的 provider/model。
- 对大上下文工具任务给出“新建 session / 减少打开文件 / 切换稳定工具模型”的提示。
- 不建议在已开始下游流后 fallback；只允许在未写下游前做候选切换。

### 4. 测试准入

发布前至少覆盖：

1. `stream=false` 上游返回 SSE 且包含完整 `create_file tool_calls`，转换后仍保留 `message.tool_calls`。
2. `stream=false` 上游返回 SSE 且 `create_file.arguments` 分片，转换后参数仍是合法 JSON。
3. 请求声明 `create_file` 但 499 且无 `response_tools` 时，日志诊断包含“工具调用未完整返回”。
4. 正常流式 `create_file` 不受影响。
5. `make tool-check` 和 `make release-check` 通过。

## 客服/维护排查顺序

遇到“无法运行 `create_file`”时，不要先猜工具缺失，应按顺序看：

1. `status_code`：如果是 499/timeout，先按断流/超时排查。
2. `request_tools`：确认请求是否声明了 `create_file`。
3. `response_tools`：确认模型是否实际返回了 `create_file`。
4. `stream_state`：如果是 `downstream_started`，说明代理已开始写下游，不能安全 fallback。
5. `request_bytes/upstream_bytes`：超过 512 KB 时优先怀疑 session 历史膨胀和文件上下文过大。
6. 对比切换模型/新 session 后是否恢复：如果恢复，优先判断为模型/渠道稳定性或上下文压力，而不是代理全局工具链路损坏。

## 本次建议落地项

- 增加 `stream=false` SSE 工具聚合测试。
- 增强日志诊断，在工具声明存在但响应工具缺失且请求失败时给出明确原因。
- 保留最小协议修复，不引入新的 provider 策略、不新增复杂自动切换。
- 发布前完成 `make tool-check`、`make release-check`。
