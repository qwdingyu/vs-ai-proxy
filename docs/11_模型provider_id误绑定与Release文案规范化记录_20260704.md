# 模型 provider_id 误绑定与 Release 文案规范化记录

日期：2026-07-04

本文记录最近两类上线前问题的定位、修复和验证过程：

1. 管理端保存配置时报 `model_provider_not_found`，典型报错为模型 `z-ai/glm-5.2` 绑定的 `provider_id` 为 `z-ai`，但 `providers[].id` 中不存在 `z-ai`。
2. GitHub Releases 页面文案不规整，最近 release 正文中出现字面量 `\n`，并且发布说明来源不稳定。

本文只记录本次已核查的事实、代码改动和验证结果，不包含未验证推测。

## 1. 问题一：模型命名空间被误当成 provider_id

### 1.1 用户现场 payload

用户在 `/admin#/models` 保存时触发校验错误。相关配置形态如下：

- `providers[].id` 包含：`useai`、`deepseek`、`ollama`、`usecpa`。
- `models[]` 中存在：
  - `name`: `z-ai/glm-5.2`
  - `provider_id`: `z-ai`
  - `provider`: `z-ai`

错误信息为：

```text
[model_provider_not_found] models[3].provider_id - 模型 "z-ai/glm-5.2" 绑定的 provider_id "z-ai" 不存在；请填写 providers[].id，留空表示按优先级自动路由。
```

### 1.2 根因

本项目中 `provider_id` 的语义是“运行时 provider 实例 ID”，必须对应用户配置里的 `providers[].id`，例如 `deepseek`、`usecpa`、`ollama`。

但内置模型元数据中的 `provider` 或模型名前缀，如 `z-ai/glm-5.2` 的 `z-ai`，只是模型厂商/命名空间标识，不等同于运行时 provider 实例 ID。

因此，`provider_id: "z-ai"` 不是一个有效运行时绑定。正确处理方式不是创建一个假的 `z-ai` provider，也不是只在前端局部清空，而是在配置加载、保存和校验入口统一识别这种“模型命名空间误填”，并迁移为空绑定，让运行时按 provider 优先级自动路由。

### 1.3 修复位置

核心修复下沉到了配置层，避免只修管理端 API 而遗漏冷启动或热加载：

- `internal/config/config.go`
  - 新增 `NormalizeForRuntime(cfg *AppConfig)`。
  - 新增 `NormalizeModelProviderBindings(models, providers)`。
  - 当 `provider_id` 不存在于 provider 引用集合中，且它正好等于模型名 slash 前缀时，例如 `z-ai/glm-5.2` 的 `z-ai`，将 `ProviderID` 和旧字段 `Provider` 都清空。
  - `NewManager()` 冷启动加载配置后执行 `NormalizeForRuntime`。
  - `Manager.Save()` 保存配置前执行 `NormalizeForRuntime`。
  - `Manager.Reload()` 热加载配置后执行 `NormalizeForRuntime`。

管理端校验复用同一规则：

- `internal/api/api.go`
  - `validateAppConfig()` 对克隆配置执行 `config.NormalizeForRuntime(normalized)` 后再校验模型和 provider 关系。
  - 真正未知的 provider_id 仍继续报 `model_provider_not_found`。

前端增加防线，但不作为唯一修复点：

- `web/dist/index.html`
  - 模型搜索结果点击后不再自动把内置元数据的 provider 写入 `provider_id`。
  - 保存模型时，如果用户手填的 `provider_id` 不在当前 provider 列表里，提示清空后按优先级自动路由。

### 1.4 回归测试

新增/调整的关键测试包括：

- `internal/api/api_test.go`
  - `TestConfigValidateClearsModelNamespaceProviderBinding`
    - 验证 `name=z-ai/glm-5.2` 且 `provider_id=z-ai` 时，配置校验通过。
  - `TestConfigValidateReportsUnknownModelProvider`
    - 验证 `provider_id=missing-provider` 仍然报 `model_provider_not_found`。
  - `TestConfigSaveMigratesUserPayloadWithModelNamespaceProviderBinding`
    - 使用接近用户现场的 payload 保存配置。
    - 验证保存后的 `z-ai/glm-5.2` provider 绑定为空。
    - 验证 registry 能对 `z-ai/glm-5.2` 产生自动路由候选。

- `internal/config/config_test.go`
  - `TestManagerReloadMigratesModelNamespaceProviderBinding`
    - 验证热加载旧配置时能迁移 `provider_id=z-ai`。
  - `TestNewManagerMigratesModelNamespaceProviderBinding`
    - 验证冷启动读取旧配置时也能迁移。

### 1.5 已执行验证

本地验证命令：

```bash
go test ./internal/config -run 'Test(NewManager|ManagerReload)MigratesModelNamespaceProviderBinding' -count=1
go test ./internal/config ./internal/api ./internal/proxy ./internal/provider
go test ./...
git diff --check
```

验证结果：上述命令均通过。

## 2. 问题二：GitHub Release 文案不规整

### 2.1 现场现象

通过 GitHub API 检查最近 release，`v0.2.12` 的正文包含字面量 `\n`，例如：

```text
Release v0.2.12\n\nThis release supersedes v0.2.11 ...
```

这会导致 GitHub Releases 页面正文显示不规整。

### 2.2 根因

发布 workflow 原先使用：

```yaml
generate_release_notes: true
```

该方式依赖 GitHub 自动生成说明，正文结构不受本项目脚本控制。同时，之前 release 正文中出现过字面量 `\n`，说明手工或脚本生成 release body 时曾把换行转义文本直接写入了正文。

此外，原 workflow 中 release job 执行：

```bash
make release
make install
```

`make release` 已生成压缩包；继续执行 `make install` 会重新构建并把裸二进制便携包放入 `dist/`，容易让 release assets 混入非预期产物。

### 2.3 修复位置

新增规范 release notes 生成脚本：

- `.bin/release-notes.sh`
  - 生成固定结构 Markdown：
    - `Highlights`
    - `Downloads`
    - `Verify`
    - `Changes since <previous tag>`
    - `Full changelog`
  - 本地预览未来 tag 时，如果目标 tag 尚未存在，使用 `<previous tag>..HEAD` 生成变更列表。
  - GitHub Actions 中目标 tag 已存在时，使用 `<previous tag>..<tag>` 生成变更列表。

新增规范打 tag 脚本：

- `.bin/tag-release.sh`
  - 接受 `0.2.13` 或 `v0.2.13`，统一生成 `vX.Y.Z` tag。
  - 检查工作区是否干净。
  - 创建 annotated tag，tag message 使用 `.bin/release-notes.sh` 生成。
  - 可通过 `--push` 推送 tag，从而触发 GitHub Actions 发布。

调整 GitHub Actions：

- `.github/workflows/build.yml`
  - release job 的 `actions/checkout@v4` 设置 `fetch-depth: 0`，确保能读取历史 tag。
  - release job 只执行 `make release`，不再执行 `make install`。
  - 新增步骤生成 `release-notes.md`。
  - `softprops/action-gh-release@v2` 改用 `body_path: release-notes.md`。

调整 Makefile：

- `Makefile`
  - 新增 `make release-notes`。
  - `release` target 不再依赖 `build-all`，因为 `.bin/release-all.sh` 自身已负责清理、跨平台构建和打包，避免重复构建。

调整构建日志：

- `.bin/release-all.sh`
  - 修正日志中可能出现的重复 `v`，例如避免 `vv0.2.13`。

### 2.4 发布验证方式

用户明确要求：发布验证必须走 GitHub Actions 编译，不要本地编译发布包，也不要手工上传产物。

实际执行流程：

1. 本地只做脚本语法和测试验证，不生成发布资产、不上传资产。
2. 提交并推送 main。
3. 使用 `.bin/tag-release.sh 0.2.13 --push` 创建并推送 `v0.2.13` tag。
4. GitHub Actions 由 tag push 自动触发。
5. 使用 `gh run watch` 等待 Actions 完成。
6. 使用 `gh release view` 检查 release 正文和 assets。

### 2.5 GitHub Actions 结果

本次发布 tag：

- `v0.2.13`

发布 commit：

- `bef58d5a9ef1b9ae6dfa3509cb3155c93878a0d4`

GitHub Actions run：

- `https://github.com/qwdingyu/vs-ai-proxy/actions/runs/28698735304`
- 状态：`completed`
- 结论：`success`

Release 页面：

- `https://github.com/qwdingyu/vs-ai-proxy/releases/tag/v0.2.13`

Release 检查结果：

- 非 draft。
- 非 prerelease。
- asset 数量为 5。
- asset 名称为：
  - `vs-ai-proxy-v0.2.13-linux-arm64.tar.gz`
  - `vs-ai-proxy-v0.2.13-linux-x64.tar.gz`
  - `vs-ai-proxy-v0.2.13-macos-arm64.tar.gz`
  - `vs-ai-proxy-v0.2.13-macos-x64.tar.gz`
  - `vs-ai-proxy-v0.2.13-windows-x64.exe.zip`

Release 正文检查结果：

- 正文为正常 Markdown。
- 未发现字面量 `\n`。
- 包含 `Highlights`、`Downloads`、`Verify`、`Changes since v0.2.12`、`Full changelog`。

### 2.6 已知非阻断提示

GitHub Actions 输出过 Node.js 20 deprecation warning，来源于：

- `actions/checkout@v4`
- `actions/setup-go@v5`
- `softprops/action-gh-release@v2`

该 warning 未导致本次 workflow 失败。本次 release job 和 check job 均成功完成。

## 3. 后续操作规范

### 3.1 修改 provider/model 相关逻辑时

必须区分两类概念：

- `provider_id`：运行时 provider 实例 ID，对应 `providers[].id`。
- 模型命名空间：模型名中的厂商前缀，例如 `z-ai/glm-5.2` 的 `z-ai`，只是模型目录元数据，不代表运行时 provider。

不要把模型目录里的 provider 字段自动写入 `provider_id`，除非能证明它等于当前配置中的 `providers[].id`。

### 3.2 发布新版本时

推荐流程：

```bash
make release-notes
.bin/tag-release.sh 0.2.14 --push
```

注意：

- 发布包应由 GitHub Actions 构建和上传。
- 不要本地编译后手工上传 release assets。
- 不要手写包含转义换行的 release body。
- 如果需要修正文案，应优先修改 `.bin/release-notes.sh`，再通过 tag 触发 Actions 验证。

## 4. 本次变更摘要

提交：

- `bef58d5 Normalize model namespace bindings before release`

主要文件：

- `.bin/release-all.sh`
- `.bin/release-notes.sh`
- `.bin/tag-release.sh`
- `.github/workflows/build.yml`
- `Makefile`
- `internal/api/api.go`
- `internal/api/api_test.go`
- `internal/config/config.go`
- `internal/config/config_test.go`
- `web/dist/index.html`

本次记录文档：

- `docs/11_模型provider_id误绑定与Release文案规范化记录_20260704.md`
