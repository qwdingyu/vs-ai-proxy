# upstream_no_response / client_gone 严肃核验与完整方案

日期：2026-07-23  
状态：基于仓库源码与既有文档的**二次交叉核验**归档；**不是**实时对 UseAI 生产账号的复测报告。  
范围：Visual Studio Copilot BYOM 场景下，本地 `vs-ai-proxy` 对 OpenAI-compatible 上游（含 UseAI / new-api / sub2api 类网关）的可用性、诊断口径、重试边界与解决方案全集。

---

## 0. 文档目的与诚信边界

### 0.1 目的

1. 对「行业需求是否满足 / 是否最佳实践 / 是否最优方案」给出**可追溯到代码与既有文档**的结论。
2. 对用户现场高频出现的：

   - 流状态 / 日志中的 `client_gone`
   - `POST /v1/chat/completions` → `502`
   - `error_code=upstream_no_response`
   - `last=waiting_response_headers/upstream_no_response`
   - `request_bytes` 约 600KB+

   给出**不臆造根因机器、不承诺可被代理单独根治**的解读与完整对策。

3. 把「立刻可做 / 代理可增强 / 上游必须做 / 明确禁止」写全，避免后续为了成功率引入**重复计费或重复工具调用**类灾难。

### 0.2 诚信边界（强制）

| 允许写 | 禁止写 |
| --- | --- |
| 源码中的行为与默认值 | 未跑 soak 就宣称“当前 UseAI 成功率 xx%” |
| 既有文档（38/39/40 等）中**标明当次**的复测数字 | 把当次 7/10 写成永久 SLA |
| “与现象相容的上游侧解释” | “已证明是 UseAI 某台机器 / 某一条渠道” |
| 代理无法消除公网与上游抖动 | “升级本代理即可彻底消灭 502” |
| 非幂等 POST 提交后不重放 | “提交后再自动重试直到成功” |

### 0.3 核验方法

本轮对上一结论中的关键断言，逐条对照：

- `internal/provider/provider.go`（httptrace 阶段、重试边界、HTTP Client）
- `internal/proxy/diagnostic_error.go`（错误分类、用户文案）
- `internal/proxy/server.go`（候选策略、超时预算、`finalizeRequestStatus`、`shouldStopCandidateFallback`）
- `internal/config/config.go`（Defense 默认值）
- `internal/store/store.go` / `internal/api/api.go`（`recent_stability` 仅观测）
- `docs/15_*`、`docs/17_*`、`docs/38_*`、`docs/39_*`、`docs/40_*`
- 相关回归测试名（见第 9 节）

`go test ./internal/provider ./internal/proxy ./internal/store -count=0` 在本轮核验时可通过（仅编译/装载，不代表跑全量用例）。

---

## 1. 现场样本再解读（用户日志）

### 1.1 典型日志（用户提供，字段已规范化）

```text
流状态: 错误 client_gone   （UI 或摘要侧可能出现的表述，需与最终日志对齐）

POST /v1/chat/completions - 502 (~3200 ms)
request_id=...
provider=useai
requested_model="UseAI - deepseek-v4-flash"
upstream=deepseek-v4-flash
error_code=upstream_no_response
reason="上游接收后未响应"
action="稍后重试，或切换到更稳定的同模型渠道。"
attempts="useai/deepseek-v4-flash ~3.1s
  upstream_no_response
  (upstream_attempts=1 last=waiting_response_headers/upstream_no_response)"
request_bytes≈611 KB
upstream_bytes≈619 KB
```

同窗口内可连续出现多条 `request_id` 不同、形态一致的 502。

### 1.2 字段级含义（源码语义，不是猜测）

| 字段 | 源码/口径含义 | 对本样本的含义 |
| --- | --- | --- |
| `error_code=upstream_no_response` | `classifyUpstreamAttempt`：在 `network_error` 基础上，若 `Stage` 为 `waiting_response_headers` 或 `receiving_response_headers`，升格为 `upstream_no_response` | **不是**“代理连不上上游域名”的笼统错误 |
| `last=waiting_response_headers/...` | httptrace：`WroteRequest` 成功后进入该阶段；`GotFirstResponseByte` 才进入 `receiving_response_headers` | 请求（含 body）侧已写出；**未拿到响应头首字节**或等头阶段连接失败 |
| `upstream_attempts=1` | 进入写请求/等头后，`isRetryableUpstreamTransportError` 返回 false，不会 provider 内手动重放 | **有意只尝试 1 次**，不是“漏了重试配置” |
| `request_bytes` ≈ `upstream_bytes` | 代理侧记录的客户端请求体与发往上游体量接近 | **不支持**“代理把 body 异常膨胀成数 MB”的主因假设 |
| 耗时 ~3.2s | 远低于默认客户端预算 90s | **不是**先跑满代理超时再失败；更像**对端很快无响应或断连** |
| HTTP 502 | 代理对上游失败的对外状态 | VS 侧表现为完成失败；与“上游业务 200 半截流”不同 |
| `reason` / `action` | `userFacingDiagnosticFor("upstream_no_response")` | 文案与代码一致：接收后未响应；建议稍后重试或换更稳定同模型渠道 |

### 1.3 阶段机（便于对照）

```text
preparing_request
  -> resolving_dns
  -> connecting
  -> tls_handshake
  -> writing_request          （GotConn / WroteHeaders / 写 body）
  -> waiting_response_headers （WroteRequest 成功）
  -> receiving_response_headers （GotFirstResponseByte）
  -> （流式 body / SSE 读取……）
```

**本样本停在 `waiting_response_headers`。**

### 1.4 `client_gone` 与最终 `502 upstream_no_response` 如何并存

必须分开两层：

1. **最终请求主因**  
   若最终日志是 `502` + `error_code=upstream_no_response`，按当前设计应以**上游已接收未响应**为主因。

2. **`client_gone` 可能来源（按可信度，非互斥）**

   | 来源 | 是否与本样本相容 | 说明 |
   | --- | --- | --- |
   | 真实下游断开（用户取消、VS 放弃、网络抖） | 可能 | `finalizeRequestStatus` 仅在**没有**已有 4xx/5xx 且**没有** `X-Proxy-Error-Code` 时补记 499/`client_gone` |
   | 历史误报：收尾覆盖上游主因 | **旧版本风险** | `docs/40` 与 `finalizeRequestStatus` 注释、`TestLoggingMiddlewareDoesNotOverwriteExplicitUpstreamFailureWhenContextCanceled` 针对此修复；含该修复的版本不应再把已写好的 `upstream_no_response` 盖成 `client_gone` |
   | UI 文案 “流状态” 与最终 `error_code` 不同步 | 可能 | Web 词典有 `logs.tooltip.streamState`；排障必须以 **request_id 对齐的最终 RequestLog / 控制台最终 INFO 行** 为准 |
   | `cancel_reason` 统计 | 需看字段 | 对明确 502 上游失败，设计意图是 **`cancel_reason` 应为空**，避免把上游问题算进客户端断开（见 docs/40） |

**核验结论：** 用户看到的 “流状态: 错误 client_gone” **不能单独推翻** 同条请求上的 `upstream_no_response`。正确做法是按 `request_id` 对齐：`status`、`error_code`、`stream_state`、`attempts_summary`、`cancel_reason`。

### 1.5 本样本不能证明 / 能证明

| 能较稳妥证明 | 不能仅凭该日志证明 |
| --- | --- |
| 代理已完成对上游的请求写出（等头阶段） | UseAI 内部具体哪条渠道、哪台实例 |
| 失败发生在“等响应头”，而非 DNS/TCP 未建连 | 一定是 WAF / 一定是模型推理超时 |
| 代理对该次失败未做提交后手动重放（attempts=1） | 官方 DeepSeek 一定更稳或更不稳（需对照实验） |
| 请求体量约 600KB 级，属大上下文高风险区 | 代理本地逻辑 bug 是唯一根因 |

---

## 2. 产品是否满足行业需求（再核验）

### 2.1 行业问题拆解

| 行业子需求 | 本项目职责边界 | 核验结论 |
| --- | --- | --- |
| VS/Copilot 接入第三方 OpenAI-compatible 模型 | 本地统一 endpoint、模型列表、chat | **满足**（README + `/v1/*`） |
| 工具调用 / 流式协议兼容 | 协议归一、DSML、契约测试 | **强项，持续投入** |
| 多模型 / 多 Key 管理 | 配置与管理面板 | **满足** |
| 故障可诊断 | `error_code` / stage / attempts / recent_stability | **满足且近期强化** |
| 上游算力与渠道 SLA | 属上游/网关 | **不在代理可根治范围** |
| 企业级自动多活路由 / 对账级幂等重试 | 需上游幂等与策略产品 | **当前刻意保守，非完整 HA 层** |

### 2.2 与同类组件的分工（架构诚实描述）

```text
VS/Copilot
   -> vs-ai-proxy（协议适配、参数治理、安全边界内短重试、诊断）
      -> UseAI / new-api / sub2api（渠道选择、限流、排队、内部重试）
         -> 真实模型后端
```

- **本地代理最优价值**：让严格客户端（VS）在多方言上游上“能用、能看懂失败、不重复搞砸工具调用”。  
- **不能替代**：网关渠道健康、账号额度、模型排队、公网中断。

### 2.3 是否“最佳实践”

**符合且应坚持：**

1. chat POST **非幂等** → 写请求 / 等头后 **不手动重放**（`isRetryableUpstreamTransportError`）。  
2. 不为 chat 发送 `Idempotency-Key` 诱导 Go `Transport` 扩大重放（注释 + 测试 `TestOpenAIProviderDoesNotSendIdempotencyHeaderForChatRequests`）。  
3. 默认 **只执行首选候选**（`applyDefenseCandidatePolicy` → `candidates[:1]`），避免悄悄换 provider、掩盖错误、放大计费。  
4. `upstream_no_response` / `upstream_stream_interrupted` 等 **停止候选 fallback**（`shouldStopCandidateFallback`）。  
5. 带结构化 `UpstreamAttempts` 且已有 stage/HTTP 状态时，**不**再做流式/非流式互切重放（`ShouldAttemptAlternateChatMode`）。  
6. 客户端等待预算默认 90s（`DefaultClientTimeoutBudgetSeconds`），避免代理傻等超过 VS 耐心。  
7. `recent_stability` **只观测、不进路由**（store/api 注释与实现）。  
8. 上游主因不得被收尾 `client_gone` 覆盖（`finalizeRequestStatus` + 回归测试）。

**有意识的取舍（不是疏忽）：**

| 取舍 | 代价 | 收益 |
| --- | --- | --- |
| 提交后不重试 | 间歇 502 需用户或上游侧消化 | 避免双倍扣费 / 双工具副作用 |
| 不跨 provider 自动 failover | 单线挂了就失败 | 错误真实、绑定模型不被偷换 |
| 无 `ResponseHeaderTimeout` | 慢死连接依赖总预算或对端 RST | 避免误杀“首 token 很慢但合法”的模型（需可配才加） |
| Defense 下 5xx 仍可短重试 | 可能二次请求 | 缓解 new-api 单渠道 503 透出（`shouldRetryOpenAIProviderError`） |

### 2.4 是否“最优方案”

| 目标 | 评价 |
| --- | --- |
| 单机 VS BYOM 兼容代理 | **合理且接近该 niche 的最优工程形态之一** |
| 在不稳定聚合网关上保证大包工具请求高可用 | **不是最优**；最优是 **更稳上游/官方线 + 上下文治理 + 网关渠道治理**；代理只做安全边界内增强 |
| 用代理“消灭” `waiting_response_headers` 无响应 | **不存在不付出灾难风险的单一本地最优解** |

---

## 3. 关键源码事实清单（二次核验通过）

### 3.1 超时与预算

| 项 | 位置 | 事实 |
| --- | --- | --- |
| 默认 client budget | `internal/config/config.go` | `DefaultClientTimeoutBudgetSeconds = 90` |
| 有效超时裁剪 | `effectiveClientBoundTimeoutSeconds` | 配置更长会被压到 budget；更短保留 |
| provider 操作 context | `providerOperationContext` | 用模型/预算超时包住整次操作 |
| HTTP Client.Timeout | `newProviderHTTPClient` | 构造时带 timeout；实际 Do 路径会避免与 operation deadline 双重误伤（见 provider 注释） |
| `ResponseHeaderTimeout` | 全仓库 `*.go` | **不存在** |
| 客户端约 90s 断开诊断 | `clientDeadlineDiagnosticThreshold = 90_000` ms | 499 + 原 `client_gone` 且耗时够长 → 可升格 `client_deadline_reached` |

### 3.2 重试

| 项 | 事实 |
| --- | --- |
| Defense 关 | `openAIProviderMaxAttempts()` → 1 |
| Defense 开 | 最多 3；仅可恢复错误 |
| 传输错误可重试阶段 | `preparing_request` / `resolving_dns` / `connecting` / `tls_handshake` |
| 传输错误不可重试阶段 | `writing_request` / `waiting_response_headers` / 默认 default 分支 |
| HTTP 5xx | Defense 开时允许短重试（**会再次请求**） |
| 4xx / 429 | 不盲目重试 |
| Cancel / DeadlineExceeded | 不重试 |

### 3.3 候选与 fallback

| 项 | 事实 |
| --- | --- |
| `applyDefenseCandidatePolicy` | 多候选时 **只留第一个** |
| `shouldStopCandidateFallback` | 含 `upstream_no_response`、`upstream_stream_interrupted`、`client_gone` 等 |
| 冷却/健康排序 | `registry` 仍有；但默认策略截断后，**当前请求**不会跨 provider 打满列表 |

### 3.4 诊断分类

| 分类 | 触发要点 |
| --- | --- |
| `upstream_no_response` | 等头/收头阶段的网络类失败 |
| `upstream_stream_interrupted` | 流已建立或读取中断、缺合法终态（与 38/40 一致） |
| `network_error` | 更早连接阶段或未升格时 |
| `client_gone` | 下游取消类；且分类时注意**勿抢在明确上游错误之前**（docs/40、classify 顺序） |
| 用户文案 | `上游接收后未响应` / `切换到更稳定的同模型渠道` |

### 3.5 传输层其它

| 项 | 事实 |
| --- | --- |
| `ForceAttemptHTTP2` | `false`（有测试约束） |
| `DisableCompression` | `true` |
| IdleConnTimeout | 120s |
| Dial Timeout | 30s |
| TLSHandshakeTimeout | 10s |

### 3.6 观测

| 项 | 事实 |
| --- | --- |
| `GetRecentStabilitySummary` | 按 provider/model/upstream 聚合成功率与 top error |
| `/diagnostics` 等 API | 暴露 `recent_stability`；**不参与**路由决策 |

### 3.7 相关 git 提交主题（历史，非实时）

近期相关主题包括（`git log` 摘要，便于对照版本）：

- Prevent unstable upstream failures from being misreported or replayed  
- Prevent upstream failures from being mislabeled as client disconnects  
- Stop replaying already-submitted upstream 5xx  

现场应以运行中二进制版本与是否包含上述行为为准。

---

## 4. 与历史文档的一致性

| 文档 | 与本核验关系 |
| --- | --- |
| `15_上游网关防御模式与new-api多渠道踩坑记录_20260710.md` | 防御模式、不可无限重试、new-api 不透明兜底 |
| `17_大请求长上下文UseAI与DeepSeek踩坑记录_20260711.md` | 大 body 与 UseAI 阈值/失败；Web 小请求不能代替 VS 大请求 |
| `20` / `21` / `24` / `26` | 上下文压力、499、超时与流式终态 |
| `38_UseAI大请求间歇性502诊断与重试边界复盘_20260722.md` | stage 诊断、`upstream_no_response`、非幂等边界；当次 soak 数字仅当次 |
| `39_近期稳定性摘要增强与诊断观测口径说明_20260723.md` | `recent_stability` 口径 |
| `40_近期稳定性修复与踩坑总复盘_20260723.md` | 不覆盖 client_gone、不危险重放、真实复测与不能承诺清单 |

本文件 **不替代** 上述专题，而是：**在 2026-07-23 对“审查结论 + 现场 502 样本”做闭合核验与方案总表**。

---

## 5. 根因分层模型（排障用）

### 5.1 责任分层

| 层 | 可能故障 | 本代理能做什么 |
| --- | --- | --- |
| L0 用户/会话 | 超大历史、重复贴文件、频繁取消 | 文档与告警建议；不能改 VS 行为 |
| L1 VS/Copilot | ~90–100s 等待、工具协议严格 | 协议兼容、timeout budget、清晰错误 |
| L2 vs-ai-proxy | 错误分类、错误重试、误报 | **本仓库职责核心** |
| L3 聚合网关 (UseAI/new-api) | 渠道挂、排队、413、空连接、5xx | 诊断透传；短重试仅限安全阶段；**不能根治** |
| L4 模型后端 | 慢推理、限流、断流 | 同上 |
| L5 公网/TLS/代理环境 | 中断、RST | 可观测；有限短重试 |

### 5.2 本样本最可能落点

**L3–L5 在 `waiting_response_headers` 无响应头**，L2 已正确分类且未危险重放。  
**不能**在未做 direct 对照前把锅唯一扣在 L2 实现 bug。

### 5.3 推荐对照实验（证据链）

1. 同一 `request_id` 拉全字段。  
2. 同 body 大小：`MODE=direct-proxy` 对 UseAI 与可选第二 provider 做 `REPEAT≥20`（`tests/large_request_matrix_diagnostic.sh`）。  
3. 若 direct 与 proxy 同败 → 上游/链路；若 direct 稳 proxy 败 → 再查代理改写（当前大包场景 historically `delta_bytes=0` 样本不支持膨胀论）。  
4. 缩小 body 后失败率是否下降（验证上下文压力）。  

---

## 6. 完整解决方案（不遗漏分层）

下列方案按 **优先级与风险** 排列。实施时默认：**不改动“提交后不重放 chat”铁律**，除非上游提供可验证幂等。

### 6.1 P0 — 现场止血（用户/运维，零代码或少配置）

| # | 动作 | 目的 | 验收 |
| --- | --- | --- | --- |
| P0-1 | 确认运行版本包含 docs/40 所述“不覆盖 upstream 主因”行为 | 消除假 `client_gone` | 人为构造：上游失败 + context cancel，日志仍为 `upstream_no_response` |
| P0-2 | 新会话 / 裁剪上下文 / 少贴全仓；目标把 `request_bytes` 从 600KB+ 压到显著更低 | 降低网关排队与断流概率 | 同任务 `request_bytes` 下降且成功率升 |
| P0-3 | 配置**同模型第二入口**（官方 DeepSeek 或其它网关），在 VS 中手动切换，不依赖自动 failover | 可用性旁路 | 第二入口小请求与中等大请求可用 |
| P0-4 | 打开 Defense（默认开）；排障时可关一次对比“原始上游次数” | 对比可观测 | Defense 关时 attempts 行为符合预期 |
| P0-5 | 用 request_id 建表：时间、bytes、error_code、stage、elapsed | 证明间歇性与体量相关 | 表格可展示 |
| P0-6 | 将时间窗 + model + 体量反馈 UseAI/网关方 | 推动 L3 修复 | 工单号 |

### 6.2 P1 — 观测与发布门禁（工程，低风险）

| # | 动作 | 目的 | 代码/脚本落点 |
| --- | --- | --- | --- |
| P1-1 | 管理页展示 `recent_stability`（只读） | 用户可见坏线 | `store`/`api` 已有数据；UI 待接 |
| P1-2 | 控制台/日志对 `request_bytes ≥ 阈值`（如 400KB）打 WARN | 大包预警 | proxy logging |
| P1-3 | 发布前 `REPEAT=20~30` UseAI 400KB–600KB soak | 量化间歇 | `tests/large_request_matrix_diagnostic.sh` |
| P1-4 | 固定门禁：`go test ./internal/provider ./internal/proxy`、`tool_call_release_check` | 防回归 | docs/38、40 已列 |
| P1-5 | 文档与 UI 明确：“502 upstream_no_response ≠ 本地未联网” | 减少误排障 | 本文件 + 管理页文案 |

### 6.3 P2 — 代理可配置增强（需设计评审，中风险）

| # | 动作 | 收益 | 风险与约束 |
| --- | --- | --- | --- |
| P2-1 | 可选 `response_header_timeout_seconds`（默认关闭或较大） | 慢死连接更快失败 | 误杀慢首 token；**必须可配、按模型** |
| P2-2 | 可选“同逻辑模型多 BaseURL 列表”，**仅**在 `connecting` 前失败换下一 URL | 提高连前失败恢复 | **禁止**在 `waiting_response_headers` 后自动换线重放同一 body |
| P2-3 | 大包预检：超过硬限制返回明确 `upstream_payload_too_large` 类提示（若本地可判断） | 快速失败 | 阈值因上游而异，避免误伤 |
| P2-4 | 导出单次失败诊断包（脱敏 JSON：stage、attempts、bytes、timeouts） | 给网关方 | 严禁含 API Key 与 prompt 正文 |

### 6.4 P3 — 上游 / 账号 / 架构（根因层）

| # | 动作 | 说明 |
| --- | --- | --- |
| P3-1 | 网关渠道健康检查与剔除坏渠道 | new-api/sub2api/UseAI 侧能力；代理无法替代 |
| P3-2 | 提高大 body / 工具请求的网关超时与连接保持 | 针对等头无响应 |
| P3-3 | 官方模型线作为生产默认，聚合网关作备用 | 架构建议，非代码强制 |
| P3-4 | 账号额度、402/429 与渠道权限分离排查 | 避免与 no_response 混谈（docs/40 有 402 样本） |

### 6.5 P4 — 明确禁止（灾难预防清单）

| 禁止项 | 原因 |
| --- | --- |
| 在 `writing_request` / `waiting_response_headers` 后自动重放同一 chat POST | 重复计费、重复工具（写文件、shell、git） |
| 空或假 `Idempotency-Key` 诱导 Transport 重放 | 扩大本地重放语义，非真幂等 |
| Hedge 双发取最快 | 双倍费用与双副作用 |
| 把 `recent_stability` 直接焊进自动熔断路由且无人工确认 | 短窗口噪声导致误熔断 |
| 客户端已 `client_gone` 后继续打上游兜底 | 用户已走仍扣费 |
| 流式已向 VS 写出 token 后再换 provider/模式 | 双段输出、工具错乱 |
| 承诺“升级代理即可 100% 消除 UseAI 502” | 虚假承诺 |

### 6.6 方案决策树（运维）

```text
出现 502
  ├─ error_code=upstream_no_response 且 stage=waiting_response_headers
  │    ├─ request_bytes 很大？ → P0 裁上下文 + P3 网关
  │    ├─ direct 同样失败？ → 上游/网络，非代理逻辑
  │    └─ 仅 proxy 失败且 delta_bytes 异常？ → 查代理改写（异常路径）
  ├─ error_code=upstream_stream_interrupted
  │    └─ 上游断流；不重放；换渠道或减负载
  ├─ error_code=client_gone / client_deadline_reached
  │    ├─ elapsed ~90s+ → 客户端/预算耐心；减上下文或更快模型
  │    └─ 与 502 主因并存 → 对齐 request_id，勿覆盖主因
  ├─ 413 / upstream_payload_too_large
  │    └─ 减 body 或换更大限额渠道（docs/17）
  └─ 429 / 5xx 明确 HTTP
       └─ 冷却/换模型；Defense 下 5xx 或有短重试（知悉二次请求）
```

---

## 7. 与用户现场问题的直接对应

| 用户痛点 | 核验后结论 | 首选动作 |
| --- | --- | --- |
| 频繁 502 + 上游接收后未响应 | 主因在等头无响应；代理分类正确；attempts=1 为安全边界 | P0 减包 + 第二渠道 + 上游工单；勿要求提交后重试 |
| 流状态 client_gone | 可能为下游事件、UI 不同步或旧版误报；**不能盖过** 502 主因 | 升版本核对；按 request_id 对齐字段 |
| 是否行业最优 | 作 VS 适配层优；作上游 HA 否 | 接受分层；补 P1/P2 观测与可选超时 |
| 是否要大改架构 | 否（docs/40 原则） | 小步：观测、预警、可配 header timeout |
| 会不会漏重试 | 等头失败**故意不重试** | 用多渠道配置，不用危险重放 |

---

## 8. 测试与门禁（完整列表）

### 8.1 单元 / 集成（仓库内已有相关覆盖，改动后必跑）

```bash
go test ./internal/provider ./internal/proxy ./internal/store ./internal/api -count=1
go test ./... -count=1
go vet ./...
go build ./...
```

与本主题强相关的测试意图（名称级，便于检索）：

- `TestOpenAIProviderDoesNotManuallyRetryAfterRequestWriteStarts`
- `TestOpenAIProviderDoesNotSendIdempotencyHeaderForChatRequests`
- `TestLoggingMiddlewareDoesNotOverwriteExplicitUpstreamFailureWhenContextCanceled`
- diagnostic 中 `waiting_response_headers` → `upstream_no_response`
- `shouldStopCandidateFallback` 对 `upstream_no_response` / `upstream_stream_interrupted`

### 8.2 脚本门禁

```bash
bash -n tests/useai_large_request_diagnostic.sh
bash -n tests/large_request_matrix_diagnostic.sh
bash -n tests/model_release_diagnostic.sh
bash tests/tool_call_release_check.sh
```

### 8.3 真实上游 soak（发布 / 重大投诉时）

```bash
REPEAT=30 \
SIZES="400000 600000" \
CASES="useai|UseAI|deepseek-v4-flash" \
MODE=proxy-only \
PROXY_TIMEOUT_SECONDS=140 \
tests/large_request_matrix_diagnostic.sh
```

对照第二渠道时使用 `MODE=direct-proxy` 与多行 `CASES`（见 docs/38）。  
**当次数字只写进当次报告，不写入本文件作为永久 SLA。**

---

## 9. 配置参考（与源码一致）

```json
{
  "defense": {
    "enabled": true,
    "client_timeout_budget_seconds": 90
  }
}
```

说明：

- `enabled`：短重试、稳定 UA、冷却、协议兜底等；**不再**表示跨 provider 自动 failover。  
- `client_timeout_budget_seconds`：把过长模型超时压到客户端可等待窗口。  
- 单模型仍可配 `timeout_seconds`；有效值受 budget 约束。

---

## 10. 总括结论（核验后定稿）

1. **行业需求：** 作为 VS Copilot 第三方模型本地兼容与诊断代理，**核心需求满足**；作为上游高可用与渠道调度层，**不满足也不应假装满足**。  
2. **最佳实践：** 非幂等安全、阶段诊断、默认不跨 provider 偷换、主因不被 `client_gone` 覆盖，**符合**严格代理实践；提交后自动重试 **不符合**。  
3. **最优方案：** 本地代理 + 上游治理 + 上下文治理 的分层才是大包场景的最优组合；**单改代理重试不能最优且危险**。  
4. **现场 502：** `upstream_no_response` @ `waiting_response_headers` + 约 600KB body + ~3s + `upstream_attempts=1`，应解释为 **请求已提交、上游/链路未在短时间内给出响应头，代理拒绝危险重放**。  
5. **完整对策：** 第 6 节 P0–P4 为全量方案集；执行时以 P0/P1 为先，P2 需评审，P3 找上游，P4 为红线。

---

## 11. 修订记录

| 日期 | 说明 |
| --- | --- |
| 2026-07-23 | 初版：对审查结论做源码交叉核验，归档完整方案与禁止项；关联 15/17/38/39/40 |

