# 测试脚本说明与使用边界

本文档说明 `tests/` 目录下脚本的目的、适用场景、输出解释和注意事项，避免后续把不同测试混用或误解为“所有场景都已覆盖”。

## 1. 测试分层

本项目目前测试分为四层：

| 层级 | 入口 | 目的 | 是否访问真实上游 |
| --- | --- | --- | --- |
| 单元/集成测试 | `go test ./...` | 验证路由、转换、错误分类、API 行为 | 否 |
| Web 国际化运行时测试 | `make i18n-check` | 验证词典加载、插值、DOM 安全和语言属性 | 否 |
| Race 核心测试 | `go test -race ./cmd/server ./internal/proxy ./internal/provider ./internal/config` | 验证核心包并发安全 | 否 |
| 本地流式冒烟 | `tests/streaming_test.sh`、`tests/streaming_ollama_test.sh` | 验证本地代理流式协议、工具分片、Ollama 兼容流 | 否，脚本启动本地 mock 上游 |
| 真实上游大请求诊断 | `tests/useai_large_request_diagnostic.sh`、`tests/large_request_matrix_diagnostic.sh` | 验证真实 provider 在 VS/Copilot 风格大上下文工具请求下的行为 | 是 |

不要用某一层测试替代另一层测试：

- Web 管理页小请求成功，不代表 VS/Copilot 大工具请求成功。
- `go test` 成功，不代表真实上游 key/group/channel 没有限制。
- 大请求真实上游脚本失败，不一定代表代理 bug，必须看 `request_bytes`、`upstream_bytes` 和 direct/proxy 对照。

## 2. 脚本清单

### 2.1 `streaming_test.sh`

用途：

- 构建当前 `cmd/server`。
- 启动本地 mock OpenAI-compatible 上游。
- 启动本地 vs-ai-proxy。
- 发起流式 `POST /v1/chat/completions`。
- 验证代理能正确转发 SSE chunk，并以 `STREAMING_OK` 结束。

适用场景：

- 修改 OpenAI 流式代理代码后。
- 修改 `ChatStream`、SSE 解析、工具调用流式透传后。
- 发布前基础冒烟。

不适用场景：

- 不能证明真实 `useai`、`deepseek`、`usecpa` 上游可用。
- 不能证明大上下文请求不会触发上游 413。

运行：

```bash
bash tests/streaming_test.sh
```

成功标志：

```text
STREAMING_OK
```

### 2.2 `streaming_ollama_test.sh`

用途：

- 构建当前 `cmd/server`。
- 启动本地 mock Ollama 上游。
- 验证 Ollama 风格 `/api/chat` 流式响应转换为 OpenAI-compatible SSE。
- 验证最终 `[DONE]`、usage 和流式内容结构。

适用场景：

- 修改 Ollama provider、Ollama/OpenAI 转换层后。
- 修改流式输出统一逻辑后。
- 发布前基础冒烟。

运行：

```bash
bash tests/streaming_ollama_test.sh
```

成功标志：

```text
STREAMING_OLLAMA_OK
```

### 2.3 `tool_call_release_check.sh`

用途：

- 运行 provider、converter、proxy 的正式工具协议契约测试。
- 覆盖 modern/legacy、raw/SSE、BOM、错误帧、截断、object arguments、DSML 和 Ollama。
- 覆盖 alternate mode 的 legacy `function_call` 保真，以及截断 DSML 不得重新变成可执行工具。
- 覆盖任意声明工具、复杂 schema/history/tool_choice、缺失 id/type 修复、parallel tool calls、identity/arguments 分片、固定种子随机压力和 5 MiB 单 SSE 事件。
- 覆盖 OpenAI/Ollama 空响应、无终态 EOF，以及截断响应同时残留 modern/legacy 调用的防执行规则。
- 覆盖 OpenAI/Ollama 双向转换的逻辑多行 SSE、synthetic finish、截断/残缺工具尾部和 typed fallback 空响应。
- 覆盖有无空行的 `[DONE]` 后上游延迟 EOF、DSML 探测终态、SSE 元数据含 `data:`、refusal-only 合法响应、空 legacy functions 普通聊天回归和独立 wire 客户端解析。
- 覆盖非流式 SSE 在 `[DONE]` 后不等待 EOF、无工具 payload 的伪工具终态降级、跨分片工具 ID 稳定和重复 ID 修复。
- 覆盖 Ollama `options.stop` 映射、原生非流 `done=true` 门禁、实际 raw body 工具参数/终态规范化，以及无效 typed fallback 不污染缓存和诊断。
- 再运行 OpenAI/Ollama 本地流式冒烟。

运行：

```bash
make tool-check
```

成功标志：

```text
TOOL_CALL_RELEASE_CHECK_OK
```

维护规则：能稳定复现工具调用核心缺陷的测试必须保存在仓库，并以 `TestToolProtocolContract` 或对应低层正式测试进入本脚本；禁止只写 `/tmp` 测试或 overlay，验证后再删除。

通用工具契约测试位于 `internal/proxy/copilot_tool_contract_matrix_test.go`。测试中的常规工具名只用于覆盖不同使用场景；生产实现不得读取这份列表或要求新工具先登记。

### 2.4 `i18n_runtime_test.js`

用途：

- 直接执行正式 `web/dist/i18n/*.js`，不复制运行时实现。
- 验证中文、英文查找和占位符插值。
- 验证 `placeholder`、`aria-label`、`alt`、`title` 属性翻译。
- 验证带子元素的节点不会被 `textContent` 清空。
- 验证 `html lang` 与当前语言一致。

运行：

```bash
make i18n-check
```

成功标志：

```text
I18N_RUNTIME_TEST_OK
```

该测试不访问网络，也不引入前端测试框架；修改 i18n 运行时、词典或页面翻译标记后必须执行。

### 2.5 `useai_large_request_diagnostic.sh`

用途：

- 生成一个 VS/Copilot 风格的大请求体。
- 请求体包含工具声明，例如 `create_file`、`apply_patch`、`git`、`powershell`、`get_file` 等。
- 使用同一个请求分别测试：
  1. 直连真实上游 provider。
  2. 经过本地 vs-ai-proxy。
- 输出 direct/proxy 状态码和代理日志中的 `request_bytes`、`upstream_bytes`、`delta_bytes`。

适用场景：

- 排查“Web 测试页成功，但 VS/Copilot 真实使用失败”。
- 排查 `413`、`502`、`client_gone`、`context canceled`。
- 判断代理是否异常放大请求体。
- 判断是代理 bug，还是上游 provider/channel 限制。

默认参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `CONFIG_SOURCE` | `$HOME/.config/vs-ai-proxy/config.json` | 真实配置来源，只读复制到 `.bin` |
| `PROVIDER_ID` | `useai` | provider 实例 ID |
| `DISPLAY_PROVIDER` | `UseAI` | VS 展示名前缀 |
| `MODEL` | `deepseek-v4-flash` | 上游模型名 |
| `TARGET_BYTES` | `1060000` | 目标请求体大小 |
| `INJECT_MODEL_BINDING` | `1` | 是否在临时 config 中注入模型绑定；正式对照建议设为 `0` |
| `PROXY_PORT` | `12346` | 临时代理端口 |
| `DIRECT_TIMEOUT_SECONDS` | `90` | 直连上游超时 |
| `PROXY_TIMEOUT_SECONDS` | `120` | 代理请求超时 |

示例：官方 DeepSeek 1.06MB 对照：

```bash
INJECT_MODEL_BINDING=0 \
PROVIDER_ID=deepseek \
DISPLAY_PROVIDER=deepseek \
MODEL=deepseek-v4-flash \
TARGET_BYTES=1060000 \
DIRECT_TIMEOUT_SECONDS=140 \
PROXY_TIMEOUT_SECONDS=160 \
tests/useai_large_request_diagnostic.sh
```

示例：UseAI 1.06MB 对照：

```bash
INJECT_MODEL_BINDING=0 \
PROVIDER_ID=useai \
DISPLAY_PROVIDER=UseAI \
MODEL=deepseek-v4-flash \
TARGET_BYTES=1060000 \
DIRECT_TIMEOUT_SECONDS=140 \
PROXY_TIMEOUT_SECONDS=160 \
tests/useai_large_request_diagnostic.sh
```

输出文件：

```text
.bin/useai-large-diagnostic/result.json
.bin/useai-large-diagnostic/logs.json
.bin/useai-large-diagnostic/direct.out
.bin/useai-large-diagnostic/proxy.out
```

关键字段解释：

| 字段 | 含义 | 如何判断 |
| --- | --- | --- |
| `direct_status` | 直连上游状态码 | 如果直连失败，优先查上游 provider/channel |
| `direct_elapsed_ms` | 直连上游总耗时 | 用于和代理耗时对比，粗略判断额外转发成本 |
| `proxy_status` | 经过代理状态码 | 与 direct 对比判断代理是否引入差异 |
| `proxy_elapsed_ms` | 经过代理总耗时 | 大幅高于 direct 时再排查代理或本机网络 |
| `request_bytes` | 客户端进入代理的请求体大小 | VS/Copilot 真实大上下文通常显著大于 Web 测试页 |
| `upstream_bytes` | 代理发给上游的 JSON 请求体大小 | 用于判断代理是否异常膨胀 |
| `delta_bytes` | `upstream_bytes - request_bytes` | 很小表示代理没有明显放大 |
| `error_code` | 代理分类后的错误 | 例如 `upstream_payload_too_large`、`client_deadline_reached` |
| `cancel_reason` | 499/取消类请求的进一步原因 | `client_deadline_reached` 表示接近 VS/Copilot 等待上限 |
| `network_peer` | 网络错误中解析出的远端 IP:port | 如果是 `104.21.x.x` / `172.67.x.x`，通常是 Cloudflare/CDN 边缘 IP，不是源站 IP |
| `stream_state` | 流式请求进度 | `upstream_connecting`、`upstream_connected`、`downstream_started` |
| `upstream_stage` | provider HTTP 失败时当前 hop 的最后网络阶段 | `preparing_request`、`resolving_dns`、`connecting`、`tls_handshake`、`writing_request`、`waiting_response_headers`、`receiving_response_headers` |

判断规则：

- `direct=200` 且 `proxy=200`：代理和上游都可承载该请求。
- `direct=413/502` 且 `proxy=502`，同时 `delta_bytes` 很小：优先判定为上游链路限制，不是代理体积膨胀。
- `direct=200` 但 `proxy!=200`：重点排查代理路由、参数转换、模型名映射、stream 处理。
- `upstream_bytes` 明显大于 `request_bytes`：重点排查 request transformer、reasoning 注入、参数覆盖。
- `error_code=client_deadline_reached` 且耗时接近 100 秒：优先排查上游首 token、new-api 渠道排队/重试和客户端等待上限。
- `error_code=network_error` 且 `network_peer` 是 CDN IP：优先排查客户端到 CDN、CDN 到源站、Cloudflare/WAF 或边缘连接关闭，不要直接当成 new-api 源站 IP。
- `direct_elapsed_ms` 和 `proxy_elapsed_ms` 都接近 100 秒：优先查上游/客户端等待上限；只有 proxy 明显更慢时才优先怀疑代理。
- `stream_state=upstream_connecting`：尚未收到上游 HTTP 响应头/可读流；可能包含上传、连接、TLS、CDN/WAF、网关排队和首 token 等待，不能只凭该字段归因 DNS/TCP。
- `upstream_stage=waiting_response_headers`：请求已经写完但没有收到响应头首字节；优先查网关排队、渠道选择、模型首响应和大上下文能力，不再优先怀疑本地上传。
- `upstream_stage=preparing_request`：请求尚未进入可观测网络阶段，优先检查 URL 解析、请求构造或极短预算。
- `upstream_stage=receiving_response_headers`：已经收到响应头首字节但响应建立未完成，优先查重定向、代理中间层和连接关闭。
- `upstream_stage=resolving_dns/connecting/tls_handshake`：优先查 Windows DNS、系统代理、安全软件、证书链和 CDN 网络。
- `stream_state=upstream_connected`：已连上上游但未向 VS 写出首个 chunk；优先查上游首 token 或 new-api 渠道排队。
- `stream_state=downstream_started`：已经向 VS 写出过 chunk；后续失败不能安全自动重试。

### 2.6 `large_request_matrix_diagnostic.sh`

用途：

- 批量复用 `useai_large_request_diagnostic.sh`。
- 一次性比较多个 provider、多个模型、多个请求体大小。
- 避免每次临时写一堆 curl/Python，降低误判和漂移。

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

自定义真实配置矩阵：

```bash
SIZES='50000' \
CASES=$'deepseek|deepseek|deepseek-v4-flash\nuseai|UseAI|deepseek-v4-flash\nusecpa|UseCpa|step-3.7-flash\nuseai2|UseAI2|gpt-5.5' \
tests/large_request_matrix_diagnostic.sh
```

结果文件：

```text
.bin/large-request-matrix/results.jsonl
.bin/large-request-matrix/summary.json
```

摘要示例：

```text
deepseek deepseek-v4-flash 1060000 direct=200 proxy=200 code=- req=1077092 up=1077127 delta=35
useai    deepseek-v4-flash 1060000 direct=502 proxy=502 code=upstream_payload_too_large req=1077089 up=1077099 delta=10
```

### 2.7 `model_release_diagnostic.sh`

用途：

- 新模型发布或 provider 新接入后，统一执行真实上游 direct/proxy 对照。
- 默认覆盖小请求、中请求、真实问题请求大小，避免只用 Web 测试页小请求误判。
- 作为 GLM、Kimi、MiMo、DeepSeek 等模型复测的固定入口，不再临时手写脚本。

默认值：

```text
PROVIDER_ID=useai
DISPLAY_PROVIDER=UseAI
MODEL=glm-5.2
SIZES='30000 200000 673100'
INJECT_MODEL_BINDING=1
MODE=direct-proxy
COOLDOWN_SECONDS=0
```

GLM 示例：

```bash
PROVIDER_ID=useai \
DISPLAY_PROVIDER=UseAI \
MODEL=glm-5.2 \
SIZES='30000 200000 673100 1060000' \
tests/model_release_diagnostic.sh
```

频控严格模型的冷却后代理单独验证：

```bash
PROVIDER_ID=useai \
DISPLAY_PROVIDER=UseAI \
MODEL=glm-5.2 \
SIZES='30000 673100' \
MODE=proxy-only \
COOLDOWN_SECONDS=20 \
tests/model_release_diagnostic.sh
```

其它模型示例：

```bash
PROVIDER_ID=kimi \
DISPLAY_PROVIDER=Kimi \
MODEL=kimi-k2 \
SIZES='30000 200000 673100' \
tests/model_release_diagnostic.sh
```

重要判断：

- `direct=429` 表示上游或聚合商已经限流。
- `direct=200` 后紧接着 `proxy=upstream_rate_limit`，可能是 direct/proxy 连续两次请求触发短窗口频控；使用 `MODE=proxy-only COOLDOWN_SECONDS=20` 再确认。
- `upstream_bytes - request_bytes` 很小，说明代理没有明显放大请求体。
- 必须按 `request_id` 关联日志，不要把相邻时间的不同请求合并判断。

完整方法论见：

```text
docs/36_GLM与新模型真实上游限流诊断方法_20260719.md
```

## 3. 典型使用场景

### 3.1 发布前本地基础验证

```bash
go test ./... -count=1
go test -race ./cmd/server ./internal/proxy ./internal/provider ./internal/config -count=1
make tool-check
```

适合每次 release 前执行。

### 3.2 排查工具调用异常

先跑：

```bash
tests/tool_call_release_check.sh
```

该脚本是发布前工具调用专项核查入口，覆盖：

- Provider JSON 保真：`tools`、`tool_calls`、legacy `function_call`、object arguments、嵌套 unknown 字段。
- Proxy 归一化：OpenAI `tool_calls`、legacy `function_call`、流式工具参数分片、DSML 方言。
- 常见 VS/Copilot 工具别名：`run_tests`、`dotnet_test`、`npm_test`、`write_file`、`apply_diff`、`read_file`、`read_files`、`git_diff`、`search_symbol` 等只会映射到当前请求已声明且语义兼容的目标工具；不会把 `delete_file` 这类文件语义工具降级成 `powershell` 以避免参数 schema 不匹配。
- E2E 执行语义：模拟 `create_file` 后 `get_file`，确认客户端拿到的是可执行 JSON 参数，而不是仅“不报错”。
- 流式业务 smoke：OpenAI-compatible SSE 和 Ollama-to-OpenAI SSE 转换不丢 chunk。

如果真实 VS/Copilot 仍失败，再跑大请求：

```bash
SIZES='50000 800000 1060000' \
CASES=$'deepseek|deepseek|deepseek-v4-flash\nuseai|UseAI|deepseek-v4-flash' \
tests/large_request_matrix_diagnostic.sh
```

### 3.3 排查 provider/model 张冠李戴

观察矩阵输出中的 `provider` 和 `upstream`：

- `deepseek|deepseek|deepseek-v4-flash` 应该 upstream=`deepseek-v4-flash`。
- `useai|UseAI|deepseek-v4-flash` 应该 upstream=`deepseek-v4-flash`。
- `usecpa|UseCpa|step-3.7-flash` 应该 upstream=`step-3.7-flash`。
- `useai2|UseAI2|gpt-5.5` 应该 upstream=`gpt-5.5`。

如果 `upstream` 变成 `deepseek/deepseek-v4-flash`、`z-ai/...` 或其他非预期值，说明模型解析链路可能回归。

### 3.4 排查 413 / 大上下文失败

至少比较小、中、大三个档位：

```bash
SIZES='50000 800000 1060000' tests/large_request_matrix_diagnostic.sh
```

结论示例：

- 50KB 成功，800KB 成功，1.06MB 失败：通常是上游链路大小阈值。
- 官方 provider 1.06MB 成功，聚合 provider 1.06MB 失败：优先查聚合网关的 body/context/channel 限制。
- direct 失败且 proxy 失败，`delta_bytes` 很小：不要优先怀疑代理体积膨胀。

## 4. 代理相对直连的负担与效率评估

### 4.1 客观结论

vs-ai-proxy 一定会比直连多一层本地 HTTP 转发和 JSON 处理，因此理论上不可能比直连更快。但在真实 VS/Copilot 场景中，主要耗时通常来自：

1. 上游模型首 token 延迟。
2. 上游推理时间。
3. 网络链路和聚合网关排队。
4. 大上下文上传时间。
5. 客户端等待上限。

本地代理自身的额外开销通常很小，更多是毫秒级到几十毫秒级；对几秒到几十秒的模型请求来说，通常不是主要瓶颈。

### 4.2 从当前观测数据看

本轮大请求脚本显示：

- 官方 deepseek 1.06MB：`request_bytes=1077092`，`upstream_bytes=1077127`，`delta_bytes=35`。
- UseAI 1.06MB：`request_bytes=1077089`，`upstream_bytes=1077099`，`delta_bytes=10`。
- 小请求矩阵中代理正常返回，`delta_bytes` 约 `9~35` 字节。

这说明：

- 代理没有明显放大请求体。
- 代理没有给大请求额外增加大量 payload。
- 当前大请求失败的主要矛盾不是“代理太重”，而是上游 provider/channel 对大请求的限制或波动。

### 4.3 代理会增加哪些成本

| 成本 | 说明 | 大致影响 |
| --- | --- | --- |
| 本地 HTTP 入站/出站 | VS -> proxy -> upstream | 通常很小 |
| JSON 解码/编码 | 需要读取请求、治理参数、记录日志 | 与请求体大小相关，1MB 请求也通常远低于模型耗时 |
| 流式转发 | 读取上游 SSE 并写回客户端 | 主要是 I/O 转发，通常不是瓶颈 |
| 日志记录 | 记录请求状态、字节数、错误分类 | 可控，必要的可观测性成本 |
| 参数治理 | 过滤/转换不兼容参数 | 换来兼容性，成本小 |

### 4.4 代理带来的收益

| 收益 | 说明 |
| --- | --- |
| 模型展示名适配 | 支持 VS/Copilot 的 `Provider - model` 模型名 |
| 多 provider 管理 | 同一端口聚合官方、UseAI、UseCpa、Ollama 等 |
| 工具调用兼容 | 治理 OpenAI tool_calls、legacy function_call、流式工具分片、DSML 方言 |
| 参数兼容 | 处理 `max_tokens`、`max_output_tokens`、`reasoning_effort` 等模型差异 |
| 错误分类 | 区分 413、429、client_gone、client_deadline、upstream_server_error |
| 断连观测 | 记录 `cancel_reason` 和 `network_peer`，便于区分客户端超时、用户取消、CDN/网络断开 |
| 可观测性 | 记录 provider、upstream、request_bytes、upstream_bytes、tools |
| 防误路由 | provider 展示名前缀固定路由，避免跨 provider 张冠李戴 |

### 4.5 是否“加重负担”的评判

客观评判：

- 如果只是普通 OpenAI-compatible 单 provider、小请求聊天，直连最轻，代理不是必要路径。
- 如果目标是 Visual Studio Copilot BYOM、工具调用、多 provider、new-api/sub2api 兼容、错误可观测，代理带来的收益明显大于本地转发开销。
- 当前已观测的主要失败点不是代理性能开销，而是上游 provider/channel 的大请求限制、超时、网关波动和模型参数兼容。
- 对用户体验来说，代理最重要的职责不是“零开销”，而是“稳定、可诊断、少误路由、工具调用正确”。

### 4.6 后续性能观测建议

如果后续要量化“比直连慢多少”，建议增加固定基准：

1. 同一 provider、同一模型、同一请求体。
2. 分别测 direct 和 proxy。
3. 记录：
   - total elapsed
   - time to first byte / first SSE chunk
   - request_bytes
   - upstream_bytes
   - status code
   - error_code
4. 每组至少跑 5 次，取中位数，不要用单次结果判断。
5. 对小请求和大请求分别统计。

现有 `useai_large_request_diagnostic.sh` 已具备 direct/proxy 对照能力，但它目前主要用于功能和限制诊断，不是严格性能 benchmark。若要做严谨性能结论，应新增专门 benchmark 脚本，避免和故障诊断混用。
