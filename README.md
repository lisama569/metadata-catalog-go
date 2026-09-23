# Metadata Catalog Service (Go)

一个可本地运行的元数据目录服务，覆盖：

- **多来源摄入**：实体与关系可从多个来源重复、乱序提交；
- **版本化结构演进**：字段增删改名、类型变化按固定规则给出稳定的兼容性结论；
- **血缘维护与影响分析**：按标签 / 术语 / 所有者筛选，循环、悬挂引用与合并冲突不会导致
  无限遍历、静默丢边或非确定输出；
- **可追溯失败**：每次成功或失败的写入都生成一条机器可区分、带规则编号与上下文的持久化决策记录；
- **崩溃一致**：决策与变更都先写 JSONL 追加日志（fsync），重启重放即可恢复全部状态，失败后重试收敛一致。

> 纯标准库实现，无第三方依赖。

## 目录结构

```
cmd/server/           可执行入口
internal/model/       实体、关系、Schema、决策记录；确定性内容指纹
internal/store/       追加式 JSONL 日志与重放
internal/compat/      结构演进兼容性规则（固定规则 ID，结论可复现）
internal/graph/       并发安全血缘图、环检测、影响/血缘遍历、快照
internal/merge/       实体合并规划（纯函数，确定性结论）
internal/catalog/     摄入、版本 CAS、并发控制、决策持久化、合并应用
internal/server/      本地 HTTP API
```

## 快速开始

```bash
go build -o bin/mdserver ./cmd/server
./bin/mdserver -addr :8080 -journal catalog.jsonl
# -journal "" 表示纯内存（测试用）；--allow-field-removal 把字段删除降级为警告
```

### 示例：摄入 → 重复 → 冲突 → 演进 → 血缘 → 影响分析 → 合并

```bash
B=http://localhost:8080

# 1) 首次摄入
curl -s -X POST $B/v1/entities -d '{
  "id":"table_a","version":1,"typeName":"table","source":"s1","owner":"alice",
  "tags":["pii"],"terms":["customer"],
  "schema":{"version":1,"fields":[{"name":"id","type":"string","required":true},{"name":"name","type":"string"}]},
  "attrs":{"zone":"raw"}}'

# 2) 完全相同的重复提交 -> decision="duplicate"
curl -s -X POST $B/v1/entities -d '{ ...同上... }'

# 3) 同版本不同内容 -> 409 reject_content_conflict（含双方指纹）
# 4) 结构演进：不兼容（string -> int） -> 409 reject_schema_incompatible
curl -s -X POST $B/v1/entities/table_a/evolve -d '{
  "source":"s1",
  "schema":{"fields":[{"name":"id","type":"string","required":true},{"name":"name","type":"int"}]}}'

# 5) 兼容演进（新增可选字段）-> 200 accepted
curl -s -X POST $B/v1/entities/table_a/evolve -d '{
  "source":"s1",
  "schema":{"fields":[{"name":"id","type":"string","required":true},{"name":"name","type":"string"},{"name":"email","type":"string"}]}}'

# 6) 血缘关系
curl -s -X POST $B/v1/relationships -d '{"upstream":"table_a","downstream":"table_b","kind":"derived_from","version":1,"source":"s2"}'
# 反向成环 -> 409 reject_cycle
# 指向尚不存在的实体 -> 200 accepted，边被标记 dangling（GET /v1/dangling 可见）

# 7) 影响分析（可按 owner/tags/terms 筛选）
curl -s "$B/v1/entities/table_a/impact?owner=alice&tags=pii"
curl -s "$B/v1/entities/table_b/lineage"

# 8) 合并（冲突 -> 409 reject_merge_conflict，含 M001..M004 明细）
curl -s -X POST $B/v1/merges -d '{"survivorId":"table_a","victimId":"table_d","source":"s2"}'

# 9) 决策历史（所有接受/拒绝都在）
curl -s "$B/v1/history"
```

重启服务后，实体、关系、悬挂标记、合并结果、决策历史全部从 `catalog.jsonl` 恢复；
重复提交仍被判为 `duplicate`，失败版本重试在补齐中间版本后被接受。

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET  | `/healthz` | 健康检查 |
| POST | `/v1/entities` | 摄入/更新实体（版本 CAS） |
| GET  | `/v1/entities/{id}` | 查询实体 |
| POST | `/v1/entities/{id}/evolve` | 提交下一版 Schema，返回兼容性报告与决策 |
| POST | `/v1/relationships` | 摄入血缘边（成环拒绝，悬挂保留） |
| POST | `/v1/merges` | 实体合并（冲突拒绝并返回明细） |
| GET  | `/v1/entities/{id}/impact` | 下游影响分析，参数 `owner`、`tags=a,b`、`terms=x,y` |
| GET  | `/v1/entities/{id}/lineage` | 上游血缘分析，同上筛选 |
| GET  | `/v1/graph` | 一致性图快照（节点 + 边，含 dangling 标记） |
| GET  | `/v1/dangling` | 悬挂边列表 |
| GET  | `/v1/history?limit=n` | 决策历史（按 seq 升序） |

接受返回 HTTP 200；业务拒绝返回 **HTTP 409**，响应体 `decision` 为可区分原因码；
请求非法返回 4xx；合并/演进的冲突明细在 `detail` 中。

## 元数据模型

- **Entity**：`id / version / typeName / source / schema / attrs / tags / owner / terms / updatedAt`。
- **Schema**：`version + fields[]`；字段为 `name / type / required / renameOf`。
  支持类型：`string int float bool date ref`。`renameOf` 声明该字段由旧字段改名而来。
- **Relationship**：有向边 `upstream -> downstream`（数据从 upstream 流向 downstream），
  `kind ∈ {derived_from, depends_on, produces}`，键为 `upstream|kind|downstream`（也可显式给 `key`）。
- **DecisionRecord**：`seq / op / decision / accepted / reason / ruleId / subject / source /
  version / detail / submitted / time`，是排障与复现的唯一权威记录。

## 版本与合并规则

### 实体/关系版本判定（按顺序，原因码可区分）

1. 校验失败 → `reject_validation`（缺 id、version<1、未知类型等）；
2. 关系成环（下游已能到达上游，含自环）→ `reject_cycle`；
3. 首次见到 → `accepted`（接受来源给出的任意起始正版本）；
4. `submitted < current` → `reject_stale_version`（旧版本永不覆盖新版本）；
5. `submitted > current+1` → `reject_out_of_order_gap`（缺中间版本的乱序提交）；
6. `submitted == current` 且内容指纹相同 → `duplicate`（幂等，算成功）；
7. `submitted == current` 但指纹不同 → `reject_content_conflict`；
8. `submitted == current+1`：实体若 Schema 变化，先过兼容性闸门，不兼容 →
   `reject_schema_incompatible`，否则 `accepted`。

并发下所有判定在 catalog 互斥锁内完成，因此**恰好一个**顺序提交胜出，终态版本必然连续到达最大版本。
内容指纹是对业务字段（排除 version/updatedAt）的规范化 SHA-256；
这保证「同一版本不同内容」与「纯粹重复」可区分，重试同一条消息得到同一结论。

### 结构演进规则（`compat-rules-v1`，结论在规则不变时可复现）

| 规则 | 场景 | 结论 |
| --- | --- | --- |
| E001 | 版本不是 +1 | incompatible |
| E002 | 字段被删除且未声明 renameOf | incompatible（`--allow-field-removal` 下降级 warning） |
| E003 | 新增字段 | 可选 info；必填 warning（不阻断） |
| E004 | 改名（renameOf）：同源缺失/类型不符/多重声明 | 同名改名 info；类型不符 incompatible；多重声明 E007 |
| E005 | 类型变化 | incompatible；仅 `int -> float` 拓宽为 warning |
| E006 | 必填性变化 | optional→required incompatible；required→optional info |
| E007 | 新 schema 重名字段 / 多个字段声称同一 rename 源 | incompatible |

发现项按 `(field, ruleId)` 稳定排序；报告带 `ruleVersion`，只要规则版本不变，相同输入必然得到逐字节一致的判定序列。
被拒绝的演进保留上下文（提交内容、from/to 版本、规则明细），实体 Schema 维持旧版本。

### 实体合并规则（`merge-rules-v1`，纯函数规划）

- `M001` 类型名不同；`M002` 同名字段定义冲突（类型/必填/renameOf）；
  `M003` 同名属性值冲突；`M004` 双方声明了不同的非空 owner。
- 无冲突时：保留 survivor；schema 字段取并集；属性取并集；tags/terms 取并集（排序）；
  新版本 = `max(s,v)+1`。
- catalog 应用合并时，把 victim 的所有边端点重定向到 survivor（`merge:<victim>-><survivor>`），
  victim 删除；所有重定向明细写日志，可重放。**不会静默丢边。**

## 影响分析的边界与保证

- **环**：写入侧拒绝成环；遍历侧仍用「路径内 visited 集合」保证即便存在环也必然终止，
  只枚举简单路径（节点不重复）。
- **悬挂引用**：缺端点的边**照样保留**并标 `dangling`，在 `/v1/dangling` 可见；遍历时可穿过，
  但未摄入的端点不会进入「匹配命中」集合；端点之后被摄入，边自动转为非悬挂。
- **确定性**：邻接边按 key 排序展开，路径按节点序列去重并排序；同图同查询多次调用结果一致。
- **筛选**：`owner` 精确匹配；`tags/terms` 是「全部包含」（AND）语义；筛选作用于命中节点。
- **一致性**：查询持 catalog 读锁；遍历基于持锁期间的图状态，持续写入期间不会读到半更新结构
  （读多写少场景；写路径串行化）。`GET /v1/graph` 返回点时快照。
- **规模边界**：路径枚举为简单路径，最坏情况（DAG）可达指数级；服务面向本地中小规模目录。
  `matched` 节点列表可用于只关心影响面而不关心每条路径的场景。
- 合并导致的自环（某条边两端都被并到 survivor）会保留在图中并在遍历时被 visited 集合安全跳过，
  重定向记录可在决策历史与合并日志中追溯。

## 持久化与失败恢复

`internal/store` 是每行一个 JSON 对象的追加日志，每条记录有单调递增 `seq`：

1. 接受写入时**先持久化决策，再持久化变更**，两次追加各自 fsync；
   若在两者之间崩溃，重放后会看到一条 `accepted` 决策但无对应变更——重试同一条提交会再次被接受或幂等命中，
   状态最终一致，且拒绝原因永不丢失。
2. 启动时顺序重放：entity/rel/merge 重建内存态与图（含悬挂标记、边重定向），decision 重建历史。
3. 乱序/重复/并发的判定只依赖已重放状态，所以跨重启结论稳定。

## 本地验证方法

```bash
# 全部单元/集成测试（含重复/乱序、并发 CAS、兼容演进、环、悬挂、合并、重放）
go test ./... -v

# 竞态检测
go test -race ./...

# 测试日志会打印：关键输入、版本号、判定原因码、规则 ID、指纹、路径
go test ./internal/catalog/ -run TestDuplicateAndOutOfOrderIngestion -v
go test ./internal/compat/ -v
```

测试覆盖对照：

| 需求 | 测试 |
| --- | --- |
| 重复 / 同版本内容冲突 / 旧版本 / 乱序缺口 / 重试恢复 | `catalog: TestDuplicateAndOutOfOrderIngestion` |
| 并发更新旧版本不覆盖新版本 | `catalog: TestConcurrentUpdatesOlderNeverOverwrites` |
| 兼容/不兼容结构演进 + 结论复现 | `compat: TestCheckSchemas*`、`catalog: TestSchemaEvolutionRejectionPersisted` |
| 循环血缘拒绝 + 图遍历在环下终止 | `catalog: TestLineageCycleRejectedAndDanglingKept`、`graph: TestCycleDetection / TestTraversalTerminatesWithCycle` |
| 悬挂边保留与自动转正 | `graph: TestDanglingEdgesKept` |
| 合并冲突可区分 + 合并应用 + 边重定向 | `merge: TestPlanMerge*`、`catalog: TestMergeConflictAndApply` |
| 标签/所有者筛选影响分析 | `catalog: TestImpactFiltering`、`graph: TestImpactAndLineage` |
| 崩溃重启重放 + 跨重启幂等 | `catalog: TestJournalReplayRestoresState` |
| HTTP 状态码与原因码 | `server: TestHTTP*` |
