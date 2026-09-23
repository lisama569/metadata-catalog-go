// Package compat evaluates schema evolution compatibility against a fixed,
// versioned set of rules. Rule identifiers are stable so historical decisions
// remain reproducible as long as the rules are unchanged.
//
// Rule catalog (fixed identifiers):
//
//	E001 version must advance by exactly one
//	E002 field removed (incompatible; downgraded to warning when removals allowed)
//	E003 field added (optional=info, required=warning)
//	E004 field rename (info; target missing=warning; type mismatch=incompatible)
//	E005 field type changed (incompatible; int->float widening=warning)
//	E006 required flag changed (optional->required=incompatible; reverse=info)
//	E007 duplicate field name or rename target (incompatible)
package compat

import (
	"sort"

	"github.com/lisama569/metadata-catalog-go/internal/model"
)

// Rule version: bumping this invalidates "same rules" reproducibility claims.
const RuleVersion = "compat-rules-v1"

// Severity of a compatibility finding.
type Severity string

const (
	SevIncompatible Severity = "incompatible"
	SevWarning      Severity = "warning"
	SevInfo         Severity = "info"
)

// Finding describes one rule evaluation against an evolution.
type Finding struct {
	RuleID   string         `json:"ruleId"`
	Severity Severity       `json:"severity"`
	Field    string         `json:"field,omitempty"`
	Message  string         `json:"message"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// Options controls compatibility evaluation.
type Options struct {
	// AllowFieldRemoval turns E002 from incompatible into a warning.
	AllowFieldRemoval bool
}

// Report is the deterministic result of comparing two schema versions.
type Report struct {
	Compatible  bool      `json:"compatible"`
	RuleVersion string    `json:"ruleVersion"`
	FromVersion int       `json:"fromVersion"`
	ToVersion   int       `json:"toVersion"`
	Findings    []Finding `json:"findings"`
}

// CheckSchemas evaluates the evolution from -> to under opts. Conclusions are
// deterministic: identical inputs always yield an identical ordered report.
func CheckSchemas(from, to model.Schema, opts Options) Report {
	rep := Report{
		RuleVersion: RuleVersion,
		FromVersion: from.Version,
		ToVersion:   to.Version,
	}

	// E001: version must advance by exactly one.
	if to.Version != from.Version+1 {
		rep.Findings = append(rep.Findings, Finding{
			RuleID:   "E001",
			Severity: SevIncompatible,
			Message:  "schema version must advance by exactly one",
			Detail:   map[string]any{"from": from.Version, "to": to.Version},
		})
	}

	toByName := make(map[string]model.Field, len(to.Fields))
	renameSources := make(map[string][]model.Field) // old name -> new field(s)
	dupTo := make(map[string]bool)
	for _, f := range to.Fields {
		if _, ok := toByName[f.Name]; ok && !dupTo[f.Name] {
			dupTo[f.Name] = true
			rep.Findings = append(rep.Findings, Finding{
				RuleID: "E007", Severity: SevIncompatible, Field: f.Name,
				Message: "duplicate field name in new schema",
				Detail:  map[string]any{"name": f.Name},
			})
		}
		toByName[f.Name] = f
		if f.RenameOf != "" {
			renameSources[f.RenameOf] = append(renameSources[f.RenameOf], f)
		}
	}

	consumed := make(map[string]bool) // new field names already accounted for

	for _, old := range from.Fields {
		// Duplicate rename target: several new fields claim the same old name.
		if claims := renameSources[old.Name]; len(claims) > 1 {
			names := make([]string, 0, len(claims))
			for _, c := range claims {
				names = append(names, c.Name)
			}
			sort.Strings(names)
			rep.Findings = append(rep.Findings, Finding{
				RuleID: "E007", Severity: SevIncompatible, Field: old.Name,
				Message: "multiple fields claim to rename the same source field",
				Detail:  map[string]any{"source": old.Name, "claimants": names},
			})
		}

		if newF, ok := toByName[old.Name]; ok {
			consumed[old.Name] = true
			compareField("E005", "E006", old, newF, &rep)
			continue
		}

		claims := renameSources[old.Name]
		if len(claims) == 1 {
			newF := claims[0]
			consumed[newF.Name] = true
			rep.Findings = append(rep.Findings, Finding{
				RuleID: "E004", Severity: SevInfo, Field: newF.Name,
				Message: "field renamed",
				Detail:  map[string]any{"from": old.Name, "to": newF.Name},
			})
			compareField("E004", "E006", old, newF, &rep)
			continue
		}

		// No same-name successor and no rename claim -> removal.
		sev := SevIncompatible
		if opts.AllowFieldRemoval {
			sev = SevWarning
		}
		rep.Findings = append(rep.Findings, Finding{
			RuleID: "E002", Severity: sev, Field: old.Name,
			Message: "field removed without a rename declaration",
			Detail:  map[string]any{"name": old.Name, "allowRemoval": opts.AllowFieldRemoval},
		})
	}

	// Remaining new fields are additions (sorted by name for stability).
	added := make([]model.Field, 0)
	for _, f := range to.Fields {
		if !consumed[f.Name] {
			added = append(added, f)
		}
	}
	sort.Slice(added, func(i, j int) bool { return added[i].Name < added[j].Name })
	for _, f := range added {
		sev := SevInfo
		msg := "optional field added"
		if f.Required {
			sev = SevWarning
			msg = "required field added; producers may not supply it"
		}
		rep.Findings = append(rep.Findings, Finding{
			RuleID: "E003", Severity: sev, Field: f.Name, Message: msg,
			Detail: map[string]any{"name": f.Name, "required": f.Required, "type": string(f.Type)},
		})
	}

	// Stable order: field name, then rule id.
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		if rep.Findings[i].Field != rep.Findings[j].Field {
			return rep.Findings[i].Field < rep.Findings[j].Field
		}
		return rep.Findings[i].RuleID < rep.Findings[j].RuleID
	})

	rep.Compatible = true
	for _, f := range rep.Findings {
		if f.Severity == SevIncompatible {
			rep.Compatible = false
			break
		}
	}
	return rep
}

// compareField emits type-change and required-change findings. typeRule is
// E005 for plain changes and E004 for rename-driven type mismatches.
func compareField(typeRule string, _ string, old, newF model.Field, rep *Report) {
	if old.Type != newF.Type {
		// Allowed widening: int -> float.
		if old.Type == model.TypeInt && newF.Type == model.TypeFloat {
			rep.Findings = append(rep.Findings, Finding{
				RuleID: typeRule, Severity: SevWarning, Field: newF.Name,
				Message: "type widened from int to float",
				Detail:  map[string]any{"from": string(old.Type), "to": string(newF.Type)},
			})
		} else {
			rep.Findings = append(rep.Findings, Finding{
				RuleID: typeRule, Severity: SevIncompatible, Field: newF.Name,
				Message: "incompatible field type change",
				Detail:  map[string]any{"from": string(old.Type), "to": string(newF.Type)},
			})
		}
	}
	if !old.Required && newF.Required {
		rep.Findings = append(rep.Findings, Finding{
			RuleID: "E006", Severity: SevIncompatible, Field: newF.Name,
			Message: "optional field became required; existing data may lack it",
		})
	} else if old.Required && !newF.Required {
		rep.Findings = append(rep.Findings, Finding{
			RuleID: "E006", Severity: SevInfo, Field: newF.Name,
			Message: "required field relaxed to optional",
		})
	}
}
