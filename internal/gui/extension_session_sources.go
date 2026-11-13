package gui

import (
	"net/http"

	"github.com/yetone/magpie/internal/sessions"
)

func extensionSessionSourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sessions/sources", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, sessions.InspectSessionSources(r.Context()))
	})
}
