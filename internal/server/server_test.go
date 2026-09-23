package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lisama569/metadata-catalog-go/internal/catalog"
	"github.com/lisama569/metadata-catalog-go/internal/model"
)

func newSrv(t *testing.T) (*Server, *catalog.Catalog) {
	t.Helper()
	cat, err := catalog.Open(catalog.Config{JournalPath: ""})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	return New(cat), cat
}

func do(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestHTTPIngestDuplicateConflict(t *testing.T) {
	srv, _ := newSrv(t)
	h := srv.Handler()
	e := model.Entity{ID: "t1", Version: 1, TypeName: "table",
		Schema: model.Schema{Version: 1, Fields: []model.Field{{Name: "a", Type: model.TypeString}}}}

	code, body := do(t, h, "POST", "/v1/entities", e)
	t.Logf("POST entity -> %d decision=%v", code, body["decision"])
	if code != http.StatusOK {
		t.Fatalf("first ingest code=%d", code)
	}
	code, body = do(t, h, "POST", "/v1/entities", e)
	t.Logf("POST duplicate -> %d decision=%v", code, body["decision"])
	if code != http.StatusOK || body["decision"] != "duplicate" {
		t.Fatalf("duplicate handling wrong: %d %v", code, body["decision"])
	}
	e.Owner = "x"
	code, body = do(t, h, "POST", "/v1/entities", e)
	t.Logf("POST conflict -> %d decision=%v", code, body["decision"])
	if code != http.StatusConflict || body["decision"] != "reject_content_conflict" {
		t.Fatalf("conflict handling wrong: %d %v", code, body["decision"])
	}
}

func TestHTTPImpactAndDangling(t *testing.T) {
	srv, _ := newSrv(t)
	h := srv.Handler()
	do(t, h, "POST", "/v1/entities", model.Entity{ID: "a", Version: 1, TypeName: "table",
		Owner: "alice", Schema: model.Schema{Version: 1}})
	do(t, h, "POST", "/v1/entities", model.Entity{ID: "b", Version: 1, TypeName: "table",
		Owner: "bob", Schema: model.Schema{Version: 1}})

	code, body := do(t, h, "POST", "/v1/relationships", model.Relationship{
		Upstream: "a", Downstream: "ghost", Kind: model.RelProduces, Version: 1})
	t.Logf("POST dangling rel -> %d decision=%v", code, body["decision"])
	if code != http.StatusOK {
		t.Fatal("dangling rel must be accepted")
	}
	code, body = do(t, h, "GET", "/v1/dangling", nil)
	edges := body["edges"].([]any)
	t.Logf("GET dangling -> %d count=%d", code, len(edges))
	if len(edges) != 1 {
		t.Fatal("expected 1 dangling edge via HTTP")
	}

	do(t, h, "POST", "/v1/relationships", model.Relationship{
		Upstream: "a", Downstream: "b", Kind: model.RelProduces, Version: 1})
	code, body = do(t, h, "GET", "/v1/entities/a/impact?owner=bob", nil)
	t.Logf("GET impact owner=bob -> %d body=%v", code, body["matched"])
	if code != http.StatusOK {
		t.Fatal("impact failed")
	}
	matched := body["matched"].([]any)
	if len(matched) != 1 || matched[0] != "b" {
		t.Fatalf("impact matched=%v", matched)
	}

	code, body = do(t, h, "GET", "/healthz", nil)
	if code != 200 || body["status"] != "ok" {
		t.Fatalf("healthz=%d %v", code, body)
	}
}

func TestHTTPSchemaEvolveAndMerge(t *testing.T) {
	srv, _ := newSrv(t)
	h := srv.Handler()
	do(t, h, "POST", "/v1/entities", model.Entity{ID: "e", Version: 1, TypeName: "table",
		Schema: model.Schema{Version: 1, Fields: []model.Field{{Name: "a", Type: model.TypeString}}}})

	// Incompatible evolution: type change.
	code, body := do(t, h, "POST", "/v1/entities/e/evolve", map[string]any{
		"source": "s",
		"schema": map[string]any{"fields": []map[string]any{{"name": "a", "type": "int"}}},
	})
	t.Logf("evolve incompatible -> %d", code)
	if code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%v)", code, body)
	}

	// Compatible evolution adding optional field.
	code, _ = do(t, h, "POST", "/v1/entities/e/evolve", map[string]any{
		"source": "s",
		"schema": map[string]any{"fields": []map[string]any{
			{"name": "a", "type": "string"}, {"name": "b", "type": "int"},
		}},
	})
	t.Logf("evolve compatible -> %d", code)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}

	// Merge cycle: self merge rejected.
	code, body = do(t, h, "POST", "/v1/merges", map[string]string{"survivorId": "e", "victimId": "e"})
	t.Logf("self merge -> %d decision=%v", code, body["decision"])
	if code != http.StatusConflict {
		t.Fatalf("self merge should be 409, got %d", code)
	}
}
