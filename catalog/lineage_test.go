package catalog

import (
	"fmt"
	"sync"
	"testing"
)

func addLineage(c *Catalog, src, dst string, v int64) {
	c.IngestRelation(Relation{Type: RelationLineage, SrcID: src, DstID: dst, Source: "test", Version: v})
}

func TestImpactAnalysisLinear(t *testing.T) {
	c := New()
	for _, id := range []string{"raw", "stg", "mart", "dash"} {
		e := testEntity(id, 1)
		c.IngestEntity(e)
	}
	addLineage(c, "raw", "stg", 1)
	addLineage(c, "stg", "mart", 1)
	addLineage(c, "mart", "dash", 1)

	rep := c.Impact("raw", ImpactFilter{})
	for _, n := range rep.Nodes {
		t.Logf("impacted: id=%s depth=%d via=%v", n.EntityID, n.Depth, n.Via)
	}
	if len(rep.Nodes) != 3 {
		t.Fatalf("expected 3 impacted nodes, got %d", len(rep.Nodes))
	}
	// 确定性：按 ID 排序。
	if rep.Nodes[0].EntityID != "dash" || rep.Nodes[2].EntityID != "stg" {
		t.Fatalf("output not deterministically sorted: %+v", rep.Nodes)
	}
}

func TestImpactAnalysisCycleTerminates(t *testing.T) {
	c := New()
	for _, id := range []string{"a", "b", "c"} {
		c.IngestEntity(testEntity(id, 1))
	}
	addLineage(c, "a", "b", 1)
	addLineage(c, "b", "c", 1)
	addLineage(c, "c", "a", 1) // 成环

	rep := c.Impact("a", ImpactFilter{})
	t.Logf("cycle report: nodes=%d cycle_edges=%v", len(rep.Nodes), rep.CycleEdges)
	if len(rep.Nodes) != 2 { // b, c；a 是根
		t.Fatalf("cycle should not duplicate nodes, got %+v", rep.Nodes)
	}
	if len(rep.CycleEdges) != 1 || rep.CycleEdges[0] != "c->a" {
		t.Fatalf("expected truncated cycle edge c->a, got %v", rep.CycleEdges)
	}
}

func TestImpactAnalysisDangling(t *testing.T) {
	c := New()
	c.IngestEntity(testEntity("a", 1))
	addLineage(c, "a", "ghost", 1) // 悬挂引用

	rep := c.Impact("a", ImpactFilter{})
	t.Logf("dangling report: nodes=%d dangling=%v", len(rep.Nodes), rep.DanglingIDs)
	if len(rep.DanglingIDs) != 1 || rep.DanglingIDs[0] != "ghost" {
		t.Fatalf("dangling target should be reported, got %v", rep.DanglingIDs)
	}
	if len(rep.Nodes) != 0 {
		t.Fatalf("dangling target must not be expanded as node")
	}
}

func TestImpactFilterByTagTermOwner(t *testing.T) {
	c := New()
	mk := func(id string, tags, terms, owners []string) {
		e := testEntity(id, 1)
		e.Tags, e.Terms, e.Owners = tags, terms, owners
		c.IngestEntity(e)
	}
	mk("root", nil, nil, nil)
	mk("n1", []string{"pii"}, nil, nil)
	mk("n2", nil, []string{"finance"}, nil)
	mk("n3", nil, nil, []string{"alice"})
	mk("n4", []string{"other"}, nil, nil)
	for _, d := range []string{"n1", "n2", "n3", "n4"} {
		addLineage(c, "root", d, 1)
	}

	rep := c.Impact("root", ImpactFilter{Tags: []string{"pii"}, Terms: []string{"finance"}, Owners: []string{"alice"}})
	t.Logf("filtered report: nodes=%v", rep.Nodes)
	if len(rep.Nodes) != 3 {
		t.Fatalf("expected 3 filtered nodes, got %+v", rep.Nodes)
	}
	for _, n := range rep.Nodes {
		if n.EntityID == "n4" {
			t.Fatalf("n4 should be filtered out")
		}
	}
}

func TestMergeEntitiesRedirectsLineage(t *testing.T) {
	c := New()
	c.IngestEntity(testEntity("main", 1))
	c.IngestEntity(testEntity("dup", 1))
	c.IngestEntity(testEntity("downstream", 1))
	addLineage(c, "dup", "downstream", 1)

	res := c.MergeEntities("main", "dup", "test", 2)
	t.Logf("merge dup->main: applied=%v rejection=%v", res.Applied, reasonOf(res))
	if !res.Applied {
		t.Fatalf("merge should apply")
	}
	src, _ := c.Store().GetEntity("dup")
	if src.MergedInto != "main" {
		t.Fatalf("dup should be marked merged into main")
	}
	// 关系已重定向：main -> downstream。
	rep := c.Impact("main", ImpactFilter{})
	if len(rep.Nodes) != 1 || rep.Nodes[0].EntityID != "downstream" {
		t.Fatalf("lineage not redirected: %+v", rep.Nodes)
	}
	// 已合并实体拒绝直接写入。
	w := c.IngestEntity(testEntity("dup", 5))
	t.Logf("write to merged entity: rejection=%v", reasonOf(w))
	if w.Rejection == nil || w.Rejection.Reason != ReasonEntityMerged {
		t.Fatalf("expected ENTITY_MERGED, got %+v", w.Rejection)
	}
}

func TestMergeConflictRejected(t *testing.T) {
	c := New()
	a := testEntity("a", 1)
	a.Schema = Schema{Fields: []Field{{Name: "v", Type: FieldTypeInt}}}
	b := testEntity("b", 1)
	b.Schema = Schema{Fields: []Field{{Name: "v", Type: FieldTypeString}}} // 同名字段类型冲突
	c.IngestEntity(a)
	c.IngestEntity(b)

	res := c.MergeEntities("a", "b", "test", 2)
	t.Logf("conflicting merge: rejection=%v detail=%s", reasonOf(res), res.Rejection.Detail)
	if res.Rejection == nil || res.Rejection.Reason != ReasonMergeConflict {
		t.Fatalf("expected MERGE_CONFLICT, got %+v", res.Rejection)
	}
	// 拒绝后状态不变。
	if e, _ := c.Store().GetEntity("b"); e.MergedInto != "" {
		t.Fatalf("rejected merge must not mark entity")
	}
}

func TestQueryConsistentDuringWrites(t *testing.T) {
	c := New()
	c.IngestEntity(testEntity("root", 1))
	var wg sync.WaitGroup
	// 持续写入。
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("n-%03d", i)
			c.IngestEntity(testEntity(id, 1))
			addLineage(c, "root", id, 1)
		}(i)
	}
	// 并发查询：每次结果必须内部一致（无重复、排序确定）。
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep := c.Impact("root", ImpactFilter{})
			seen := map[string]bool{}
			for j, n := range rep.Nodes {
				if seen[n.EntityID] {
					t.Errorf("duplicate node in report: %s", n.EntityID)
				}
				seen[n.EntityID] = true
				if j > 0 && rep.Nodes[j-1].EntityID > n.EntityID {
					t.Errorf("report not sorted")
				}
			}
		}()
	}
	wg.Wait()
	final := c.Impact("root", ImpactFilter{})
	t.Logf("final report after concurrent writes: nodes=%d", len(final.Nodes))
	if len(final.Nodes) != 50 {
		t.Fatalf("expected 50 nodes, got %d", len(final.Nodes))
	}
}
