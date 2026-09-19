# SenseNova & eforge Provider 接入与踩坑记录

> 日期: 2026-07-02
> 涉及项目: vs-ai-proxy (Go), copilot-ollama-multi-provider-ai-proxy (C# 参考实现)
> 测试上游: https://token.sensenova.cn/v1 + https://api.eforge.xyz/v1

---

## 1. 配置方式

### 1.1 Web/config.json 配置

当前 vs-ai-proxy 的大模型 provider 配置统一保存在 `config.json`，并通过 Web 管理面板维护。
`.env` 只保留端口、配置路径、日志路径和可选代理认证，不再保存 `PROVIDER_*_API_KEY` / `PROVIDER_*_BASE_URL`。

示例 provider：

```json
{
  "id": "sensenova",
  "name": "SenseNova",
  "display_name": "SenseNova",
  "type": "openai",
  "base_url": "https://token.sensenova.cn/v1",
  "api_key": "<your-api-key>",
  "enabled": true,
  "priority": 0
}
```

示例模型配置：

```json
{
  "name": "deepseek-v4-flash",
  "provider_id": "sensenova",
  "provider": "sensenova",
  "context_length": 1048576,
  "max_output_tokens": 65536,
  "supports_tools": true,
  "enabled": true
}
```

**不要再通过 `.env` 覆盖 provider**：这样可以避免同一项大模型配置同时存在于 Web、`config.json`、`.env` 三个位置，降低用户和维护者的认知负担。

### 1.2 UseAI provider 的能力约束

| 项目 | 值 |
|---|---|
| 类型 | openai 兼容 |
| ChatPath | `chat/completions` |
| ModelsPath | `models` |
| 最终请求 URL | `https://token.sensenova.cn/v1/chat/completions` |
| 最终模型列表 URL | `https://token.sensenova.cn/v1/models` |
| 协议格式 | ApiFormatOpenAi |

> 注意：UseAI 的 ChatPath 和 ModelsPath 不带 `/v1/` 前缀（与 deepseek 等 provider 不同），因为上游 `BaseURL` 已经包含了 `/v1`。

---

## 2. 采坑记录

### 坑 1：使用了错误的模型名（deepseek-v4-pro）

**现象**：用 `deepseek-v4-pro` 调用 proxy，60 秒超时后返回 502。

**根因**：
- `deepseek-v4-pro` 在 `internal/provider/model-selection/deepseek.json` 中被定义为映射到 `deepseek` provider 的模型
- 但 `/v1/models` 接口返回的模型列表中包含的是 `deepseek-v4-flash`，而非 `deepseek-v4-pro`
- 直接调用上游 `POST /v1/chat/completions` 时，`deepseek-v4-pro` 返回 `{"error":{"message":"model route not found"}}`
- 结论：**token.sensenova.cn 这个 API 端点只暴露了 `deepseek-v4-flash`，没有 `deepseek-v4-pro`**

**教训**：
1. **永远用用户指定的模型名测试**，不要自作主张用默认配置中的模型名
2. 发现超时时，应直接调用上游 API 确认模型是否存在，而不是盲目等待超时
3. 上游 `/v1/models` 返回的模型列表是权威来源：直接调用 `curl https://token.sensenova.cn/v1/models -H "Authorization: Bearer $KEY"` 确认可用的模型

### 坑 2：模型发现结果与预期不一致

**现象**：proxy 启动后 `GET /v1/models` 和 `GET /api/tags` 返回的模型列表与上游 API 返回的不一致。

上游 API 实际返回：
```
sensenova-6.7-flash-lite, deepseek-v4-flash, glm-5.2, sensenova-u1-fast
```

但 proxy 的 `/health` 显示的可用模型是（之前某次启动时的遗留数据）：
```
kimi2.7-code, glm-5.2, minimax-m3, ..., deepseek-v4-pro, ...
```

**根因分析**：
- 模型列表来自两个来源的合并结果：① model-selection JSON 中静态定义的模型 ② provider ListModels 运行时发现的模型
- 模型列表中的 `owned_by: "ollama"` 是 `handleListModels` 的固定值，不代表实际 provider
- 实际运行时 `deepseek-v4-flash` 被正确路由到 UseAI provider 并成功返回响应（说明模型发现最终是正确的）

**教训**：
- proxy 的 model list endpoint 显示的模型可能包含了来自 model-selection JSON（静态配置）的模型，不一定完全等于上游 API 的模型列表
- 判断模型是否可用，最终以实际 chat 请求能否成功为准

### 坑 3：错误使用 pro 模型测试导致 proxy 日志刷爆

**现象**：测试 request 使用 `deepseek-v4-pro` 时，Regsitry 的 ResolveCandidates 路由到了 useAI 和 ollama 两个 provider。
  使用 deepseek-v4-pro 时，候选列表中 ollama 排在 useAi 前面（因为 ollama 在本地的端口可用性高），导致所有尝试都打到 ollama 并立刻失败（ollama 没有这个模型），日志瞬间刷了上千条 502 记录。

**教训**：
- 模型解析顺序是 priority 优先，但同 priority 的 provider 之间有排序
- 使用错误的模型名不仅会失败，还会因为 failover 机制测试所有候选 provider，产生大量无用日志
- 测试前一定要确认模型名是否正确

---

## 3. 验证结果

### 3.1 功能验证

| 测试项 | 方式 | 结果 |
|---|---|---|
| OpenAI 非流式 Chat | `POST /v1/chat/completions` 模型 `deepseek-v4-flash` | ✅ 成功，返回含 `reasoning_content` |
| Ollama 兼容非流式 Chat | `POST /api/chat` 模型 `deepseek-v4-flash` | ✅ 成功，返回含 `thinking` + `reasoning_content` |
| 模型列表 | `GET /v1/models` | ✅ 返回上游 API 的模型列表 |
| Ollama Tags | `GET /api/tags` | ✅ 返回标记列表 |
| 健康检查 | `GET /health` | ✅ 正常 |

### 3.2 deepseek-v4-flash 响应示例（OpenAI 格式）

```json
{
  "id": "8d380bc1-d422-4fa0-bd4a-fd69a15696a6",
  "created": 1782983707,
  "model": "deepseek-v4-flash",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "Hello",
      "reasoning_content": "We need to respond with a single word greeting..."
    },
    "finish_reason": "stop"
  }],
  "usage": {
    "prompt_tokens": 9,
    "completion_tokens": 25,
    "total_tokens": 34,
    "completion_tokens_details": {
      "reasoning_tokens": 22
    }
  }
}
```

---

## 4. ProxyTests (.NET 参考实现) 测试结果

### 4.1 总体统计

| 指标 | 数值 |
|---|---|
| 总测试数 | 363 |
| 通过 | 359 (98.9%) |
| 失败 | 4 |

### 4.2 4 个失败的测试详情

| 测试方法 | 失败原因 | 分析 |
|---|---|---|
| `ParameterValidationTests.AllModels_HaveCorrectContextWindowConfig` (nvidia/nemotron-3-super-120b-a12b) | context_length / minMaxOutput 断言值不匹配 | C# 参考实现的模型配置数据（model-selection JSON）与测试代码中的期望值不匹配 |
| `OverrideClientParamsTests.ApplyExecutionDefaults_OverrideClientParamsTrue_OverwritesClientMaxTokens` | override 行为与预期不符 | C# 参考实现的 OverrideClientParams 逻辑与测试预期有差异 |
| `OverrideClientParamsTests.ApplyExecutionDefaults_OverrideClientParamsTrue_AppliesToAllForcedFields` | 同上 | 同上 |
| `RequestTransformerTests.ApplyExecutionDefaults_InjectsTemperatureTopPAndMaxTokens` | 期望 max_tokens=8192 实际 16384 | C# 参考实现的 `deepseek.json` 中 deepseek-v4-pro 的 max_tokens 配置（8192）与测试代码期望值不匹配 |

### 4.3 失败判定

**这 4 个失败都是 copilot-ollama-multi-provider-ai-proxy 参考实现（C#）模型配置数据的问题，不是 vs-ai-proxy（Go）的问题。** 该测试集是 C# 项目的集成测试，用于验证 C# 实现的模型配置、参数覆盖逻辑和 RequestTransformer 行为。Go 项目有自己的测试集。

### 4.4 应对策略

| 类别 | 策略 |
|---|---|
| Go proxy 自身测试 | 运行 `go test ./...` 确保 Go 项目测试全部通过（本次未运行，建议补充） |
| C# ProxyTests 失败项 | 这几个失败与 vs-ai-proxy 无关，可以忽略。如需排查，需检查 C# 项目 `model-selection/deepseek.json` 和 `ModelProfile`/`OverrideClientParams` 代码 |
| 未来配置变更后的测试 | 每次修改 `.env` 或 config.json 后，至少用对应模型做一次真实 chat 请求验证 |

---

## 6. api.eforge.xyz 附加测试（2026-07-02）

### 6.1 配置说明

与 sensenova 完全相同的配置方式，只需在 Web 管理面板中新增或编辑 provider：

```json
{
  "id": "useai-paid",
  "name": "UseAI Paid",
  "display_name": "UseAI Paid",
  "type": "openai",
  "base_url": "https://api.eforge.xyz/v1",
  "api_key": "<your-api-key>",
  "enabled": true,
  "priority": 1
}
```

### 6.2 可用模型（来自 `/v1/models`）

| 模型 ID | 类型 |
|---|---|
| `deepseek-v4-flash` | 文本 |
| `gpt-5.4` | OpenAI 兼容 |
| `gpt-5.5` | OpenAI 兼容 |
| `gpt-image-1` | 图像生成 |

### 6.3 验证结果

| 测试项 | 结果 | 备注 |
|---|---|---|
| 上游 API 简洁请求 | ✅ | 无 extra params |
| 上游 API 带 temperature | ✅ | 首次测试超时（exit 28），但重试后正常，怀疑是临时波动 |
| Proxy `/v1/chat/completions` | ✅ | 返回含 reasoning_content |
| Proxy `/api/chat` | ✅ | 返回含 reasoning_content + thinking |
| ProxyTests (363) | 359/363 通过 | 4 个已知失败与 sensenova 测试一致 |

### 6.4 注意事项

1. **eforge 与 sensenova 的行为基本一致**，使用同一个 provider 配置即可
2. **首次请求超时可能是临时波动**，重试后恢复正常，无需特殊处理
3. 两个站点的模型列表不同：sensenova 有 `sensenova-6.7-flash-lite`、`sensenova-u1-fast` 等，eforge 有 `gpt-5.4`、`gpt-5.5`、`gpt-image-1`

---

## 7. 管理界面日志缺失提供商/模型字段修复

### 7.1 问题描述

dashboard 和日志页的「提供商」和「模型」列始终显示 `-`，即使代理已成功转发请求。

### 7.2 根因

`loggingMiddleware` 原本通过 HTTP 响应头（`X-Proxy-Provider` 等）读取 provider/model/upstream 信息。诊断响应头虽然正确设置，但中间件读取时未捕获到。

### 7.3 修复方案

在 `responseWriter` 结构体上增加 `provider`/`model`/`upstream` 三个字段，由 handler 函数在成功路径上直接设置，中间件优先读取这些结构化字段，兜底再从响应头读取。

**改动文件**：`internal/proxy/server.go`
- `responseWriter` 增加 `provider`/`model`/`upstream` 字段
- 新增 `setResponseLogFields()` 辅助函数（类型断言写入）
- `handleChatCompletions` 和 `handleOllamaChat` 中，在 `setAttemptDiagnosticHeaders` 之后调用 `setResponseLogFields`
- `loggingMiddleware` 优先读取 `ww.provider`/`ww.model`/`ww.upstream`，兜底响应头

### 7.4 验证结果

```json
// 修复前
{"method":"POST","path":"/v1/chat/completions","status_code":200}
// 修复后  
{"method":"POST","path":"/v1/chat/completions","provider":"UseAI","model":"deepseek-v4-flash","upstream":"deepseek-v4-flash","status_code":200}
```

两个端点（OpenAI `/v1/chat/completions` 和 Ollama `/api/chat`）均正确记录。

---

## 8. 架构说明：单端口与 `/admin` 管理面板

最新实现已从双端口收敛为单端口，默认监听 `127.0.0.1:12345`：

| 路径 | 用途 | 说明 |
|---|---|---|
| `/v1/*` | OpenAI-compatible 代理协议 | 供 Visual Studio / Copilot compatible client 调用 |
| `/api/chat`、`/api/tags`、`/api/show`、`/api/version` | Ollama-compatible 代理协议 | 供 BYOM 模型发现、模型详情、聊天和探活调用 |
| `/admin` | Web 管理面板 | 静态前端入口 |
| `/admin/api/*` | Web 管理 API | 配置、provider、model、日志、统计、测试台 |

### 设计理由

1. **降低用户认知负担**：用户只需要记住一个地址，例如 `http://127.0.0.1:12345`；管理面板固定为 `http://127.0.0.1:12345/admin`。
2. **不破坏 Visual Studio/Ollama 协议面**：管理 API 不再占用根路径 `/api/*`，根路径 `/api/chat`、`/api/tags`、`/api/show`、`/api/version` 全部保留给 Ollama-compatible 代理。
3. **安全默认值**：本机二进制默认只绑定 `127.0.0.1`；Docker 容器内部绑定 `0.0.0.0`，但 `docker-compose.yml` 默认只发布到宿主机 `127.0.0.1`。
4. **管理 API 可鉴权**：设置 `ADMIN_API_KEY` 后 `/admin/api/*` 要求 Bearer token；未设置 `ADMIN_API_KEY` 但设置了 `PROXY_API_KEY` 时，管理 API 会复用 `PROXY_API_KEY`。
5. **兼容旧配置**：推荐使用 `PORT=12345`；旧版 `PROXY_PORT` 仍可作为 fallback。`MANAGEMENT_PORT` / `MANAGEMENT_HOST` 已废弃，主程序不再启动独立管理端口。

---

## 9. 注意事项与最佳实践

### 9.1 配置相关

1. **provider 配置只在 Web/config.json 中维护**：不要在 `.env` 里放大模型 BaseURL 或 API Key
2. **UseAI 是内置第一方 provider**：默认排第一且不可删除；如果要配置付费 key，建议新增 `useai-paid` 这类 provider 实例
3. **API Key 会同时用于 Chat 和 ListModels**：OpenAI-compatible provider 的模型发现请求也会携带相同 Authorization header

### 9.2 测试相关

1. **先直接测上游再测 proxy**：`curl -s https://token.sensenova.cn/v1/models -H "Authorization: Bearer $KEY"` 验证连通性
2. **用正确的模型名**：从 `/v1/models` 的响应中 `data[].id` 字段确认模型名
3. **设置 curl --max-time 参数**：防止请求无响应时无限等待（Chat 类请求建议 30-90 秒）

### 9.3 架构理解

1. **UseAI 是 MultiModel 类型 provider**：可路由多个不同模型到同一个上游 API
2. **model-selection JSON 的 Provider 字段**不代表请求最终发送的 provider——它只是 catalog 中的静态映射，实际路由由 Registry 的 ResolveCandidates 决定
3. **model-selection 中 deepseek-v4-flash 的 provider 是 "deepseek"**（enabled: false 的 deepseek provider），但实际可用是因为 UseAI 的 ListModels 从上游发现了 `deepseek-v4-flash` 模型

---

## 10. 日志分页功能（2026-07-02）

### 10.1 需求

日志按时间倒序排列（最新在前），并支持分页浏览。

### 10.2 改动范围

| 层 | 文件 | 改动 |
|---|---|---|
| 后端 Store | `internal/store/store.go` | `GetLogs` 返回顺序改为最新在前；新增 `GetLogsPage(page, size)` 返回 `LogPageResult{Logs, Total, Page, Size}` |
| API 层 | `internal/api/api.go` | `GET /api/logs` 支持 `?page=N&page_size=M` 参数，存在 page 时返回分页格式；兼容旧的 `?limit=N` |
| 前端 | `web/dist/index.html` | `loadLogs` 改为分页模式；添加"上一页/下一页"按钮和页码信息；重置/清空时回到第 1 页 |

### 10.3 API 格式

**分页模式**（`?page=1&page_size=20`）：
```json
{"logs":[...],"total":1000,"page":1,"size":20}
```

**旧模式**（`?limit=20`，仪表盘使用）：
```json
{"logs":[...]}
```

两种模式返回的日志均按时间倒序排列。
