# API-Switch 模型 Token 统计分析与实施方案（2026-07-17）

## 1. 结论

API-Switch 的 token 统计有三项值得借鉴：

1. 计量发生在协议解析和流生命周期内，而不是只看 HTTP 状态码。
2. 非流式响应、OpenAI SSE、Anthropic、Responses 等协议先归一化，再进入统计。
3. 流式请求使用共享状态和终结守卫，覆盖正常结束、读取错误、超时和客户端丢弃。

但 API-Switch 不能整套照搬到 `vs-ai-proxy`。其现有实现把“上游未返回 usage”写成 `0`，失败重试按每次上游尝试写多条日志，缓存 token 和推理 token 没有稳定的独立持久化口径，流式异步落库也不是数据库意义上的 exactly-once。直接复制会破坏本项目现有的“一次客户端请求只生成一条请求日志”语义，并制造看似精确、实际不可审计的数字。

本项目采用以下方案：

- 只采集上游明确返回的 token usage，不在本地估算。
- `usage` 不存在表示 unknown；`usage` 存在且各字段为 `0` 才表示上游报告了真实零值。
- 每次客户端请求仍只写一条 `RequestLog`；候选重试不会重复累计最终响应。
- `prompt_tokens`/`completion_tokens` 是主计量；`cached_tokens` 是输入子集，`reasoning_tokens` 是输出子集，不能再次加到 `total_tokens`。
- 同时支持 OpenAI 非流式 JSON、OpenAI SSE 最终 usage、Ollama 原生计数，以及流式/非流式互相兜底后的最终响应。
- 最近请求明细继续保存在有限日志环中；累计统计和按模型统计写入版本化 sidecar 快照，重启后不因日志截断而丢失。
- 不引入 SQLite 或新依赖。本次产品需要的是本机累计观测，不是按任意时间区间查询的账单审计系统。

## 2. 核验范围

本次只读核验的 API-Switch 基线：

- 仓库：`/Users/dingyuwang/0-X/5-rust/API-Switch`
- Git：`23c3d4e258c95cf98fc8280b0dc3aa0e8c9340b6`
- 仓库当时存在用户自己的未提交文件；本次未修改该仓库。

本项目实施基线：

- 仓库：`/Users/dingyuwang/0-X/4-go/vs-ai-proxy`
- Git：`1bb94770244f9da5835b2805e8cbb15ca73919b9`

重点核验文件：

| 项目 | 文件 | 作用 |
| --- | --- | --- |
| API-Switch | `src-tauri/src/proxy/forwarder.rs` | 非流和流式 usage 抽取、重试、`StreamLogGuard`、日志终结 |
| API-Switch | `src-tauri/src/proxy/protocol/*.rs` | OpenAI、Claude、Gemini、Responses 等协议归一化 |
| API-Switch | `src-tauri/src/database/schema.rs` | `usage_logs` 表和索引 |
| API-Switch | `src-tauri/src/database/dao/usage_dao.rs` | 插入、总量、趋势和模型聚合 |
| API-Switch | `src-tauri/src/services/log_service.rs` | 测试请求日志和简单 usage 抽取 |
| vs-ai-proxy | `internal/proxy/server.go` | 请求生命周期、候选重试、流式/非流式转换、唯一日志终结点 |
| vs-ai-proxy | `internal/provider/provider.go` | OpenAI/Ollama 响应结构和 SSE collector |
| vs-ai-proxy | `internal/proxy/stream_reasoning.go` | 已有流式跨事件状态 |
| vs-ai-proxy | `internal/store/store.go` | 请求日志、累计统计、轮换和 JSON 快照 |
| vs-ai-proxy | `web/dist/index.html` | 管理页统计和请求日志展示 |

## 3. API-Switch 的真实实现

### 3.1 非流式

`forwarder.rs::extract_usage_tokens` 从以下位置读取 usage：

- OpenAI 顶层 `usage`；
- Responses 的 `/response/usage`；
- Anthropic 的 `/message/usage`。

输入兼容 `prompt_tokens` 和 Anthropic input/cache 字段，输出兼容 `completion_tokens`/`output_tokens`，并尝试读取 reasoning details。正常非流式请求在 `forward_with_retry` 返回前调用 `log_usage`。

问题在于返回值是 `(i64, i64, i64)`，所有缺失字段都 `unwrap_or(0)`。因此存储层无法区分：

- 上游明确报告 `0`；
- 上游没有 usage；
- 协议适配漏掉 usage；
- 流在 usage 终态前中断。

### 3.2 流式

API-Switch 在 `forwarder.rs` 中维护原子状态：输入 token、输出 token、首 token 时间、文本/工具输出、chunk 数、字节数和是否已记录。正常 EOF、读取错误、空闲超时和 `Drop` 都使用同一个 `logged.swap(true, ...)` 防止同一流在进程内重复调度日志写入。

这个方向正确，因为流式 usage 往往只出现在最后一个 SSE 事件，普通 HTTP middleware 无法仅凭响应头完成统计。

但该保证有明确边界：

- `AtomicBool` 只保证进程内最多调度一次；
- 实际写库通过异步 `tokio::spawn` 完成；
- 日志表没有以请求/尝试 ID 建立唯一约束；
- 写库失败没有持久重试。

因此它是 best-effort at-most-once scheduling，不是数据库 exactly-once。

### 3.3 重试和失败

`forward_with_retry` 对每个失败 entry 立即写一条 usage log，继续尝试下一个 entry；成功 entry 再写一条。API-Switch 的 `COUNT(*)` 因此表示上游尝试数，不等于客户端请求数。

失败尝试固定以 `prompt_tokens=0`、`completion_tokens=0` 写入，即使上游可能已经开始推理或产生账单。这个选择适合排查 entry/channel，但不适合直接作为用户账单或客户端请求统计。

### 3.4 持久化和聚合

`usage_logs` 采用 SQLite，主要字段包括：

- entry/channel/requested model/actual model；
- stream、prompt/completion token；
- latency、first token、status、success；
- request ID、诊断 JSON、错误和创建时间。

索引覆盖 `created_at`、`model`、`access_key_id`、`channel_id`。仪表盘使用 `SUM(prompt_tokens)`、`SUM(completion_tokens)` 和 `COUNT(*)`；本地午夜通过 `chrono::Local` 计算，落库时间使用 UTC epoch。

SQLite 对完整历史、时间范围和排名查询是合适的，但当前表缺少：

- usage 是否可用；
- cached token 独立列；
- reasoning token 独立列；
- 客户端请求 ID/上游尝试 ID 的幂等唯一约束；
- 明确的保留/归档策略。

`reasoning_tokens` 只被塞进诊断 JSON `other`，常规聚合不会读取；cached token 也没有稳定的独立统计列。

### 3.5 已确认的协议缺口

1. unknown 被统一写成零，统计覆盖率不可见。
2. Responses/Claude 原生直通 SSE 的扫描按网络 chunk 转字符串并逐行解析，没有跨 chunk 的完整 SSE 事件缓冲；usage JSON 跨网络 chunk 时可能漏计。
3. Gemini Native 的 `usageMetadata` 并未进入主 `extract_usage_tokens` 口径，直通路径存在漏计风险。
4. reasoning token 仅部分非流路径进入诊断 JSON，流式不形成可聚合字段。
5. cached token 在 Claude/OpenAI/Responses 的归一化方式不同，存储层没有统一字段。
6. `SUM(prompt_tokens + completion_tokens)` 会把 unknown 当零参与图表，无法展示数据质量。

这些结论用于确定设计边界，不表示 API-Switch 整体功能不可用。

源码锚点（基于本次核验 commit）：

| 结论 | API-Switch 源码位置 |
| --- | --- |
| middleware 不负责 usage | `src-tauri/src/proxy/middleware.rs:22-24` |
| 非流统一抽取 | `src-tauri/src/proxy/forwarder.rs:1377-1443` |
| `StreamLogGuard` 和 Drop fallback | `src-tauri/src/proxy/forwarder.rs:750-884` |
| 流正常 EOF 终结 | `src-tauri/src/proxy/forwarder.rs:2154-2285` |
| unknown 被 `unwrap_or(0)` | `src-tauri/src/proxy/forwarder.rs:1422-1443` |
| prompt/completion 是仅有正式 token 列 | `src-tauri/src/database/schema.rs:63-92` |
| 失败 attempt 固定零 usage | `src-tauri/src/proxy/forwarder.rs:975-994` |
| dashboard 按 attempt 计数 | `src-tauri/src/database/dao/usage_dao.rs:274-304` |
| Gemini Native usage 字段 | `src-tauri/src/proxy/protocol/gemini.rs:397-412` |
| Responses/Claude 直通按网络 chunk 切行 | `src-tauri/src/proxy/forwarder.rs:2590-2719` |

## 4. vs-ai-proxy 的约束

### 4.1 保持一请求一日志

本项目的 `loggingMiddleware` 是请求日志唯一终结点。候选 provider 重试、流式/非流式模式兜底都在 handler 内完成，middleware 最终只调用一次 `Store.AddLog`。

token usage 应附着到 `responseWriter`，由现有 middleware 与状态、耗时、模型、重试摘要一起原子地形成一条内存记录。不能像 API-Switch 一样为每个失败候选再插入请求日志，否则会同时破坏请求成功率、平均耗时和现有管理页语义。

### 4.2 权威值优先，不做估算

本项目不会根据请求字符串、模型 tokenizer 或输出字节数估算 token，原因是：

- 不同模型 tokenizer 不同；
- 工具 schema、图片、多模态和 provider 注入内容会造成偏差；
- 本地估算和上游账单混在一起会产生虚假精确度；
- 引入 tokenizer 会增加依赖、体积和模型维护成本。

如果将来确实需要估算，必须使用独立的 `source=estimated`，不能与 `source=upstream` 相加或混显。本次不实现估算。

### 4.3 总量关系

统一字段定义：

| 字段 | 含义 | 聚合规则 |
| --- | --- | --- |
| `prompt_tokens` | 输入 token | 直接累加 |
| `completion_tokens` | 输出 token | 直接累加 |
| `total_tokens` | 上游报告总量；缺失且输入/输出存在时用二者之和补齐 | 直接累加 |
| `cached_tokens` | 输入 token 中的缓存命中子集 | 单独展示，不加到 total |
| `reasoning_tokens` | 输出 token 中的推理子集 | 单独展示，不加到 total |
| `source` | 本次固定为 `upstream` | 用于未来隔离估算值 |

usage 对象存在本身就是“上游报告了 usage”的标志。即使所有字段为零，也必须保留该对象。

## 5. 实施设计

### 5.1 采集层

在 `provider.Usage` 中保留 OpenAI 标准详情：

- `prompt_tokens_details.cached_tokens`；
- `completion_tokens_details.reasoning_tokens`。

OpenAI SSE collector 必须保留最后出现的 usage 快照，不能累加每个 chunk，因为兼容上游可能重复发送累计值。usage-only 终态 chunk 即使 `choices=[]` 也必须被处理。

Ollama 从终态 `prompt_eval_count`/`eval_count` 读取。所有流式协议继续复用现有逻辑事件 parser 或 accumulator，不新增按原始网络 chunk 切行的旁路解析器。

### 5.2 生命周期

```text
上游响应
  -> 协议解析/完整性校验
  -> 得到最终权威 usage（可能为 nil）
  -> 写入 responseWriter 的请求上下文
  -> 向下游提交响应
  -> loggingMiddleware 结束时形成一条 RequestLog
  -> Store 同步更新累计和按模型统计
```

设置 usage 的时机：上游响应已经通过协议完整性校验、但可以在最后一次下游写入前。这样即使客户端在最终写入时断开，仍能记录上游已经明确报告、通常也已经计费的 usage；未完成或未通过校验的候选不会污染最终请求统计。

### 5.3 重试语义

- 建立连接失败、HTTP 4xx/5xx 且没有可接受响应：usage unknown。
- 候选 A 失败、候选 B 成功：只记录 B 最终被采用响应的 usage。
- 流式下游已经开始后失败：不会切候选；若终态 usage 尚未出现，则 unknown。
- 上游完成并报告 usage、下游最终写入失败：保留该 usage，同时请求状态仍按 499/失败记录。

这是一份客户端请求观测，不是所有上游尝试的计费审计。未来若要审计失败尝试，应新增独立的 `upstream_attempts` 数据模型和幂等 attempt ID，不能复用 `RequestLog`。

### 5.4 存储和重启兼容

`RequestLog` 增加可空 `usage` 对象。`Statistics` 增加：

- token 统计适用请求数；
- usage 上报请求数；
- 输入、输出、总、缓存、推理 token；
- 按 provider/requested model/upstream model 分组的累计统计。

`logs.json` 必须继续保持旧版裸数组格式，使旧版 EXE 回滚后仍可读取请求日志。累计统计写入同目录的版本化 `logs.stats.json` sidecar：

```json
{
  "version": 1,
  "statistics": {}
}
```

新版在 sidecar 不存在时从旧 `logs.json` 重建统计；旧版会忽略 sidecar。sidecar 同时保存保留日志数量和最新日志 ID，只有与 `logs.json` 同代时才恢复累计值；如果用户回滚旧版并产生了新日志，再升级时会检测不匹配并从当前日志重建，避免加载陈旧累计值。这样最近 1000 条明细仍受现有限制，累计值和按模型值不会在正常重启后退化为“仅最近 1000 条”，并保持新旧版本双向回滚兼容。两个文件各自使用当前原子替换机制，不增加数据库依赖；进程在两次替换之间异常退出时，累计统计可能落后一个保存周期，但不会破坏请求日志。

### 5.5 API、Metrics 和管理页

- `/admin/api/statistics` 在保持旧字段不变的同时返回 token 总量、有用量数据比例和模型分组。
- `/metrics` 增加不带模型 label 的 token 累计 counter，避免高基数标签。
- 管理页展示输入、输出、总 token 和有用量数据比例。
- 请求日志展示本次 usage；unknown 显示 `-`，不能显示 `0`。
- 模型统计按 total token 降序，显示 provider、模型路由、请求/有用量数据次数和 token 明细。

## 6. 不采用的方案

### 6.1 直接复制 API-Switch SQLite

拒绝原因：本项目已有并发安全 Store、原子 JSON 快照和固定数量的本地请求明细；为一个累计统计功能引入数据库迁移、连接管理和新依赖，风险大于收益。SQLite 应在产品明确需要完整历史、任意时间区间、成本账单或多进程写入时再引入。

### 6.2 在 HTTP middleware 解析全部响应正文

拒绝原因：流式响应可能很大且需要即时 flush；middleware 全量缓存会增加延迟和内存，也会重复协议解析。usage 应由已有协议 parser/accumulator 顺带保留。

### 6.3 按 chunk 累加 token

拒绝原因：OpenAI-compatible 上游通常在终态返回累计值，有些网关会重复发送 usage；逐 chunk 相加会重复计数。采用“最后一个有效 usage 快照覆盖前值”。

### 6.4 把 cached/reasoning 加到 total

拒绝原因：两者分别是输入和输出的子集，重复相加会制造虚高。

### 6.5 把 unknown 当成零

拒绝原因：这正是 API-Switch 当前最影响可信度的口径问题。unknown 必须通过空 usage 和覆盖率明确暴露。

## 7. 测试和上线门禁

必须覆盖：

1. OpenAI 非流式 raw 和 typed usage。
2. OpenAI SSE usage-only 最终 chunk、重复累计 usage、缺失 usage。
3. `prompt_tokens_details.cached_tokens` 和 `completion_tokens_details.reasoning_tokens`。
4. Ollama 非流式和流式终态计数。
5. stream-to-nonstream、nonstream-to-stream 兜底。
6. 多候选失败后成功不重复累计。
7. 客户端中断、上游截断、协议错误不伪造 usage。
8. usage unknown 与 reported zero 的区别。
9. `logs.json` 继续保持旧数组格式；sidecar 跨重启保留超出日志环的累计值；sidecar 缺失时可重建。
10. Store 并发和 race 测试。
11. 管理页中英文 key、DOM 引用和表格列一致。
12. `/metrics` counter。

上线前至少运行：

```bash
go test ./internal/provider ./internal/proxy ./internal/store ./internal/api ./web -count=1
go test -race ./internal/provider ./internal/proxy ./internal/store -count=1
go vet ./...
make i18n-check
make release-check
```

## 8. 已知边界

- 统计取决于上游是否返回 usage；覆盖率必须与 token 总量一起看。
- 这是本地累计观测，不是 provider 账单对账工具。
- 当前只处理本项目支持的 OpenAI chat-completions 和 Ollama 协议面，不宣称支持未暴露的 Anthropic/Gemini 原生入口。
- 最近明细受日志环限制；累计和模型聚合由快照保留，但不提供任意历史时间范围。
- 进程在 30 秒定期快照之间被强制杀死，可能丢失最近一小段累计值；正常退出会再次保存。
- provider 账单可能包含代理看不到的失败尝试或网关附加 token，应以上游账单为最终依据。

## 9. 实施结果

已实施：

- `internal/provider/provider.go`：usage 使用 `int64`，保留 cached/reasoning details；SSE collector 保存最后一个 usage 快照；Ollama 显式零值保持 known。
- `internal/proxy/token_usage.go`：OpenAI raw、typed 和 Ollama raw 统一转换到请求日志 usage。
- `internal/proxy/stream_reasoning.go`、`internal/proxy/server.go`：OpenAI/Ollama 流、双向协议转换、DSML 和两种模式 fallback 在既有完整性门禁内采集 usage。
- `internal/store/store.go`：unknown/reported-zero、累计统计、按 provider/requested/upstream 模型统计、旧数组日志和 versioned sidecar。
- `internal/proxy/metrics.go`：增加无高基数 label 的 token counter。
- `web/dist/index.html` 和中英文词典：累计卡片、有用量数据比例、按模型统计、单条日志 Token 列。
- `docs/README.md` 和系统架构总览：已加入本专题入口和运行机制。

验证结果：

- 全仓：`go test ./... -count=1`，`1312 passed / 12 packages`。
- race：`go test -race ./internal/provider ./internal/proxy ./internal/store ./internal/api ./web -count=1`，`1240 passed / 5 packages`。
- `go vet ./...`：无问题。
- `make i18n-check`：`I18N_RUNTIME_TEST_OK`。
- `make release-check`：成功，包含工具调用门禁、`govulncheck`、i18n 和 Windows amd64 交叉构建。
- `git diff --check`：通过。

未验证/不能夸大的部分：

- 未使用用户真实 API key 做 provider 账单对账；测试使用本地 HTTP/SSE fixture，验证的是协议和统计口径。
- 未在 Windows GUI 中人工查看管理页；Windows amd64 编译已通过，DOM/i18n 契约由自动测试覆盖。
- 未运行 API-Switch 的 Cargo 测试；该仓库是只读分析对象且原本存在未提交状态，本次结论来自逐源码静态审计。
- 不宣称统计等于 provider 账单；失败但已计费、未返回 usage 的上游尝试仍属于 unknown。
