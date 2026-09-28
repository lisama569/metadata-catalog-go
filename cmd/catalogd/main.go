// catalogd：本地常驻元数据目录服务。
// 读写均通过 HTTP JSON 暴露，数据经 WAL 持久化到本地目录，重启后自动恢复。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"strings"

	catalog "github.com/lisama569/metadata-catalog-go/catalog"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	dir := flag.String("dir", "catalog-data", "data directory for the durable log")
	flag.Parse()

	c, err := catalog.Open(*dir)
	if err != nil {
		log.Fatalf("open catalog in %s: %v", *dir, err)
	}
	defer c.Close()

	srv := &server{c: c}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /entities", srv.ingestEntity)
	mux.HandleFunc("GET /entities", srv.listEntities)
	mux.HandleFunc("GET /entities/{id}", srv.getEntity)
	mux.HandleFunc("POST /entities/{id}/evolve", srv.evolveSchema)
	mux.HandleFunc("POST /relations", srv.ingestRelation)
	mux.HandleFunc("GET /relations", srv.listRelations)
	mux.HandleFunc("POST /merge", srv.merge)
	mux.HandleFunc("GET /rejections", srv.listRejections)
	mux.HandleFunc("GET /impact", srv.impact)

	log.Printf("catalogd listening on %s, data dir %s", *addr, *dir)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

type server struct{ c *catalog.Catalog }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return false
	}
	return true
}

// resultView 统一的写入响应：applied / duplicate / rejection 三态可区分。
func resultView(res catalog.UpsertResult) map[string]any {
	out := map[string]any{"applied": res.Applied, "duplicate": res.Duplicate}
	if res.Rejection != nil {
		out["rejection"] = res.Rejection
	}
	return out
}

func (s *server) ingestEntity(w http.ResponseWriter, r *http.Request) {
	var e catalog.Entity
	if !decode(w, r, &e) {
		return
	}
	writeJSON(w, http.StatusOK, resultView(s.c.IngestEntity(e)))
}

func (s *server) ingestRelation(w http.ResponseWriter, r *http.Request) {
	var rel catalog.Relation
	if !decode(w, r, &rel) {
		return
	}
	writeJSON(w, http.StatusOK, resultView(s.c.IngestRelation(rel)))
}

func (s *server) listEntities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.c.Store().Entities())
}

func (s *server) getEntity(w http.ResponseWriter, r *http.Request) {
	e, ok := s.c.Store().GetEntity(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *server) listRelations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.c.Store().Relations())
}

func (s *server) listRejections(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.c.Store().Rejections())
}

type evolveRequest struct {
	Schema  catalog.Schema       `json:"schema"`
	Renames []catalog.RenameHint `json:"renames,omitempty"`
	Source  string               `json:"source"`
	Version int64                `json:"version"`
}

func (s *server) evolveSchema(w http.ResponseWriter, r *http.Request) {
	var req evolveRequest
	if !decode(w, r, &req) {
		return
	}
	verdict, res := s.c.EvolveSchema(r.PathValue("id"), req.Schema, req.Renames, req.Source, req.Version)
	writeJSON(w, http.StatusOK, map[string]any{
		"verdict": verdict,
		"result":  resultView(res),
	})
}

type mergeRequest struct {
	Dst     string `json:"dst"`
	Src     string `json:"src"`
	Source  string `json:"source"`
	Version int64  `json:"version"`
}

func (s *server) merge(w http.ResponseWriter, r *http.Request) {
	var req mergeRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Dst == "" || req.Src == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errors.New("dst and src are required").Error()})
		return
	}
	writeJSON(w, http.StatusOK, resultView(s.c.MergeEntities(req.Dst, req.Src, req.Source, req.Version)))
}

func (s *server) impact(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	if root == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "root query param is required"})
		return
	}
	q := r.URL.Query()
	filter := catalog.ImpactFilter{
		Tags:   splitCSV(q.Get("tags")),
		Terms:  splitCSV(q.Get("terms")),
		Owners: splitCSV(q.Get("owners")),
	}
	writeJSON(w, http.StatusOK, s.c.Impact(root, filter))
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
