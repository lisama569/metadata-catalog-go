package catalog

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Handler 返回目录的 HTTP API 处理器，
// 供本地常驻服务以命令行/脚本方式调用读写接口。
//
//	POST   /api/entities        摄入实体（body 为 Entity JSON）
//	POST   /api/relations       摄入关系（body 为 Relation JSON）
//	POST   /api/evolve          结构演进（body 为 evolveRequest）
//	POST   /api/merge           合并实体（body 为 mergeRequest）
//	GET    /api/entities        全部实体（确定性排序）
//	GET    /api/entities/{id}   单个实体
//	GET    /api/relations       全部关系
//	GET    /api/rejections      全部拒绝记录
//	GET    /api/impact?root=..&tags=a,b&terms=..&owners=..  影响分析
func (c *Catalog) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/entities", c.handleIngestEntity)
	mux.HandleFunc("POST /api/relations", c.handleIngestRelation)
	mux.HandleFunc("POST /api/evolve", c.handleEvolve)
	mux.HandleFunc("POST /api/merge", c.handleMerge)
	mux.HandleFunc("GET /api/entities", c.handleListEntities)
	mux.HandleFunc("GET /api/entities/{id}", c.handleGetEntity)
	mux.HandleFunc("GET /api/relations", c.handleListRelations)
	mux.HandleFunc("GET /api/rejections", c.handleListRejections)
	mux.HandleFunc("GET /api/impact", c.handleImpact)
	return mux
}

// writeResponse 写入类接口的统一响应。
type writeResponse struct {
	Applied   bool       `json:"applied"`
	Duplicate bool       `json:"duplicate"`
	Rejection *Rejection `json:"rejection,omitempty"`
	Error     string     `json:"error,omitempty"`
}

func toResponse(r UpsertResult) writeResponse {
	resp := writeResponse{Applied: r.Applied, Duplicate: r.Duplicate, Rejection: r.Rejection}
	if r.Err != nil {
		resp.Error = r.Err.Error()
	}
	return resp
}

// writeResult 按写入结果选择状态码：拒绝 409，持久化错误 500，其余 200。
func writeResult(w http.ResponseWriter, r UpsertResult) {
	code := http.StatusOK
	switch {
	case r.Err != nil:
		code = http.StatusInternalServerError
	case r.Rejection != nil:
		code = http.StatusConflict
	}
	writeJSON(w, code, toResponse(r))
}

func (c *Catalog) handleIngestEntity(w http.ResponseWriter, r *http.Request) {
	var e Entity
	if !decodeBody(w, r, &e) {
		return
	}
	writeResult(w, c.IngestEntity(e))
}

func (c *Catalog) handleIngestRelation(w http.ResponseWriter, r *http.Request) {
	var rel Relation
	if !decodeBody(w, r, &rel) {
		return
	}
	writeResult(w, c.IngestRelation(rel))
}

// evolveRequest 结构演进请求。
type evolveRequest struct {
	EntityID string       `json:"entity_id"`
	Schema   Schema       `json:"schema"`
	Renames  []RenameHint `json:"renames,omitempty"`
	Source   string       `json:"source"`
	Version  int64        `json:"version"`
}

func (c *Catalog) handleEvolve(w http.ResponseWriter, r *http.Request) {
	var req evolveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	verdict, res := c.EvolveSchema(req.EntityID, req.Schema, req.Renames, req.Source, req.Version)
	code := http.StatusOK
	switch {
	case res.Err != nil:
		code = http.StatusInternalServerError
	case res.Rejection != nil:
		code = http.StatusConflict
	}
	writeJSON(w, code, struct {
		Verdict Verdict       `json:"verdict"`
		Result  writeResponse `json:"result"`
	}{Verdict: verdict, Result: toResponse(res)})
}

// mergeRequest 合并请求：将 Src 合并进 Dst。
type mergeRequest struct {
	Dst     string `json:"dst"`
	Src     string `json:"src"`
	Source  string `json:"source"`
	Version int64  `json:"version"`
}

func (c *Catalog) handleMerge(w http.ResponseWriter, r *http.Request) {
	var req mergeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	writeResult(w, c.MergeEntities(req.Dst, req.Src, req.Source, req.Version))
}

func (c *Catalog) handleListEntities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, c.Store().Entities())
}

func (c *Catalog) handleGetEntity(w http.ResponseWriter, r *http.Request) {
	e, ok := c.Store().GetEntity(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (c *Catalog) handleListRelations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, c.Store().Relations())
}

func (c *Catalog) handleListRejections(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, c.Store().Rejections())
}

func (c *Catalog) handleImpact(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	root := q.Get("root")
	if root == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing root parameter"})
		return
	}
	filter := ImpactFilter{
		Tags:   splitCSV(q.Get("tags")),
		Terms:  splitCSV(q.Get("terms")),
		Owners: splitCSV(q.Get("owners")),
	}
	writeJSON(w, http.StatusOK, c.Impact(root, filter))
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	// 响应头已发出，编码失败（本地服务场景几乎不会发生）无法补救，忽略。
	_ = json.NewEncoder(w).Encode(v)
}
