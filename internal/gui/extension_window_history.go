package gui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"

	"github.com/yetone/magpie/internal/sessions"
)

type extensionWindowHistoryRequest struct {
	Ref        string `json:"ref"`
	Operation  string `json:"operation"`
	Source     string `json:"source,omitempty"`
	ViewRef    string `json:"viewRef,omitempty"`
	RootRef    string `json:"rootRef,omitempty"`
	ConsentRef string `json:"consentRef,omitempty"`
	SessionRef string `json:"sessionRef,omitempty"`
	ReadRef    string `json:"readRef,omitempty"`
	MediaRef   string `json:"mediaRef,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

func (in extensionWindowHistoryRequest) valid() bool {
	allowed := extensionWindowHistoryRequest{Ref: in.Ref, Operation: in.Operation}
	if in.Operation != "hello" {
		allowed.Source = in.Source
		if in.Source == "" || len(in.Source) > 128 {
			return false
		}
	}
	if in.Operation != "hello" && in.Operation != "inspect" {
		allowed.ViewRef = in.ViewRef
		if len(in.ViewRef) != 64 {
			return false
		}
	}
	switch in.Operation {
	case "hello", "inspect":
	case "request":
		allowed.RootRef = in.RootRef
		if len(in.RootRef) != 64 {
			return false
		}
	case "release":
		allowed.ConsentRef = in.ConsentRef
		if len(in.ConsentRef) != 64 {
			return false
		}
	case "list":
		allowed.RootRef, allowed.Cursor, allowed.Limit = in.RootRef, in.Cursor, in.Limit
		if len(in.RootRef) != 64 || len(in.Cursor) > 256 || in.Limit < 0 || in.Limit > 256 {
			return false
		}
	case "read":
		allowed.SessionRef = in.SessionRef
		if len(in.SessionRef) != 64 {
			return false
		}
	case "media":
		allowed.SessionRef, allowed.ReadRef, allowed.MediaRef = in.SessionRef, in.ReadRef, in.MediaRef
		if len(in.SessionRef) != 64 || len(in.ReadRef) != 64 || len(in.MediaRef) != 64 {
			return false
		}
	default:
		return false
	}
	return allowed == in
}

func extensionWindowHistoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/plugin-pages/history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var in extensionWindowHistoryRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&in) != nil || decoder.Decode(&struct{}{}) != io.EOF || !in.valid() {
			http.Error(w, "invalid history selection", http.StatusBadRequest)
			return
		}
		part, err := resolveExtensionWindow(r.Context(), in.Ref)
		if err != nil {
			http.Error(w, "history window changed", http.StatusConflict)
			return
		}
		if in.Operation == "hello" {
			features := []string{}
			if len(part.historySources) > 0 {
				features = append(features, "session-history-v1")
			}
			writeJSON(w, struct {
				Features []string `json:"features"`
			}{features})
			return
		}
		if !slices.Contains(part.historySources, in.Source) {
			http.Error(w, "undeclared history source", http.StatusConflict)
			return
		}
		owner := sessions.WindowStorageOwner{Module: part.id, PackageRevision: part.packageRevision,
			ConfigVersion: part.config.Version, PageRef: part.ref, Component: part.part}
		ctx := r.Context()
		var result any
		switch in.Operation {
		case "inspect":
			result, err = sessions.InspectWindowStorage(ctx, owner, in.Source)
		case "request":
			result = sessions.RequestWindowStorage(ctx, owner, in.Source, in.ViewRef, in.RootRef)
		case "release":
			result = sessions.ReleaseWindowStorage(ctx, owner, in.Source, in.ViewRef, in.ConsentRef)
		case "list":
			result, err = sessions.ListWindowStorage(ctx, owner, in.Source, in.ViewRef, in.RootRef, in.Cursor, in.Limit)
		case "read":
			result, err = sessions.ReadWindowStorage(ctx, owner, in.Source, in.ViewRef, in.SessionRef)
		case "media":
			media, mediaErr := sessions.MediaWindowStorage(ctx, owner, in.Source, in.ViewRef, in.SessionRef, in.ReadRef, in.MediaRef)
			if mediaErr != nil {
				err = mediaErr
				break
			}
			if _, resolveErr := resolveExtensionWindow(ctx, in.Ref); resolveErr != nil || ctx.Err() != nil {
				err = errors.New("history window changed before media publication")
				break
			}
			mime := "application/octet-stream"
			switch media.MIME {
			case "image/png", "image/jpeg", "image/gif", "image/webp", "text/plain":
				mime = media.MIME
			}
			if mime != "text/plain" && mime != "application/octet-stream" && http.DetectContentType(media.Bytes) != mime {
				mime = "application/octet-stream"
			}
			w.Header().Set("Content-Type", mime)
			w.Header().Set("Content-Length", strconv.Itoa(len(media.Bytes)))
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
			w.Header().Set("Content-Disposition", "attachment")
			w.Write(media.Bytes)
			return
		default:
			http.Error(w, "unknown history operation", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "history selection changed or is unavailable", http.StatusConflict)
			return
		}
		if _, err := resolveExtensionWindow(ctx, in.Ref); err != nil || ctx.Err() != nil {
			http.Error(w, "history window changed before publication", http.StatusConflict)
			return
		}
		writeJSON(w, result)
	})
}
