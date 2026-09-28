package catalog

import "sort"

// ImpactFilter 影响分析筛选条件（按标签、术语或所有权）。
type ImpactFilter struct {
	Tags   []string `json:"tags,omitempty"`
	Terms  []string `json:"terms,omitempty"`
	Owners []string `json:"owners,omitempty"`
}

// ImpactNode 影响分析结果中的一个节点。
type ImpactNode struct {
	EntityID string   `json:"entity_id"`
	Depth    int      `json:"depth"`
	Via      []string `json:"via"` // 从起点到达该节点的一条确定性路径
	Dangling bool     `json:"dangling,omitempty"`
}

// ImpactReport 影响分析报告；输出确定性排序，循环不会导致无限遍历。
type ImpactReport struct {
	Root        string       `json:"root"`
	Nodes       []ImpactNode `json:"nodes"`
	CycleEdges  []string     `json:"cycle_edges,omitempty"`  // 因环被截断的边
	DanglingIDs []string     `json:"dangling_ids,omitempty"` // 悬挂引用目标
}

// ImpactAnalysis 从 root 出发沿血缘下游（src -> dst 方向）做影响分析。
// 基于 Store 的单次加锁一致性快照执行，持续写入期间单次查询内部一致；
// 使用 visited 集合防止循环导致无限遍历；输出按 ID 排序保证确定性。
func ImpactAnalysis(s *Store, root string, filter ImpactFilter) ImpactReport {
	snap := s.Snapshot()
	entities := map[string]Entity{}
	for _, e := range snap.Entities {
		entities[e.ID] = e
	}
	// 下游邻接表：src -> []dst（排序保证确定性）。
	adj := map[string][]string{}
	for _, r := range snap.Relations {
		if r.Type != RelationLineage {
			continue
		}
		adj[r.SrcID] = append(adj[r.SrcID], r.DstID)
	}
	for k := range adj {
		sort.Strings(adj[k])
	}

	report := ImpactReport{Root: root}
	visited := map[string]bool{root: true}
	type item struct {
		id    string
		depth int
		via   []string
	}
	queue := []item{{id: root, depth: 0, via: []string{root}}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.id != root {
			if !matchesFilter(entities[cur.id], filter) {
				// 不满足筛选的节点不进入结果，但仍继续遍历其下游。
			} else {
				report.Nodes = append(report.Nodes, ImpactNode{
					EntityID: cur.id, Depth: cur.depth, Via: cur.via,
				})
			}
		}
		for _, next := range adj[cur.id] {
			if visited[next] {
				report.CycleEdges = append(report.CycleEdges, cur.id+"->"+next)
				continue
			}
			visited[next] = true
			if _, ok := entities[next]; !ok {
				report.DanglingIDs = append(report.DanglingIDs, next)
				continue // 悬挂引用：记录但不展开
			}
			via := append(append([]string{}, cur.via...), next)
			queue = append(queue, item{id: next, depth: cur.depth + 1, via: via})
		}
	}

	sort.Slice(report.Nodes, func(i, j int) bool { return report.Nodes[i].EntityID < report.Nodes[j].EntityID })
	sort.Strings(report.CycleEdges)
	sort.Strings(report.DanglingIDs)
	return report
}

// matchesFilter 空筛选匹配一切；否则任一条件命中即可。
func matchesFilter(e Entity, f ImpactFilter) bool {
	if len(f.Tags) == 0 && len(f.Terms) == 0 && len(f.Owners) == 0 {
		return true
	}
	return intersects(e.Tags, f.Tags) || intersects(e.Terms, f.Terms) || intersects(e.Owners, f.Owners)
}

func intersects(have, want []string) bool {
	if len(want) == 0 {
		return false
	}
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if set[w] {
			return true
		}
	}
	return false
}
