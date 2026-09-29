package catalog

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// 目标实体缺失时，同一条悬挂关系由来源 A 首次提交后，
// 来源 B（以及更多来源）提交内容相同、仅来源不同的同一条关系：
// 只能算幂等重复——不得新增任何拒绝/告警记录，Seq 也不得继续跳。
func TestCrossSourceDuplicateDanglingRelationNoNewRecord(t *testing.T) {
	s := NewStore()
	s.UpsertEntity(testEntity("a", 1))

	rel := Relation{Type: RelationLineage, SrcID: "a", DstID: "ghost", Source: "etl-a", Version: 1}
	first := s.UpsertRelation(rel)
	if !first.Applied {
		t.Fatalf("first dangling relation should apply: %+v", first)
	}
	rejs := s.Rejections()
	if len(rejs) != 1 || rejs[0].Reason != ReasonDanglingReference {
		t.Fatalf("first dangling submission must log exactly one DANGLING_REFERENCE, got %+v", rejs)
	}

	for _, src := range []string{"etl-b", "etl-c", "etl-a", "etl-d"} {
		dup := rel
		dup.Source = src
		r := s.UpsertRelation(dup)
		if !r.Duplicate || r.Applied || r.Rejection != nil {
			t.Fatalf("cross-source duplicate from %q must be pure duplicate, got %+v", src, r)
		}
	}

	rejs = s.Rejections()
	if len(rejs) != 1 {
		t.Fatalf("duplicates must not add rejection records: got %d, want 1 (%+v)", len(rejs), rejs)
	}
	if rejs[0].Seq != 1 {
		t.Fatalf("seq must not advance on duplicates: got %d, want 1", rejs[0].Seq)
	}
	if len(s.Relations()) != 1 {
		t.Fatalf("duplicates must not add edges: got %d, want 1", len(s.Relations()))
	}
}

// 悬挂关系跨来源重复之后，无论马上重启、穿插一次其他成功写入再重启，
// 还是再重启一次，恢复出的实体、关系、拒绝记录与序号都与重启前完全一致。
func TestDanglingDuplicateStateConsistentAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	c1, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c1.IngestEntity(testEntity("a", 1))
	rel := Relation{Type: RelationLineage, SrcID: "a", DstID: "ghost", Source: "etl-a", Version: 1}
	if r := c1.IngestRelation(rel); !r.Applied {
		t.Fatalf("first dangling ingest should apply: %+v", r)
	}
	dup := rel
	dup.Source = "etl-b"
	if r := c1.IngestRelation(dup); !r.Duplicate {
		t.Fatalf("cross-source duplicate should be duplicate: %+v", r)
	}

	snap := func(c *Catalog) ([]Entity, []Relation, []Rejection) {
		return c.Store().Entities(), c.Store().Relations(), c.Store().Rejections()
	}
	assertState := func(t *testing.T, c *Catalog, wantE []Entity, wantR []Relation, wantJ []Rejection, phase string) {
		gotE, gotR, gotJ := snap(c)
		if !reflect.DeepEqual(wantE, gotE) || !reflect.DeepEqual(wantR, gotR) || !reflect.DeepEqual(wantJ, gotJ) {
			t.Fatalf("state mismatch at %s:\n entities before=%+v after=%+v\n relations before=%+v after=%+v\n rejections before=%+v after=%+v",
				phase, wantE, gotE, wantR, gotR, wantJ, gotJ)
		}
	}

	wantE, wantR, wantJ := snap(c1)
	if len(wantJ) != 1 || wantJ[0].Seq != 1 {
		t.Fatalf("before restart: want exactly 1 dangling rejection seq=1, got %+v", wantJ)
	}

	// 马上重启：状态必须一致。
	c2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	assertState(t, c2, wantE, wantR, wantJ, "immediate restart")

	// 重启后继续重试同一重复，仍不得新增任何记录。
	if r := c2.IngestRelation(dup); !r.Duplicate {
		t.Fatalf("duplicate after immediate restart must stay duplicate: %+v", r)
	}
	assertState(t, c2, wantE, wantR, wantJ, "duplicate retry after restart")

	// 穿插其他成功写入（新实体 + 正常边），再补一次重复提交。
	c2.IngestEntity(testEntity("b", 1))
	other := Relation{Type: RelationLineage, SrcID: "a", DstID: "b", Source: "etl-c", Version: 1}
	if r := c2.IngestRelation(other); !r.Applied {
		t.Fatalf("interleaved successful write should apply: %+v", r)
	}
	if r := c2.IngestRelation(dup); !r.Duplicate {
		t.Fatalf("duplicate after interleaved write must stay duplicate: %+v", r)
	}
	wantE, wantR, wantJ = snap(c2)

	// 带穿插写入再重启，状态仍须一致。
	c3, err := Open(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	assertState(t, c3, wantE, wantR, wantJ, "restart after interleaved write")

	// 再重启一次，结果不变；序号没有因重复提交而多跳。
	c4, err := Open(path)
	if err != nil {
		t.Fatalf("reopen 3: %v", err)
	}
	assertState(t, c4, wantE, wantR, wantJ, "second restart")
	_, _, gotJ := snap(c4)
	if len(gotJ) != 1 || gotJ[0].Reason != ReasonDanglingReference || gotJ[0].Seq != 1 {
		t.Fatalf("after restarts: want exactly 1 DANGLING_REFERENCE seq=1, got %+v", gotJ)
	}
}

// 多个来源并发重复提交同一条悬挂关系：恰好一次接受、恰好一条悬挂告警，
// 其余全部为幂等重复；重启后并发重试，结论与计数不变。
func TestConcurrentCrossSourceDuplicateDanglingRelation(t *testing.T) {
	s := NewStore()
	s.UpsertEntity(testEntity("a", 1))
	rel := Relation{Type: RelationLineage, SrcID: "a", DstID: "ghost", Version: 1}

	const writers = 24
	var wg sync.WaitGroup
	results := make([]UpsertResult, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := rel
			r.Source = "etl-" + string(rune('a'+i%26))
			results[i] = s.UpsertRelation(r)
		}(i)
	}
	wg.Wait()

	applied, duplicates, rejected := 0, 0, 0
	for i, r := range results {
		switch {
		case r.Applied:
			applied++
		case r.Duplicate:
			duplicates++
		default:
			rejected++
			t.Errorf("writer %d unexpectedly rejected: %+v", i, r)
		}
	}
	if applied != 1 || duplicates != writers-1 || rejected != 0 {
		t.Fatalf("want 1 applied / %d duplicates / 0 rejected, got %d/%d/%d",
			writers-1, applied, duplicates, rejected)
	}
	if rejs := s.Rejections(); len(rejs) != 1 || rejs[0].Reason != ReasonDanglingReference || rejs[0].Seq != 1 {
		t.Fatalf("concurrent duplicates must yield exactly 1 DANGLING_REFERENCE seq=1, got %+v", rejs)
	}
	if len(s.Relations()) != 1 {
		t.Fatalf("concurrent duplicates must yield exactly 1 edge, got %d", len(s.Relations()))
	}

	// 重启后继续并发重试：仍然全部是重复，不再产生任何记录。
	path := filepath.Join(t.TempDir(), "state.json")
	c1, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c1.IngestEntity(testEntity("a", 1))
	if r := c1.IngestRelation(rel); !r.Applied {
		t.Fatalf("seed dangling relation should apply: %+v", r)
	}
	c2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for round := 0; round < 2; round++ {
		var wg2 sync.WaitGroup
		res2 := make([]UpsertResult, writers)
		for i := 0; i < writers; i++ {
			wg2.Add(1)
			go func(i int) {
				defer wg2.Done()
				r := rel
				r.Source = "etl-remote-" + string(rune('a'+i%26))
				res2[i] = c2.IngestRelation(r)
			}(i)
		}
		wg2.Wait()
		for i, r := range res2 {
			if !r.Duplicate {
				t.Fatalf("round %d writer %d: want duplicate, got applied=%v rejection=%v", round, i, r.Applied, r.Rejection)
			}
		}
	}
	if rejs := c2.Store().Rejections(); len(rejs) != 1 || rejs[0].Reason != ReasonDanglingReference || rejs[0].Seq != 1 {
		t.Fatalf("after restart retries: want exactly 1 DANGLING_REFERENCE seq=1, got %+v", rejs)
	}
}

// 真正的内容冲突、旧版本覆盖在悬挂边上仍被区分拒绝，且被拒绝的提交
// 不产生悬挂告警；首次悬挂记录不被吞掉。
func TestDanglingEdgeStillDistinguishesConflictAndStale(t *testing.T) {
	s := NewStore()
	s.UpsertEntity(testEntity("a", 1))
	rel := Relation{Type: RelationLineage, SrcID: "a", DstID: "ghost", Source: "etl-a", Version: 1}
	if r := s.UpsertRelation(rel); !r.Applied {
		t.Fatalf("first dangling ingest should apply: %+v", r)
	}

	// 同版本内容真正不同（关系类型不同）：VERSION_CONFLICT，且不再追加悬挂告警。
	conflict := Relation{
		ID:   RelationID(RelationLineage, "a", "ghost"),
		Type: RelationReference, SrcID: "a", DstID: "ghost", Source: "etl-b", Version: 1,
	}
	r := s.UpsertRelation(conflict)
	if r.Rejection == nil || r.Rejection.Reason != ReasonVersionConflict {
		t.Fatalf("want VERSION_CONFLICT, got %+v", r)
	}

	// 旧版本覆盖：STALE_VERSION。
	stale := rel
	stale.Source = "etl-c"
	stale.Version = 0
	r = s.UpsertRelation(stale)
	if r.Rejection == nil || r.Rejection.Reason != ReasonStaleVersion {
		t.Fatalf("want STALE_VERSION, got %+v", r)
	}

	rejs := s.Rejections()
	if len(rejs) != 3 {
		t.Fatalf("want 3 records (dangling, conflict, stale), got %d: %+v", len(rejs), rejs)
	}
	want := []RejectReason{ReasonDanglingReference, ReasonVersionConflict, ReasonStaleVersion}
	for i, w := range want {
		if rejs[i].Reason != w {
			t.Fatalf("rejection %d reason=%s want %s", i, rejs[i].Reason, w)
		}
		if rejs[i].Seq != int64(i+1) {
			t.Fatalf("rejection %d seq=%d want %d", i, rejs[i].Seq, i+1)
		}
	}
}
