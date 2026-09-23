package catalog

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Store 线程安全的内存元数据存储，带版本检查与拒绝日志。
type Store struct {
	mu         sync.RWMutex
	entities   map[string]*Entity
	relations  map[string]*Relation
	rejections []Rejection
	seq        int64
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
		return UpsertResult{Rejection: s.record(Rejection{
			Reason: ReasonEntityMerged, EntityID: e.ID, Source: e.Source, Version: e.Version,
			Detail: fmt.Sprintf("entity already merged into %s", cur.MergedInto),
		})}
	}
	if ok {
		switch {
		case e.Version < cur.Version:
			return UpsertResult{Rejection: s.record(Rejection{
				Reason: ReasonStaleVersion, EntityID: e.ID, Source: e.Source, Version: e.Version,
				Detail: fmt.Sprintf("incoming version %d < current version %d", e.Version, cur.Version),
			})}
		case e.Version == cur.Version:
			if entitiesEqual(*cur, e) {
				return UpsertResult{Duplicate: true}
			}
			return UpsertResult{Rejection: s.record(Rejection{
				Reason: ReasonVersionConflict, EntityID: e.ID, Source: e.Source, Version: e.Version,
				Detail: fmt.Sprintf("concurrent conflicting write at version %d", e.Version),
			})}
		}
	}
	cp := e
	s.entities[e.ID] = &cp
	return UpsertResult{Applied: true}
}

// entitiesEqual 判断两次写入内容是否一致（忽略来源，以便多来源重复提交幂等）。
func entitiesEqual(a, b Entity) bool {
	a.Source, b.Source = "", ""
	return reflect.DeepEqual(a, b)
}

// UpsertRelation 写入关系；悬挂引用会被保留并记录。
func (s *Store) UpsertRelation(r Relation) UpsertResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ID == "" {
		r.ID = RelationID(r.Type, r.SrcID, r.DstID)
	}
	// 悬挂引用：保留边，但记录可追溯的拒绝/告警日志。
	if _, ok := s.entities[r.DstID]; !ok {
		s.record(Rejection{
			Reason: ReasonDanglingReference, Relation: r.ID, Source: r.Source, Version: r.Version,
			Detail: fmt.Sprintf("relation %s targets missing entity %s; edge kept as dangling", r.ID, r.DstID),
		})
	}
	cur, ok := s.relations[r.ID]
	if ok {
		switch {
		case r.Version < cur.Version:
			return UpsertResult{Rejection: s.record(Rejection{
				Reason: ReasonStaleVersion, Relation: r.ID, Source: r.Source, Version: r.Version,
				Detail: fmt.Sprintf("incoming version %d < current version %d", r.Version, cur.Version),
			})}
		case r.Version == cur.Version:
			if *cur == r {
				return UpsertResult{Duplicate: true}
			}
			return UpsertResult{Rejection: s.record(Rejection{
				Reason: ReasonVersionConflict, Relation: r.ID, Source: r.Source, Version: r.Version,
				Detail: fmt.Sprintf("concurrent conflicting write at version %d", r.Version),
			})}
		}
	}
	cp := r
	s.relations[r.ID] = &cp
	return UpsertResult{Applied: true}
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

// RecordRejection 供门面层持久化一条拒绝记录（如结构演进不兼容）。
func (s *Store) RecordRejection(r Rejection) *Rejection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record(r)
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

	dst, ok := s.entities[dstID]
	if !ok {
		return UpsertResult{Rejection: s.record(Rejection{
			Reason: ReasonEntityNotFound, EntityID: dstID, Source: source, Version: version,
			Detail: "merge target not found",
		})}
	}
	src, ok := s.entities[srcID]
	if !ok {
		return UpsertResult{Rejection: s.record(Rejection{
			Reason: ReasonEntityNotFound, EntityID: srcID, Source: source, Version: version,
			Detail: "merge source not found",
		})}
	}
	if src.MergedInto != "" {
		return UpsertResult{Rejection: s.record(Rejection{
			Reason: ReasonMergeConflict, EntityID: srcID, Source: source, Version: version,
			Detail: fmt.Sprintf("source already merged into %s", src.MergedInto),
		})}
	}
	// 合并冲突：双方同名字段类型不可调和。
	for _, sf := range src.Schema.Fields {
		for _, df := range dst.Schema.Fields {
			if sf.Name == df.Name && sf.Type != df.Type {
				return UpsertResult{Rejection: s.record(Rejection{
					Reason: ReasonMergeConflict, EntityID: srcID, Source: source, Version: version,
					Detail: fmt.Sprintf("field %q type conflict: %s (%s) vs %s (%s)",
						sf.Name, srcID, sf.Type, dstID, df.Type),
				})}
			}
		}
	}

	// 重定向所有指向 src 的关系到 dst（保持确定性 ID，冲突时保留已有边）。
	for id, rel := range s.relations {
		if rel.SrcID == srcID || rel.DstID == srcID {
			nr := *rel
			if nr.SrcID == srcID {
				nr.SrcID = dstID
			}
			if nr.DstID == srcID {
				nr.DstID = dstID
			}
			nr.ID = RelationID(nr.Type, nr.SrcID, nr.DstID)
			delete(s.relations, id)
			if _, exists := s.relations[nr.ID]; !exists && nr.SrcID != nr.DstID {
				s.relations[nr.ID] = &nr
			}
		}
	}
	// 合并标签/术语/所有者（并集，去重）。
	dst.Tags = unionStrings(dst.Tags, src.Tags)
	dst.Terms = unionStrings(dst.Terms, src.Terms)
	dst.Owners = unionStrings(dst.Owners, src.Owners)
	if src.Version > dst.Version {
		dst.Version = src.Version
	}
	src.MergedInto = dstID
	return UpsertResult{Applied: true}
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
