package gui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/yetone/magpie/internal/sessions"
)

func extensionStorageSourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sessions/storage-sources", func(w http.ResponseWriter, r *http.Request) {
		roots, consents, err := sessions.InspectStorageSources(r.Context())
		if err != nil {
			http.Error(w, "storage source inspection failed", http.StatusConflict)
			return
		}
		writeJSON(w, struct {
			Roots    []sessions.StorageSourceRoot    `json:"roots"`
			Consents []sessions.StorageSourceConsent `json:"consents"`
		}{roots, consents})
	})
	mux.HandleFunc("POST /api/sessions/storage-sources/bind", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Plugin           string `json:"plugin"`
			Source           string `json:"source"`
			RootSlot         string `json:"rootSlot"`
			RootRef          string `json:"rootRef"`
			ExpectedRevision string `json:"expectedRevision"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid storage source selection", http.StatusBadRequest)
			return
		}
		if err := sessions.BindStorageSource(r.Context(), request.Plugin, request.Source, request.RootSlot, request.RootRef, request.ExpectedRevision); err != nil {
			http.Error(w, "storage source selection changed", http.StatusConflict)
			return
		}
		writeJSON(w, struct{}{})
	})
	mux.HandleFunc("POST /api/sessions/storage-sources/unbind", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Agent            string `json:"agent"`
			ExpectedIdentity string `json:"expectedIdentity"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid storage source release", http.StatusBadRequest)
			return
		}
		if err := sessions.UnbindStorageSource(r.Context(), request.Agent, request.ExpectedIdentity); err != nil {
			http.Error(w, "storage source consent changed", http.StatusConflict)
			return
		}
		writeJSON(w, struct{}{})
	})
	mux.HandleFunc("GET /api/sessions/storage-media", func(w http.ResponseWriter, r *http.Request) {
		media, err := sessions.StorageSourceMedia(r.Context(), r.URL.Query().Get("agent"), r.URL.Query().Get("session"), r.URL.Query().Get("media"))
		if err != nil {
			http.Error(w, "selected storage media is unavailable", http.StatusConflict)
			return
		}
		mime := "application/octet-stream"
		if strings.HasPrefix(media.MIME, "image/") && media.MIME != "image/svg+xml" ||
			strings.HasPrefix(media.MIME, "audio/") || strings.HasPrefix(media.MIME, "video/") || media.MIME == "text/plain" {
			mime = media.MIME
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition", "attachment")
		w.Write(media.Bytes)
	})
}
