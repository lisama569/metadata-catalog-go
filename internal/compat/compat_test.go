package compat

import (
	"testing"

	"github.com/lisama569/metadata-catalog-go/internal/model"
)

func field(name string, t model.FieldType, req bool) model.Field {
	return model.Field{Name: name, Type: t, Required: req}
}

func ruleIDs(rep Report) map[string]Severity {
	m := map[string]Severity{}
	for _, f := range rep.Findings {
		m[f.RuleID] = f.Severity
	}
	return m
}

func TestCheckSchemas(t *testing.T) {
	cases := []struct {
		name         string
		from, to     model.Schema
		allowRemoval bool
		wantCompat   bool
		wantRule     string
	}{
		{
			name:       "add optional field compatible",
			from:       model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeString, true)}},
			to:         model.Schema{Version: 2, Fields: []model.Field{field("a", model.TypeString, true), field("b", model.TypeInt, false)}},
			wantCompat: true, wantRule: "E003",
		},
		{
			name:       "add required field warning but compatible",
			from:       model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeString, false)}},
			to:         model.Schema{Version: 2, Fields: []model.Field{field("a", model.TypeString, false), field("b", model.TypeInt, true)}},
			wantCompat: true, wantRule: "E003",
		},
		{
			name:       "remove field incompatible by default",
			from:       model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeString, false), field("b", model.TypeInt, false)}},
			to:         model.Schema{Version: 2, Fields: []model.Field{field("a", model.TypeString, false)}},
			wantCompat: false, wantRule: "E002",
		},
		{
			name:         "remove field warning when allowed",
			from:         model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeString, false), field("b", model.TypeInt, false)}},
			to:           model.Schema{Version: 2, Fields: []model.Field{field("a", model.TypeString, false)}},
			allowRemoval: true,
			wantCompat:   true, wantRule: "E002",
		},
		{
			name:       "rename same type info compatible",
			from:       model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeString, false)}},
			to:         model.Schema{Version: 2, Fields: []model.Field{{Name: "a2", Type: model.TypeString, Required: false, RenameOf: "a"}}},
			wantCompat: true, wantRule: "E004",
		},
		{
			name:       "rename with type mismatch incompatible",
			from:       model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeString, false)}},
			to:         model.Schema{Version: 2, Fields: []model.Field{{Name: "a2", Type: model.TypeInt, Required: false, RenameOf: "a"}}},
			wantCompat: false, wantRule: "E004",
		},
		{
			name:       "type change string to int incompatible",
			from:       model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeString, false)}},
			to:         model.Schema{Version: 2, Fields: []model.Field{field("a", model.TypeInt, false)}},
			wantCompat: false, wantRule: "E005",
		},
		{
			name:       "int to float widening compatible warning",
			from:       model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeInt, false)}},
			to:         model.Schema{Version: 2, Fields: []model.Field{field("a", model.TypeFloat, false)}},
			wantCompat: true, wantRule: "E005",
		},
		{
			name:       "optional to required incompatible",
			from:       model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeString, false)}},
			to:         model.Schema{Version: 2, Fields: []model.Field{field("a", model.TypeString, true)}},
			wantCompat: false, wantRule: "E006",
		},
		{
			name:       "version jump rejected E001",
			from:       model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeString, false)}},
			to:         model.Schema{Version: 5, Fields: []model.Field{field("a", model.TypeString, false)}},
			wantCompat: false, wantRule: "E001",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{AllowFieldRemoval: tc.allowRemoval}
			rep := CheckSchemas(tc.from, tc.to, opts)

			t.Logf("INPUT from=v%d to=v%d fields %d->%d allowRemoval=%v",
				tc.from.Version, tc.to.Version, len(tc.from.Fields), len(tc.to.Fields), tc.allowRemoval)
			for _, f := range rep.Findings {
				t.Logf("FINDING rule=%s severity=%s field=%q msg=%q", f.RuleID, f.Severity, f.Field, f.Message)
			}
			t.Logf("VERDICT compatible=%v (want %v) ruleVersion=%s", rep.Compatible, tc.wantCompat, rep.RuleVersion)

			if rep.Compatible != tc.wantCompat {
				t.Fatalf("compatible=%v want %v; findings=%+v", rep.Compatible, tc.wantCompat, rep.Findings)
			}
			if _, ok := ruleIDs(rep)[tc.wantRule]; !ok {
				t.Fatalf("expected rule %s in findings, got %+v", tc.wantRule, rep.Findings)
			}
		})
	}
}

func TestCheckSchemasDeterministic(t *testing.T) {
	from := model.Schema{Version: 1, Fields: []model.Field{field("a", model.TypeString, false), field("b", model.TypeInt, false)}}
	to := model.Schema{Version: 2, Fields: []model.Field{
		field("a", model.TypeString, true), field("c", model.TypeBool, false),
	}}
	r1 := CheckSchemas(from, to, Options{})
	r2 := CheckSchemas(from, to, Options{})
	if len(r1.Findings) != len(r2.Findings) {
		t.Fatal("non-deterministic finding count")
	}
	for i := range r1.Findings {
		if r1.Findings[i].RuleID != r2.Findings[i].RuleID || r1.Findings[i].Field != r2.Findings[i].Field {
			t.Fatalf("non-deterministic ordering: %+v vs %+v", r1.Findings, r2.Findings)
		}
	}
	t.Logf("stable order across runs: %v", r1.Findings)
}
