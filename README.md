# metadata-catalog-go

可本地运行的元数据目录服务：支持多来源摄入、版本化结构演进、血缘维护与影响分析。
在重复、乱序和并发写入下保持可追溯且结论稳定。

## 元数据模型

- **Entity**：元数据实体（数据集、看板等）。关键字段：
  - `ID` / `Kind` / `Source`：标识、类别、来源系统。
  - `Version`：单调递增的版本号，并发与乱序控制的依据。
  - `Schema`：字段列表，每个字段含 `Name` / `Type` / `Required`。
  - `Tags` / `Terms` / `Owners`：影响分析筛选维度。
  - `MergedInto`：非空表示该实体已被合并，拒绝直接写入。
- **Relation**：实体间有向关系（`lineage` 血缘、`reference` 引用）。
  关系 ID 由 `type|src->dst` 确定性生成，多来源重复提交天然幂等。
- **Rejection**：每一次被拒绝的写入都会持久化到拒绝日志，带单调递增
  `Seq`、可区分的 `Reason` 与上下文 `Detail`。

## 版本与合并规则

写入采用乐观并发控制，结果分三类：

| 情形 | 结果 | 持久化原因 |
|---|---|---|
| 版本高于当前 | 接受（Applied） | — |
| 同版本同内容 | 幂等成功（Duplicate） | — |
| 同版本不同内容（并发冲突） | 拒绝 | `VERSION_CONFLICT` |
| 版本低于当前（乱序） | 拒绝，不覆盖新版本 | `STALE_VERSION` |
| 写入已合并实体 | 拒绝 | `ENTITY_MERGED` |
| 引用不存在的实体 | 边保留为悬挂，记录日志 | `DANGLING_REFERENCE` |
| 结构演进不兼容 | 拒绝，状态不变 | `SCHEMA_INCOMPATIBLE` |
| 合并双方字段类型冲突 / 重复合并 | 拒绝 | `MERGE_CONFLICT` |

冲突方以更高版本号重试即可恢复一致状态（见
`TestConcurrentUpdatesConflictAndRetry`）。

**结构演进兼容性规则**（规则固定，相同输入结论可复现）：

- 新增可选字段：兼容；新增必填字段：不兼容。
- 删除字段：不兼容；通过 `RenameHint` 显式声明的改名：兼容。
- 类型加宽（`int→float`、`int/float/bool→string`）：兼容；其余类型变化：不兼容。

**实体合并**：`MergeEntities(dst, src)` 将指向 src 的关系重定向到 dst，
标签/术语/所有者取并集，src 标记 `MergedInto`；双方同名字段类型冲突时
整体拒绝且状态不变。

## 影响分析的边界

`Impact(root, filter)` 从起点沿血缘下游（src→dst 方向）做 BFS：

- **循环**：visited 集合保证每个节点只展开一次；成环的边被截断并列入
  `CycleEdges`，不会无限遍历。
- **悬挂引用**：目标不存在的边不静默丢弃，目标 ID 列入 `DanglingIDs`，
  但不作为节点展开。
- **筛选**：按 `Tags` / `Terms` / `Owners` 过滤（任一命中即保留）；
  被过滤的节点不出现在结果中，但其下游仍会被遍历。
- **一致性**：查询基于 `RWMutex` 保护的快照，持续写入期间单次查询结果
  内部一致；输出按实体 ID 排序，保证确定性。

## 本地验证

```bash
go build ./...        # 编译
go test ./... -v      # 单元测试（日志含关键输入、版本号与判定依据）
go test -race ./...   # 竞态检测
go run ./cmd/catalog  # 端到端演示：摄入/演进/血缘/合并/拒绝日志
```

测试覆盖：重复与乱序摄入、并发更新冲突与重试、兼容/不兼容结构演进、
显式改名、循环血缘、悬挂引用、合并冲突、写入期间查询一致性。
