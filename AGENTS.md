# AGENTS.md — 本仓库的强制约定

> 本文件是**硬性规则**，不是建议。任何自动化代理（AI agent）或协作者在本仓库操作时
> 都必须遵守。若某条规则与临时指令冲突，先停下来向仓库所有者确认。

---

## 1. `docs/` 目录不纳入版本管理（强制）

**规则**：`docs/` 目录（含所有子目录与文件）**永远不得进入 Git 版本管理，也不得推送到 GitHub**。

- 文档文件**保留在本地磁盘**，作为仓库所有者的私有笔记。
- `.gitignore` 已包含目录级规则 `/docs/`。**不要删除这条规则**。
- 决策沿革：`docs/` 曾于 2026-09 进入版本管理（原 `docs/32` 的 P0 决策），
  现已**整体移出版本管理**。这是仓库所有者的明确决策，不要"好心"恢复跟踪。

### 禁止的操作

```bash
git add docs/                 # ✗ 禁止
git add -f docs/              # ✗ 禁止（-f 会绕过 .gitignore）
git add -A                    # ✗ 禁止（会连带暂存 docs/ 与 .bin/ 产物）
git add .                     # ✗ 禁止（同上）
git rm --cached -r docs 后再次 add docs/   # ✗ 禁止
```

### 必须的做法

提交前**显式列出路径**，不要依赖通配：

```bash
git add internal/proxy/foo.go internal/proxy/foo_test.go
git status --porcelain        # 提交前自查：输出里不得出现 docs/
```

### 提交前自查

任何提交前，确认暂存区**没有新增或修改** `docs/` 下的文件：

```bash
# 只放行删除（D），拦截新增(A)/修改(M)/重命名(R)
bad=$(git diff --cached --name-status | awk '$1 != "D" && $2 ~ /^docs\// {print}')
[ -n "$bad" ] && { echo "违规：docs/ 被暂存："; echo "$bad"; exit 1; }
echo "OK：暂存区无 docs/ 新增或修改"
```

> 注意：不要用 `git diff --cached --name-only | grep '^docs/'` 做这条自查。
> 那个写法对**删除**也会命中，会在"移除 docs"这类合法提交上误报。

若发现 `docs/` 被误暂存，立即撤销：

```bash
git restore --staged docs/
```

---

## 2. 不要使用 `git add -A` / `git add .`

本仓库同时存在两类**不应提交**的内容：

- `docs/`（私有文档，见上）
- `.bin/large-request-matrix/` 下每次跑矩阵都会新增的逐用例诊断产物（几十个）

**只显式添加你确实要提交的文件。**

---

## 3. 提交信息

- 使用中文或英文均可，但需说明**为什么**改，而不只是改了什么。
- 涉及行为变更的修复，请写明根因与验证方式。

---

## 4. 代码变更的最低验证要求

提交前至少通过：

```bash
gofmt -l <改动文件>      # 必须为空
go vet ./...             # 必须无输出
go build ./...           # 必须成功
go test ./... -count=1   # 必须全绿
```

发布前另需 `make release-check`。
