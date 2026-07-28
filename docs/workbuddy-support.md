# WorkBuddy（腾讯）支持评估

## 状态：待确认

创建时间：2026-07-28

## 背景

WorkBuddy 声称支持 OpenAI 兼容 API 模式，vs-ai-proxy 当前为通用 OpenAI-compatible provider 提供了完整的支持（流式、工具调用、认证透传），理论上可以直接使用。

## 待确认事项

1. **WorkBuddy 的 API 地址**：是 `https://api.hunyuan.cloud.tencent.com/v1`（腾讯混元官方 OpenAI 兼容接口），还是企业微信内部的另一个地址？
2. **模型名称**：计划使用的模型是 `hunyuan-turbos-latest` 还是其他？
3. **联通性测试**：需要 API Key 做一次完整的配置到测试的链路验证。

## 分析结论

- 当前 vs-ai-proxy 不需要改代码，`type: "openai"` 即可支持
- 内置 `models.json` 已有 `tencent/hy3-preview` 条目（256K 上下文，支持推理和工具调用）
- 远程元数据拉取器（OpenRouter → LiteLLM）会自动补齐 `tencent/hunyuan-*` 系列模型的能力信息
- 注意混元 API 默认 5 并发限制，防御模式的短重试可能触发该限制