# metadata-catalog-go

可本地长期运行的元数据目录服务：支持多来源摄入、版本化结构演进、血缘维护与影响分析。
状态持久化到本地文件，进程重启后自动恢复；在重复、乱序和并发写入下保持可追溯且结论稳定。

## 本地常驻服务

`cmd/catalogd` 把读取和写入都以 HTTP JSON API 暴露出来，可直接起在本地，
用 curl 或脚本调用：

```bash
go run ./cmd/catalogd -addr 127.0.0.1:8080 -data catalog-data.json
```

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/entities` | 摄入实体（body 为 Entity JSON） |
| POST | `/api/relations` | 摄入关系（body 为 Relation JSON） |
| POST | `/api/evolve` | 结构演进（`entity_id`/`schema`/`renames`/`source`/`version`） |
| POST | `/api/merge` | 合并实体（`dst`/`src`/`source`/`version`） |
| GET | `/api/entities`、`/api/entities/{id}` | 查询实体 |
| GET | `/api/relations` | 查询全部关系 |
| GET | `/api/rejections` | 查询全部拒绝记录 |
| GET | `/api/impact?root=ID&tags=a,b&terms=..&owners=..` | 影响分析 |

写入响应统一为 `{"applied":..,"duplicate":..,"rejection":{..}}`：
接受/重复返回 200，判定拒绝返回 409（`rejection.reason` 可区分原因），
持久化失败返回 500（`error` 字段）。

示例：

```bash
curl -X POST localhost:8080/api/entities -d '{"id":"orders","kind":"dataset","source":"etl-a","version":2,"schema":{"fields":[{"name":"id","type":"int","required":true}]}}'
curl localhost:8080/api/rejections
curl 'localhost:8080/api/impact?root=orders&tags=finance'
```

## 持久化与恢复

- 每次状态变更（实体/关系写入、合并、拒绝记录）后，将完整状态以 JSON
  **整体快照**写入 `-data` 指定文件：先写临时文件再 `rename` 原子替换，
  崩溃不会留下半截文件。
- 重启时一次性加载快照，实体、关系、**历次拒绝记录**（含单调递增 `Seq`）
  全部恢复，恢复后的状态与重启前完全一致。
- 重启前被判为重复的提交，重启后再提交**仍然是重复**；乱序/冲突写入重启后
  仍按相同原因拒绝，`Seq` 接续不重置、不重复计数；以更高版本重试继续收敛到
  一致状态。
- 兼容范围：快照格式为当前版本的 JSON 结构，向后兼容不在本版本承诺范围内；
  纯内存用法（`catalog.New()`）行为不变。

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
| 同版本同内容（**忽略来源**，跨来源重复也算） | 幂等成功（Duplicate） | — |
| 同版本不同内容（并发冲突） | 拒绝 | `VERSION_CONFLICT` |
| 版本低于当前（乱序） | 拒绝，不覆盖新版本 | `STALE_VERSION` |
| 写入已合并实体 | 拒绝 | `ENTITY_MERGED` |
| 引用不存在的实体 | 边保留为悬挂，**落地时**记录一次日志 | `DANGLING_REFERENCE` |
| 结构演进不兼容 | 拒绝，状态不变 | `SCHEMA_INCOMPATIBLE` |
| 合并双方字段类型冲突 / 重复合并 / 自我合并 | 拒绝 | `MERGE_CONFLICT` |

**目标实体缺失时的跨来源重复判定**：悬挂检查发生在版本与重复判定**之后**，
只有真正落地的写入（新边或更高版本）才会记录一条 `DANGLING_REFERENCE`。
因此同一悬挂关系由来源 A 提交后，来源 B 再提交内容相同、仅来源不同的同一
关系时只算幂等重复（Duplicate）：**不新增任何拒绝/告警记录，可追溯序号
`Seq` 也不会空跳**；多来源并发重复提交时恰好落地一次、记录一条。而真正的
乱序（`STALE_VERSION`）、同版本内容冲突（`VERSION_CONFLICT`）与目标实体
确实缺失时的首次记录都不会被吞掉，原因始终可区分。

**重启恢复的一致性**：上述判定产生的状态（实体、关系、拒绝记录、`Seq`）
在每次变更后整体快照落盘，因此重复提交之后无论马上重启、先穿插一次其他
成功写入再重启、还是连续重启多次，恢复出的状态都与重启前查询到的完全一致；
重启后继续重试同一关系仍判重复，状态与序号原地不动（见
`TestDanglingDuplicateRestartConsistency`、`TestConcurrentDanglingDuplicateSources`）。

**多来源重复与冲突的判定边界**：实体与关系的"内容相同"比较都**忽略
`Source` 字段**——同一实体/关系从不同来源重复提交，只要内容实质相同就统一
按幂等重复处理，不会误报冲突；但同版本而内容（结构、标签、关系类型、端点等）
真正不同的写入仍判为 `VERSION_CONFLICT`。判定规则固定：同样的输入在重启前后、
任何部署规模下都给出同样的结论与同样的原因。

冲突方以更高版本号重试即可恢复一致状态（见
`TestConcurrentUpdatesConflictAndRetry`）。

**结构演进兼容性规则**（规则固定，相同输入结论可复现）：

- 新增可选字段：兼容；新增必填字段：不兼容。
- 删除字段：不兼容；通过 `RenameHint` 显式声明的改名：兼容。
- 类型加宽（`int→float`、`int/float/bool→string`）：兼容；其余类型变化：不兼容。

**实体合并**：`MergeEntities(dst, src)` 将指向 src 的关系重定向到 dst，
标签/术语/所有者取并集，src 标记 `MergedInto`；双方同名字段类型冲突、
重复合并或自我合并时整体拒绝且状态不变。

**合并的一致性边界（不丢边）**：重定向按确定性两阶段执行（先按原 ID 排序
收集受影响边，再统一重写），保证一条边都不丢：

- 重定向后产生的**自环**（src==dst）保留，不静默丢弃；
- 重定向后的规范 ID 与已有边冲突时，保留原 ID 作为确定性去重键，
  **重复边**因此两条都在、`Relations` 可查；
- 相同输入的合并结果逐字节一致（见 `TestMergePreservesAllEdges`）。

## 影响分析的边界

`Impact(root, filter)` 从起点沿血缘下游（src→dst 方向）做 BFS：

- **一致性**：查询基于 `Store.Snapshot()` 在**单次读锁**内取走的实体+关系
  快照，持续写入期间单次查询看到的一定是某一时刻的完整状态，
  不会一半旧数据一半新数据（见 `TestImpactConsistentSnapshotDuringWrites`）。
- **循环**：visited 集合保证每个节点只展开一次；成环的边（含合并产生的
  自环）被截断并列入 `CycleEdges`，不会无限遍历。
- **悬挂引用**：目标不存在的边不静默丢弃，目标 ID 列入 `DanglingIDs`，
  但不作为节点展开。
- **筛选**：按 `Tags` / `Terms` / `Owners` 过滤（任一命中即保留）；
  被过滤的节点不出现在结果中，但其下游仍会被遍历。
- **确定性**：输出按实体 ID 排序，相同图状态下结果可复现。

## 本地验证

```bash
go build ./...        # 编译
go test ./... -v      # 单元测试（日志含关键输入、版本号与判定依据）
go test -race ./...   # 竞态检测
go run ./cmd/catalog  # 内存版端到端演示：摄入/演进/血缘/合并/拒绝日志

# 常驻服务 + 重启恢复冒烟：
go run ./cmd/catalogd -addr 127.0.0.1:8080 -data /tmp/catalog.json &
curl -X POST localhost:8080/api/entities -d '{"id":"a","kind":"dataset","source":"s1","version":1,"schema":{"fields":[]}}'
kill %1                # 模拟重启
go run ./cmd/catalogd -addr 127.0.0.1:8080 -data /tmp/catalog.json &
curl localhost:8080/api/entities    # 状态已恢复
```

测试覆盖：重复与乱序摄入、并发更新冲突与重试、兼容/不兼容结构演进、
显式改名、循环血缘、悬挂引用、合并冲突、写入期间查询一致性，以及：
重启后恢复（实体/关系/拒绝记录与 `Seq` 接续）、跨来源重复提交（实体与关系）、
合并不丢边（重复边与自环确定性保留）、持续写入期间影响分析的快照一致性、
目标实体缺失时跨来源重复提交（不新增记录、序号不空跳，含马上重启/穿插写入
后重启/连续重启/重启后重试的对照）、多来源并发重复提交同一悬挂关系。

本地复现与验证：

```bash
go test ./catalog/ -run 'Dangling|ConcurrentDangling' -v  # 悬挂关系跨来源重复与重启一致性
go test -race ./...                                        # 全量 + 竞态检测
```
