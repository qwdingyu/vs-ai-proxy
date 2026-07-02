---
name: integrate-new-component-with-tests
description: 在已有测试覆盖的代码库中接入新组件时，保持向后兼容并逐步完成集成的可复用流程。
source: auto-skill
extracted_at: '2026-07-02T02:05:54.514Z'
---

# 在已有测试库中接入新组件

## 适用场景

- 向已有单元测试覆盖的代码库引入新组件（如 ModelCatalog）
- 需要修改现有函数签名（如 snapshot 返回更多值）
- 新组件可能在测试环境中为 nil，需要 nil-guard 保护
- 需要跨多个文件同步更新函数调用和测试

## 核心原则

1. **先保证编译通过**：修改签名后，先修复所有编译错误，再优化逻辑
2. **nil-guard 优于重构测试**：在新组件未完全集成前，为 nil 情况提供安全降级
3. **测试优先修复**：运行 `go test` 发现失败，先修测试再修实现
4. **最小改动原则**：每次只改一个关注点，验证后再继续

## 标准流程

### 阶段 1：准备新组件

```go
// 确保新组件可以安全创建空实例
func NewXxx(...) *Xxx {
    if xxx == nil {
        return &Xxx{...} // 返回可用空实例
    }
    // 正常初始化
}
```

### 阶段 2：修改核心函数签名

1. 修改函数返回值（如 `snapshot()` 从 2 值改为 3 值）
2. 全局搜索所有调用点：`grep "s\\.snapshot\\(\\)"` 或等效
3. 逐个修复调用点，接受新返回值或使用 `_` 忽略

### 阶段 3：接入新组件到业务逻辑

1. 在 handler 中获取新组件：`_, _, catalog := s.snapshot()`
2. 添加 nil 检查：`if catalog != nil { ... }`
3. 调用新组件方法，处理返回值

### 阶段 4：更新测试

1. 运行 `go test` 查看失败
2. 更新测试中的函数调用以匹配新签名
3. 如果新组件需要，在测试中创建最小可用实例

### 阶段 5：验证

```bash
gofmt -l <files>    # 检查格式
go build ./...      # 确保编译通过
go test ./...       # 确保所有测试通过
```

## 常见陷阱

- **忘记更新所有调用点**：修改函数签名后，使用 grep 确保全覆盖
- **测试中创建不完整的 Server**：如果 Server 结构体新增字段，测试中直接 `&Server{}` 可能导致 nil panic，需要在 `snapshot()` 或函数入口加 nil-guard
- **忽略返回值数量变化**：Go 对返回值数量严格，2 值变 3 值会导致编译错误
- **过度设计**：先实现最小可用版本，再逐步增强

## 示例：集成 ModelCatalog

1. 修改 `snapshot()` 返回 3 值
2. 在 `handleListModels` 中使用 `catalog.AllEntries()` 替代 `registry.AllModels()`
3. 在 `handleChatCompletions` 中获取 catalog 并调用 `catalog.Profile()`
4. 在 `buildOllamaShowBody` 中传入 catalog 并使用 profile 信息
5. 修复所有测试中的 `snapshot()` 调用
6. 运行 `go test ./...` 验证

## 检查清单

- [ ] 新组件可以安全创建空实例
- [ ] 所有函数签名已更新
- [ ] 所有调用点已修复（grep 验证）
- [ ] 业务逻辑已接入新组件
- [ ] 测试已更新并全部通过
- [ ] gofmt 和 go build 通过
