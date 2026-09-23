// Package catalog ties ingestion, version control, schema evolution,
// lineage graph maintenance and durable decisions together.
package catalog

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/lisama569/metadata-catalog-go/internal/compat"
	"github.com/lisama569/metadata-catalog-go/internal/graph"
	"github.com/lisama569/metadata-catalog-go/internal/merge"
	"github.com/lisama569/metadata-catalog-go/internal/model"
	"github.com/lisama569/metadata-catalog-go/internal/store"
)

// ErrNotFound is returned for unknown subjects.
var ErrNotFound = errors.New("not found")

// Config controls catalog behavior.
type Config struct {
	JournalPath       string
	AllowFieldRemoval bool
	// Clock defaults to time.Now; injectable for reproducible tests.
	Clock func() time.Time
}

// Catalog is the concurrent-safe metadata catalog.
type Catalog struct {
	mu        sync.RWMutex
	store     *store.Store
	graph     *graph.Graph
	compatOpt compat.Options
	clock     func() time.Time

	entities  map[string]model.Entity
	entFP     map[string]string
	rels      map[string]model.Relationship
	relFP     map[string]string
	decisions []model.DecisionRecord
}

// Open opens the catalog and replays the journal to rebuild state.
func Open(cfg Config) (*Catalog, error) {
	st, err := store.Open(cfg.JournalPath)
	if err != nil {
		return nil, err
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	c := &Catalog{
		store:     st,
		graph:     graph.NewGraph(),
		compatOpt: compat.Options{AllowFieldRemoval: cfg.AllowFieldRemoval},
		clock:     clock,
		entities:  map[string]model.Entity{},
		entFP:     map[string]string{},
		rels:      map[string]model.Relationship{},
		relFP:     map[string]string{},
	}
	if err := c.replay(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Catalog) replay() error {
	return c.store.Replay(func(e store.Entry) error {
		switch e.Type {
		case store.EntryDecision:
			if e.Decision != nil {
				c.decisions = append(c.decisions, *e.Decision)
			}
		case store.EntryEntity:
			c.applyEntity(*e.Entity)
		case store.EntryRel:
			c.graph.UpsertEdge(*e.Rel)
			c.rels[e.Rel.Key] = *e.Rel
			c.relFP[e.Rel.Key] = model.RelFingerprint(*e.Rel)
		case store.EntryMerge:
			m := e.Merge
			c.applyEntity(m.Survivor)
			delete(c.entities, m.VictimID)
			delete(c.entFP, m.VictimID)
			c.graph.RedirectEdges(m.VictimID, m.SurvivorID)
		}
		return nil
	})
}

func (c *Catalog) applyEntity(e model.Entity) {
	c.entities[e.ID] = e
	c.entFP[e.ID] = model.EntityFingerprint(e)
	c.graph.UpsertNode(graph.Node{
		ID: e.ID, Owner: e.Owner, Version: e.Version,
		Tags:  append([]string(nil), e.Tags...),
		Terms: append([]string(nil), e.Terms...),
	})
}

// Close flushes the journal.
func (c *Catalog) Close() error { return c.store.Close() }

// persistDecision appends a decision entry while holding the write lock.
func (c *Catalog) persistDecision(rec *model.DecisionRecord) error {
	rec.Seq = 0 // assigned by store
	seq, err := c.store.Append(&store.Entry{Type: store.EntryDecision, Decision: rec})
	if err != nil {
		return err
	}
	rec.Seq = seq
	c.decisions = append(c.decisions, *rec)
	return nil
}

// IngestEntity accepts or rejects an entity submission under version CAS and
// schema compatibility rules. The returned record is always durable.
func (c *Catalog) IngestEntity(e model.Entity) (model.DecisionRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ingestEntityLocked(e)
}

func (c *Catalog) ingestEntityLocked(e model.Entity) (model.DecisionRecord, error) {
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = c.clock()
	}
	fp := model.EntityFingerprint(e)
	rec := model.DecisionRecord{
		Op: model.OpIngestEntity, Subject: e.ID, Source: e.Source,
		Version: e.Version, Time: c.clock(), Submitted: e,
	}

	if verrs := validateEntity(e); len(verrs) > 0 {
		rec.Decision = model.RejectValidation
		rec.Reason = "entity failed validation"
		rec.Detail = map[string]any{"errors": verrs}
		return rec, c.persistDecision(&rec)
	}

	existing, ok := c.entities[e.ID]
	if !ok {
		// First sighting: accept at whatever version the source provides.
		rec.Decision, rec.Accepted, rec.Reason = model.DecisionAccepted, true, "first version accepted"
		return rec, c.commitEntity(&rec, e)
	}

	switch {
	case e.Version < existing.Version:
		rec.Decision = model.RejectStaleVersion
		rec.Reason = "submitted version is older than current version"
		rec.Detail = map[string]any{"submitted": e.Version, "current": existing.Version}
	case e.Version > existing.Version+1:
		rec.Decision = model.RejectOutOfOrderGap
		rec.Reason = "version gap: missing intervening versions"
		rec.Detail = map[string]any{"submitted": e.Version, "current": existing.Version}
	case e.Version == existing.Version && fp == c.entFP[e.ID]:
		rec.Decision, rec.Accepted, rec.Reason = model.DecisionDuplicate, true, "identical submission already applied"
	case e.Version == existing.Version:
		rec.Decision = model.RejectContentConflict
		rec.Reason = "same version submitted with different content"
		rec.Detail = map[string]any{
			"submittedFingerprint": fp, "storedFingerprint": c.entFP[e.ID],
		}
	default: // version == existing.Version+1
		if model.SchemaFingerprint(e.Schema) != model.SchemaFingerprint(existing.Schema) {
			rep := compat.CheckSchemas(existing.Schema, e.Schema, c.compatOpt)
			if !rep.Compatible {
				rec.Decision = model.RejectSchemaIncompat
				rec.Reason = "schema evolution is incompatible"
				rec.RuleID = compat.RuleVersion
				rec.Detail = map[string]any{"report": rep}
				return rec, c.persistDecision(&rec)
			}
		}
		rec.Decision, rec.Accepted, rec.Reason = model.DecisionAccepted, true, "newer version accepted"
		return rec, c.commitEntity(&rec, e)
	}
	return rec, c.persistDecision(&rec)
}

func (c *Catalog) commitEntity(rec *model.DecisionRecord, e model.Entity) error {
	rec.Accepted = true
	if err := c.persistDecision(rec); err != nil {
		return err
	}
	if _, err := c.store.Append(&store.Entry{Type: store.EntryEntity, Entity: &e}); err != nil {
		return err
	}
	c.applyEntity(e)
	return nil
}

// IngestRelationship accepts or rejects a relationship submission. Cyclic
// edges are rejected with a distinct durable reason; dangling edges are
// accepted and flagged (never dropped).
func (c *Catalog) IngestRelationship(r model.Relationship) (model.DecisionRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = c.clock()
	}
	if r.Key == "" {
		r.Key = r.Upstream + "|" + string(r.Kind) + "|" + r.Downstream
	}
	fp := model.RelFingerprint(r)
	rec := model.DecisionRecord{
		Op: model.OpIngestRel, Subject: r.Key, Source: r.Source,
		Version: r.Version, Time: c.clock(), Submitted: r,
	}

	if verrs := validateRel(r); len(verrs) > 0 {
		rec.Decision = model.RejectValidation
		rec.Reason = "relationship failed validation"
		rec.Detail = map[string]any{"errors": verrs}
		return rec, c.persistDecision(&rec)
	}
	if c.graph.IsCycle(r.Upstream, r.Downstream) {
		rec.Decision = model.RejectCycle
		rec.Reason = "relationship would create a directed cycle"
		rec.Detail = map[string]any{"upstream": r.Upstream, "downstream": r.Downstream}
		return rec, c.persistDecision(&rec)
	}

	existing, ok := c.rels[r.Key]
	if !ok {
		rec.Decision, rec.Accepted, rec.Reason = model.DecisionAccepted, true, "first version accepted"
		return rec, c.commitRel(&rec, r)
	}
	switch {
	case r.Version < existing.Version:
		rec.Decision = model.RejectStaleVersion
		rec.Reason = "submitted version is older than current version"
		rec.Detail = map[string]any{"submitted": r.Version, "current": existing.Version}
	case r.Version > existing.Version+1:
		rec.Decision = model.RejectOutOfOrderGap
		rec.Reason = "version gap: missing intervening versions"
		rec.Detail = map[string]any{"submitted": r.Version, "current": existing.Version}
	case r.Version == existing.Version && fp == c.relFP[r.Key]:
		rec.Decision, rec.Accepted, rec.Reason = model.DecisionDuplicate, true, "identical submission already applied"
	default:
		if r.Version == existing.Version {
			rec.Decision = model.RejectContentConflict
			rec.Reason = "same version submitted with different content"
			rec.Detail = map[string]any{
				"submittedFingerprint": fp, "storedFingerprint": c.relFP[r.Key],
			}
		} else {
			rec.Decision, rec.Accepted, rec.Reason = model.DecisionAccepted, true, "newer version accepted"
			return rec, c.commitRel(&rec, r)
		}
	}
	return rec, c.persistDecision(&rec)
}

func (c *Catalog) commitRel(rec *model.DecisionRecord, r model.Relationship) error {
	rec.Accepted = true
	rec.Decision = model.DecisionAccepted
	if err := c.persistDecision(rec); err != nil {
		return err
	}
	if _, err := c.store.Append(&store.Entry{Type: store.EntryRel, Rel: &r}); err != nil {
		return err
	}
	c.graph.UpsertEdge(r)
	c.rels[r.Key] = r
	c.relFP[r.Key] = model.RelFingerprint(r)
	return nil
}

// EvolveSchema builds a candidate entity with the given schema version and
// runs it through the normal ingestion path, additionally returning the
// compatibility report (even when rejected).
func (c *Catalog) EvolveSchema(id string, schema model.Schema, source string) (compat.Report, model.DecisionRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	existing, ok := c.entities[id]
	if !ok {
		rep := compat.Report{Compatible: false, RuleVersion: compat.RuleVersion}
		rec := model.DecisionRecord{
			Op: model.OpEvolve, Subject: id, Source: source, Decision: model.RejectValidation,
			Reason: "unknown entity; ingest it before evolving its schema",
			Time:   c.clock(),
		}
		return rep, rec, c.persistDecision(&rec)
	}
	schema.Version = existing.Schema.Version + 1
	rep := compat.CheckSchemas(existing.Schema, schema, c.compatOpt)
	candidate := existing
	candidate.Schema = schema
	candidate.Version = existing.Version + 1
	candidate.Source = source
	candidate.UpdatedAt = c.clock()
	rec, err := c.ingestEntityLocked(candidate)
	return rep, rec, err
}

// ApplyMerge plans and, if compatible, applies an entity merge.
func (c *Catalog) ApplyMerge(req merge.Request) (merge.Plan, model.DecisionRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rec := model.DecisionRecord{
		Op: model.OpMerge, Subject: req.SurvivorID, Source: req.Source,
		Time: c.clock(), Submitted: req,
	}
	if req.SurvivorID == "" || req.VictimID == "" {
		rec.Decision = model.RejectValidation
		rec.Reason = "survivorId and victimId are required"
		return merge.Plan{Request: req}, rec, c.persistDecision(&rec)
	}
	if req.SurvivorID == req.VictimID {
		rec.Decision = model.RejectValidation
		rec.Reason = "cannot merge an entity into itself"
		return merge.Plan{Request: req}, rec, c.persistDecision(&rec)
	}
	survivor, sok := c.entities[req.SurvivorID]
	victim, vok := c.entities[req.VictimID]
	if !sok || !vok {
		rec.Decision = model.RejectValidation
		rec.Reason = "both merge participants must exist"
		rec.Detail = map[string]any{"survivorExists": sok, "victimExists": vok}
		return merge.Plan{Request: req}, rec, c.persistDecision(&rec)
	}

	plan := merge.PlanMerge(survivor, victim, req.Source)
	if !plan.Compatible {
		rec.Decision = model.RejectMergeConflict
		rec.RuleID = merge.RuleVersion
		rec.Reason = "merge conflicts detected"
		rec.Detail = map[string]any{"conflicts": plan.Conflicts}
		return plan, rec, c.persistDecision(&rec)
	}

	redirects := c.graph.RedirectEdges(req.VictimID, req.SurvivorID)
	plan.Redirects = make([]merge.EdgeRedirect, 0, len(redirects))
	storedRedirects := make([]store.RelRedirect, 0, len(redirects))
	// Update in-memory rel projections to match redirected endpoints.
	for _, rd := range redirects {
		plan.Redirects = append(plan.Redirects, merge.EdgeRedirect{
			RelKey: rd.RelKey, Upstream: rd.Upstream, Downstream: rd.Downstream, Reason: rd.Reason,
		})
		storedRedirects = append(storedRedirects, store.RelRedirect{
			RelKey: rd.RelKey, Upstream: rd.Upstream, Downstream: rd.Downstream,
		})
		if rel, ok := c.rels[rd.RelKey]; ok {
			rel.Upstream, rel.Downstream = rd.Upstream, rd.Downstream
			c.rels[rd.RelKey] = rel
			c.relFP[rd.RelKey] = model.RelFingerprint(rel)
		}
	}

	rec.Decision, rec.Accepted, rec.Reason = model.DecisionAccepted, true, "merge applied"
	rec.Version = plan.Merged.Version
	rec.Detail = map[string]any{"redirects": len(redirects)}
	if err := c.persistDecision(&rec); err != nil {
		return plan, rec, err
	}

	merged := plan.Merged
	merged.UpdatedAt = c.clock()
	me := &store.MergeEntry{
		SurvivorID: req.SurvivorID, VictimID: req.VictimID,
		Survivor: merged, Redirects: storedRedirects, Source: req.Source,
	}
	if _, err := c.store.Append(&store.Entry{Type: store.EntryMerge, Merge: me}); err != nil {
		return plan, rec, err
	}
	c.applyEntity(merged)
	delete(c.entities, req.VictimID)
	delete(c.entFP, req.VictimID)
	return plan, rec, nil
}

// Impact runs a filtered downstream impact analysis on a consistent snapshot.
func (c *Catalog) Impact(id string, f graph.Filter) ([]graph.Path, []string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, ok := c.entities[id]; !ok {
		return nil, nil, ErrNotFound
	}
	paths, matched, err := c.graph.Impact(id, f)
	return paths, matched, err
}

// Lineage runs a filtered upstream lineage analysis.
func (c *Catalog) Lineage(id string, f graph.Filter) ([]graph.Path, []string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, ok := c.entities[id]; !ok {
		return nil, nil, ErrNotFound
	}
	return c.graph.Lineage(id, f)
}

// GetEntity returns one entity.
func (c *Catalog) GetEntity(id string) (model.Entity, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entities[id]
	if !ok {
		return model.Entity{}, ErrNotFound
	}
	return e, nil
}

// SnapshotGraph returns a consistent graph snapshot.
func (c *Catalog) SnapshotGraph() graph.Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.graph.Snapshot()
}

// DanglingEdges returns currently dangling relationship edges.
func (c *Catalog) DanglingEdges() []graph.Edge {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.graph.DanglingEdges()
}

// History returns decision records in durable sequence order.
func (c *Catalog) History(limit int) []model.DecisionRecord {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := append([]model.DecisionRecord(nil), c.decisions...)
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}
