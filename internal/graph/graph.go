// Package graph maintains the lineage graph and performs deterministic
// impact / lineage analysis. Traversal is cycle-safe, dangling edges are
// visible (never silently dropped), and outputs are deterministically ordered.
package graph

import (
	"sort"
	"sync"

	"github.com/lisama569/metadata-catalog-go/internal/model"
)

// Edge wraps a relationship with runtime flags.
type Edge struct {
	Rel      model.Relationship `json:"rel"`
	Dangling bool               `json:"dangling"`
}

// Node is the traversal projection of an entity.
type Node struct {
	ID      string   `json:"id"`
	Owner   string   `json:"owner,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Terms   []string `json:"terms,omitempty"`
	Version int      `json:"version"`
}

// Filter scopes impact/lineage queries by labels, terms or ownership.
type Filter struct {
	Owner string
	Tags  []string
	Terms []string
}

// Path is one deterministic lineage/impact path between two nodes.
type Path struct {
	Nodes []string `json:"nodes"`
	Edges []string `json:"edges"`
}

// EdgeRedirect describes one repointed edge.
type EdgeRedirect struct {
	RelKey     string `json:"relKey"`
	Upstream   string `json:"upstream"`
	Downstream string `json:"downstream"`
	Reason     string `json:"reason"`
}

// Snapshot is a point-in-time consistent view of the graph.
type Snapshot struct {
	Nodes map[string]Node
	Edges []Edge
}

// Graph is the concurrent-safe in-memory lineage graph.
type Graph struct {
	mu sync.RWMutex
	// meta holds ingested entity projections. Edge endpoints absent from
	// meta are dangling references; the edges themselves are still kept.
	meta  map[string]Node
	edges map[string]Edge
	out   map[string][]string // node -> rel keys (downstream)
	in    map[string][]string // node -> rel keys (upstream)
}

// NewGraph returns an empty graph.
func NewGraph() *Graph {
	return &Graph{
		meta:  map[string]Node{},
		edges: map[string]Edge{},
		out:   map[string][]string{},
		in:    map[string][]string{},
	}
}

// UpsertNode inserts or replaces a node.
func (g *Graph) UpsertNode(n Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.meta[n.ID] = n
	g.recomputeDanglingLocked()
}

// RemoveNode removes a node. Remaining edges pointing at it become dangling
// rather than being silently dropped.
func (g *Graph) RemoveNode(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.meta, id)
	g.recomputeDanglingLocked()
}

// UpsertEdge inserts or replaces an edge keyed by Rel.Key.
func (g *Graph) UpsertEdge(rel model.Relationship) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if old, ok := g.edges[rel.Key]; ok {
		g.detachLocked(old.Rel)
	}
	e := Edge{Rel: rel}
	g.edges[rel.Key] = e
	g.out[rel.Upstream] = append(g.out[rel.Upstream], rel.Key)
	g.in[rel.Downstream] = append(g.in[rel.Downstream], rel.Key)
	g.recomputeDanglingLocked()
}

// removeEdgeKey deletes an edge entirely (used during merge redirects).
func (g *Graph) removeEdgeKey(key string) {
	if e, ok := g.edges[key]; ok {
		g.detachLocked(e.Rel)
		delete(g.edges, key)
	}
}

func (g *Graph) detachLocked(rel model.Relationship) {
	g.out[rel.Upstream] = removeKey(g.out[rel.Upstream], rel.Key)
	g.in[rel.Downstream] = removeKey(g.in[rel.Downstream], rel.Key)
}

func removeKey(s []string, key string) []string {
	out := s[:0]
	for _, k := range s {
		if k != key {
			out = append(out, k)
		}
	}
	return out
}

// recomputeDanglingLocked marks an edge dangling when either endpoint lacks
// ingested entity metadata.
func (g *Graph) recomputeDanglingLocked() {
	for k, e := range g.edges {
		_, upOK := g.meta[e.Rel.Upstream]
		_, downOK := g.meta[e.Rel.Downstream]
		e.Dangling = !upOK || !downOK
		g.edges[k] = e
	}
}

// RedirectEdges moves every edge endpoint equal to fromID onto toID. The
// returned list records every repoint; edges that would become exact
// duplicates or self loops are reported but always retained/normalized
// deterministically (no edge is dropped).
func (g *Graph) RedirectEdges(fromID, toID string) []EdgeRedirect {
	g.mu.Lock()
	defer g.mu.Unlock()
	redirects := []EdgeRedirect{}

	// Stable processing order.
	keys := make([]string, 0, len(g.edges))
	for k := range g.edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		e := g.edges[key]
		rel := e.Rel
		moved := false
		if rel.Upstream == fromID {
			rel.Upstream = toID
			moved = true
		}
		if rel.Downstream == fromID {
			rel.Downstream = toID
			moved = true
		}
		if !moved {
			continue
		}
		g.detachLocked(e.Rel)
		rel.Key = key // keys remain stable across redirects
		g.edges[key] = Edge{Rel: rel}
		g.out[rel.Upstream] = append(g.out[rel.Upstream], key)
		g.in[rel.Downstream] = append(g.in[rel.Downstream], key)
		redirects = append(redirects, EdgeRedirect{
			RelKey:     key,
			Upstream:   rel.Upstream,
			Downstream: rel.Downstream,
			Reason:     "merge:" + fromID + "->" + toID,
		})
	}
	delete(g.meta, fromID)
	g.recomputeDanglingLocked()
	return redirects
}

// IsCycle reports whether adding edge up->down would create a directed cycle.
// A self-loop is always a cycle. Existing cycles (which are never accepted)
// cannot occur, but the check is defensive and cycle-safe.
func (g *Graph) IsCycle(up, down string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if up == down {
		return true
	}
	// A cycle exists iff down can already reach up.
	return g.reachesLocked(down, up)
}

// reachesLocked answers whether from can reach target following out edges,
// with visited-set termination guaranteeing no infinite traversal.
func (g *Graph) reachesLocked(from, target string) bool {
	visited := map[string]bool{}
	stack := []string{from}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == target {
			return true
		}
		if visited[n] {
			continue
		}
		visited[n] = true
		next := append([]string(nil), g.out[n]...)
		sort.Strings(next)
		for _, k := range next {
			e, ok := g.edges[k]
			if ok {
				stack = append(stack, e.Rel.Downstream)
			}
		}
	}
	return false
}

// Impact returns deterministic downstream paths starting at id and the set of
// matched (filter-passing) impacted node IDs. Cycles terminate via a visited
// set; the start node is never counted as impacted by itself.
func (g *Graph) Impact(id string, f Filter) ([]Path, []string, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	paths, matched := g.traverseLocked(id, f, true)
	return paths, matched, nil
}

// Lineage returns deterministic upstream paths reaching id and the matched
// upstream node IDs.
func (g *Graph) Lineage(id string, f Filter) ([]Path, []string, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	paths, matched := g.traverseLocked(id, f, false)
	return paths, matched, nil
}

func (g *Graph) traverseLocked(id string, f Filter, downstream bool) ([]Path, []string) {
	// Iterative DFS enumerating every simple (non-repeating) path. A path
	// ends at a leaf; every simple path is returned. Visited-on-path set
	// guarantees termination even if a cycle somehow existed.
	type frame struct {
		node string
		path Path
	}
	paths := []Path{}
	reachable := map[string]bool{}
	start := frame{node: id, path: Path{Nodes: []string{id}}}
	stack := []frame{start}

	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		onPath := map[string]bool{}
		for _, n := range cur.path.Nodes {
			onPath[n] = true
		}

		var keys []string
		if downstream {
			keys = g.out[cur.node]
		} else {
			keys = g.in[cur.node]
		}
		keys = append([]string(nil), keys...)
		sort.Strings(keys)

		for _, k := range keys {
			e := g.edges[k]
			var next string
			if downstream {
				next = e.Rel.Downstream
			} else {
				next = e.Rel.Upstream
			}
			if onPath[next] {
				continue // close a cycle without infinite traversal
			}
			np := Path{
				Nodes: append(append([]string(nil), cur.path.Nodes...), next),
				Edges: append(append([]string(nil), cur.path.Edges...), k),
			}
			stack = append(stack, frame{node: next, path: np})
		}

		// Each frame is a simple path prefix. Record it; dedupe later.
		if len(cur.path.Nodes) > 1 {
			paths = append(paths, cur.path)
			last := cur.path.Nodes[len(cur.path.Nodes)-1]
			reachable[last] = true
		}
	}

	// Deduplicate paths by node sequence, then sort deterministically.
	uniq := map[string]Path{}
	order := []string{}
	for _, p := range paths {
		key := pathKey(p)
		if _, ok := uniq[key]; !ok {
			uniq[key] = p
			order = append(order, key)
		}
	}
	sort.Strings(order)
	out := make([]Path, 0, len(order))
	for _, k := range order {
		out = append(out, uniq[k])
	}
	matchedIDs := make([]string, 0)
	for n := range reachable {
		if g.matchesLocked(n, f) {
			matchedIDs = append(matchedIDs, n)
		}
	}
	sort.Strings(matchedIDs)
	return out, matchedIDs
}

// matchesFilter checks a node against a filter.
func (g *Graph) matchesLocked(id string, f Filter) bool {
	n, ok := g.meta[id]
	if !ok {
		return false
	}
	if f.Owner != "" && n.Owner != f.Owner {
		return false
	}
	if !containsAll(n.Tags, f.Tags) {
		return false
	}
	if !containsAll(n.Terms, f.Terms) {
		return false
	}
	return true
}

func containsAll(haystack, needles []string) bool {
	if len(needles) == 0 {
		return true
	}
	set := map[string]bool{}
	for _, h := range haystack {
		set[h] = true
	}
	for _, n := range needles {
		if !set[n] {
			return false
		}
	}
	return true
}

func pathKey(p Path) string {
	out := ""
	for i, n := range p.Nodes {
		if i > 0 {
			out += ">"
		}
		out += n
	}
	return out
}

// DanglingEdges returns edges with at least one missing endpoint, sorted.
func (g *Graph) DanglingEdges() []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := []Edge{}
	keys := []string{}
	for k, e := range g.edges {
		if e.Dangling {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, g.edges[k])
	}
	return out
}

// Snapshot returns a consistent point-in-time copy.
func (g *Graph) Snapshot() Snapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	nodes := make(map[string]Node, len(g.meta))
	for k, v := range g.meta {
		tags := append([]string(nil), v.Tags...)
		terms := append([]string(nil), v.Terms...)
		v.Tags, v.Terms = tags, terms
		nodes[k] = v
	}
	keys := make([]string, 0, len(g.edges))
	for k := range g.edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	edges := make([]Edge, 0, len(keys))
	for _, k := range keys {
		edges = append(edges, g.edges[k])
	}
	return Snapshot{Nodes: nodes, Edges: edges}
}
