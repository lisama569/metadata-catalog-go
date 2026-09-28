package catalog

import (
	"path/filepath"
	"testing"
)

// reopen 关闭并重新打开同一数据目录，模拟进程重启。
func reopen(t *testing.T, s *Store, dir string) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	ns, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	return ns
}

func TestRestartRecoversEntitiesRelationsRejections(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// 写入：实体、关系（含悬挂）、一次乱序拒绝。
	if r := s.UpsertEntity(testEntity("ds-1", 2)); !r.Applied {
		t.Fatalf("entity should apply")
	}
	if r := s.UpsertEntity(testEntity("ds-2", 1)); !r.Applied {
		t.Fatalf("entity should apply")
	}
	rel := Relation{Type: RelationLineage, SrcID: "ds-1", DstID: "ds-2", Source: "a", Version: 1}
	if r := s.UpsertRelation(rel); !r.Applied {
		t.Fatalf("relation should apply")
	}
	dangling := Relation{Type: RelationLineage, SrcID: "ds-1", DstID: "ghost", Source: "a", Version: 1}
	s.UpsertRelation(dangling)
	stale := s.UpsertEntity(testEntity("ds-1", 1))
	if stale.Rejection == nil || stale.Rejection.Reason != ReasonStaleVersion {
		t.Fatalf("expected stale rejection, got %+v", stale)
	}
	before := s.Rejections()
	t.Logf("before restart: rejections=%d", len(before))

	s = reopen(t, s, dir)

	// 实体恢复。
	e, ok := s.GetEntity("ds-1")
	if !ok || e.Version != 2 {
		t.Fatalf("entity ds-1 not recovered correctly: %+v ok=%v", e, ok)
	}
	// 关系恢复（含悬挂边）。
	rels := s.Relations()
	if len(rels) != 2 {
		t.Fatalf("expected 2 relations after restart, got %d: %+v", len(rels), rels)
	}
	// 拒绝记录恢复，内容与重启前一致。
	after := s.Rejections()
	if len(after) != len(before) {
		t.Fatalf("rejections not recovered: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("rejection %d changed across restart: %+v -> %+v", i, before[i], after[i])
		}
	}
	t.Logf("after restart: rejections recovered=%d last seq=%d", len(after), after[len(after)-1].Seq)

	// Seq 单调延续：重启后新的拒绝接着编号，不重复计数。
	again := s.UpsertEntity(testEntity("ds-1", 1))
	if again.Rejection == nil || again.Rejection.Seq != after[len(after)-1].Seq+1 {
		t.Fatalf("seq should continue monotonically, got %+v", again.Rejection)
	}
}

func TestDuplicateStaysDuplicateAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	e := testEntity("ds-1", 3)
	s.UpsertEntity(e)
	s.UpsertEntity(testEntity("ds-2", 1))
	dup := e
	dup.Source = "other"
	if r := s.UpsertEntity(dup); !r.Duplicate {
		t.Fatalf("expected duplicate before restart, got %+v", r)
	}
	rel := Relation{Type: RelationLineage, SrcID: "ds-1", DstID: "ds-2", Source: "a", Version: 1}
	s.UpsertRelation(rel)

	s = reopen(t, s, dir)

	// 重启后再提交同一内容（换一个来源）：仍是重复，不是接受也不是冲突。
	dup2 := e
	dup2.Source = "third-source"
	r := s.UpsertEntity(dup2)
	t.Logf("resubmit entity after restart: duplicate=%v rejection=%v", r.Duplicate, r.Rejection)
	if !r.Duplicate || r.Rejection != nil {
		t.Fatalf("duplicate must stay duplicate across restart, got %+v", r)
	}
	relDup := rel
	relDup.Source = "other-source"
	r2 := s.UpsertRelation(relDup)
	t.Logf("resubmit relation after restart: duplicate=%v rejection=%v", r2.Duplicate, r2.Rejection)
	if !r2.Duplicate || r2.Rejection != nil {
		t.Fatalf("relation duplicate must stay duplicate across restart, got %+v", r2)
	}
	// 重复提交不产生新的拒绝记录。
	if n := len(s.Rejections()); n != 0 {
		t.Fatalf("duplicates must not add rejections, got %d", n)
	}
}

func TestRejectedRetryConvergesAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.UpsertEntity(testEntity("ds-1", 5))
	// 乱序写入被拒。
	if r := s.UpsertEntity(testEntity("ds-1", 2)); r.Rejection == nil {
		t.Fatalf("stale write should be rejected")
	}

	s = reopen(t, s, dir)

	// 重启后以更高版本重试：收敛到一致状态。
	retry := testEntity("ds-1", 6)
	retry.Tags = []string{"final"}
	if r := s.UpsertEntity(retry); !r.Applied {
		t.Fatalf("retry after restart should apply, got %+v", r)
	}
	got, _ := s.GetEntity("ds-1")
	if got.Version != 6 || len(got.Tags) != 1 || got.Tags[0] != "final" {
		t.Fatalf("state not converged: %+v", got)
	}
	// 再次重启，收敛结果仍然保持。
	s = reopen(t, s, dir)
	got, _ = s.GetEntity("ds-1")
	if got.Version != 6 {
		t.Fatalf("converged state lost across restart: %+v", got)
	}
	// 悬挂边重复提交不会重复记录日志（不重复计数）。
	dangling := Relation{Type: RelationLineage, SrcID: "ds-1", DstID: "ghost", Source: "a", Version: 1}
	s.UpsertRelation(dangling)
	n1 := len(s.Rejections())
	s.UpsertRelation(dangling)
	dangling.Source = "b"
	s.UpsertRelation(dangling)
	if n2 := len(s.Rejections()); n2 != n1 {
		t.Fatalf("duplicate dangling submissions must not double-count: %d -> %d", n1, n2)
	}
}

func TestMergeResultSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.UpsertEntity(testEntity("main", 1))
	s.UpsertEntity(testEntity("dup", 1))
	s.UpsertEntity(testEntity("x", 1))
	s.UpsertRelation(Relation{Type: RelationLineage, SrcID: "main", DstID: "x", Source: "a", Version: 1})
	s.UpsertRelation(Relation{Type: RelationLineage, SrcID: "dup", DstID: "x", Source: "a", Version: 1})
	if r := s.MergeEntity("main", "dup", "test", 2); !r.Applied {
		t.Fatalf("merge should apply")
	}
	before := s.Relations()

	s = reopen(t, s, dir)

	after := s.Relations()
	if len(after) != len(before) {
		t.Fatalf("merged edges lost across restart: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("relation %d changed across restart: %+v -> %+v", i, before[i], after[i])
		}
	}
	src, _ := s.GetEntity("dup")
	if src.MergedInto != "main" {
		t.Fatalf("merged marker lost across restart")
	}
	// 重启后写入已合并实体仍然被拒。
	if r := s.UpsertEntity(testEntity("dup", 9)); r.Rejection == nil || r.Rejection.Reason != ReasonEntityMerged {
		t.Fatalf("merged entity must stay write-protected across restart, got %+v", r)
	}
}
