# Visual Studio Copilot `/v1/chat/completions` 503 诊断记录

> 日期: 2026-07-03
> 现象: Visual Studio Copilot 实测每次请求 `/v1/chat/completions` 返回 503。
> 后续: 503/模型路由问题解决后，又发现了 HTTP 200 但 Visual Studio 流式解析失败的问题，详见 `docs/08_VisualStudio_Copilot_流式_finish_reason_踩坑记录_20260703.md`。

## 1. 已复现的真实原因

本次 503 不是上游模型服务直接返回的 503，而是代理路由层在找不到候选 provider 时返回了 `503 Service Unavailable`。

复现请求中的模型名是：

```text
deepseed-v4-flash
```

正确模型名应为：

```text
deepseek-v4-flash
```

当请求错误模型名时，旧行为会返回：

```text
503
X-Proxy-Candidate-Count: 0
所有候选提供商均不可用
```

这会误导排查方向，看起来像上游服务不可用。实际问题是模型名无法解析到任何 provider。

## 2. 本次修复

1. 修正本地配置文件中的模型拼写。
   - 已把 `deepseed-v4-flash` 修正为 `deepseek-v4-flash`。
   - 已清空错误的 `provider_id=deepseed-v4-flash`，让路由按 provider 优先级自动选择。

2. 修复 OpenAI chat 入口的错误状态码。
   - `/v1/chat/completions` 现在在候选 provider 为空时返回 `400 Bad Request`。
   - 响应体会返回结构化诊断 JSON，`error.code=model_not_routable`，并包含请求模型、解析模型、候选 provider 数量和中文排查建议。

3. 修复刚启动后直接 chat 的冷启动问题。
   - 旧逻辑依赖 `/v1/models` 或 `/api/tags` 先触发 catalog rebuild。
   - 新逻辑在 `/v1/chat/completions` 和 `/api/chat` 入口都会先重建 catalog。
   - 如果模型尚未被发现，但 provider 启用，会按 provider 优先级 fallback 尝试原始模型名。

## 3. 当前验证结果

刚启动后，不先访问 `/v1/models`，直接请求：

```text
model=deepseek-v4-flash
```

结果：

```text
200 OK
X-Proxy-Candidate-Count: 1
X-Proxy-Primary-Provider: useai
X-Proxy-Upstream-Model: deepseek-v4-flash
```

错误拼写：

```text
model=deepseed-v4-flash
```

现在不再返回代理层 503。它会进入 provider fallback，最终由上游返回模型错误或网关错误，诊断头中会显示实际尝试的 provider 和 upstream model。
响应体也会包含结构化错误分类，例如 `network_error`、`upstream_api_error`、`timeout`、`proxy_parse_error`，用于区分网络问题、上游 API 问题、超时和协议解析问题。

## 4. Visual Studio 侧处理建议

如果 Visual Studio 仍继续请求 `deepseed-v4-flash`：

1. 在 Visual Studio / Copilot 配置里重新选择模型 `deepseek-v4-flash`。
2. 如果 UI 有缓存，重启 Visual Studio。
3. 重新访问代理模型列表，确认 `/v1/models` 里展示的是 `deepseek-v4-flash`。
4. 不要手动填 `deepseed-v4-flash`。

## 5. 快速排查命令

```bash
curl -s http://127.0.0.1:12345/v1/models | python3 -m json.tool
```

最小 chat 验证：

```bash
curl -i http://127.0.0.1:12345/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"ping"}],"stream":false,"max_tokens":8}'
```

看响应头：

- `X-Proxy-Candidate-Count: 0` 表示模型名无法解析到 provider。
- `X-Proxy-Provider` 表示实际尝试的 provider。
- `X-Proxy-Upstream-Model` 表示转发给上游的真实模型名。
