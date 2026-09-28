package catalog

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// 重启后实体、关系、拒绝记录完整恢复，状态与重启前一致。
func TestRestartRecoversEntitiesRelationsRejections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	c1, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c1.IngestEntity(testEntity("orders", 2))
	c1.IngestEntity(testEntity("mart", 1))
	addLineage(c1, "orders", "mart", 1)
	// 制造一条拒绝记录（乱序写入）。
	stale := c1.IngestEntity(testEntity("orders", 1))
	if stale.Rejection == nil {
		t.Fatalf("expected stale rejection before restart")
	}
	wantEntities := c1.Store().Entities()
	wantRelations := c1.Store().Relations()
	wantRejections := c1.Store().Rejections()
	t.Logf("before restart: entities=%d relations=%d rejections=%d",
		len(wantEntities), len(wantRelations), len(wantRejections))

	// 模拟重启：重新打开同一状态文件。
	c2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	gotEntities := c2.Store().Entities()
	gotRelations := c2.Store().Relations()
	gotRejections := c2.Store().Rejections()

	if !reflect.DeepEqual(wantEntities, gotEntities) {
		t.Fatalf("entities differ after restart:\n before=%+v\n after=%+v", wantEntities, gotEntities)
	}
	if !reflect.DeepEqual(wantRelations, gotRelations) {
		t.Fatalf("relations differ after restart:\n before=%+v\n after=%+v", wantRelations, gotRelations)
	}
	if !reflect.DeepEqual(wantRejections, gotRejections) {
		t.Fatalf("rejections differ after restart:\n before=%+v\n after=%+v", wantRejections, gotRejections)
	}
	t.Logf("after restart: state identical (entities=%d relations=%d rejections=%d)",
		len(gotEntities), len(gotRelations), len(gotRejections))
}

// 重启前被判为重复的提交，重启后再提交仍然是重复而不是接受。
func TestDuplicateStaysDuplicateAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	c1, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	e := testEntity("ds", 3)
	e.Source = "etl-a"
	if r := c1.IngestEntity(e); !r.Applied {
		t.Fatalf("first ingest should apply")
	}
	dup := e
	dup.Source = "etl-b" // 跨来源、内容实质相同
	if r := c1.IngestEntity(dup); !r.Duplicate {
		t.Fatalf("cross-source duplicate before restart should be duplicate, got %+v", r)
	}

	c2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	r := c2.IngestEntity(dup)
	t.Logf("after restart: applied=%v duplicate=%v rejection=%v", r.Applied, r.Duplicate, reasonOf(r))
	if !r.Duplicate || r.Applied || r.Rejection != nil {
		t.Fatalf("duplicate after restart must stay duplicate, got %+v", r)
	}
	// 关系同理。
	rel := Relation{Type: RelationLineage, SrcID: "ds", DstID: "ds2", Source: "etl-a", Version: 1}
	c2.IngestEntity(testEntity("ds2", 1))
	if rr := c2.IngestRelation(rel); !rr.Applied {
		t.Fatalf("relation ingest should apply")
	}
	c3, err := Open(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	rel.Source = "etl-b"
	if rr := c3.IngestRelation(rel); !rr.Duplicate {
		t.Fatalf("relation duplicate after restart must stay duplicate, got %+v", rr)
	}
}

// 被拒绝的写入在重启后重试仍能收敛到一致状态：
// 拒绝序号接续不重置、不重复计数，状态不多不少。
func TestRejectedRetryConvergesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	c1, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c1.IngestEntity(testEntity("ds", 5))
	// 乱序写入被拒绝。
	if r := c1.IngestEntity(testEntity("ds", 2)); r.Rejection == nil || r.Rejection.Reason != ReasonStaleVersion {
		t.Fatalf("expected STALE_VERSION, got %+v", r)
	}
	rejBefore := c1.Store().Rejections()
	t.Logf("before restart: rejections=%d lastSeq=%d", len(rejBefore), rejBefore[len(rejBefore)-1].Seq)

	c2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	// 重启后再次乱序写入：仍拒绝，且 Seq 接续而不是从 1 重新计数。
	r := c2.IngestEntity(testEntity("ds", 3))
	if r.Rejection == nil || r.Rejection.Reason != ReasonStaleVersion {
		t.Fatalf("expected STALE_VERSION after restart, got %+v", r)
	}
	if r.Rejection.Seq != rejBefore[len(rejBefore)-1].Seq+1 {
		t.Fatalf("rejection seq must continue across restart, got %d want %d",
			r.Rejection.Seq, rejBefore[len(rejBefore)-1].Seq+1)
	}
	// 以更高版本重试：收敛到一致状态。
	final := testEntity("ds", 6)
	final.Tags = []string{"final"}
	if rr := c2.IngestEntity(final); !rr.Applied {
		t.Fatalf("retry with newer version should apply")
	}
	got, _ := c2.Store().GetEntity("ds")
	if got.Version != 6 || len(got.Tags) != 1 || got.Tags[0] != "final" {
		t.Fatalf("state not converged after retry: %+v", got)
	}
	// 拒绝记录恰好两条，无重复无丢失。
	rejs := c2.Store().Rejections()
	if len(rejs) != 2 || rejs[0].Seq != 1 || rejs[1].Seq != 2 {
		t.Fatalf("rejections miscounted across restart: %+v", rejs)
	}
	// 再次重启，恢复出的拒绝记录仍一致。
	c3, err := Open(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	if !reflect.DeepEqual(rejs, c3.Store().Rejections()) {
		t.Fatalf("rejections differ after second restart")
	}
}

// 同一关系从不同来源重复提交（内容实质相同）：统一按重复处理，不误报冲突。
func TestCrossSourceDuplicateRelation(t *testing.T) {
	s := NewStore()
	rel := Relation{Type: RelationLineage, SrcID: "a", DstID: "b", Source: "etl-a", Version: 1}
	if r := s.UpsertRelation(rel); !r.Applied {
		t.Fatalf("first ingest should apply")
	}
	dup := rel
	dup.Source = "etl-b"
	r := s.UpsertRelation(dup)
	t.Logf("cross-source duplicate relation: duplicate=%v rejection=%v", r.Duplicate, reasonOf(r))
	if !r.Duplicate || r.Rejection != nil {
		t.Fatalf("cross-source duplicate must be idempotent, got %+v", r)
	}
	// 真正内容不同的同版本提交仍然是冲突，原因可区分。
	conflict := Relation{
		ID: RelationID(RelationLineage, "a", "b"), // 与已存边同 ID
		Type: RelationReference, SrcID: "a", DstID: "b", Source: "etl-c", Version: 1,
	}
	r2 := s.UpsertRelation(conflict)
	if r2.Rejection == nil || r2.Rejection.Reason != ReasonVersionConflict {
		t.Fatalf("real conflict must be rejected as VERSION_CONFLICT, got %+v", r2)
	}
}

// 合并时不丢边：重定向产生的重复边与自环都确定性保留且可查。
func TestMergePreservesAllEdges(t *testing.T) {
	build := func() *Catalog {
		c := New()
		for _, id := range []string{"main", "dup", "x", "y"} {
			c.IngestEntity(testEntity(id, 1))
		}
		addLineage(c, "main", "x", 1) // 重定向后与 dup->x 冲突
		addLineage(c, "dup", "x", 1)  // 重定向为 main->x（重复边）
		addLineage(c, "dup", "main", 1)
		addLineage(c, "x", "dup", 1) // 重定向为 x->main
		addLineage(c, "dup", "dup", 1)
		return c
	}

	c := build()
	before := len(c.Store().Relations())
	res := c.MergeEntities("main", "dup", "test", 2)
	if !res.Applied {
		t.Fatalf("merge should apply: %+v", res.Rejection)
	}
	rels := c.Store().Relations()
	t.Logf("relations before=%d after=%d", before, len(rels))
	for _, r := range rels {
		t.Logf("  edge: id=%s %s -> %s", r.ID, r.SrcID, r.DstID)
	}
	if len(rels) != before {
		t.Fatalf("merge must not lose edges: before=%d after=%d", before, len(rels))
	}
	// 没有任何边仍指向已合并实体。
	for _, r := range rels {
		if r.SrcID == "dup" || r.DstID == "dup" {
			t.Fatalf("edge still references merged entity: %+v", r)
		}
	}
	// 重复边（两条 main->x）与自环（main->main）都保留且可查。
	mainToX, selfLoops := 0, 0
	for _, r := range rels {
		if r.SrcID == "main" && r.DstID == "x" {
			mainToX++
		}
		if r.SrcID == "main" && r.DstID == "main" {
			selfLoops++
		}
	}
	if mainToX != 2 {
		t.Fatalf("duplicate edges must be preserved: main->x count=%d, want 2", mainToX)
	}
	if selfLoops != 2 { // dup->main 与 dup->dup 都重定向为 main->main
		t.Fatalf("self loops must be preserved: count=%d, want 2", selfLoops)
	}
	// 确定性：相同输入再跑一次，结果完全一致。
	c2 := build()
	c2.MergeEntities("main", "dup", "test", 2)
	if !reflect.DeepEqual(rels, c2.Store().Relations()) {
		t.Fatalf("merge result not deterministic:\n run1=%+v\n run2=%+v", rels, c2.Store().Relations())
	}
	// 自环不会导致影响分析失控：有界且确定。
	rep := c.Impact("main", ImpactFilter{})
	if len(rep.CycleEdges) == 0 {
		t.Fatalf("self loop should surface as cycle edge, got %+v", rep)
	}
	t.Logf("impact after merge: nodes=%d cycleEdges=%v", len(rep.Nodes), rep.CycleEdges)
}

// 持续写入期间，影响分析基于内部一致的快照：
// 所有被引用的实体若已摄入，报告中绝不允许出现对应的悬挂引用。
func TestImpactConsistentSnapshotDuringWrites(t *testing.T) {
	c := New()
	const n = 60
	c.IngestEntity(testEntity("root", 1))
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("n-%03d", i)
		c.IngestEntity(testEntity(ids[i], 1)) // 实体先全部就位
	}
	existing := map[string]bool{"root": true}
	for _, id := range ids {
		existing[id] = true
	}

	var writers, readers sync.WaitGroup
	// 写入方：只追加关系（实体已存在）。
	for i := 0; i < n; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			addLineage(c, "root", ids[i], 1)
		}(i)
	}
	// 查询方：任何一次报告中都不允许把已存在的实体误判为悬挂。
	stop := make(chan struct{})
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			rep := c.Impact("root", ImpactFilter{})
			for _, d := range rep.DanglingIDs {
				if existing[d] {
					t.Errorf("existing entity %s reported dangling: inconsistent snapshot", d)
				}
			}
			// 每个节点的实体必须与边来自同一快照：via 路径上的边都应在
			// 本次报告对应的图中（节点本身出现在结果中即说明实体与边同时可见）。
			for _, nd := range rep.Nodes {
				if !existing[nd.EntityID] {
					t.Errorf("unknown node in report: %s", nd.EntityID)
				}
			}
		}
	}()
	writers.Wait() // 等写入完成
	close(stop)
	readers.Wait()

	final := c.Impact("root", ImpactFilter{})
	if len(final.Nodes) != n {
		t.Fatalf("final report should contain %d nodes, got %d", n, len(final.Nodes))
	}
	if len(final.DanglingIDs) != 0 {
		t.Fatalf("no dangling expected in final report, got %v", final.DanglingIDs)
	}
	t.Logf("final consistent report: nodes=%d", len(final.Nodes))
}
