package catalog

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lisama569/metadata-catalog-go/internal/graph"
	"github.com/lisama569/metadata-catalog-go/internal/merge"
	"github.com/lisama569/metadata-catalog-go/internal/model"
)

func testClock() func() time.Time {
	t := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Millisecond)
		return t
	}
}

func newTestCatalog(t *testing.T, allowRemoval ...bool) *Catalog {
	t.Helper()
	c, err := Open(Config{JournalPath: "", Clock: testClock(), AllowFieldRemoval: len(allowRemoval) > 0 && allowRemoval[0]})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func ent(id string, ver int, source string, fields ...model.Field) model.Entity {
	return model.Entity{
		ID: id, Version: ver, TypeName: "table", Source: source,
		Schema:    model.Schema{Version: ver, Fields: fields},
		UpdatedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC),
	}
}

func TestDuplicateAndOutOfOrderIngestion(t *testing.T) {
	c := newTestCatalog(t)
	e1 := ent("e1", 1, "s1", model.Field{Name: "a", Type: model.TypeString})

	r1, err := c.IngestEntity(e1)
	if err != nil || !r1.Accepted {
		t.Fatalf("first ingest: %+v %v", r1, err)
	}
	t.Logf("INGEST e1 v%d -> %s (%s)", r1.Version, r1.Decision, r1.Reason)

	// Identical resubmission.
	rDup, _ := c.IngestEntity(e1)
	t.Logf("RESUBMIT identical e1 v%d -> %s (%s)", rDup.Version, rDup.Decision, rDup.Reason)
	if rDup.Decision != model.DecisionDuplicate {
		t.Fatalf("expected duplicate, got %s", rDup.Decision)
	}

	// Same version, different content.
	eConflict := e1
	eConflict.Owner = "someone"
	rConf, _ := c.IngestEntity(eConflict)
	t.Logf("RESUBMIT e1 v%d changed -> %s fp submitted=%s stored=%s",
		rConf.Version, rConf.Decision, rConf.Detail["submittedFingerprint"], rConf.Detail["storedFingerprint"])
	if rConf.Decision != model.RejectContentConflict {
		t.Fatalf("expected content conflict, got %s", rConf.Decision)
	}

	// Older version.
	e0 := ent("e1", 0, "s1") // invalid version actually; use separate id logic
	_ = e0
	eStale := ent("e1", 1, "s1", model.Field{Name: "a", Type: model.TypeString})
	// create a valid stale scenario: advance to v2 then submit v1 again
	e2 := ent("e1", 2, "s1", model.Field{Name: "a", Type: model.TypeString}, model.Field{Name: "b", Type: model.TypeInt})
	r2, _ := c.IngestEntity(e2)
	t.Logf("INGEST e1 v%d -> %s (%s)", r2.Version, r2.Decision, r2.Reason)
	if !r2.Accepted {
		t.Fatalf("v2 ingest rejected: %s %s", r2.Decision, r2.Reason)
	}
	rStale, _ := c.IngestEntity(eStale)
	t.Logf("RESUBMIT stale v1 (current=2) -> %s detail=%v", rStale.Decision, rStale.Detail)
	if rStale.Decision != model.RejectStaleVersion {
		t.Fatalf("expected stale rejection, got %s", rStale.Decision)
	}

	// Out-of-order gap v4 when current=2.
	e4 := ent("e1", 4, "s1", model.Field{Name: "a", Type: model.TypeString},
		model.Field{Name: "b", Type: model.TypeInt}, model.Field{Name: "c", Type: model.TypeBool})
	rGap, _ := c.IngestEntity(e4)
	t.Logf("INGEST e1 v4 gap -> %s detail=%v", rGap.Decision, rGap.Detail)
	if rGap.Decision != model.RejectOutOfOrderGap {
		t.Fatalf("expected gap rejection, got %s", rGap.Decision)
	}

	// Retrying v3 after the gap rejection converges to accepted.
	e3 := ent("e1", 3, "s1", model.Field{Name: "a", Type: model.TypeString}, model.Field{Name: "b", Type: model.TypeInt})
	r3, _ := c.IngestEntity(e3)
	t.Logf("RETRY e1 v3 -> %s (%s)", r3.Decision, r3.Reason)
	if !r3.Accepted {
		t.Fatalf("retry of v3 must succeed, got %s", r3.Decision)
	}
	got, _ := c.GetEntity("e1")
	if got.Version != 3 {
		t.Fatalf("current version = %d want 3", got.Version)
	}

	// Rejection reasons are distinct and durable.
	hist := c.History(0)
	t.Logf("HISTORY %d durable decisions", len(hist))
	seen := map[model.Decision]bool{}
	for _, h := range hist {
		seen[h.Decision] = true
	}
	for _, want := range []model.Decision{model.DecisionDuplicate, model.RejectContentConflict, model.RejectStaleVersion, model.RejectOutOfOrderGap} {
		if !seen[want] {
			t.Fatalf("history missing decision %s; got %+v", want, seen)
		}
	}
}

func TestConcurrentUpdatesOlderNeverOverwrites(t *testing.T) {
	c := newTestCatalog(t)
	base := ent("c1", 1, "s", model.Field{Name: "a", Type: model.TypeString})
	if r, _ := c.IngestEntity(base); !r.Accepted {
		t.Fatal("seed failed")
	}

	var wg sync.WaitGroup
	accepted := make(chan model.DecisionRecord, 100)
	results := make([]model.DecisionRecord, 0)
	var mu sync.Mutex
	// Fire v2..v6 from multiple goroutines repeatedly.
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for v := 2; v <= 6; v++ {
				e := ent("c1", v, "s", model.Field{Name: "a", Type: model.TypeString})
				e.Owner = "g"
				r, err := c.IngestEntity(e)
				if err != nil {
					t.Errorf("ingest err: %v", err)
					return
				}
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
				if r.Accepted {
					select {
					case accepted <- r:
					default:
					}
				}
			}
		}(g)
	}
	wg.Wait()

	got, _ := c.GetEntity("c1")
	t.Logf("FINAL c1 version=%d (want 6); accepted events=%d total attempts=%d",
		got.Version, len(accepted), len(results))
	if got.Version != 6 {
		t.Fatalf("final version = %d want 6: an older/gap write must not win", got.Version)
	}
}

func TestSchemaEvolutionRejectionPersisted(t *testing.T) {
	c := newTestCatalog(t)
	e := ent("e2", 1, "s", model.Field{Name: "a", Type: model.TypeString, Required: false})
	if r, _ := c.IngestEntity(e); !r.Accepted {
		t.Fatal("seed")
	}
	// Incompatible: change type string -> int.
	rep, rec, err := c.EvolveSchema("e2", model.Schema{Fields: []model.Field{{Name: "a", Type: model.TypeInt}}}, "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("EVOLVE e2 v%d->v%d compatible=%v decision=%s rules=%s",
		rep.FromVersion, rep.ToVersion, rep.Compatible, rec.Decision, rec.RuleID)
	for _, f := range rep.Findings {
		t.Logf("  rule=%s sev=%s field=%q %s", f.RuleID, f.Severity, f.Field, f.Message)
	}
	if rec.Decision != model.RejectSchemaIncompat {
		t.Fatalf("expected schema rejection, got %s", rec.Decision)
	}
	got, _ := c.GetEntity("e2")
	if got.Schema.Version != 1 {
		t.Fatalf("schema must remain v1 after rejection, got %d", got.Schema.Version)
	}
	// Repro: same inputs same report rule ids.
	rep2, _, _ := c.EvolveSchema("e2", model.Schema{Fields: []model.Field{{Name: "a", Type: model.TypeInt}}}, "s")
	if len(rep.Findings) != len(rep2.Findings) || rep.Findings[0].RuleID != rep2.Findings[0].RuleID {
		t.Fatal("compatibility conclusion not reproducible")
	}
	t.Logf("REPRO rule id stable: %s", rep2.Findings[0].RuleID)
}

func TestLineageCycleRejectedAndDanglingKept(t *testing.T) {
	c := newTestCatalog(t)
	for _, id := range []string{"a", "b"} {
		if r, _ := c.IngestEntity(ent(id, 1, "s")); !r.Accepted {
			t.Fatal("seed entity " + id)
		}
	}
	r1, _ := c.IngestRelationship(model.Relationship{Upstream: "a", Downstream: "b", Kind: model.RelDerivedFrom, Version: 1})
	t.Logf("REL a->b -> %s (%s)", r1.Decision, r1.Reason)
	if !r1.Accepted {
		t.Fatal("a->b should be accepted")
	}
	// Dangling edge accepted.
	rd, _ := c.IngestRelationship(model.Relationship{Upstream: "b", Downstream: "ghost", Kind: model.RelDerivedFrom, Version: 1})
	t.Logf("REL b->ghost -> %s (%s) dangling=%d", rd.Decision, rd.Reason, len(c.DanglingEdges()))
	if !rd.Accepted {
		t.Fatal("dangling edges must be accepted and flagged")
	}
	if len(c.DanglingEdges()) != 1 {
		t.Fatal("expected exactly 1 dangling edge")
	}
	// Cycle rejected.
	rc, _ := c.IngestRelationship(model.Relationship{Upstream: "b", Downstream: "a", Kind: model.RelDerivedFrom, Version: 1})
	t.Logf("REL b->a cycle -> %s (%s)", rc.Decision, rc.Reason)
	if rc.Decision != model.RejectCycle {
		t.Fatalf("expected cycle rejection, got %s", rc.Decision)
	}
}

func TestMergeConflictAndApply(t *testing.T) {
	c := newTestCatalog(t)
	s := ent("s1", 1, "s")
	s.TypeName, s.Owner = "table", "alice"
	v := ent("s2", 1, "s")
	v.TypeName, v.Owner = "topic", "bob"
	c.IngestEntity(s)
	c.IngestEntity(v)

	// Incompatible merge (type + owner conflict).
	plan, rec, err := c.ApplyMerge(merge.Request{SurvivorID: "s1", VictimID: "s2", Source: "s"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MERGE s1<-s2 decision=%s conflicts=%d", rec.Decision, len(plan.Conflicts))
	for _, cf := range plan.Conflicts {
		t.Logf("  rule=%s field=%q values=%v", cf.RuleID, cf.Field, cf.Values)
	}
	if rec.Decision != model.RejectMergeConflict {
		t.Fatalf("expected merge conflict, got %s", rec.Decision)
	}
	if _, err := c.GetEntity("s2"); err != nil {
		t.Fatal("victim must still exist after rejected merge")
	}

	// Compatible merge: use a fresh victim with same type and owner.
	s3 := ent("s3", 1, "s")
	s3.TypeName, s3.Owner = "table", "alice"
	s3.Tags = []string{"x"}
	c.IngestEntity(s3)
	plan2, rec2, err := c.ApplyMerge(merge.Request{SurvivorID: "s1", VictimID: "s3", Source: "s"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MERGE compatible=%v decision=%s version=%d redirects=%d",
		plan2.Compatible, rec2.Decision, rec2.Version, len(plan2.Redirects))
	if !rec2.Accepted {
		t.Fatalf("merge should apply: %s", rec2.Reason)
	}
	if _, err := c.GetEntity("s3"); err == nil {
		t.Fatal("victim must be gone after merge")
	}
}

func TestImpactFiltering(t *testing.T) {
	c := newTestCatalog(t)
	a := ent("a", 1, "s")
	a.Owner, a.Tags = "alice", []string{"pii"}
	b := ent("b", 1, "s")
	b.Owner = "bob"
	c.IngestEntity(a)
	c.IngestEntity(b)
	c.IngestRelationship(model.Relationship{Upstream: "a", Downstream: "b", Kind: model.RelProduces, Version: 1})

	paths, matched, err := c.Impact("a", graph.Filter{Owner: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("IMPACT a owner=bob paths=%d matched=%v", len(paths), matched)
	if len(matched) != 1 || matched[0] != "b" {
		t.Fatalf("filter by owner failed: %v", matched)
	}
}

func TestJournalReplayRestoresState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")

	c1, err := Open(Config{JournalPath: path, Clock: testClock()})
	if err != nil {
		t.Fatal(err)
	}
	e := ent("r1", 1, "s", model.Field{Name: "a", Type: model.TypeString})
	rec, _ := c1.IngestEntity(e)
	t.Logf("session1 ingest r1 v1 seq=%d decision=%s", rec.Seq, rec.Decision)
	c1.IngestEntity(ent("r1", 1, "s", model.Field{Name: "a", Type: model.TypeString})) // duplicate
	c1.IngestRelationship(model.Relationship{Upstream: "r1", Downstream: "missing", Kind: model.RelDependsOn, Version: 1})
	c1.Close()

	c2, err := Open(Config{JournalPath: path, Clock: testClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	got, err := c2.GetEntity("r1")
	if err != nil {
		t.Fatalf("entity not restored: %v", err)
	}
	t.Logf("session2 restored r1 version=%d dangling=%d decisions=%d",
		got.Version, len(c2.DanglingEdges()), len(c2.History(0)))
	if got.Version != 1 {
		t.Fatalf("restored version=%d", got.Version)
	}
	if len(c2.DanglingEdges()) != 1 {
		t.Fatal("dangling edge must survive replay")
	}
	if len(c2.History(0)) != 3 {
		t.Fatalf("decisions restored=%d want 3", len(c2.History(0)))
	}
	// Duplicate still recognized after replay.
	rDup, _ := c2.IngestEntity(e)
	t.Logf("session2 duplicate check -> %s", rDup.Decision)
	if rDup.Decision != model.DecisionDuplicate {
		t.Fatalf("idempotency lost across restart: %s", rDup.Decision)
	}
}
