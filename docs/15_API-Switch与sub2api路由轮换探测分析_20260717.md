# API-Switch 与 sub2api 路由、轮换、探测机制分析

日期：2026-07-17

## 分析范围

本次只读审查了三个代码库：

- 当前项目：`/Users/dingyuwang/0-X/4-go/vs-ai-proxy`
- API-Switch：`/Users/dingyuwang/0-X/5-rust/API-Switch`
- sub2api：`/Users/dingyuwang/0-X/0-AI工具/sub2api`

目标是判断：

- API-Switch 是否真正实现“对外 auto，对内轮换、探测”。
- sub2api 的内部轮换机制是什么。
- 哪些机制适合吸收到当前产品，哪些不适合。

## 总结

API-Switch 当前不是严格意义上的轮询或智能负载均衡。它更接近：

> 对外暴露逻辑模型/分组名 -> 对内解析为一个有序候选列表 -> 按顺序串行尝试 -> 失败后冷却或禁用。

sub2api 的机制更复杂。它面向多租户和账号池，有：

- 分组与账号池。
- sticky session。
- 账号并发槽位和等待队列。
- priority + LRU。
- load factor。
- 高级调度器的 top-K 加权选择。
- 临时不可调度、限流、过载、过期账号过滤。
- 监控、用量、计费、分组限制。

当前产品最适合吸收的是“稳定逻辑模型 + 健康感知路由 + 简洁可解释观测”，而不是直接搬 sub2api 的多租户计费系统。

## API-Switch 事实

### 对外 auto 模型

API-Switch 会把 group_name 合成为 OpenAI `/v1/models` 返回中的虚拟模型；因此客户端可以看到或请求 `auto`。

证据：

- `API-Switch/src-tauri/src/proxy/handlers.rs:416-454`
- `API-Switch/src-tauri/src/database/schema.rs:177-178`

但协议行为不完全一致：

- OpenAI 模型列表会合成 group/auto。
- Claude、Gemini、Azure 相关列表主要列真实 entry。
- 请求时仍可以填 `auto`，因为路由层会处理。

### 路由解析顺序

API-Switch 的 `resolve()` 顺序是：

1. 空模型归一化为 `auto`。
2. group_name 精确匹配。
3. model 精确匹配。
4. display_name 精确匹配。
5. model contains 模糊匹配。
6. fallback 到 auto 组。

证据：

- `API-Switch/src-tauri/src/proxy/router.rs:65-184`

候选列表排序默认按 `sort_index` 升序。

证据：

- `API-Switch/src-tauri/src/proxy/router.rs:27-35`
- `API-Switch/src-tauri/src/proxy/router.rs:79-97`

### 关键风险：显式 auto 与 fallback auto 不是同一条过滤路径

数据库中 `get_enabled_entries_for_auto()` 会过滤：

- entry enabled。
- channel enabled。
- cooldown 已过期。

证据：

- `API-Switch/src-tauri/src/database/dao/api_entry_dao.rs:626-643`

但显式请求 `auto` 会先在 `all_entries` 做 group_name 精确匹配。`all_entries` 来自 `get_entries_for_routing()`，这个查询不按 `channel.enabled = 1` 过滤。

证据：

- `API-Switch/src-tauri/src/database/dao/api_entry_dao.rs:611-624`
- `API-Switch/src-tauri/src/proxy/router.rs:115-132`

因此当前实现里：

- 显式 `auto`、空模型、命中任意 group_name 的请求，可能选中已禁用 channel 下的 entry。
- 真正使用 `get_enabled_entries_for_auto()` 的，是前面所有匹配都失败后的 fallback auto。

这个实现不能照搬。

### 轮换机制

API-Switch 的 `forward_with_retry()` 对候选 entry 逐个尝试。它没有发现以下机制：

- round robin 游标。
- 权重字段。
- 随机或加权随机。
- 会话粘性。
- 最少连接。
- 动态 EWMA 延迟评分。
- 成本感知路由。

证据：

- `API-Switch/src-tauri/src/proxy/forwarder.rs:887-1047`

所以它是“固定优先级主备故障转移”，不是“轮询负载均衡”。

### 探测机制

API-Switch 有人工触发的探测和测速：

- URL 可达性探测。
- 协议和模型列表探测。
- 单模型非流式请求测速。
- 批量模型测速。

值得借鉴：

- 同渠道内串行测速，避免单渠道被并发打爆。
- 探测结果记录延迟和成功失败。
- 探测和真实转发分开。

风险：

- URL 可达不等于模型可用。
- 只收到 2xx 和非空 body 可能不足以证明模型响应合法。
- 测速成功/失败直接改写 entry enabled，可能覆盖用户手动启停意图。
- 没有后台健康探测和自动恢复。

### 熔断与冷却

API-Switch 有 `CircuitBreaker`，但生产路径更依赖 DB cooldown 和 enabled。

证据：

- `API-Switch/src-tauri/src/proxy/circuit_breaker.rs:1-77`
- `API-Switch/src-tauri/src/proxy/forwarder.rs:3020-3205`

达到失败阈值后，它会写 6 小时 cooldown 并把 entry disabled。由于 enabled=false 不会随 cooldown 到期自动恢复，所以实际可能变成长期禁用，需要人工恢复或测速恢复。

风险：

- 内存熔断状态不持久。
- 半开探测没有单探针保护。
- 失败计数在并发场景下可能快速累积。
- `circuit_retry_codes` 配置存在，但本次没有在转发路径看到实际消费。

## sub2api 事实

### 基础账号模型

sub2api 的账号有这些调度相关字段：

- platform / type。
- concurrency。
- priority。
- load_factor。
- last_used_at。
- rate_limit_reset_at。
- overload_until。
- temp_unschedulable_until。
- group_ids / account_groups。

证据：

- `sub2api/backend/internal/service/account.go:15-62`
- `sub2api/backend/internal/service/account.go:118-134`
- `sub2api/backend/migrations/001_init.sql:60-84`
- `sub2api/backend/migrations/067_add_account_load_factor.sql:1`

### sticky session

sub2api 会优先尝试：

1. previous_response_id 绑定。
2. session_hash 绑定。
3. load balance。

证据：

- `sub2api/backend/internal/service/openai_account_scheduler.go:260-334`

这适合长会话/Agent 场景：同一个会话尽量打到同一个上游账号，减少上下文连续性、缓存和 upstream session 污染问题。

### 基础选择策略：priority + LRU

在非高级调度或降级路径中，sub2api 会按：

- priority 数值小者优先。
- 同 priority 下，从未使用优先。
- 再按 last_used_at 最久未使用优先。

证据：

- `sub2api/backend/internal/service/openai_gateway_service.go:1620-1836`

这比 API-Switch 固定 `sort_index` 更接近“轻量轮换”，成本低、可解释。

### Load-aware 调度

sub2api 支持基于并发负载的选择：

- 获取候选账号当前 load。
- 排除满载账号。
- 按 priority、load_rate、last_used_at 排序。
- 同排序组内打散。
- 未抢到槽位时提供 WaitPlan。

证据：

- `sub2api/backend/internal/service/openai_gateway_service.go:1837-2128`

### 高级调度器

sub2api 的高级调度器会计算候选分数，因素包括：

- priority。
- load。
- waiting queue。
- error rate EWMA。
- TTFT EWMA。

然后从 top-K 中做加权选择，避免长期垄断。

证据：

- `sub2api/backend/internal/service/openai_account_scheduler.go:609-1030`
- `sub2api/backend/internal/service/openai_account_scheduler.go:1059-1098`

这是 Pro 级特性，不建议直接做进 Lite。

### 临时不可调度

sub2api 会根据错误码和关键词，把账号临时标记为不可调度，而不是立即永久禁用。

证据：

- `sub2api/backend/internal/service/account.go:118-134`
- `sub2api/backend/internal/service/ratelimit_service.go:1777-1910`

这是非常值得吸收的机制，因为它能减少“偶发上游问题导致永久误伤”的概率。

### 监控与探测

sub2api 有 channel monitor 表，支持周期性对 provider/endpoint/api_key/model 做心跳检测，并保存历史状态。

证据：

- `sub2api/backend/migrations/125_add_channel_monitors.sql:1-47`
- `sub2api/backend/internal/service/channel_monitor_const.go:12-44`

这是形成 Pro 护城河的关键能力：用户不只是需要“能转发”，更需要知道“哪个渠道现在靠谱、为什么不靠谱、是否该自动降级”。

## 当前项目应采用的最优方向

### 不建议直接照搬 API-Switch

原因：

- 它不是严格轮换。
- 显式 auto 的过滤存在风险。
- 失败策略会把 400/404 等业务错误也纳入 failover，容易污染健康状态。
- 熔断/冷却/恢复语义不够一致。

### 不建议直接照搬 sub2api

原因：

- sub2api 是多租户平台，包含余额、用户、订阅、账号池、OAuth、支付、复杂计费。
- 当前产品定位更接近本地/团队代理工具，直接引入会提高部署、配置和维护成本。

### 推荐吸收的低成本高价值机制

P0：先做“正确的 auto”

- 对外暴露稳定逻辑模型名，例如 `auto`、`coding-fast`、`coding-pro`。
- 对内映射到候选 provider/model。
- 候选必须统一过滤 enabled、channel enabled、cooldown、capability。
- 明确区分 requested_model 与 upstream_model，日志里同时记录。
- 未知模型不要静默 fallback 到 auto，除非用户显式配置了 fallback 策略。

P1：做“轻量健康感知路由”

- 默认策略：priority + last_success_at / last_used_at。
- 失败只对可重试错误做 cooldown。
- 400、参数不兼容、模型不存在、上下文超限，默认不计入渠道健康失败。
- 流式开始后不透明切换，只记录失败和健康事件。
- cooldown 到期后用单探针半开恢复，不直接放量。

P1：做“人工探测 + 后台轻量探测”

- 人工测试保留。
- Pro 增加后台探测：按 provider/model 定期发小请求。
- 探测结果只写 health_state，不直接覆盖用户 enabled。
- 探测有全局并发预算，同渠道串行。

P2：做“sticky session”

- 对 VS/Copilot/Codex 这类长会话，按 session hash 绑定到同一 upstream。
- 上游出错、满载、延迟过高时允许逃逸。
- sticky 命中率、逃逸原因进入日志。

P2：做“加权 top-K”

- 使用 priority、近期成功率、TTFT、排队/并发、成本倍率计算分数。
- 从 top-K 加权选择，避免单一最高分长期吃满。
- 该功能应放 Pro，因为需要 UI、指标、解释和回归测试。

## TODO

P0 工程 TODO：

- 统一候选过滤函数，避免 auto、具体模型、Azure/Gemini 等路径过滤不一致。
- 设计 `RouteCandidate`：provider、model、upstream_model、capabilities、enabled、cooldown、priority、health_state。
- 明确错误分类：不可重试、可重试、鉴权/额度、客户端取消、流式中断。
- 文案中避免称“轮换”，除非实现了 round robin、weighted random、least-load 等真实策略。

P1 工程 TODO：

- 增加 health state：healthy、cooldown、degraded、disabled、probing。
- cooldown 只影响路由，不覆盖用户手动 enabled。
- half-open 使用单探针令牌。
- 增加 route decision 日志：为什么选它、为什么跳过其他候选。

P2 工程 TODO：

- 增加 sticky session 表或本地缓存。
- 增加 provider/model 级别健康历史。
- 增加后台探测 worker，带并发预算和退避。
- 增加 Pro 的路由策略配置：优先低价、优先稳定、优先快速、平衡。

测试 TODO：

- auto 显式请求不会命中 disabled provider/channel。
- unknown model 默认不静默 fallback。
- 400/404 不污染健康状态。
- 429/5xx/网络错误进入 cooldown。
- 流式响应开始后不重放。
- cooldown 到期只允许单探针。
- sticky 命中、逃逸、清理。
- top-K 不长期垄断。

## 审核结论

- API-Switch 的“对外 auto、对内候选”边界值得借鉴，但其当前实现不是最优方案。
- sub2api 的调度模型更成熟，但只能提炼思想，不能整体迁入。
- 当前产品最优路线是：Lite 保持简单稳定，Pro 做健康感知 auto router、后台探测、sticky session 和可解释成本/质量路由。
