package catalog

import "fmt"

// Catalog 门面：组合存储、结构演进校验与血缘分析。
type Catalog struct {
	store *Store
}

// New 创建空目录。
func New() *Catalog {
	return &Catalog{store: NewStore()}
}

// Open 打开（不存在则创建）持久化目录；
// 重启后实体、关系与拒绝记录完整恢复，判定行为与重启前一致。
func Open(path string) (*Catalog, error) {
	s, err := OpenStore(path)
	if err != nil {
		return nil, err
	}
	return &Catalog{store: s}, nil
}

// Store 暴露底层存储（只读用途）。
func (c *Catalog) Store() *Store { return c.store }

// IngestEntity 摄入实体；并发安全，乱序与重复提交均可处理。
func (c *Catalog) IngestEntity(e Entity) UpsertResult {
	return c.store.UpsertEntity(e)
}

// IngestRelation 摄入关系。
func (c *Catalog) IngestRelation(r Relation) UpsertResult {
	return c.store.UpsertRelation(r)
}

// EvolveSchema 校验并应用实体结构演进；不兼容时拒绝并保留上下文。
// 判定结论（Verdict）无论接受与否都会返回，保证历史结论可复现。
func (c *Catalog) EvolveSchema(entityID string, next Schema, renames []RenameHint, source string, version int64) (Verdict, UpsertResult) {
	cur, ok := c.store.GetEntity(entityID)
	if !ok {
		rej, err := c.store.RecordRejection(Rejection{
			Reason: ReasonEntityNotFound, EntityID: entityID, Source: source, Version: version,
			Detail: "cannot evolve schema of missing entity",
		})
		return Verdict{Compatible: false}, UpsertResult{Rejection: rej, Err: err}
	}
	verdict := CheckCompatibility(cur.Schema, next, renames)
	if !verdict.Compatible {
		rej, err := c.store.RecordRejection(Rejection{
			Reason: ReasonSchemaIncompatible, EntityID: entityID, Source: source, Version: version,
			Detail: fmt.Sprintf("schema evolution rejected: %d incompatible change(s)", countIncompatible(verdict)),
		})
		return verdict, UpsertResult{Rejection: rej, Err: err}
	}
	nextEntity := cur
	nextEntity.Schema = next
	nextEntity.Source = source
	nextEntity.Version = version
	return verdict, c.store.UpsertEntity(nextEntity)
}

// MergeEntities 将 srcID 合并进 dstID：重定向关系、标记合并、冲突时拒绝。
func (c *Catalog) MergeEntities(dstID, srcID, source string, version int64) UpsertResult {
	return c.store.MergeEntity(dstID, srcID, source, version)
}

// Impact 影响分析入口。
func (c *Catalog) Impact(root string, filter ImpactFilter) ImpactReport {
	return ImpactAnalysis(c.store, root, filter)
}

func countIncompatible(v Verdict) int {
	n := 0
	for _, c := range v.Changes {
		if !c.Compatible {
			n++
		}
	}
	return n
}
