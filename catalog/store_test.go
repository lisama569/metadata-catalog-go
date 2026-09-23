package catalog

import (
	"fmt"
	"sync"
	"testing"
)

func testEntity(id string, version int64) Entity {
	return Entity{
		ID: id, Kind: "dataset", Source: "test", Version: version,
		Schema: Schema{Fields: []Field{{Name: "id", Type: FieldTypeInt, Required: true}}},
		Tags:   []string{"pii"},
	}
}

func TestDuplicateIngestIsIdempotent(t *testing.T) {
	s := NewStore()
	e := testEntity("ds-1", 3)
	r1 := s.UpsertEntity(e)
	t.Logf("first ingest: id=%s version=%d applied=%v", e.ID, e.Version, r1.Applied)
	if !r1.Applied {
		t.Fatalf("first ingest should apply")
	}
	// 多来源重复提交同一版本同一内容：幂等。
	e2 := e
	e2.Source = "other-source"
	r2 := s.UpsertEntity(e2)
	t.Logf("duplicate ingest from %q: version=%d duplicate=%v rejection=%v", e2.Source, e2.Version, r2.Duplicate, r2.Rejection)
	if !r2.Duplicate || r2.Rejection != nil {
		t.Fatalf("duplicate should be idempotent success, got %+v", r2)
	}
	got, _ := s.GetEntity("ds-1")
	if got.Version != 3 {
		t.Fatalf("version changed unexpectedly: %d", got.Version)
	}
}

func TestOutOfOrderIngestRejected(t *testing.T) {
	s := NewStore()
	if r := s.UpsertEntity(testEntity("ds-1", 5)); !r.Applied {
		t.Fatalf("v5 should apply")
	}
	stale := testEntity("ds-1", 2)
	r := s.UpsertEntity(stale)
	t.Logf("out-of-order ingest: id=%s incoming version=%d current=5 reason=%s detail=%s",
		stale.ID, stale.Version, r.Rejection.Reason, r.Rejection.Detail)
	if r.Rejection == nil || r.Rejection.Reason != ReasonStaleVersion {
		t.Fatalf("expected STALE_VERSION rejection, got %+v", r.Rejection)
	}
	got, _ := s.GetEntity("ds-1")
	if got.Version != 5 {
		t.Fatalf("stale version overwrote newer: got version %d", got.Version)
	}
	// 拒绝原因已持久化，可区分。
	rejs := s.Rejections()
	if len(rejs) != 1 || rejs[0].Reason != ReasonStaleVersion || rejs[0].Seq == 0 {
		t.Fatalf("rejection not persisted correctly: %+v", rejs)
	}
}

func TestConcurrentUpdatesConflictAndRetry(t *testing.T) {
	s := NewStore()
	s.UpsertEntity(testEntity("ds-1", 1))

	const writers = 16
	var wg sync.WaitGroup
	results := make([]UpsertResult, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := testEntity("ds-1", 2)
			e.Tags = []string{fmt.Sprintf("tag-%d", i)} // 同版本不同内容
			results[i] = s.UpsertEntity(e)
		}(i)
	}
	wg.Wait()

	applied, conflicts := 0, 0
	for i, r := range results {
		t.Logf("writer %d: version=2 applied=%v rejection=%v", i, r.Applied, reasonOf(r))
		if r.Applied {
			applied++
		}
		if r.Rejection != nil && r.Rejection.Reason == ReasonVersionConflict {
			conflicts++
		}
	}
	t.Logf("summary: applied=%d conflicts=%d (basis: single winner per version)", applied, conflicts)
	if applied != 1 || conflicts != writers-1 {
		t.Fatalf("expected exactly 1 applied and %d conflicts, got %d/%d", writers-1, applied, conflicts)
	}

	// 失败者以更高版本重试，恢复一致状态。
	retry := testEntity("ds-1", 3)
	retry.Tags = []string{"final"}
	r := s.UpsertEntity(retry)
	t.Logf("retry with version=3: applied=%v", r.Applied)
	if !r.Applied {
		t.Fatalf("retry should succeed")
	}
	got, _ := s.GetEntity("ds-1")
	if got.Version != 3 || len(got.Tags) != 1 || got.Tags[0] != "final" {
		t.Fatalf("state inconsistent after retry: %+v", got)
	}
}

func TestDanglingRelationKeptAndLogged(t *testing.T) {
	s := NewStore()
	s.UpsertEntity(testEntity("a", 1))
	rel := Relation{Type: RelationLineage, SrcID: "a", DstID: "ghost", Source: "test", Version: 1}
	r := s.UpsertRelation(rel)
	t.Logf("dangling relation %s: applied=%v", RelationID(rel.Type, rel.SrcID, rel.DstID), r.Applied)
	if !r.Applied {
		t.Fatalf("dangling edge should be kept, not silently dropped")
	}
	rejs := s.Rejections()
	if len(rejs) != 1 || rejs[0].Reason != ReasonDanglingReference {
		t.Fatalf("expected DANGLING_REFERENCE log, got %+v", rejs)
	}
	t.Logf("logged rejection: reason=%s detail=%s", rejs[0].Reason, rejs[0].Detail)
}

func reasonOf(r UpsertResult) string {
	if r.Rejection == nil {
		return "<nil>"
	}
	return string(r.Rejection.Reason)
}
