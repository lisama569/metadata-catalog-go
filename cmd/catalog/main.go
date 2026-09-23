// 本地演示：多来源摄入、乱序拒绝、结构演进、合并与影响分析。
package main

import (
	"fmt"

	catalog "github.com/lisama569/metadata-catalog-go/catalog"
)

func main() {
	c := catalog.New()

	fmt.Println("== 1. 多来源摄入（重复幂等、乱序拒绝） ==")
	base := catalog.Entity{
		ID: "orders", Kind: "dataset", Source: "etl-a", Version: 2,
		Schema: catalog.Schema{Fields: []catalog.Field{
			{Name: "id", Type: catalog.FieldTypeInt, Required: true},
			{Name: "amount", Type: catalog.FieldTypeInt},
		}},
		Tags: []string{"finance"},
	}
	printRes("ingest orders v2 (etl-a)", c.IngestEntity(base))
	dup := base
	dup.Source = "etl-b"
	printRes("ingest orders v2 duplicate (etl-b)", c.IngestEntity(dup))
	stale := base
	stale.Version = 1
	printRes("ingest orders v1 stale", c.IngestEntity(stale))

	fmt.Println("\n== 2. 结构演进 ==")
	evolved := base.Schema
	evolved.Fields[1].Type = catalog.FieldTypeFloat // int -> float 加宽
	v, res := c.EvolveSchema("orders", evolved, nil, "etl-a", 3)
	fmt.Printf("  widen amount int->float: compatible=%v applied=%v\n", v.Compatible, res.Applied)
	bad := catalog.Schema{Fields: []catalog.Field{{Name: "id", Type: catalog.FieldTypeInt, Required: true}}}
	v2, res2 := c.EvolveSchema("orders", bad, nil, "etl-a", 4)
	fmt.Printf("  remove amount: compatible=%v rejection=%s\n", v2.Compatible, reason(res2))

	fmt.Println("\n== 3. 血缘与影响分析 ==")
	for _, id := range []string{"stg_orders", "mart_revenue", "dash_revenue"} {
		c.IngestEntity(catalog.Entity{ID: id, Kind: "dataset", Source: "etl-a", Version: 1,
			Tags: []string{"finance"}, Owners: []string{"alice"}})
	}
	edge := func(src, dst string) {
		c.IngestRelation(catalog.Relation{Type: catalog.RelationLineage, SrcID: src, DstID: dst, Source: "etl-a", Version: 1})
	}
	edge("orders", "stg_orders")
	edge("stg_orders", "mart_revenue")
	edge("mart_revenue", "dash_revenue")
	edge("dash_revenue", "orders") // 人为造环
	edge("orders", "ghost_table")  // 悬挂引用

	rep := c.Impact("orders", catalog.ImpactFilter{Tags: []string{"finance"}})
	fmt.Println("  impacted (tag=finance):")
	for _, n := range rep.Nodes {
		fmt.Printf("    %s depth=%d via=%v\n", n.EntityID, n.Depth, n.Via)
	}
	fmt.Printf("  cycle edges: %v\n", rep.CycleEdges)
	fmt.Printf("  dangling: %v\n", rep.DanglingIDs)

	fmt.Println("\n== 4. 实体合并 ==")
	c.IngestEntity(catalog.Entity{ID: "orders_dup", Kind: "dataset", Source: "etl-b", Version: 1,
		Schema: base.Schema, Tags: []string{"finance"}})
	printRes("merge orders_dup -> orders", c.MergeEntities("orders", "orders_dup", "etl-a", 5))

	fmt.Println("\n== 5. 拒绝日志（持久化、可区分） ==")
	for _, r := range c.Store().Rejections() {
		fmt.Printf("  seq=%d reason=%s entity=%s relation=%s detail=%s\n",
			r.Seq, r.Reason, r.EntityID, r.Relation, r.Detail)
	}
}

func printRes(label string, r catalog.UpsertResult) {
	fmt.Printf("  %s: applied=%v duplicate=%v rejection=%s\n", label, r.Applied, r.Duplicate, reason(r))
}

func reason(r catalog.UpsertResult) string {
	if r.Rejection == nil {
		return "<nil>"
	}
	return string(r.Rejection.Reason)
}
