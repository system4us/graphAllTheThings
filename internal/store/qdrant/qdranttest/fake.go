// Package qdranttest is a minimal in-memory stand-in for the parts of the
// Qdrant REST API that gatt uses, for tests. Page size is deliberately tiny so
// scroll pagination is exercised.
package qdranttest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
)

type Server struct {
	*httptest.Server
	mu   sync.Mutex
	cols map[string]map[string]map[string]any // collection -> point id -> payload
	// Recreates counts DELETE /collections/x calls: a regression on "Upsert wipes
	// the collection" shows up as extra recreations.
	Recreates int
}

const pageSize = 3

func New() *Server {
	s := &Server{cols: map[string]map[string]map[string]any{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Points returns a copy of the payloads stored in a collection, keyed by node id.
func (s *Server) Points(col string) map[string]map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]any{}
	for _, p := range s.cols[col] {
		out[p["node_id"].(string)] = p
	}
	return out
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // collections/<c>[/points[/op]]
	if len(parts) < 2 || parts[0] != "collections" {
		reply(w, 404, map[string]any{"status": "bad path"})
		return
	}
	col := parts[1]
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)

	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet:
			if _, ok := s.cols[col]; !ok {
				reply(w, 404, map[string]any{"status": map[string]any{"error": "Not found"}})
				return
			}
			reply(w, 200, map[string]any{"result": map[string]any{"status": "green"}})
		case http.MethodPut:
			if _, ok := s.cols[col]; ok {
				reply(w, 409, map[string]any{"status": map[string]any{"error": "already exists"}})
				return
			}
			s.cols[col] = map[string]map[string]any{}
			reply(w, 200, map[string]any{"result": true})
		case http.MethodDelete:
			s.Recreates++
			delete(s.cols, col)
			reply(w, 200, map[string]any{"result": true})
		}
		return
	}

	pts, ok := s.cols[col]
	if !ok {
		reply(w, 404, map[string]any{"status": map[string]any{"error": "Not found: collection"}})
		return
	}
	switch {
	case r.Method == http.MethodPut && parts[2] == "points":
		for _, p := range body["points"].([]any) {
			pm := p.(map[string]any)
			pts[pm["id"].(string)] = pm["payload"].(map[string]any)
		}
		reply(w, 200, map[string]any{"result": map[string]any{"status": "completed"}})
	case parts[len(parts)-1] == "scroll":
		ids := make([]string, 0, len(pts))
		for id := range pts {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		start := 0
		if off, ok := body["offset"].(string); ok {
			start = sort.SearchStrings(ids, off)
		}
		end := min(start+pageSize, len(ids))
		var res []map[string]any
		for _, id := range ids[start:end] {
			res = append(res, map[string]any{"id": id, "payload": pts[id]})
		}
		var next any
		if end < len(ids) {
			next = ids[end]
		}
		reply(w, 200, map[string]any{"result": map[string]any{"points": res, "next_page_offset": next}})
	case parts[len(parts)-1] == "delete":
		for _, id := range body["points"].([]any) {
			delete(pts, id.(string))
		}
		reply(w, 200, map[string]any{"result": map[string]any{"status": "completed"}})
	default:
		reply(w, 404, map[string]any{"status": "unsupported"})
	}
}
