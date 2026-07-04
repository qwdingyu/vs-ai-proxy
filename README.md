# VS AI Proxy

VS AI Proxy 是一个面向 Visual Studio / Copilot BYOM 场景的本地 AI 代理服务。它把多个上游 AI provider 聚合到一个本地端口，兼容 OpenAI `/v1/*` 和 Ollama `/api/*` 常用接口，并提供 Web 管理面板用于配置 provider、模型和运行状态。

## 核心功能

- **单端口代理**：默认监听 `127.0.0.1:12345`，同时提供代理 API 和管理面板。
- **多 provider 路由**：支持 OpenAI-compatible provider、DeepSeek、UseAI、Ollama 等配置形态，可按优先级自动路由。
- **Visual Studio / Copilot 适配**：兼容 `/v1/chat/completions`、`/v1/models`、`/api/chat`、`/api/tags`、`/api/show` 等路径。
- **Web 管理面板**：在 `/admin` 中管理 provider、模型、健康状态、请求日志和测试连接。
- **社群入口**：管理面板内置“联系我们”，可加入 QQ 群获取更新通知和交流支持。
- **模型元数据补齐**：按模型名补齐上下文长度、最大输出、tools/vision/reasoning 等展示和默认参数。
- **配置热加载**：配置文件变更后自动热加载；端口变更仍需重启进程。
- **版本检查与下载**：可检查 GitHub Release 最新版本，并自动下载当前平台资产。

## 快速开始

从 GitHub Releases 下载当前平台压缩包：

- macOS Apple Silicon：`vs-ai-proxy-vX.Y.Z-macos-arm64.tar.gz`
- macOS Intel：`vs-ai-proxy-vX.Y.Z-macos-x64.tar.gz`
- Linux x64：`vs-ai-proxy-vX.Y.Z-linux-x64.tar.gz`
- Linux ARM64：`vs-ai-proxy-vX.Y.Z-linux-arm64.tar.gz`
- Windows x64：`vs-ai-proxy-vX.Y.Z-windows-x64.exe.zip`

解压后运行：

```bash
./vs-ai-proxy
```

启动后打开管理面板：

```text
http://127.0.0.1:12345/admin
```

默认配置文件位置：

```text
~/.config/vs-ai-proxy/config.json
```

## 常用命令

```bash
# 查看当前版本
./vs-ai-proxy --version

# 检查是否有新版本
./vs-ai-proxy --check-update

# 下载并解压最新版本到默认更新目录
./vs-ai-proxy --update

# 全自动安装更新并重启到新版
./vs-ai-proxy --self-update

# 下载并解压到指定目录
./vs-ai-proxy --update --update-dir /tmp/vs-ai-proxy-update
```

说明：正式版本普通启动时会先自动检查更新；如果发现 GitHub Release 中存在更新版本，会下载、校验、备份当前二进制、替换为新版，并自动切换到新版进程。macOS/Linux 使用原地进程切换，Windows 使用延迟替换脚本。若检查或替换失败，程序会记录警告并继续启动当前版本，避免因为 GitHub 网络或权限问题阻断服务。

如需关闭启动自动更新，可设置：

```bash
VS_AI_PROXY_AUTO_UPDATE=0 ./vs-ai-proxy
```

`--update` 只下载并解压更新包；`--self-update` 会立即执行同样的全自动安装和重启流程，适合在命令行中主动触发升级。

如果没有配置 `GITHUB_TOKEN`，程序仍可检查更新；只有遇到 GitHub 匿名 API 限流时，才会提示稍后重试或设置 token。可选配置：

```bash
export GITHUB_TOKEN=你的_token
./vs-ai-proxy --check-update
```

## 基本配置流程

1. 启动 `vs-ai-proxy`。
2. 打开 `http://127.0.0.1:12345/admin`。
3. 在「提供商」页面添加或编辑 provider：
   - `id`：运行时 provider 实例 ID，例如 `deepseek`、`usecpa`、`ollama`。
   - `type`：`openai` 或 `ollama`。
   - `base_url`：上游 API 地址。
   - `api_key`：上游密钥，Ollama 本地通常可留空。
   - `priority`：数字越小越优先。
4. 在「模型」页面添加模型。
5. 在「测试」页面测试 provider 和模型连通性。

注意：模型名中的厂商前缀不是 provider ID。例如 `z-ai/glm-5.2` 中的 `z-ai` 是模型命名空间，不一定对应 `providers[].id`。如果不确定 provider 绑定，请留空 `provider_id`，系统会按 provider 优先级自动路由。

## Visual Studio / Copilot 配置

把本服务作为本地 BYOM endpoint 使用：

```text
http://127.0.0.1:12345
```

根据客户端要求选择 OpenAI-compatible 或 Ollama-compatible 路径。本项目同时保留 `/v1/*` 和 `/api/*` 两类接口，便于 Visual Studio / Copilot 不同版本的模型发现和聊天请求。

## 环境变量

| 变量 | 作用 |
| --- | --- |
| `CONFIG_PATH` | 指定配置文件路径 |
| `STORE_PATH` | 指定请求日志文件路径 |
| `HOST` | 指定监听地址，默认 `127.0.0.1` |
| `PORT` | 覆盖配置中的端口 |
| `ADMIN_API_KEY` | 启用管理面板/API 访问保护 |
| `GITHUB_TOKEN` | 版本检查时使用 GitHub 鉴权，避免 API 限流 |

## 联系我们

欢迎加入 QQ 群：`390485182`。

群内主要交流 Visual Studio / Copilot BYOM、多 provider 配置、Ollama/DeepSeek/OpenAI-compatible 接入和部署排障；加入后也会不定期发布免费大模型体验活动信息。

## 从源码构建

要求 Go 版本见 `go.mod`。

```bash
go test ./...
make build
./vs-ai-proxy --version
```

不建议直接运行裸 `go build ./cmd/server` 作为发布构建；请通过 `make build` 或 release workflow 注入版本号，避免 Web 页面和 `--version` 显示为开发兜底版本。

Docker 构建时同样建议传入版本号：

```bash
docker build --build-arg VERSION="$(git describe --tags --always --dirty)" -t vs-ai-proxy:local .
```

跨平台 Release 包由 GitHub Actions 构建。维护者发布时使用：

```bash
make release-notes
.bin/tag-release.sh 0.2.14 --push
```

不要手工上传本地构建产物到 GitHub Release。

## 更多文档

详细排障和设计记录见 `docs/`：

- `docs/11_模型provider_id误绑定与Release文案规范化记录_20260704.md`
- `docs/12_版本检查与自动下载设计记录_20260704.md`
