package graph

import (
	"testing"
	"time"

	"github.com/lisama569/metadata-catalog-go/internal/model"
)

func rel(key, up, down string) model.Relationship {
	return model.Relationship{Key: key, Upstream: up, Downstream: down, Kind: model.RelDerivedFrom, Version: 1, UpdatedAt: time.Unix(1, 0)}
}

func TestCycleDetection(t *testing.T) {
	g := NewGraph()
	for _, id := range []string{"a", "b", "c"} {
		g.UpsertNode(Node{ID: id})
	}
	g.UpsertEdge(rel("e1", "a", "b"))
	g.UpsertEdge(rel("e2", "b", "c"))

	if g.IsCycle("a", "b") {
		t.Fatal("a->b should not be a cycle")
	}
	if !g.IsCycle("c", "a") {
		t.Fatal("c->a must be detected as a cycle")
	}
	if !g.IsCycle("a", "a") {
		t.Fatal("self loop must be a cycle")
	}
	t.Logf("INPUT edges a->b,b->c; c->a cycle=%v (want true); a->b cycle=%v (want false)", g.IsCycle("c", "a"), g.IsCycle("a", "b"))
}

func TestImpactAndLineage(t *testing.T) {
	g := NewGraph()
	nodes := map[string]Node{
		"a": {ID: "a", Owner: "alice", Tags: []string{"pii"}, Terms: []string{"customer"}},
		"b": {ID: "b", Owner: "bob"},
		"c": {ID: "c", Owner: "alice"},
		"d": {ID: "d", Owner: "carol"},
	}
	for _, n := range nodes {
		g.UpsertNode(n)
	}
	// a -> b -> d ; a -> c -> d
	g.UpsertEdge(rel("ab", "a", "b"))
	g.UpsertEdge(rel("bd", "b", "d"))
	g.UpsertEdge(rel("ac", "a", "c"))
	g.UpsertEdge(rel("cd", "c", "d"))

	paths, matched, err := g.Impact("a", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("IMPACT a: %d paths, matched=%v", len(paths), matched)
	for _, p := range paths {
		t.Logf("  path nodes=%v edges=%v", p.Nodes, p.Edges)
	}
	want := map[string]bool{"b": true, "c": true, "d": true}
	for _, id := range matched {
		if !want[id] {
			t.Fatalf("unexpected impacted node %s", id)
		}
	}
	if len(matched) != 3 {
		t.Fatalf("matched=%v want b,c,d", matched)
	}

	// Owner filter.
	_, matchedOwner, _ := g.Impact("a", Filter{Owner: "alice"})
	t.Logf("IMPACT a owner=alice matched=%v (want [a? no -> c])", matchedOwner)
	if len(matchedOwner) != 1 || matchedOwner[0] != "c" {
		t.Fatalf("owner filter matched=%v want [c]", matchedOwner)
	}
	// Tag filter.
	_, matchedTag, _ := g.Impact("a", Filter{Tags: []string{"pii"}})
	t.Logf("IMPACT a tags=pii matched=%v", matchedTag)
	if len(matchedTag) != 0 {
		t.Fatalf("downstream tagged pii should be none, got %v", matchedTag)
	}

	// Lineage of d.
	up, upMatched, _ := g.Lineage("d", Filter{})
	t.Logf("LINEAGE d: %d paths, matched=%v", len(up), upMatched)
	if len(up) != 4 {
		t.Fatalf("upstream simple paths = %d want 4 (d-b-a, d-c-a, d-b, d-c)", len(up))
	}
}

func TestDanglingEdgesKept(t *testing.T) {
	g := NewGraph()
	g.UpsertNode(Node{ID: "a"})
	g.UpsertEdge(rel("ax", "a", "x")) // x missing
	dl := g.DanglingEdges()
	if len(dl) != 1 || !dl[0].Dangling {
		t.Fatalf("expected 1 dangling edge, got %+v", dl)
	}
	t.Logf("DANGLING before x exists: %+v", dl)
	// Supplying x promotes the edge.
	g.UpsertNode(Node{ID: "x"})
	if len(g.DanglingEdges()) != 0 {
		t.Fatal("edge should no longer be dangling after x ingested")
	}
	t.Log("edge promoted after downstream entity ingested")
}

func TestTraversalTerminatesWithCycle(t *testing.T) {
	// Build a raw cycle by inserting edges in an order that bypasses
	// application-level rejection (graph itself stores whatever it is given).
	g := NewGraph()
	g.UpsertNode(Node{ID: "a"})
	g.UpsertNode(Node{ID: "b"})
	g.UpsertEdge(rel("ab", "a", "b"))
	// Force the back edge directly into the structure to prove traversal
	// safety even if a cycle were present.
	g.mu.Lock()
	g.edges["ba"] = Edge{Rel: rel("ba", "b", "a")}
	g.out["b"] = append(g.out["b"], "ba")
	g.in["a"] = append(g.in["a"], "ba")
	g.mu.Unlock()

	done := make(chan struct{})
	var (
		paths []Path
		err   error
	)
	go func() {
		paths, _, err = g.Impact("a", Filter{})
		close(done)
	}()
	select {
	case <-done:
	case <-makeTimer():
		t.Fatal("impact traversal did not terminate on cyclic graph")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cycle graph traversal terminated, %d simple paths", len(paths))
	for _, p := range paths {
		t.Logf("  %v", p.Nodes)
	}
}

func TestSnapshotDeterministic(t *testing.T) {
	g := NewGraph()
	g.UpsertNode(Node{ID: "a"})
	g.UpsertNode(Node{ID: "b"})
	g.UpsertEdge(rel("a-b", "a", "b"))
	s1 := g.Snapshot()
	s2 := g.Snapshot()
	if len(s1.Edges) != len(s2.Edges) || s1.Edges[0].Rel.Key != s2.Edges[0].Rel.Key {
		t.Fatal("snapshots differ")
	}
}
