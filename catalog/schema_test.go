package catalog

import "testing"

func baseSchema() Schema {
	return Schema{Fields: []Field{
		{Name: "id", Type: FieldTypeInt, Required: true},
		{Name: "amount", Type: FieldTypeInt},
		{Name: "note", Type: FieldTypeString},
	}}
}

func logVerdict(t *testing.T, name string, v Verdict) {
	t.Helper()
	t.Logf("%s: compatible=%v", name, v.Compatible)
	for _, c := range v.Changes {
		t.Logf("  change: kind=%s field=%s %s->%s compatible=%v reason=%s",
			c.Kind, c.Field, c.OldType, c.NewType, c.Compatible, c.Reason)
	}
}

func TestCompatibleEvolution(t *testing.T) {
	next := baseSchema()
	next.Fields[1].Type = FieldTypeFloat // int -> float 加宽
	next.Fields = append(next.Fields, Field{Name: "extra", Type: FieldTypeString})
	v := CheckCompatibility(baseSchema(), next, nil)
	logVerdict(t, "widening+optional-add", v)
	if !v.Compatible {
		t.Fatalf("expected compatible, got %+v", v.Changes)
	}
}

func TestIncompatibleEvolution(t *testing.T) {
	cases := []struct {
		name string
		next Schema
	}{
		{"remove-field", Schema{Fields: []Field{
			{Name: "id", Type: FieldTypeInt, Required: true},
			{Name: "amount", Type: FieldTypeInt},
		}}},
		{"narrowing-type", Schema{Fields: []Field{
			{Name: "id", Type: FieldTypeInt, Required: true},
			{Name: "amount", Type: FieldTypeInt},
			{Name: "note", Type: FieldTypeBytes}, // string -> bytes 不允许
		}}},
		{"required-added", Schema{Fields: append(baseSchema().Fields,
			Field{Name: "must", Type: FieldTypeString, Required: true})}},
	}
	for _, tc := range cases {
		v := CheckCompatibility(baseSchema(), tc.next, nil)
		logVerdict(t, tc.name, v)
		if v.Compatible {
			t.Fatalf("%s: expected incompatible", tc.name)
		}
	}
}

func TestRenameWithHintIsCompatible(t *testing.T) {
	next := baseSchema()
	next.Fields[2].Name = "comment" // note -> comment
	withHint := CheckCompatibility(baseSchema(), next, []RenameHint{{Old: "note", New: "comment"}})
	logVerdict(t, "rename-with-hint", withHint)
	if !withHint.Compatible {
		t.Fatalf("declared rename should be compatible")
	}
	withoutHint := CheckCompatibility(baseSchema(), next, nil)
	logVerdict(t, "rename-without-hint", withoutHint)
	if withoutHint.Compatible {
		t.Fatalf("undeclared rename should be treated as removal (incompatible)")
	}
}

func TestVerdictReproducible(t *testing.T) {
	next := baseSchema()
	next.Fields[1].Type = FieldTypeBytes
	v1 := CheckCompatibility(baseSchema(), next, nil)
	v2 := CheckCompatibility(baseSchema(), next, nil)
	if v1.Compatible != v2.Compatible || len(v1.Changes) != len(v2.Changes) {
		t.Fatalf("verdict not reproducible")
	}
	for i := range v1.Changes {
		if v1.Changes[i] != v2.Changes[i] {
			t.Fatalf("change %d differs between runs", i)
		}
	}
	t.Logf("reproducible verdict: compatible=%v changes=%d", v1.Compatible, len(v1.Changes))
}

func TestEvolveSchemaThroughCatalog(t *testing.T) {
	c := New()
	c.IngestEntity(testEntity("ds-1", 1))

	// 兼容演进：加宽类型。
	ok := baseSchema()
	ok.Fields[1].Type = FieldTypeFloat
	v, res := c.EvolveSchema("ds-1", ok, nil, "test", 2)
	t.Logf("compatible evolve: verdict=%v applied=%v", v.Compatible, res.Applied)
	if !v.Compatible || !res.Applied {
		t.Fatalf("compatible evolution should apply")
	}

	// 不兼容演进：删字段，拒绝且原因持久化。
	bad := Schema{Fields: []Field{{Name: "id", Type: FieldTypeInt, Required: true}}}
	v2, res2 := c.EvolveSchema("ds-1", bad, nil, "test", 3)
	t.Logf("incompatible evolve: verdict=%v rejection=%v", v2.Compatible, reasonOf(res2))
	if v2.Compatible || res2.Rejection == nil || res2.Rejection.Reason != ReasonSchemaIncompatible {
		t.Fatalf("expected SCHEMA_INCOMPATIBLE rejection, got %+v", res2.Rejection)
	}
	got, _ := c.Store().GetEntity("ds-1")
	if got.Version != 2 {
		t.Fatalf("rejected evolution must not change state, version=%d", got.Version)
	}
}
