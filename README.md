# VS AI Proxy

VS AI Proxy 是给 **Windows + Visual Studio Copilot BYOM** 场景使用的本地 AI 代理。它把 OpenAI-compatible 上游统一代理到本机 `127.0.0.1:12345`，让 Visual Studio 可以通过本地 endpoint 使用你配置的模型。

## 适合谁使用

- 使用 Windows 和 Visual Studio 的 Copilot 用户。
- 想在 Visual Studio 中接入自定义 OpenAI-compatible 服务的用户。
- 需要管理多个 provider、多个模型，并在一个本地地址中统一使用的用户。

## 主要功能

- **Visual Studio 适配**：支持 `/v1/models`、`/v1/chat/completions` 等 OpenAI-compatible 接口。
- **本地管理面板**：通过浏览器配置 provider、模型和测试请求。
- **模型下拉测试**：测试页按所选 provider 加载官方返回的模型，减少填错模型名。
- **工具调用兼容**：兼容常见 `tool_calls`、流式工具调用和部分 provider 的工具调用方言。
- **故障诊断**：请求日志会记录 provider、模型、上游模型、兼容档案和错误原因，方便排查。
- **Token 用量统计**：只统计上游明确返回 `usage` 的请求；未返回 usage 的请求显示为未知，不按 0 计算。
- **自动更新**：启动或命令行可检查并更新到最新 Release。

## Windows 快速开始

1. 打开 GitHub Releases，下载：

   ```text
   vs-ai-proxy-vX.Y.Z-windows-x64.exe.zip
   ```

2. 解压到固定目录，例如：

   ```text
   C:\vs-ai-proxy\
   ```

3. 双击运行：

   ```text
   vs-ai-proxy.exe
   ```

4. 浏览器打开管理面板：

   ```text
   http://127.0.0.1:12345/admin
   ```

5. 在「提供商」页面新增 provider：

   - `类型`：选择 `openai`
   - `Base URL`：填写上游地址，例如 `https://api.example.com/v1`
   - `API Key`：填写上游密钥
   - `启用`：保持开启

6. 在「测试」页面选择 provider，再从模型下拉框选择模型并点击测试。

## Visual Studio 配置

在 Visual Studio / Copilot 的 BYOM 或自定义模型服务配置中，填写本地 endpoint：

```text
http://127.0.0.1:12345
```

如果客户端要求 OpenAI-compatible 地址，通常使用：

```text
http://127.0.0.1:12345/v1
```

实际以 Visual Studio 当前版本的配置界面提示为准。

## 常用命令（Windows PowerShell）

进入解压目录后执行：

```powershell
.\vs-ai-proxy.exe --version
.\vs-ai-proxy.exe --check-update
.\vs-ai-proxy.exe --self-update
```

工具调用异常用户建议先执行：

```powershell
.\vs-ai-proxy.exe --check-update
.\vs-ai-proxy.exe --self-update
.\vs-ai-proxy.exe --version
```

确认版本为 GitHub Releases 中的最新版本后，再重新启动 Visual Studio。

如需临时指定端口：

```powershell
$env:PORT="12345"
.\vs-ai-proxy.exe
```

如需关闭启动自动更新：

```powershell
$env:VS_AI_PROXY_AUTO_UPDATE="0"
.\vs-ai-proxy.exe
```

如果客户环境不能访问 GitHub，可以把更新源改成内网静态 manifest：

```powershell
$env:VS_AI_PROXY_UPDATE_MANIFEST_URL="https://intranet.example/vs-ai-proxy/latest.json"
.\vs-ai-proxy.exe --check-update
.\vs-ai-proxy.exe --self-update
```

`latest.json` 使用 GitHub Release 响应的最小字段即可，资产文件和 `checksums.txt` 可以放在同一目录并使用相对 URL：

```json
{
  "tag_name": "v0.2.53",
  "html_url": "https://intranet.example/vs-ai-proxy/v0.2.53",
  "assets": [
    {
      "name": "vs-ai-proxy-v0.2.53-windows-x64.exe.zip",
      "browser_download_url": "./vs-ai-proxy-v0.2.53-windows-x64.exe.zip"
    },
    {
      "name": "checksums.txt",
      "browser_download_url": "./checksums.txt"
    }
  ]
}
```

也可以只针对一次命令指定：

```powershell
.\vs-ai-proxy.exe --check-update --update-manifest-url "https://intranet.example/vs-ai-proxy/latest.json"
```

## 配置文件位置

Windows 默认配置文件通常位于：

```text
%USERPROFILE%\.config\vs-ai-proxy\config.json
```

例如：

```text
C:\Users\你的用户名\.config\vs-ai-proxy\config.json
```

一般建议优先使用管理面板修改配置，不建议手工编辑 JSON。

## 常见问题

### 1. 测试页成功，Visual Studio 仍失败怎么办？

请优先查看管理面板中的请求日志，确认：

- Visual Studio 请求的模型名是否和测试页一致。
- 请求是否走到了正确 provider。
- 上游返回的是 401/403、404、429、5xx，还是超时。

### 2. 模型列表里有模型，但聊天失败怎么办？

模型列表成功只代表 `/models` 可访问，不代表 `/chat/completions` 一定可用。请在测试页选择同一个 provider 和模型做真实聊天测试。

管理面板中的「兼容档案」用于说明当前 provider 会按哪套规则转发请求，包括：

- 聊天路径，例如 `v1/chat/completions` 或 `chat/completions`。
- 模型路径，例如 `v1/models` 或 `models`。
- 输出 Token 参数，例如 `max_tokens` 或 `max_completion_tokens`。

如果日志显示 404，优先检查 Base URL 是否已经包含版本路径，避免拼成 `/v1/v1/...` 或 `/v4/v1/...`。如果日志显示“上游响应格式不兼容”，优先新建会话重试；若只在某个 provider 发生，再检查兼容档案和该 provider 的工具/输出 token 参数兼容性。

### 3. 工具调用失败怎么办？

请确认所选模型和上游 provider 支持工具调用。不同上游对流式、非流式、`tool_calls` 的兼容程度不同，建议优先使用测试页和请求日志定位。

如果在 Visual Studio Copilot 中看到 `create_file` / `apply_patch` / `get_file` / `grep_search` / `powershell` 等工具无法执行，或流式工具调用参数丢失，请先升级到最新版本。当前版本默认采用 stable 策略：OpenAI `tool_calls`、legacy `function_call` 和流式工具分片会尽量透传；DSML 文本方言会在当前请求声明了对应工具或安全别名时转换为标准工具调用。代理只做必要的工具名别名归一化与 `finish_reason` / `done_reason` 修正，不再默认注入 `Proxy blocked undeclared tool calls` 这类会干扰 Copilot 执行的内容。

### 4. Token 统计里的字段是什么意思？

- `Token 明细覆盖`：有多少请求拿到了上游返回的 `usage`。它不是成功率。
- `缓存命中的输入`：`prompt_tokens_details.cached_tokens`，是输入 Token 的子项，已经包含在输入 Token / 总 Token 中。
- `推理输出子项`：`completion_tokens_details.reasoning_tokens`，是输出 Token 的子项，已经包含在输出 Token / 总 Token 中。

因此，覆盖率低时不要把“总 Token”当作真实账单总额，只能理解为“已上报 usage 的那部分消耗”。

## 开源许可证

本项目代码使用 [Apache License 2.0](LICENSE)。

选择 Apache-2.0 的原因：它是真正开源许可证，包含专利授权，适合基础代理能力公开协作；商业分析、未脱敏排障记录、路线图和高级功能方案不通过许可证保护，而是不会进入公开仓库。

## 仓库公开边界

`docs/` 是本地私有知识库目录，已被 `.gitignore` 整体忽略。该目录可能包含未脱敏日志、provider 账号线索、商业分析和未来路线图，不应提交到公开仓库。

公开仓库只保留：

- `README.md`：用户可见的安装、配置和排障说明。
- `LICENSE`：开源许可证。
- 源码和测试。

如果需要公开某份文档，先脱敏后放到根目录或专门的公开文档路径，不要直接从本地 `docs/` 复制。

## 加入 QQ 群

欢迎加入 QQ 群交流 Visual Studio Copilot BYOM、provider 配置和排障问题。

QQ群：`390485182`

![QQ 群二维码](web/dist/assets/images/qrcode_qq.png)

## 开发者构建

普通用户不需要从源码构建。开发者可使用：

```powershell
go test ./...
make build
```

发布包由 GitHub Actions 构建，请不要手工上传本地构建产物到 Release。

## 更多文档

详细设计、排障和版本记录见 `docs/` 目录；建议先阅读 `docs/00_文档索引与系统架构总览_20260714.md`。
