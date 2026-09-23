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
# 必须加 -c core.quotepath=false，否则中文路径会被 git 转义成 "docs/\344\270\255…"
# 而匹配不到 ^docs/ —— 本仓库 docs/ 下绝大多数文件名含中文，不加此参数等于形同虚设。
bad=$(git -c core.quotepath=false diff --cached --name-status \
      | awk '$1 != "D" && $2 ~ /^docs\// {print}')
[ -n "$bad" ] && { echo "违规：docs/ 被暂存："; echo "$bad"; exit 1; }
echo "OK：暂存区无 docs/ 新增或修改"
```

> **两个已知的写法陷阱**（都实际踩过）：
>
> 1. 不要用 `git diff --cached --name-only | grep '^docs/'` —— 它对**删除**也会命中，
>    会在「移除 docs」这类合法提交上误报。
> 2. 不要省略 `-c core.quotepath=false` —— git 默认转义非 ASCII 路径，
>    中文名文档会**静默漏过**检查。可用下面方式自证守卫有效：
>
>    ```bash
>    echo x > "docs/中文名测试.md" && git add -f "docs/中文名测试.md"
>    # 守卫必须报错；随后 git reset -q "docs/中文名测试.md" && rm "docs/中文名测试.md"
>    ```

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

---

## 5. 验证脚本必须沉淀为仓库资产（强制）

**规则**：任何用于验证行为的脚本，只要它**以后还可能再用**，就必须提交进仓库，
不得留在 `/tmp`、桌面或聊天记录里。

### 为什么

2026-09 排查 VS Copilot 故障时，验证「真实二进制是否会把能力上限当成 max_tokens」
的脚本被写成一次性 `/tmp` 脚本，用完即弃。后果：

- 下次遇到同类问题要从零重写，重复投入；
- 一次性脚本没有 review、没有回归，**它自己就出过 bug**（HTTP 方法默认成 GET，
  导致一次误判为产品缺陷，浪费了一轮排查）；
- 这类**产物级**缺陷（代码测试全绿、真实二进制却不可用）无法被 `go test` 拦住，
  只能靠沉淀下来的门槛脚本。

### 该放哪里

| 类型 | 位置 | 接入方式 |
|---|---|---|
| 真实二进制端到端核查 | `tests/e2e_binary_check.py` | `make e2e-check`（已并入 `release-check`） |
| 协议契约矩阵 | `internal/proxy/contract_matrix_test.go` | `make contract-matrix` |
| 工具调用核查 | `tests/tool_call_release_check.sh` | `make tool-check` |
| 国际化核查 | `tests/i18n_runtime_test.js` | `make i18n-check` |
| Go 单元/集成测试 | 与被测代码同包的 `*_test.go` | `go test ./...` |

### 硬性要求

1. **不得**把验证逻辑只写在 `/tmp` 或临时命令里就交付。
2. 新增发布门槛时，必须同时：
   - 提交脚本本身；
   - 在 `Makefile` 增加对应 target；
   - 若是发布门槛，加入 `release-check` 的依赖；
   - 在脚本头部注释里写明**它拦的是哪一类缺陷**。
3. 门槛脚本必须**可失败**：提交前用注入缺陷的方式验证它能报错。
   一个永远不会失败的门槛等于没有门槛。
4. 门槛脚本必须**确定性**：端口自动选取、用轮询替代固定 sleep、
   结束时无论成败都清理进程与临时目录，避免变成 flaky 门槛。
