// Package merge plans entity merges as pure functions: it never mutates
// catalog state, so merge conclusions are deterministic and reproducible.
//
// Conflict rule catalog (fixed identifiers):
//
//	M001 type name differs between participants
//	M002 same-named schema field has conflicting definition
//	M003 same-named attribute has conflicting value
//	M004 both participants declare different non-empty owners
package merge

import (
	"sort"

	"github.com/lisama569/metadata-catalog-go/internal/model"
)

// RuleVersion identifies the merge rule set for reproducibility claims.
const RuleVersion = "merge-rules-v1"

// ConflictKind enumerates merge conflict categories.
type ConflictKind string

const (
	ConflictSchema ConflictKind = "schema"
	ConflictAttr   ConflictKind = "attr"
	ConflictOwner  ConflictKind = "owner"
)

// Conflict is one irreconcilable difference between merge participants.
type Conflict struct {
	RuleID string            `json:"ruleId"`
	Kind   ConflictKind      `json:"kind"`
	Field  string            `json:"field"`
	Values map[string]string `json:"values"`
	Reason string            `json:"reason"`
}

// Request identifies the survivor and victim of a merge.
type Request struct {
	SurvivorID string `json:"survivorId"`
	VictimID   string `json:"victimId"`
	Source     string `json:"source"`
}

// EdgeRedirect is a lineage edge that must be repointed at the survivor.
type EdgeRedirect struct {
	RelKey     string `json:"relKey"`
	Upstream   string `json:"upstream"`
	Downstream string `json:"downstream"`
	Reason     string `json:"reason,omitempty"`
}

// Plan is the deterministic output of planning a merge.
type Plan struct {
	Request     Request        `json:"request"`
	RuleVersion string         `json:"ruleVersion"`
	Compatible  bool           `json:"compatible"`
	Merged      model.Entity   `json:"merged"`
	Conflicts   []Conflict     `json:"conflicts"`
	Redirects   []EdgeRedirect `json:"redirects"`
}

// PlanMerge merges victim into survivor and returns a stable plan. It never
// mutates its inputs. Conflicts make the plan incompatible; the catalog must
// reject such merges. Redirects are filled in by the catalog, which owns the
// lineage graph.
func PlanMerge(survivor, victim model.Entity, source string) Plan {
	plan := Plan{
		Request:     Request{SurvivorID: survivor.ID, VictimID: victim.ID, Source: source},
		RuleVersion: RuleVersion,
	}
	conflicts := []Conflict{}

	merged := model.Entity{
		ID:       survivor.ID,
		Version:  maxInt(survivor.Version, victim.Version) + 1,
		TypeName: survivor.TypeName,
		Source:   source,
	}

	// M001: differing entity types.
	if survivor.TypeName != "" && victim.TypeName != "" && survivor.TypeName != victim.TypeName {
		conflicts = append(conflicts, Conflict{
			RuleID: "M001", Kind: ConflictSchema, Field: "typeName",
			Values: map[string]string{survivor.ID: survivor.TypeName, victim.ID: victim.TypeName},
			Reason: "entities have different types and cannot be merged",
		})
	}

	// Schema fields: union by name with M002 definition conflicts.
	merged.Schema.Version = maxInt(survivor.Schema.Version, victim.Schema.Version) + 1
	fields := make(map[string]model.Field)
	order := []string{}
	addField := func(e model.Entity, f model.Field) {
		if existing, ok := fields[f.Name]; ok {
			if existing.Type != f.Type || existing.Required != f.Required || existing.RenameOf != f.RenameOf {
				conflicts = append(conflicts, Conflict{
					RuleID: "M002", Kind: ConflictSchema, Field: f.Name,
					Values: map[string]string{
						e.ID:                         fieldSig(f),
						otherID(e, survivor, victim): fieldSig(existing),
					},
					Reason: "same-named field has conflicting definitions",
				})
			}
			return
		}
		fields[f.Name] = f
		order = append(order, f.Name)
	}
	for _, f := range survivor.Schema.Fields {
		addField(survivor, f)
	}
	for _, f := range victim.Schema.Fields {
		addField(victim, f)
	}
	sort.Strings(order)
	for _, name := range order {
		merged.Schema.Fields = append(merged.Schema.Fields, fields[name])
	}

	// Attributes: union with M003 value conflicts.
	merged.Attrs = map[string]string{}
	attrKeys := map[string]struct{}{}
	for k := range survivor.Attrs {
		attrKeys[k] = struct{}{}
	}
	for k := range victim.Attrs {
		attrKeys[k] = struct{}{}
	}
	keys := make([]string, 0, len(attrKeys))
	for k := range attrKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sv, sok := survivor.Attrs[k]
		vv, vok := victim.Attrs[k]
		switch {
		case sok && vok && sv != vv:
			conflicts = append(conflicts, Conflict{
				RuleID: "M003", Kind: ConflictAttr, Field: k,
				Values: map[string]string{survivor.ID: sv, victim.ID: vv},
				Reason: "attribute values conflict",
			})
		case sok:
			merged.Attrs[k] = sv
		default:
			merged.Attrs[k] = vv
		}
	}

	// Owner: non-empty wins; two different non-empty owners are M004.
	switch {
	case survivor.Owner == victim.Owner:
		merged.Owner = survivor.Owner
	case survivor.Owner == "":
		merged.Owner = victim.Owner
	case victim.Owner == "":
		merged.Owner = survivor.Owner
	default:
		merged.Owner = survivor.Owner
		conflicts = append(conflicts, Conflict{
			RuleID: "M004", Kind: ConflictOwner, Field: "owner",
			Values: map[string]string{survivor.ID: survivor.Owner, victim.ID: victim.Owner},
			Reason: "different non-empty owners",
		})
	}

	merged.Tags = unionSorted(survivor.Tags, victim.Tags)
	merged.Terms = unionSorted(survivor.Terms, victim.Terms)

	// Deterministic conflict order.
	sort.SliceStable(conflicts, func(i, j int) bool {
		if conflicts[i].Field != conflicts[j].Field {
			return conflicts[i].Field < conflicts[j].Field
		}
		return conflicts[i].RuleID < conflicts[j].RuleID
	})
	plan.Merged = merged
	plan.Conflicts = conflicts
	plan.Compatible = len(conflicts) == 0
	return plan
}

func fieldSig(f model.Field) string {
	req := "opt"
	if f.Required {
		req = "req"
	}
	s := string(f.Type) + "/" + req
	if f.RenameOf != "" {
		s += "/renameOf:" + f.RenameOf
	}
	return s
}

func otherID(current model.Entity, a, b model.Entity) string {
	if current.ID == a.ID {
		return b.ID
	}
	return a.ID
}

func unionSorted(a, b []string) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for _, v := range a {
		set[v] = struct{}{}
	}
	for _, v := range b {
		set[v] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
