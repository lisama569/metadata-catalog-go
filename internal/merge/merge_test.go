package merge

import (
	"testing"

	"github.com/lisama569/metadata-catalog-go/internal/model"
)

func mkEntity(id, typ, owner string, ver int, attrs map[string]string, fields ...model.Field) model.Entity {
	return model.Entity{
		ID: id, TypeName: typ, Owner: owner, Version: ver,
		Attrs: attrs, Schema: model.Schema{Version: ver, Fields: fields},
	}
}

func TestPlanMergeCompatible(t *testing.T) {
	s := mkEntity("a", "table", "alice", 1, map[string]string{"x": "1"}, model.Field{Name: "f1", Type: model.TypeString})
	v := mkEntity("b", "table", "alice", 1, map[string]string{"y": "2"}, model.Field{Name: "f2", Type: model.TypeInt})
	p := PlanMerge(s, v, "test")
	t.Logf("INPUT survivor=%s victim=%s", s.ID, v.ID)
	t.Logf("VERDICT compatible=%v conflicts=%d", p.Compatible, len(p.Conflicts))
	if !p.Compatible {
		t.Fatalf("expected compatible merge, got conflicts %+v", p.Conflicts)
	}
	if p.Merged.ID != "a" {
		t.Fatalf("survivor id = %s want a", p.Merged.ID)
	}
	if p.Merged.Version != 2 {
		t.Fatalf("merged version = %d want 2", p.Merged.Version)
	}
	if _, ok := p.Merged.Attrs["x"]; !ok {
		t.Fatal("attr x missing from merge")
	}
	if _, ok := p.Merged.Attrs["y"]; !ok {
		t.Fatal("attr y missing from merge")
	}
}

func TestPlanMergeConflicts(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(s, v *model.Entity)
		wantRule  string
		wantField string
	}{
		{"type conflict", func(s, v *model.Entity) { v.TypeName = "topic" }, "M001", "typeName"},
		{"schema field conflict", func(s, v *model.Entity) {
			s.Schema.Fields = []model.Field{{Name: "f", Type: model.TypeString}}
			v.Schema.Fields = []model.Field{{Name: "f", Type: model.TypeInt}}
		}, "M002", "f"},
		{"attr conflict", func(s, v *model.Entity) {
			s.Attrs = map[string]string{"x": "1"}
			v.Attrs = map[string]string{"x": "2"}
		}, "M003", "x"},
		{"owner conflict", func(s, v *model.Entity) {
			s.Owner, v.Owner = "alice", "bob"
		}, "M004", "owner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mkEntity("a", "table", "alice", 1, map[string]string{})
			v := mkEntity("b", "table", "alice", 1, map[string]string{})
			tc.mutate(&s, &v)
			p := PlanMerge(s, v, "test")
			t.Logf("INPUT s=%s v=%s", s.ID, v.ID)
			for _, c := range p.Conflicts {
				t.Logf("CONFLICT rule=%s kind=%s field=%q values=%v reason=%q", c.RuleID, c.Kind, c.Field, c.Values, c.Reason)
			}
			if p.Compatible {
				t.Fatal("expected incompatible merge")
			}
			found := false
			for _, c := range p.Conflicts {
				if c.RuleID == tc.wantRule && c.Field == tc.wantField {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected rule %s on field %s, got %+v", tc.wantRule, tc.wantField, p.Conflicts)
			}
		})
	}
}

func TestPlanMergeDeterministic(t *testing.T) {
	s := mkEntity("a", "table", "", 1, nil)
	v := mkEntity("b", "other", "", 1, nil)
	v.Attrs = map[string]string{"z": "1", "a": "2"}
	p1 := PlanMerge(s, v, "x")
	p2 := PlanMerge(s, v, "x")
	if len(p1.Conflicts) != len(p2.Conflicts) {
		t.Fatal("conflict count differs")
	}
	for i := range p1.Conflicts {
		if p1.Conflicts[i].Field != p2.Conflicts[i].Field {
			t.Fatalf("conflict order not stable: %+v vs %+v", p1.Conflicts, p2.Conflicts)
		}
	}
}
