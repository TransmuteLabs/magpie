package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/memory"
)

// keepEmbedding reads new memories for search by meaning while the
// backend runs, when it is switched on.
func keepEmbedding(ctx context.Context) { memory.Semantic().Keep(ctx, time.Minute) }

func memorySemanticRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/memory/semantic", func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, memory.Semantic().Status())
	})
	// {on}: switched on, the model is fetched when it isn't on disk;
	// switched off, it is deleted
	mux.HandleFunc("POST /api/memory/semantic", func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			On bool `json:"on"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(rw, err)
			return
		}
		semantic := memory.Semantic()
		if err := semantic.Set(in.On); err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, semantic.Status())
	})
	// {q, status}: the Memory tab's search, as an agent's: the ids found,
	// best first, without noting them as used. None when search by
	// meaning is off: the tab filters by words itself.
	mux.HandleFunc("POST /api/memory/rank", func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			Q      string `json:"q"`
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(rw, err)
			return
		}
		out := struct {
			IDs []string `json:"ids"`
		}{}
		if strings.TrimSpace(in.Q) == "" || !memory.Semantic().On() {
			writeJSON(rw, out)
			return
		}
		statuses := []string{in.Status}
		if in.Status == memory.Archived {
			statuses = append(statuses, memory.Superseded)
		}
		out.IDs = []string{}
		for _, st := range statuses {
			hits, err := memory.Owner().Rank(memory.Query{Text: in.Q, Status: st, Limit: 200})
			if err != nil {
				fail(rw, err)
				return
			}
			for _, h := range hits {
				out.IDs = append(out.IDs, h.ID)
			}
		}
		writeJSON(rw, out)
	})
}
