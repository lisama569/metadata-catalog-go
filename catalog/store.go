package catalog

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Store 线程安全的元数据存储，带版本检查、拒绝日志与可选的 WAL 持久化。
type Store struct {
	mu         sync.RWMutex
	entities   map[string]*Entity
	relations  map[string]*Relation
	rejections []Rejection
	seq        int64
	wal        *wal // 非 nil 时所有状态变更先落盘
}

// NewStore 创建空存储（纯内存，不持久化）。
func NewStore() *Store {
	return &Store{
		entities:  make(map[string]*Entity),
		relations: make(map[string]*Relation),
	}
}

// OpenStore 打开（或创建）dir 下的持久化存储：重放 WAL 恢复状态，
// 后续所有状态变更先写日志再生效，进程重启后状态可完整恢复。
func OpenStore(dir string) (*Store, error) {
	w, events, err := openWAL(dir)
	if err != nil {
		return nil, err
	}
	s := NewStore()
	s.wal = w
	for _, ev := range events {
		switch ev.Type {
		case walEntity:
			cp := *ev.Entity
			s.entities[cp.ID] = &cp
		case walRelation:
			cp := *ev.Relation
			s.relations[cp.ID] = &cp
		case walRelationDelete:
			delete(s.relations, ev.RelationID)
		case walRejection:
			s.rejections = append(s.rejections, *ev.Rejection)
			if ev.Rejection.Seq > s.seq {
				s.seq = ev.Rejection.Seq
			}
		}
	}
	return s, nil
}

// Close 关闭底层日志文件（纯内存存储为 no-op）。
func (s *Store) Close() error {
	if s.wal == nil {
		return nil
	}
	return s.wal.close()
}

// logEvent 将状态变更写入 WAL；写盘失败意味着无法保证持久性，直接 panic
// （调用方持锁，defer 会解锁），宁可失败也不静默丢数据。
func (s *Store) logEvent(ev walEvent) {
	if s.wal == nil {
		return
	}
	if err := s.wal.append(ev); err != nil {
		panic(fmt.Sprintf("catalog: durable log write failed: %v", err))
	}
}

// UpsertResult 写入结果。
type UpsertResult struct {
	Applied   bool // 是否真正改变了状态
	Duplicate bool // 是否为幂等重复
	Rejection *Rejection
}

// record 追加一条拒绝日志（调用方需持有写锁）；持久化存储下先落盘。
func (s *Store) record(r Rejection) *Rejection {
	s.seq++
	r.Seq = s.seq
	s.logEvent(walEvent{Type: walRejection, Rejection: &r})
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
	s.logEvent(walEvent{Type: walEntity, Entity: &cp})
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
	cur, ok := s.relations[r.ID]
	if ok {
		switch {
		case r.Version < cur.Version:
			return UpsertResult{Rejection: s.record(Rejection{
				Reason: ReasonStaleVersion, Relation: r.ID, Source: r.Source, Version: r.Version,
				Detail: fmt.Sprintf("incoming version %d < current version %d", r.Version, cur.Version),
			})}
		case r.Version == cur.Version:
			if relationsEqual(*cur, r) {
				return UpsertResult{Duplicate: true}
			}
			return UpsertResult{Rejection: s.record(Rejection{
				Reason: ReasonVersionConflict, Relation: r.ID, Source: r.Source, Version: r.Version,
				Detail: fmt.Sprintf("concurrent conflicting write at version %d", r.Version),
			})}
		}
	}
	// 悬挂引用：边保留，但只在真正应用写入时记录一次日志，
	// 避免重复提交/重试造成重复计数。
	if _, ok := s.entities[r.DstID]; !ok {
		s.record(Rejection{
			Reason: ReasonDanglingReference, Relation: r.ID, Source: r.Source, Version: r.Version,
			Detail: fmt.Sprintf("relation %s targets missing entity %s; edge kept as dangling", r.ID, r.DstID),
		})
	}
	cp := r
	s.logEvent(walEvent{Type: walRelation, Relation: &cp})
	s.relations[r.ID] = &cp
	return UpsertResult{Applied: true}
}

// relationsEqual 判断两条关系内容是否实质相同（忽略来源，多来源重复提交幂等）。
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
	return s.relationsLocked()
}

// Entities 返回全部实体的确定性排序快照。
func (s *Store) Entities() []Entity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entitiesLocked()
}

// entitiesLocked 返回排序后的实体快照（调用方需持有读锁）。
func (s *Store) entitiesLocked() []Entity {
	out := make([]Entity, 0, len(s.entities))
	for _, e := range s.entities {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// relationsLocked 返回排序后的关系快照（调用方需持有读锁）。
func (s *Store) relationsLocked() []Relation {
	out := make([]Relation, 0, len(s.relations))
	for _, r := range s.relations {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Snapshot 在同一把读锁下抓取实体与关系，保证影响分析基于内部一致的图，
// 不会一半旧数据一半新数据。
type Snapshot struct {
	Entities  []Entity
	Relations []Relation
}

// Snapshot 返回实体与关系的一致性快照（单次加锁）。
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		Entities:  s.entitiesLocked(),
		Relations: s.relationsLocked(),
	}
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

	if dstID == srcID {
		return UpsertResult{Rejection: s.record(Rejection{
			Reason: ReasonMergeConflict, EntityID: srcID, Source: source, Version: version,
			Detail: "cannot merge entity into itself",
		})}
	}
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

	// 重定向所有指向 src 的关系到 dst。
	// 先收集再按 ID 排序处理，保证结果与 map 迭代顺序无关（确定性）。
	// 重定向后重名的边以确定性后缀保留（不静默丢边），自环同样保留。
	affected := make([]string, 0)
	for id, rel := range s.relations {
		if rel.SrcID == srcID || rel.DstID == srcID {
			affected = append(affected, id)
		}
	}
	sort.Strings(affected)
	for _, id := range affected {
		rel := s.relations[id]
		if rel == nil {
			continue // 可能已被本次合并中的重定向覆盖
		}
		nr := *rel
		if nr.SrcID == srcID {
			nr.SrcID = dstID
		}
		if nr.DstID == srcID {
			nr.DstID = dstID
		}
		nr.ID = s.freeRelationID(RelationID(nr.Type, nr.SrcID, nr.DstID), id)
		if nr.ID != id {
			s.logEvent(walEvent{Type: walRelationDelete, RelationID: id})
			delete(s.relations, id)
		}
		s.logEvent(walEvent{Type: walRelation, Relation: &nr})
		s.relations[nr.ID] = &nr
	}
	// 合并标签/术语/所有者（并集，去重）。
	dst.Tags = unionStrings(dst.Tags, src.Tags)
	dst.Terms = unionStrings(dst.Terms, src.Terms)
	dst.Owners = unionStrings(dst.Owners, src.Owners)
	if src.Version > dst.Version {
		dst.Version = src.Version
	}
	src.MergedInto = dstID
	s.logEvent(walEvent{Type: walEntity, Entity: dst})
	s.logEvent(walEvent{Type: walEntity, Entity: src})
	return UpsertResult{Applied: true}
}

// freeRelationID 为重定向后的关系分配确定性 ID：base 被占用（且不是正在
// 迁移的旧 ID）时依次尝试 base#2、base#3……，保证重名边不丢失且结果可复现。
// 调用方需持有写锁。
func (s *Store) freeRelationID(base, movingFrom string) string {
	if base == movingFrom {
		return base
	}
	if _, taken := s.relations[base]; !taken {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s#%d", base, i)
		if cand == movingFrom {
			return cand
		}
		if _, taken := s.relations[cand]; !taken {
			return cand
		}
	}
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
