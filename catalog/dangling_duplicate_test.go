package catalog

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// 抓取目录当前完整状态（实体、关系、拒绝记录），用于重启前后对照。
type fullState struct {
	entities   []Entity
	relations  []Relation
	rejections []Rejection
}

func captureState(c *Catalog) fullState {
	return fullState{
		entities:   c.Store().Entities(),
		relations:  c.Store().Relations(),
		rejections: c.Store().Rejections(),
	}
}

func assertSameState(t *testing.T, label string, want, got fullState) {
	t.Helper()
	if !reflect.DeepEqual(want.entities, got.entities) {
		t.Fatalf("%s: entities differ:\n want=%+v\n got=%+v", label, want.entities, got.entities)
	}
	if !reflect.DeepEqual(want.relations, got.relations) {
		t.Fatalf("%s: relations differ:\n want=%+v\n got=%+v", label, want.relations, got.relations)
	}
	if !reflect.DeepEqual(want.rejections, got.rejections) {
		t.Fatalf("%s: rejections differ:\n want=%+v\n got=%+v", label, want.rejections, got.rejections)
	}
}

// 当前可追溯序号（最后一条拒绝记录的 Seq；无记录时为 0）。
func lastSeq(st fullState) int64 {
	if len(st.rejections) == 0 {
		return 0
	}
	return st.rejections[len(st.rejections)-1].Seq
}

// 目标实体缺失时：来源 A 首次提交记录一条 DANGLING_REFERENCE；
// 来源 B 提交内容相同、仅来源不同的同一关系，只算重复——
// 不新增任何拒绝/告警记录，可追溯序号原地不动。
func TestDanglingRelationCrossSourceDuplicate(t *testing.T) {
	s := NewStore()
	rel := Relation{Type: RelationLineage, SrcID: "a", DstID: "ghost", Source: "etl-a", Version: 1}

	r1 := s.UpsertRelation(rel)
	if !r1.Applied {
		t.Fatalf("first dangling ingest should apply (edge kept), got %+v", r1)
	}
	rejs := s.Rejections()
	if len(rejs) != 1 || rejs[0].Reason != ReasonDanglingReference || rejs[0].Seq != 1 {
		t.Fatalf("first ingest must record exactly one DANGLING_REFERENCE with seq=1, got %+v", rejs)
	}
	t.Logf("first ingest: applied=%v rejections=%d lastSeq=%d", r1.Applied, len(rejs), rejs[len(rejs)-1].Seq)

	// 跨来源重复提交：仅来源不同。
	dup := rel
	dup.Source = "etl-b"
	r2 := s.UpsertRelation(dup)
	t.Logf("cross-source duplicate: duplicate=%v rejection=%v", r2.Duplicate, reasonOf(r2))
	if !r2.Duplicate || r2.Applied || r2.Rejection != nil {
		t.Fatalf("cross-source duplicate of dangling relation must be pure duplicate, got %+v", r2)
	}
	rejs = s.Rejections()
	if len(rejs) != 1 || rejs[0].Seq != 1 {
		t.Fatalf("duplicate must not add records or advance seq, got %+v", rejs)
	}

	// 同来源再重复一次：同样不得新增记录。
	if r := s.UpsertRelation(rel); !r.Duplicate {
		t.Fatalf("same-source retry must stay duplicate, got %+v", r)
	}
	if rejs := s.Rejections(); len(rejs) != 1 || rejs[0].Seq != 1 {
		t.Fatalf("retry must not add records or advance seq, got %+v", rejs)
	}

	// 真正的乱序写入：只记录一条 STALE_VERSION，不得夹带第二条悬挂告警。
	stale := rel
	stale.Source = "etl-c"
	stale.Version = 0
	r3 := s.UpsertRelation(stale)
	if r3.Rejection == nil || r3.Rejection.Reason != ReasonStaleVersion {
		t.Fatalf("stale write must be rejected as STALE_VERSION, got %+v", r3)
	}
	rejs = s.Rejections()
	if len(rejs) != 2 || rejs[1].Reason != ReasonStaleVersion || rejs[1].Seq != 2 {
		t.Fatalf("stale write must add exactly one STALE_VERSION record, got %+v", rejs)
	}

	// 真正的内容冲突：只记录一条 VERSION_CONFLICT。
	conflict := Relation{
		ID: RelationID(RelationLineage, "a", "ghost"),
		Type: RelationReference, SrcID: "a", DstID: "ghost", Source: "etl-d", Version: 1,
	}
	r4 := s.UpsertRelation(conflict)
	if r4.Rejection == nil || r4.Rejection.Reason != ReasonVersionConflict {
		t.Fatalf("conflicting write must be rejected as VERSION_CONFLICT, got %+v", r4)
	}
	rejs = s.Rejections()
	if len(rejs) != 3 || rejs[2].Reason != ReasonVersionConflict || rejs[2].Seq != 3 {
		t.Fatalf("conflict must add exactly one VERSION_CONFLICT record, got %+v", rejs)
	}
	t.Logf("final: rejections=%d lastSeq=%d (dangling/stale/conflict 各一条，可区分)",
		len(rejs), rejs[len(rejs)-1].Seq)
}

// 跨来源重复提交悬挂关系后，无论马上重启、穿插一次其他成功写入再重启，
// 还是连续重启两次，恢复出的状态都必须与重启前完全一致。
func TestDanglingDuplicateRestartConsistency(t *testing.T) {
	newDangling := func(c *Catalog) {
		t.Helper()
		rel := Relation{Type: RelationLineage, SrcID: "a", DstID: "ghost", Source: "etl-a", Version: 1}
		if r := c.IngestRelation(rel); !r.Applied {
			t.Fatalf("first dangling ingest should apply, got %+v", r)
		}
		rel.Source = "etl-b"
		if r := c.IngestRelation(rel); !r.Duplicate {
			t.Fatalf("cross-source duplicate expected, got %+v", r)
		}
	}

	// 场景 1：重复提交后马上重启，再重启一次。
	t.Run("restart-immediately-and-twice", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		c1, err := Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		newDangling(c1)
		want := captureState(c1)
		t.Logf("before restart: relations=%d rejections=%d lastSeq=%d",
			len(want.relations), len(want.rejections), lastSeq(want))

		c2, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		assertSameState(t, "after first restart", want, captureState(c2))

		c3, err := Open(path)
		if err != nil {
			t.Fatalf("reopen 2: %v", err)
		}
		assertSameState(t, "after second restart", want, captureState(c3))
	})

	// 场景 2：重复提交后先执行一次其他成功写入，再重启。
	t.Run("interleaved-successful-write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		c1, err := Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		newDangling(c1)
		// 穿插一次与悬挂关系无关的成功写入。
		if r := c1.IngestEntity(testEntity("a", 1)); !r.Applied {
			t.Fatalf("interleaved entity ingest should apply, got %+v", r)
		}
		want := captureState(c1)
		t.Logf("before restart (interleaved): entities=%d relations=%d rejections=%d lastSeq=%d",
			len(want.entities), len(want.relations), len(want.rejections), lastSeq(want))

		c2, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		assertSameState(t, "after restart with interleaved write", want, captureState(c2))
	})

	// 场景 3：重启后继续重试同一关系，仍是纯重复，状态与序号原地不动；
	// 再重启一次，恢复结果依然一致。
	t.Run("retry-after-restart", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		c1, err := Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		newDangling(c1)
		want := captureState(c1)

		c2, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		retry := Relation{Type: RelationLineage, SrcID: "a", DstID: "ghost", Source: "etl-c", Version: 1}
		r := c2.IngestRelation(retry)
		t.Logf("retry after restart from etl-c: duplicate=%v rejection=%v", r.Duplicate, reasonOf(r))
		if !r.Duplicate || r.Rejection != nil {
			t.Fatalf("retry after restart must stay pure duplicate, got %+v", r)
		}
		assertSameState(t, "retry must not change state", want, captureState(c2))

		c3, err := Open(path)
		if err != nil {
			t.Fatalf("reopen 2: %v", err)
		}
		assertSameState(t, "after restart following retry", want, captureState(c3))
	})
}

// 多个来源并发重复提交同一悬挂关系：恰好一次落地、其余全部判重，
// 拒绝记录恰好一条、序号为 1；重启后状态一致，重试仍全部判重。
func TestConcurrentDanglingDuplicateSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	c1, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const sources = 16
	var wg sync.WaitGroup
	results := make([]UpsertResult, sources)
	for i := 0; i < sources; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c1.IngestRelation(Relation{
				Type: RelationLineage, SrcID: "a", DstID: "ghost",
				Source: fmt.Sprintf("etl-%02d", i), Version: 1,
			})
		}(i)
	}
	wg.Wait()

	applied, duplicates := 0, 0
	for i, r := range results {
		if r.Applied {
			applied++
		}
		if r.Duplicate {
			duplicates++
		}
		if r.Rejection != nil {
			t.Fatalf("source %d: duplicate submission must not be rejected, got %+v", i, r.Rejection)
		}
	}
	t.Logf("concurrent submissions: applied=%d duplicates=%d", applied, duplicates)
	if applied != 1 || duplicates != sources-1 {
		t.Fatalf("expected exactly 1 applied and %d duplicates, got %d/%d", sources-1, applied, duplicates)
	}
	want := captureState(c1)
	if len(want.relations) != 1 {
		t.Fatalf("exactly one edge expected, got %+v", want.relations)
	}
	if len(want.rejections) != 1 || want.rejections[0].Reason != ReasonDanglingReference || want.rejections[0].Seq != 1 {
		t.Fatalf("exactly one DANGLING_REFERENCE with seq=1 expected, got %+v", want.rejections)
	}

	// 重启：恢复状态与运行中查询到的完全一致。
	c2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	assertSameState(t, "after restart", want, captureState(c2))

	// 重启后全部来源再重试一遍：仍然全部判重，状态与序号原地不动。
	for i := 0; i < sources; i++ {
		r := c2.IngestRelation(Relation{
			Type: RelationLineage, SrcID: "a", DstID: "ghost",
			Source: fmt.Sprintf("etl-%02d", i), Version: 1,
		})
		if !r.Duplicate || r.Rejection != nil {
			t.Fatalf("retry %d after restart must stay duplicate, got %+v", i, r)
		}
	}
	assertSameState(t, "retries after restart must not change state", want, captureState(c2))

	// 再重启一次，状态依然一致。
	c3, err := Open(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	assertSameState(t, "after second restart", want, captureState(c3))
}
