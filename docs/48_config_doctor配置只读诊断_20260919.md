# config doctor：配置只读诊断

`config doctor` 是发布前/升级前的**只读**配置诊断命令，用来在启动服务之前
发现「配置看起来正常、但实际会请求错地址」这类问题。

它不是新的校验层，而是把既有的归一化与路由推导规则**反向展示**出来，
让 46 号审查发现的「配置固化陷阱」在升级前就可见。

## 用法

```bash
# 诊断默认配置（<XDG_CONFIG_HOME>/vs-ai-proxy/config.json）
vs-ai-proxy --config-doctor

# 诊断指定配置
CONFIG_PATH=/path/to/config.json vs-ai-proxy --config-doctor
```

**退出码**（便于接进脚本与门禁）：

| 退出码 | 含义 |
| --- | --- |
| `0` | 未发现会导致请求失败的问题 |
| `1` | 存在 warn/error 级问题，需人工确认 |
| `2` | 配置无法读取或解析 |

## 两个入口

| 入口 | 用途 | 说明 |
| --- | --- | --- |
| CLI `vs-ai-proxy --config-doctor` | **升级前**检查 | 能在写回发生**之前**看到「写回预览」与「推导标记」 |
| 管理页「高级 → 配置体检」 | 运行期检查 | 只读调用 `GET /admin/api/config/doctor`；适合查手改过的配置 |

两个入口共用 `config.CheckConfig`，结论完全一致，只是入口不同。

### 为什么升级前必须用 CLI

服务启动时 `config.NewManager` 会把推导出的 `transport` 路径**写回磁盘**
（v0.2.70 起的既有行为）。因此：

- **升级前用 CLI**：能看到 `<- 由推导填充`、`<- 将被改写`、以及「旧 URL → 新 URL」的
  WARN——这些信息在写回之后就消失了。
- **管理页**：因为服务已经启动过，写回通常已完成，`rewrite_needed` 多为 false。
  但它对**运行期间手改配置**依然有效（`LoadForDoctor` 每次都重新读磁盘），
  能立刻报出新增 provider 的推导结果、悬空绑定与地址变化。

管理页上已用提示文字说明了这一点，避免用户误以为它可以替代升级前的检查。

## 只读保证

诊断路径**绝不构造 `config.Manager`**。原因是 `NewManager` 在发现归一化差异时
会立即 `save()` 写回磁盘——那正是本命令要**预览**的副作用。
因此 `LoadForDoctor` 只做 `os.ReadFile` + `json.Unmarshal`，全程零副作用。

这一点有专门的回归测试锁定（`TestDoctor_LoadForDoctorDoesNotWrite`、
`TestRunConfigDoctorIsReadOnly`）。

## 输出内容

### 1. 实际上游地址

逐个 provider 展示 `base_url × transport` 拼接后的**真实请求地址**，
并标注路径来源：

- `<- 由推导填充`：磁盘上 `transport` 为空，路径是启动时推导出来的
- `<- 将被改写`：磁盘上的值会被归一化改写成另一个值

地址解析复用 `provider.ResolveUpstreamURL`，与真实转发**同一事实源**，
并有奇偶校验测试（`TestResolveUpstreamURLMatchesRealRequest`）确保两者不漂移。

### 2. 诊断结论

| 级别 | 含义 |
| --- | --- |
| `ERROR` | 几乎必然导致请求失败（缺 `http(s)://`、anthropic 类型路径不是 Messages 端点、provider id 重复） |
| `WARN` | 很可能失败，需人工确认（base_url 为空、绑定的 provider_id 不存在、升级前后上游地址发生变化） |
| `INFO` | 值得注意但通常无害（启动会写回磁盘、路径写法与能力注册表不同但拼接结果一致） |

### 3. 迁移写回预览（dry-run）

展示归一化会**写回磁盘**哪些内容（`config_version`、`transport.chat_path`、
`transport.models_path`），以及会被补入的 provider。对应 46 号审查的发现 #3。

## 验收：46 号审查发现 #2 / #3 必须被明确报告

| 场景 | 期望输出 |
| --- | --- |
| 非版本路径 base_url（如 `https://host/api`）+ transport 为空 | `WARN`：升级前后上游地址变化 `https://host/api/v1/chat/completions` → `https://host/api/chat/completions`；并提示可用显式 `transport.chat_path` 恢复 |
| 升级需要写回 | `INFO`：逐条列出将被写入的字段与磁盘原值 |
| 裸域名 base_url（新旧拼接结果一致） | **不得**告警（已用测试锁定，避免误报让用户忽略告警） |

## 已知边界（有意为之）

- **不含联网可达性探测**。本命令只做确定性推导与结构校验，不发起网络请求：
  探测需要 API Key、会产生费用、且受网络环境影响而抖动，不适合作为发布前门禁。
  真正的连通性由管理页的「测试连接 / 测试对话」负责。
- **已知能力 provider 的升级地址变化只给 INFO，不推断旧 URL**。
  能力注册表自身的约定在 v0.2.69 之后变过（以 google 为例，版本路径从
  `ChatPath` 挪进了 `DefaultBaseUrl`），用当前注册表反推「旧版 URL」会得出
  两边都不对的地址，比不报更糟。未知能力 provider 的旧行为是硬编码 fallback，
  可精确复现，因此那一条给 WARN。
- **不修改配置**。发现的问题需要用户自行决定如何修，doctor 只给建议。

## 未实现（与 docs/47 ③ 的差异）

docs/47 ③ 还包含一项，本命令**未包含**：

- **写回策略收紧**（派生字段不落盘）—— 当前仍是既有行为：归一化后写回磁盘。
  这也是「升级前必须用 CLI」这条注意事项的根因。

（管理页入口已完成，见上「两个入口」。）

## 相关文档

- `46_新版本VS_Copilot对接故障改动源全面审查_20260919.md` —— 发现 #2/#3 的来源
- `47_产品核心能力增强审查与低成本高价值实施项_20260919.md` —— 本命令的立项依据
