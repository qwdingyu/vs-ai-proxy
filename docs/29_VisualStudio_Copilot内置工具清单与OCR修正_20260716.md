# Visual Studio Copilot 内置工具清单与 OCR 修正（2026-07-16）

本文记录用户从 **Visual Studio Copilot** 界面提取到的内置工具名，以及本项目对这些工具名的集成状态。

重要边界：

1. 本清单来自用户当前 Visual Studio Copilot 环境的实际提取结果，不声明为 Microsoft 官方完整清单。
2. 本项目只面向 **Visual Studio Copilot**，不是 VS Code Copilot。
3. `取消选择所有工具`、`选择所有工具` 是界面操作文案，不是工具函数名。
4. OCR/图片转文本会丢失下划线，必须按工具函数名语义修正。
5. 工具 `function.name` 应使用稳定英文 snake_case 标识符；中文只应出现在 UI、描述或提示词中，不应预先加入中文工具名别名。

## 1. OCR 修正规则

| OCR/界面文本 | 修正后工具名 | 说明 |
| --- | --- | --- |
| `create file` | `create_file` | 空格是 OCR/转写误差，工具函数名应为 snake_case。 |
| `edit files` | `edit_files` | 空格是 OCR/转写误差，且这里是复数工具名。 |
| `read file` | `read_file` | 空格是 OCR/转写误差。 |
| `file_searchfind_symbol` | `file_search` + `find_symbol` | OCR/换行丢失，应拆成两个工具。 |
| `取消选择所有工具` | 非工具名 | Visual Studio UI 操作文案。 |
| `选择所有工具` | 非工具名 | Visual Studio UI 操作文案。 |

## 2. 集成状态说明

本文中的“已集成”不是指代理会向 Visual Studio 伪造工具声明。代理仍遵循原有安全边界：

- 标准 OpenAI `tool_calls` / legacy `function_call` 默认稳定透传。
- 任意工具只要由 Visual Studio 当前请求声明，代理应保留其名称、参数、ID 和 finish reason。
- 工具目录只用于：
  - 已观察工具名登记；
  - DSML 文本方言转换；
  - 安全别名归一化；
  - 回归测试覆盖。
- 不把 NuGet、modernization、profiler 等高语义工具降级映射成 shell/terminal。
- 不把中文 UI 文案加入 function name alias。

集成方式含义：

| 集成方式 | 含义 |
| --- | --- |
| 精确工具名 | 已加入 `copilotToolCatalog` 的 canonical 工具名。 |
| 安全别名 + 精确声明透传 | 已作为 alias 收录；如果 Visual Studio 请求中精确声明同名工具，代理会优先保留该精确名称。 |

## 3. Planning 工具

| 工具名 | 已集成 | 集成方式 | 备注 |
| --- | --- | --- | --- |
| `adapt_plan` | 是 | 精确工具名 | 已有工具。 |
| `finish_plan` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `record_observation` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `signal_plan_ready` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `update_plan_progress` | 是 | 精确工具名 | 本轮补入 observed catalog。 |

## 4. GitHub Copilot 工具

| 工具名 | 已集成 | 集成方式 | 备注 |
| --- | --- | --- | --- |
| `ask_question` | 是 | 精确工具名 | 已有工具。 |
| `create_file` | 是 | 精确工具名 | OCR 文本 `create file` 已修正。 |
| `detect_memories` | 是 | 精确工具名 | 已有工具。 |
| `edit_files` | 是 | 精确工具名 | OCR 文本 `edit files` 已修正。 |
| `file_search` | 是 | 精确工具名 | 与 `find_symbol` 拆开记录。 |
| `find_symbol` | 是 | 精确工具名 | 与 `file_search` 拆开记录。 |
| `get_background_terminal_output` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `get_errors` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `get_files_in_project` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `get_output_window_logs` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `get_projects_in_solution` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `get_tests` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `get_web_pages` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `grep_search` | 是 | 精确工具名 | 已有工具。 |
| `profiler_agent` | 是 | 精确工具名 | 不映射到 terminal。 |
| `read_file` | 是 | 安全别名 + 精确声明透传 | OCR 文本 `read file` 已修正；如果 VS 精确声明 `read_file`，代理保留该名称。 |
| `remove_file` | 是 | 安全别名 + 精确声明透传 | 不映射到 shell。 |
| `run_build` | 是 | 安全别名 + 精确声明透传 | 仅在声明 terminal 类目标时做安全别名；若 VS 精确声明 `run_build`，代理保留该名称。 |
| `run_command_in_terminal` | 是 | 精确工具名 | 已有工具。 |
| `run_tests` | 是 | 安全别名 + 精确声明透传 | 仅在声明 terminal 类目标时做安全别名；若 VS 精确声明 `run_tests`，代理保留该名称。 |
| `search_agent` | 是 | 精确工具名 | 不降级为搜索工具，避免 schema 不匹配。 |
| `start_modernization` | 是 | 精确工具名 | 不映射到 terminal。 |

## 5. NuGet 工具

| 工具名 | 已集成 | 集成方式 | 备注 |
| --- | --- | --- | --- |
| `fix_vulnerable_packages` | 是 | 精确工具名 | 不映射到 terminal。 |
| `get_latest_package_version` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `get_package_context` | 是 | 精确工具名 | 本轮补入 observed catalog。 |
| `review_supply_chain_security` | 是 | 精确工具名 | 不映射到 terminal。 |
| `update_package_version` | 是 | 精确工具名 | 不映射到 terminal。 |
| `upgrade_packages_to_latest` | 是 | 精确工具名 | 不映射到 terminal。 |

## 6. 中文命令与语言切换结论

当前不应加入中文工具名，例如 `创建文件`、`编辑文件`、`运行测试`。

原因：

1. Visual Studio UI 语言切换通常影响显示文案，不应影响 wire protocol 中的 `function.name`。
2. OpenAI-compatible 工具函数名最佳实践是稳定 ASCII/snake_case 标识符。
3. 中文别名容易把普通自然语言误判为工具名，增加错误映射和 schema 不匹配风险。
4. 如果未来真实抓包证明 Visual Studio 在某个语言环境下发送中文 `function.name`，应先保存原始请求样本，再按实证补充；不能提前猜测。

因此，当前最佳实践是：

- 代码只登记英文 snake_case 工具名。
- 文档记录中文 UI 文案与 OCR 修正规则。
- 排障时以请求日志中的 `request_tools` 和 `response_tools` 为准。

## 7. 已验证项

本轮集成后已验证：

```bash
go test ./internal/proxy -run 'TestCopilot|TestCanonicalTool|TestKnownCopilot|Test.*Tool.*'
go test ./internal/proxy -count=20 -run 'TestCopilotDeclaredToolContractMatrixNonStream|TestKnownCopilotToolNamesIncludesObservedVSDeclaredTools|TestCopilotToolCatalogAliasesMapOnlyToDeclaredTargets|TestCanonicalToolNameCoversCommonCopilotToolFamilies'
go test ./...
go test -race ./...
go vet ./...
git diff --check
make release-check
```

`make release-check` 包含 Windows amd64 cross-build。

## 8. 后续维护规则

1. 新增 Visual Studio Copilot 工具名时，先确认是否是 OCR/换行/空格误差。
2. 优先登记精确工具名，不要急于添加别名。
3. 只有语义和参数 schema 明确兼容时，才允许添加别名映射。
4. 高语义工具不得降级为 terminal/shell。
5. 新工具名必须补充：
   - `TestKnownCopilotToolNamesIncludesObservedVSDeclaredTools`
   - `TestCopilotDeclaredToolContractMatrixNonStream`
   - 必要时补 DSML/流式工具协议测试。
