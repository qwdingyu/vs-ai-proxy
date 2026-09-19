# GitHub Release 构建与踩坑记录

> 日期：2026-07-02  
> 现象：GitHub 仓库 Releases 区域没有构建产物，且 About 区域显示 "No description, website, or topics provided."  
> 结论：这次未创建 Git tag，导致 `.github/workflows/build.yml` 中的 `release` job 不会触发，因此不会生成 GitHub Release。

---

## 1. 现象与排查过程

### 1.1 用户反馈
- GitHub 仓库的 **Releases** 区域显示 "No releases published"。
- About 区域显示仓库缺少描述/主题，但这与 Release 生成无关。

### 1.2 排查步骤
1. **检查 Actions 运行记录**
   - 使用 `gh run list --repo qwdingyu/vs-ai-proxy --limit 20` 查看最近的 workflow 运行。
   - 发现只有 `check` job（编译验证）成功运行，没有 `release` job 的执行记录。

2. **检查 workflow 定义**
   - 读取 `.github/workflows/build.yml`。
   - 发现 `release` job 的触发条件是：
     ```yaml
     if: startsWith(github.ref, 'refs/tags/v')
     ```
   - 这意味着 **只有推送 `v*` 格式的 tag 时**，`release` job 才会执行。

3. **检查本地 git 状态**
   - 确认当前分支 `main` 已有 commit，但没有 tag。
   - 使用 `git tag` 确认无 tag。

### 1.3 根因确认
- **根因**：将代码 push 到 `main` 分支不会触发 release 流程。
- workflow 的 `on.push.tags: [v*]` 规则要求必须推送 tag 才会创建 Release。
- 用户之前"正常"的经历，应该是每次发布时都手动或自动创建了 `v*` tag。

---

## 2. 当前实现说明

### 2.1 workflow 结构
文件：`.github/workflows/build.yml`

```yaml
name: build

on:
  push:
    branches: [main]
    tags: [v*]
  pull_request:
    branches: [main]

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: 静态检查
        run: go vet ./...
      - name: 编译（当前平台）
        run: go build ./cmd/server

  release:
    if: startsWith(github.ref, 'refs/tags/v')
    needs: check
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: 跨平台编译 + 打包
        run: make release
      - name: 创建 Release
        uses: softprops/action-gh-release@v2
        with:
          files: dist/*
          generate_release_notes: true
```

### 2.2 触发逻辑
| 事件 | 触发 job | 说明 |
|------|----------|------|
| push 到 `main` | `check` | 只做 `go vet` 和单平台编译 |
| PR 到 `main` | `check` | 同上 |
| push `v*` tag | `check` + `release` | 先验证，再全平台构建并创建 Release |

### 2.3 构建产物
`make release` 会执行：
1. `make build-all`：为以下平台编译二进制文件
   - `darwin/amd64`
   - `darwin/arm64`
   - `linux/amd64`
   - `linux/arm64`
   - `windows/amd64`
2. 打包压缩：
   - Linux/macOS：`.tar.gz`
   - Windows：`.zip`
3. 产物输出到 `./dist/` 目录
4. GitHub Release 会自动上传 `dist/*` 中的所有文件

### 2.4 版本号来源
- `Makefile` 中 `VERSION` 通过 `git describe --tags --always --dirty` 获取。
- 如果打 tag `v1.2.3`，则版本为 `v1.2.3`；如果无 tag，则 fallback 为 `dev`。
- 二进制文件命名格式：`vs-ai-proxy-v1.2.3-darwin-arm64` 等。

---

## 3. 正确发布步骤

### 3.1 发布流程
```bash
# 1. 确保代码已提交并推送到 main
git add -A
git commit -m "your message"
git push origin main

# 2. 打版本 tag（必须）
git tag v1.2.3
git push origin v1.2.3
```

### 3.2 验证 Release 创建
```bash
# 查看最近的 workflow
gh run list --repo qwdingyu/vs-ai-proxy --limit 5

# 查看 release 列表
gh release list --repo qwdingyu/vs-ai-proxy

# 查看最新 release 详情
gh release view latest --repo qwdingyu/vs-ai-proxy
```

### 3.3 注意事项
- **必须创建 tag**：push 代码到 main 不会创建 Release，必须显式打 `v*` tag。
- **tag 命名**：必须以 `v` 开头（如 `v1.2.3`），否则 workflow 中的 `startsWith(github.ref, 'refs/tags/v')` 条件不会满足。
- **权限**：`release` job 需要 `contents: write` 权限才能创建 Release，已在 workflow 中配置。
- **产物路径**：`softprops/action-gh-release@v2` 会上传 `dist/*`，确保 `make release` 成功生成产物。

---

## 4. 未来优化建议

### 4.1 自动化版本管理
考虑使用 `semantic-release` 或自定义 action 根据 commit message 自动打 tag，避免手动遗漏。

### 4.2 仓库描述
GitHub About 区域的描述、website、topics 需要在仓库 Settings 页面手动配置，或通过 `gh repo edit` 命令设置：

```bash
gh repo edit qwdingyu/vs-ai-proxy \
  --description "Visual Studio Copilot BYOM Local Proxy" \
  --add-topic go \
  --add-topic proxy \
  --add-topic ollama \
  --add-topic openai
```

### 4.3 验证清单
发布前检查：
- [ ] 代码已 push 到 main
- [ ] 已创建 `v*` tag 并 push
- [ ] Actions workflow 中 `release` job 已成功完成
- [ ] GitHub Releases 页面已出现新版本
- [ ] 所有平台的构建产物已上传

---

## 5. 相关文件索引

| 文件 | 说明 |
|------|------|
| `.github/workflows/build.yml` | GitHub Actions workflow，定义构建和发布流程 |
| `Makefile` | 定义 `build`、`build-all`、`release` 等目标 |
| `cmd/server/main.go` | 入口文件，包含 `version` 变量（通过 ldflags 注入） |
| `go.mod` | Go 模块定义，workflow 从中读取 Go 版本 |

---

## 6. 实际操作结果

### 6.1 已执行步骤
1. 确认代码已提交并推送到 `main`。
2. 创建 tag `v0.1.0` 并推送到 GitHub。
3. 触发 GitHub Actions `release` job。
4. 验证 Release 创建成功并上传构建产物。

### 6.2 验证结果
- **GitHub Actions 运行**：`https://github.com/qwdingyu/vs-ai-proxy/actions/runs/28576079509`
  - `status`: `completed`
  - `conclusion`: `success`
- **GitHub Release**：`https://github.com/qwdingyu/vs-ai-proxy/releases/tag/v0.1.0`
  - `tagName`: `v0.1.0`
  - `publishedAt`: `2026-07-02T08:25:17Z`
  - 包含以下构建产物：
    - `vs-ai-proxy-vv0.1.0-darwin-amd64.tar.gz`
    - `vs-ai-proxy-vv0.1.0-darwin-arm64.tar.gz`
    - `vs-ai-proxy-vv0.1.0-linux-amd64.tar.gz`
    - `vs-ai-proxy-vv0.1.0-linux-arm64.tar.gz`
    - `vs-ai-proxy-vv0.1.0-windows-amd64.exe`
    - `vs-ai-proxy-vv0.1.0-windows-amd64.zip`

### 6.3 发现的问题
- **二进制文件命名问题**：产物文件名中包含重复的 `v` 前缀（如 `vs-ai-proxy-vv0.1.0-...`）。
  - 原因：`Makefile` 中 `VERSION` 变量已包含 `v` 前缀（来自 `git describe --tags --always --dirty`），而文件名模板又手动加了一个 `v`。
  - 影响：Release 功能正常，但产物名称不够规范。
  - 修复建议：将 `Makefile` 中的文件名模板改为使用 `$(VERSION)` 而非 `v$(VERSION)`。
