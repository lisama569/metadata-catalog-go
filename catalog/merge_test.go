package catalog

import (
	"testing"
)

// 合并重定向后与既有边重名：两条边都必须以确定性 ID 保留，可查询。
func TestMergePreservesDuplicateEdges(t *testing.T) {
	c := New()
	c.IngestEntity(testEntity("main", 1))
	c.IngestEntity(testEntity("dup", 1))
	c.IngestEntity(testEntity("x", 1))
	addLineage(c, "main", "x", 1)
	addLineage(c, "dup", "x", 1)

	if r := c.MergeEntities("main", "dup", "test", 2); !r.Applied {
		t.Fatalf("merge should apply")
	}
	rels := c.Store().Relations()
	t.Logf("relations after merge: %v", relIDs(rels))
	if len(rels) != 2 {
		t.Fatalf("redirected duplicate edge must be kept, got %d edges: %+v", len(rels), rels)
	}
	want := map[string]bool{"lineage|main->x": true, "lineage|main->x#2": true}
	for _, r := range rels {
		if !want[r.ID] {
			t.Fatalf("unexpected relation ID %q (want deterministic base + #2 suffix)", r.ID)
		}
		if r.SrcID != "main" || r.DstID != "x" {
			t.Fatalf("edge not redirected to main->x: %+v", r)
		}
	}
	// 影响分析中 x 只出现一次（节点去重），两条边都还在。
	rep := c.Impact("main", ImpactFilter{})
	if len(rep.Nodes) != 1 || rep.Nodes[0].EntityID != "x" {
		t.Fatalf("impact should reach x exactly once: %+v", rep.Nodes)
	}
}

// 指向被合并实体的自环边：重定向后变成 dst 的自环，必须保留而非丢弃。
func TestMergePreservesSelfLoop(t *testing.T) {
	c := New()
	c.IngestEntity(testEntity("main", 1))
	c.IngestEntity(testEntity("dup", 1))
	addLineage(c, "dup", "dup", 1) // 自环
	addLineage(c, "main", "dup", 1)

	if r := c.MergeEntities("main", "dup", "test", 2); !r.Applied {
		t.Fatalf("merge should apply")
	}
	rels := c.Store().Relations()
	t.Logf("relations after merge: %v", relIDs(rels))
	if len(rels) != 2 {
		t.Fatalf("self-loop must be preserved, got %d edges: %+v", len(rels), rels)
	}
	selfLoops := 0
	for _, r := range rels {
		if r.SrcID == "main" && r.DstID == "main" {
			selfLoops++
		}
	}
	if selfLoops != 2 {
		t.Fatalf("expected 2 self-loops on main after merge, got %d: %+v", selfLoops, rels)
	}
}

// 同样的初始状态做合并，结果必须完全确定（与 map 迭代顺序无关）。
func TestMergeEdgeDeterminism(t *testing.T) {
	build := func() []Relation {
		c := New()
		for _, id := range []string{"main", "dup", "a", "b", "c"} {
			c.IngestEntity(testEntity(id, 1))
		}
		addLineage(c, "main", "a", 1)
		addLineage(c, "dup", "a", 1)
		addLineage(c, "dup", "b", 1)
		addLineage(c, "b", "dup", 1)
		addLineage(c, "dup", "dup", 1)
		c.MergeEntities("main", "dup", "test", 2)
		return c.Store().Relations()
	}
	first := build()
	for i := 0; i < 20; i++ {
		got := build()
		if len(got) != len(first) {
			t.Fatalf("run %d: edge count differs: %d vs %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d: relation %d differs: %+v vs %+v", i, j, got[j], first[j])
			}
		}
	}
	t.Logf("deterministic merge result: %v", relIDs(first))
}

// 同一关系从不同来源重复提交：按重复处理，不报冲突。
func TestCrossSourceRelationDuplicate(t *testing.T) {
	c := New()
	c.IngestEntity(testEntity("a", 1))
	c.IngestEntity(testEntity("b", 1))
	rel := Relation{Type: RelationLineage, SrcID: "a", DstID: "b", Source: "etl-a", Version: 1}
	if r := c.IngestRelation(rel); !r.Applied {
		t.Fatalf("first ingest should apply")
	}
	dup := rel
	dup.Source = "etl-b"
	r := c.IngestRelation(dup)
	t.Logf("cross-source duplicate relation: duplicate=%v rejection=%v", r.Duplicate, reasonOf(r))
	if !r.Duplicate || r.Rejection != nil {
		t.Fatalf("same relation from another source must be duplicate, got %+v", r)
	}
	// 真正冲突（同 ID 同版本不同内容）仍然拒绝。
	conflict := rel
	conflict.ID = RelationID(rel.Type, rel.SrcID, rel.DstID)
	conflict.Type = RelationReference // 内容不同
	conflict.Source = "etl-b"
	rc := c.IngestRelation(conflict)
	if rc.Rejection == nil || rc.Rejection.Reason != ReasonVersionConflict {
		t.Fatalf("real conflict must still be rejected, got %+v", rc)
	}
}

func relIDs(rels []Relation) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = r.ID
	}
	return out
}
