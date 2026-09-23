// Package server exposes the catalog over a local HTTP API.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/lisama569/metadata-catalog-go/internal/catalog"
	"github.com/lisama569/metadata-catalog-go/internal/graph"
	"github.com/lisama569/metadata-catalog-go/internal/merge"
	"github.com/lisama569/metadata-catalog-go/internal/model"
)

// Server wraps a catalog with HTTP handlers.
type Server struct {
	cat *catalog.Catalog
	mux *http.ServeMux
}

// New builds the HTTP server.
func New(cat *catalog.Catalog) *Server {
	s := &Server{cat: cat, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("POST /v1/entities", s.ingestEntity)
	s.mux.HandleFunc("GET /v1/entities/{id}", s.getEntity)
	s.mux.HandleFunc("POST /v1/entities/{id}/evolve", s.evolve)
	s.mux.HandleFunc("POST /v1/relationships", s.ingestRel)
	s.mux.HandleFunc("POST /v1/merges", s.applyMerge)
	s.mux.HandleFunc("GET /v1/entities/{id}/impact", s.impact)
	s.mux.HandleFunc("GET /v1/entities/{id}/lineage", s.lineage)
	s.mux.HandleFunc("GET /v1/graph", s.snapshot)
	s.mux.HandleFunc("GET /v1/dangling", s.dangling)
	s.mux.HandleFunc("GET /v1/history", s.history)
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe starts serving on addr.
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ingestEntity(w http.ResponseWriter, r *http.Request) {
	var e model.Entity
	if !decode(w, r, &e) {
		return
	}
	rec, err := s.cat.IngestEntity(e)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeDecision(w, rec)
}

func (s *Server) getEntity(w http.ResponseWriter, r *http.Request) {
	e, err := s.cat.GetEntity(r.PathValue("id"))
	if err != nil {
		writeDecisionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

type evolveReq struct {
	Source string       `json:"source"`
	Schema model.Schema `json:"schema"`
}

func (s *Server) evolve(w http.ResponseWriter, r *http.Request) {
	var req evolveReq
	if !decode(w, r, &req) {
		return
	}
	rep, rec, err := s.cat.EvolveSchema(r.PathValue("id"), req.Schema, req.Source)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, decisionStatus(rec), map[string]any{"report": rep, "decision": rec})
}

func (s *Server) ingestRel(w http.ResponseWriter, r *http.Request) {
	var rel model.Relationship
	if !decode(w, r, &rel) {
		return
	}
	rec, err := s.cat.IngestRelationship(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeDecision(w, rec)
}

func (s *Server) applyMerge(w http.ResponseWriter, r *http.Request) {
	var req merge.Request
	if !decode(w, r, &req) {
		return
	}
	plan, rec, err := s.cat.ApplyMerge(req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, decisionStatus(rec), map[string]any{"plan": plan, "decision": rec})
}

func (s *Server) impact(w http.ResponseWriter, r *http.Request) {
	paths, matched, err := s.cat.Impact(r.PathValue("id"), parseFilter(r))
	if err != nil {
		writeDecisionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"paths": paths, "matched": matched})
}

func (s *Server) lineage(w http.ResponseWriter, r *http.Request) {
	paths, matched, err := s.cat.Lineage(r.PathValue("id"), parseFilter(r))
	if err != nil {
		writeDecisionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"paths": paths, "matched": matched})
}

func (s *Server) snapshot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.cat.SnapshotGraph())
}

func (s *Server) dangling(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"edges": s.cat.DanglingEdges()})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": s.cat.History(limit)})
}

func parseFilter(r *http.Request) graph.Filter {
	q := r.URL.Query()
	return graph.Filter{
		Owner: q.Get("owner"),
		Tags:  splitCSV(q.Get("tags")),
		Terms: splitCSV(q.Get("terms")),
	}
}

func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeDecision(w http.ResponseWriter, rec model.DecisionRecord) {
	writeJSON(w, decisionStatus(rec), rec)
}

func decisionStatus(rec model.DecisionRecord) int {
	if rec.Accepted {
		return http.StatusOK
	}
	return http.StatusConflict
}

func writeDecisionErr(w http.ResponseWriter, err error) {
	if errors.Is(err, catalog.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
