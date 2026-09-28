# metadata-catalog-go

可本地常驻运行的元数据目录服务：支持多来源摄入、版本化结构演进、血缘维护与影响分析。
在重复、乱序和并发写入下保持可追溯且结论稳定；数据经 WAL 持久化，进程重启后状态完整恢复。

## 快速开始：本地常驻服务

```bash
go build -o bin/catalogd ./cmd/catalogd
./bin/catalogd -addr 127.0.0.1:8080 -dir ./catalog-data
```

读写均通过 HTTP JSON 暴露，可用 curl 或任意脚本调用：

```bash
# 提交实体（写入）
curl -X POST 127.0.0.1:8080/entities -d '{
  "id":"orders","kind":"dataset","source":"etl-a","version":2,
  "schema":{"fields":[{"name":"id","type":"int","required":true}]}}'
# => {"applied":true,"duplicate":false}

# 提交关系（ID 缺省时按 type|src->dst 确定性生成）
curl -X POST 127.0.0.1:8080/relations -d '{"type":"lineage","src_id":"orders","dst_id":"mart","source":"etl-a","version":1}'

# 结构演进
curl -X POST 127.0.0.1:8080/entities/orders/evolve -d '{"schema":{...},"source":"etl-a","version":3}'

# 合并实体
curl -X POST 127.0.0.1:8080/merge -d '{"dst":"orders","src":"orders_dup","source":"ops","version":4}'

# 查询
curl 127.0.0.1:8080/entities            # 全部实体
curl 127.0.0.1:8080/entities/orders     # 单个实体
curl 127.0.0.1:8080/relations           # 全部关系（含悬挂边、自环、重名边）
curl 127.0.0.1:8080/rejections          # 历次被拒绝的写入及原因
curl '127.0.0.1:8080/impact?root=orders&tags=finance'   # 影响分析
```

写入响应三态可区分：`applied`（已生效）、`duplicate`（幂等重复）、
`rejection`（拒绝，含可区分的 `reason` 与单调递增 `seq`）。

## 持久化与恢复

- 所有状态变更（实体/关系应用、关系删除、拒绝记录）先追加到
  `<dir>/wal.jsonl` 并 `fsync`，再对内存状态生效；写盘失败会直接 panic，
  宁可失败也不静默丢数据。
- 重启时按序重放 WAL，恢复出与重启前一致的状态：实体、关系（含悬挂边、
  自环、合并产生的 `#N` 重名边）、全部拒绝记录及其 `Seq`。
- 日志记录的是"应用后的状态快照"而非待重新判定的操作，因此恢复结果与
  判定规则演进无关，历史结论可复现。
- 重启前被判为重复的提交，重启后再提交仍是重复（`duplicate=true`），
  不会变成接受；被拒绝的写入以更高版本重试后正常收敛，拒绝序号单调延续，
  不重复计数、不少数据。
- 末尾半行（崩溃截断）在重放时被容忍丢弃；其余损坏会在启动时报错。
- 兼容范围：WAL 为 JSON Lines，同版本内向后兼容；跨版本升级前先备份
  数据目录。纯内存模式（`catalog.New()`）行为不变，便于测试。

## 多来源重复与冲突的判定边界

写入采用乐观并发控制，实体与关系规则一致，结果分三类：

| 情形 | 结果 | 持久化原因 |
|---|---|---|
| 版本高于当前 | 接受（Applied） | — |
| 同版本同内容（**忽略来源**） | 幂等成功（Duplicate） | — |
| 同版本不同内容（并发冲突） | 拒绝 | `VERSION_CONFLICT` |
| 版本低于当前（乱序） | 拒绝，不覆盖新版本 | `STALE_VERSION` |
| 写入已合并实体 | 拒绝 | `ENTITY_MERGED` |
| 引用不存在的实体 | 边保留为悬挂，记录一次日志 | `DANGLING_REFERENCE` |
| 结构演进不兼容 | 拒绝，状态不变 | `SCHEMA_INCOMPATIBLE` |
| 合并双方字段类型冲突 / 重复合并 / 自合并 | 拒绝 | `MERGE_CONFLICT` |

- **跨来源重复**：同一实体/关系内容实质相同（仅 `Source` 不同）时统一按
  重复处理，不会把多来源重复报成冲突。
- **重复不计数**：悬挂边的重复提交（含跨来源）不会重复追加
  `DANGLING_REFERENCE` 日志；重复提交不产生任何新记录。
- 规则不变时同样输入给出同样结论与同样原因；重启与扩容（多实例各自
  独立数据目录）不改变判定。冲突方以更高版本号重试即可恢复一致状态。

**结构演进兼容性规则**（规则固定，相同输入结论可复现）：

- 新增可选字段：兼容；新增必填字段：不兼容。
- 删除字段：不兼容；通过 `RenameHint` 显式声明的改名：兼容。
- 类型加宽（`int→float`、`int/float/bool→string`）：兼容；其余类型变化：不兼容。

## 合并的一致性边界

`MergeEntities(dst, src)` 将指向 src 的关系重定向到 dst：

- **不丢边**：重定向后与既有边重名的，按确定性后缀 `base#2`、`base#3`…
  保留（处理顺序按原关系 ID 排序，与 map 迭代顺序无关，结果可复现）；
  重定向产生的自环同样保留。所有边均可通过 `/relations` 查询。
- 标签/术语/所有者取并集，src 标记 `MergedInto`；双方同名字段类型冲突、
  重复合并或自合并时整体拒绝且状态不变。
- 合并产生的关系变更与实体标记一并写入 WAL，重启后完整恢复。

## 影响分析的一致性边界

`Impact(root, filter)` 从起点沿血缘下游（src→dst 方向）做 BFS：

- **一致性快照**：实体与关系在同一把读锁下抓取，持续写入期间单次查询
  基于内部一致的图，不会一半旧数据一半新数据（例如绝不会出现"边已可见
  但目标实体缺失"的伪悬挂）。
- **循环**：visited 集合保证每个节点只展开一次；成环的边被截断并列入
  `CycleEdges`，遍历有界、确定、可复现。
- **悬挂引用**：目标不存在的边不静默丢弃，目标 ID 列入 `DanglingIDs`，
  但不作为节点展开。
- **筛选**：按 `Tags` / `Terms` / `Owners` 过滤（任一命中即保留）；
  被过滤的节点不出现在结果中，但其下游仍会被遍历。
- 输出按实体 ID 排序，保证确定性。

## 本地验证

```bash
go build ./...        # 编译
go test ./... -v      # 单元测试（日志含关键输入、版本号与判定依据）
go test -race ./...   # 竞态检测
go run ./cmd/catalog  # 内存模式端到端演示：摄入/演进/血缘/合并/拒绝日志
```

测试覆盖：重复与乱序摄入、并发更新冲突与重试、兼容/不兼容结构演进、
显式改名、循环血缘、悬挂引用、合并冲突、**重启后恢复（实体/关系/拒绝
记录及 Seq 延续）**、**跨来源重复提交**、**合并重名边与自环保留及其
确定性**、**写入期间查询快照一致性**。
