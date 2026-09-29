package catalog

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Store 线程安全的元数据存储，带版本检查与拒绝日志。
// path 非空时每次状态变更后整体快照落盘，重启可恢复。
type Store struct {
	mu         sync.RWMutex
	entities   map[string]*Entity
	relations  map[string]*Relation
	rejections []Rejection
	seq        int64
	path       string // 持久化文件路径；空串表示纯内存
}

// NewStore 创建空存储。
func NewStore() *Store {
	return &Store{
		entities:  make(map[string]*Entity),
		relations: make(map[string]*Relation),
	}
}

// UpsertResult 写入结果。
type UpsertResult struct {
	Applied   bool // 是否真正改变了状态
	Duplicate bool // 是否为幂等重复
	Rejection *Rejection
	// Err 持久化失败等非判定类错误；判定结论（拒绝/重复）不受其影响。
	Err error
}

// record 追加一条拒绝日志（调用方需持有写锁）。
func (s *Store) record(r Rejection) *Rejection {
	s.seq++
	r.Seq = s.seq
	s.rejections = append(s.rejections, r)
	cp := r
	return &cp
}

// UpsertEntity 以乐观并发方式写入实体。
// 较旧版本不得覆盖较新版本；同版本内容不同视为冲突；完全重复为幂等成功。
func (s *Store) UpsertEntity(e Entity) UpsertResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.entities[e.ID]
	if ok && cur.MergedInto != "" && cur.MergedInto != e.ID {
		rej := s.record(Rejection{
			Reason: ReasonEntityMerged, EntityID: e.ID, Source: e.Source, Version: e.Version,
			Detail: fmt.Sprintf("entity already merged into %s", cur.MergedInto),
		})
		return UpsertResult{Rejection: rej, Err: s.persistLocked()}
	}
	if ok {
		switch {
		case e.Version < cur.Version:
			rej := s.record(Rejection{
				Reason: ReasonStaleVersion, EntityID: e.ID, Source: e.Source, Version: e.Version,
				Detail: fmt.Sprintf("incoming version %d < current version %d", e.Version, cur.Version),
			})
			return UpsertResult{Rejection: rej, Err: s.persistLocked()}
		case e.Version == cur.Version:
			if entitiesEqual(*cur, e) {
				return UpsertResult{Duplicate: true}
			}
			rej := s.record(Rejection{
				Reason: ReasonVersionConflict, EntityID: e.ID, Source: e.Source, Version: e.Version,
				Detail: fmt.Sprintf("concurrent conflicting write at version %d", e.Version),
			})
			return UpsertResult{Rejection: rej, Err: s.persistLocked()}
		}
	}
	cp := e
	s.entities[e.ID] = &cp
	return UpsertResult{Applied: true, Err: s.persistLocked()}
}

// entitiesEqual 判断两次写入内容是否一致（忽略来源，以便多来源重复提交幂等）。
func entitiesEqual(a, b Entity) bool {
	a.Source, b.Source = "", ""
	return reflect.DeepEqual(a, b)
}

// UpsertRelation 写入关系；悬挂引用会被保留并记录。
// 判定顺序固定为先幂等/版本检查、后悬挂告警：跨来源重复提交同一条悬挂
// 关系只返回 Duplicate，不新增 DANGLING_REFERENCE 记录、不推进 Seq；
// 只有真正写入（新增或更新到新版本）且目标缺失时才记录一条悬挂告警。
func (s *Store) UpsertRelation(r Relation) UpsertResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ID == "" {
		r.ID = RelationID(r.Type, r.SrcID, r.DstID)
	}
	// 先做幂等/版本判定，再处理悬挂引用：
	// 跨来源重复提交同一条悬挂关系必须只算重复——既不能再记一条
	// DANGLING_REFERENCE（否则可追溯序号会凭空往前跳），重复结论也不触发
	// 落盘（否则会出现运行中多一条、重启后少一条的不一致）。
	cur, ok := s.relations[r.ID]
	if ok {
		switch {
		case r.Version < cur.Version:
			rej := s.record(Rejection{
				Reason: ReasonStaleVersion, Relation: r.ID, Source: r.Source, Version: r.Version,
				Detail: fmt.Sprintf("incoming version %d < current version %d", r.Version, cur.Version),
			})
			return UpsertResult{Rejection: rej, Err: s.persistLocked()}
		case r.Version == cur.Version:
			if relationsEqual(*cur, r) {
				// 纯幂等重复：状态、拒绝日志、序号均不变。
				return UpsertResult{Duplicate: true}
			}
			rej := s.record(Rejection{
				Reason: ReasonVersionConflict, Relation: r.ID, Source: r.Source, Version: r.Version,
				Detail: fmt.Sprintf("concurrent conflicting write at version %d", r.Version),
			})
			return UpsertResult{Rejection: rej, Err: s.persistLocked()}
		}
	}
	cp := r
	s.relations[r.ID] = &cp
	// 真正写入（新增边或更新到新版本）时目标实体仍缺失，才记录悬挂告警：
	// 每个来源首次提交得到一条可追溯记录，重复与被拒绝的提交不追加，
	// 且告警与边在同一次落盘内一起持久化，重启前后完全一致。
	if _, exists := s.entities[r.DstID]; !exists {
		s.record(Rejection{
			Reason: ReasonDanglingReference, Relation: r.ID, Source: r.Source, Version: r.Version,
			Detail: fmt.Sprintf("relation %s targets missing entity %s; edge kept as dangling", r.ID, r.DstID),
		})
	}
	return UpsertResult{Applied: true, Err: s.persistLocked()}
}

// relationsEqual 判断两条关系内容是否实质相同（忽略来源，
// 多来源重复提交统一按幂等重复处理，而不是误报冲突）。
func relationsEqual(a, b Relation) bool {
	a.Source, b.Source = "", ""
	return a == b
}

// GetEntity 读取实体快照（拷贝）。
func (s *Store) GetEntity(id string) (Entity, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entities[id]
	if !ok {
		return Entity{}, false
	}
	return *e, true
}

// Relations 返回全部关系的确定性排序快照。
func (s *Store) Relations() []Relation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Relation, 0, len(s.relations))
	for _, r := range s.relations {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Entities 返回全部实体的确定性排序快照。
func (s *Store) Entities() []Entity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entity, 0, len(s.entities))
	for _, e := range s.entities {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Snapshot 在单次读锁内同时取走实体与关系，
// 供影响分析等需要内部一致视图的查询使用：
// 持续写入期间，单次查询看到的一定是某一时刻的完整状态，
// 不会一半旧数据一半新数据。两个切片均按 ID 确定性排序。
func (s *Store) Snapshot() (entities []Entity, relations []Relation) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entities = make([]Entity, 0, len(s.entities))
	for _, e := range s.entities {
		entities = append(entities, *e)
	}
	relations = make([]Relation, 0, len(s.relations))
	for _, r := range s.relations {
		relations = append(relations, *r)
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i].ID < entities[j].ID })
	sort.Slice(relations, func(i, j int) bool { return relations[i].ID < relations[j].ID })
	return entities, relations
}

// RecordRejection 供门面层持久化一条拒绝记录（如结构演进不兼容）。
// 返回记录本身与持久化错误（如有）。
func (s *Store) RecordRejection(r Rejection) (*Rejection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rej := s.record(r)
	return rej, s.persistLocked()
}

// Rejections 返回拒绝日志快照。
func (s *Store) Rejections() []Rejection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Rejection, len(s.rejections))
	copy(out, s.rejections)
	return out
}

// MergeEntity 将 srcID 合并进 dstID：重定向关系、标记 src 已合并。
// 冲突（如 src 更新版本更高且结构不兼容）时拒绝并持久化原因。
func (s *Store) MergeEntity(dstID, srcID, source string, version int64) UpsertResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	if dstID == srcID {
		rej := s.record(Rejection{
			Reason: ReasonMergeConflict, EntityID: srcID, Source: source, Version: version,
			Detail: "cannot merge entity into itself",
		})
		return UpsertResult{Rejection: rej, Err: s.persistLocked()}
	}
	dst, ok := s.entities[dstID]
	if !ok {
		rej := s.record(Rejection{
			Reason: ReasonEntityNotFound, EntityID: dstID, Source: source, Version: version,
			Detail: "merge target not found",
		})
		return UpsertResult{Rejection: rej, Err: s.persistLocked()}
	}
	src, ok := s.entities[srcID]
	if !ok {
		rej := s.record(Rejection{
			Reason: ReasonEntityNotFound, EntityID: srcID, Source: source, Version: version,
			Detail: "merge source not found",
		})
		return UpsertResult{Rejection: rej, Err: s.persistLocked()}
	}
	if src.MergedInto != "" {
		rej := s.record(Rejection{
			Reason: ReasonMergeConflict, EntityID: srcID, Source: source, Version: version,
			Detail: fmt.Sprintf("source already merged into %s", src.MergedInto),
		})
		return UpsertResult{Rejection: rej, Err: s.persistLocked()}
	}
	// 合并冲突：双方同名字段类型不可调和。
	for _, sf := range src.Schema.Fields {
		for _, df := range dst.Schema.Fields {
			if sf.Name == df.Name && sf.Type != df.Type {
				rej := s.record(Rejection{
					Reason: ReasonMergeConflict, EntityID: srcID, Source: source, Version: version,
					Detail: fmt.Sprintf("field %q type conflict: %s (%s) vs %s (%s)",
						sf.Name, srcID, sf.Type, dstID, df.Type),
				})
				return UpsertResult{Rejection: rej, Err: s.persistLocked()}
			}
		}
	}

	// 重定向所有指向 src 的关系到 dst。
	// 确定性两阶段处理，保证一条边都不丢：
	//  1. 自环（重定向后 src==dst）保留，不静默丢弃；
	//  2. 重定向后的规范 ID 若与已有边冲突，保留原 ID 作为确定性的
	//     去重键，重复边因此可查（Relations 快照中两条都在）。
	affected := make([]string, 0)
	for id, rel := range s.relations {
		if rel.SrcID == srcID || rel.DstID == srcID {
			affected = append(affected, id)
		}
	}
	sort.Strings(affected) // 固定处理顺序，结果与 map 遍历顺序无关
	redirected := make([]Relation, 0, len(affected))
	for _, id := range affected {
		nr := *s.relations[id]
		if nr.SrcID == srcID {
			nr.SrcID = dstID
		}
		if nr.DstID == srcID {
			nr.DstID = dstID
		}
		redirected = append(redirected, nr)
		delete(s.relations, id)
	}
	for _, nr := range redirected {
		canonical := RelationID(nr.Type, nr.SrcID, nr.DstID)
		if _, taken := s.relations[canonical]; !taken {
			nr.ID = canonical
		} // 冲突时保留原 ID：边不丢，且 ID 唯一、可复现
		cp := nr
		s.relations[nr.ID] = &cp
	}
	// 合并标签/术语/所有者（并集，去重）。
	dst.Tags = unionStrings(dst.Tags, src.Tags)
	dst.Terms = unionStrings(dst.Terms, src.Terms)
	dst.Owners = unionStrings(dst.Owners, src.Owners)
	if src.Version > dst.Version {
		dst.Version = src.Version
	}
	src.MergedInto = dstID
	return UpsertResult{Applied: true, Err: s.persistLocked()}
}

func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	out := append([]string{}, a...)
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	sort.Strings(out)
	return out
}
