package model

import "testing"

func TestFingerprintStabilityAndSensitivity(t *testing.T) {
	a := Entity{
		ID: "e", Version: 1, TypeName: "table", Source: "s",
		Schema: Schema{Version: 1, Fields: []Field{{Name: "x", Type: TypeString}}},
		Tags:   []string{"b", "a"},
		Terms:  []string{"t2", "t1"},
		Attrs:  map[string]string{"k": "v"},
	}
	b := a
	b.Version = 99 // excluded from fingerprint
	b.UpdatedAt = a.UpdatedAt
	// different slice order, same content
	b.Tags = []string{"a", "b"}
	b.Terms = []string{"t1", "t2"}

	fa, fb := EntityFingerprint(a), EntityFingerprint(b)
	t.Logf("fingerprint same-content different-order: %s", fa)
	if fa != fb {
		t.Fatal("fingerprint must be insensitive to tag/term ordering and version")
	}

	c := a
	c.Attrs = map[string]string{"k": "different"}
	if EntityFingerprint(c) == fa {
		t.Fatal("fingerprint must change when business content changes")
	}
	t.Logf("fingerprint changes on content edit (got distinct hashes)")

	r1 := Relationship{Key: "k", Upstream: "u", Downstream: "d", Kind: RelDerivedFrom, Source: "s"}
	r2 := r1
	r2.Version = 7
	if RelFingerprint(r1) != RelFingerprint(r2) {
		t.Fatal("rel fingerprint must exclude version")
	}
	t.Log("rel fingerprint excludes version: stable")
}
