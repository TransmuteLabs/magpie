package gui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/yetone/magpie/internal/update"
)

var (
	localSigningPolicy = update.Signing
	localSigningIDs    = update.SigningIdentities
	localSigningUse    = update.UseSigning
	localSigningOff    = update.DisableSigning
	localSigningApp    = update.Bundle
	localSigningOS     = runtime.GOOS
)

type signingStatus struct {
	Supported   bool            `json:"supported"`
	Configured  bool            `json:"configured"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	Team        string          `json:"team,omitempty"`
	BundleID    string          `json:"bundleID,omitempty"`
	Feed        string          `json:"feed,omitempty"`
	Identities  []signingChoice `json:"identities,omitempty"`
}

type signingChoice struct {
	Fingerprint string    `json:"fingerprint"`
	Name        string    `json:"name"`
	Team        string    `json:"team"`
	Expires     time.Time `json:"expires"`
}

func readSigningStatus(ctx context.Context, list bool) (signingStatus, error) {
	s := signingStatus{Supported: localSigningOS == "darwin" && localSigningApp() != ""}
	if !s.Supported {
		return s, nil
	}
	c, err := localSigningPolicy()
	if err != nil {
		return s, err
	}
	if c != nil {
		s.Configured, s.Fingerprint, s.Team, s.BundleID, s.Feed = true, c.Fingerprint, c.Team, c.BundleID, c.Feed
	}
	if list {
		ids, err := localSigningIDs(ctx)
		if err != nil {
			return s, err
		}
		for _, id := range ids {
			s.Identities = append(s.Identities, signingChoice{id.Fingerprint, id.Name, id.Team, id.Expires})
		}
	}
	return s, nil
}

func updateSigningRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/update/signing", func(rw http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		s, err := readSigningStatus(ctx, r.URL.Query().Get("identities") == "1")
		if err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, s)
	})
	mux.HandleFunc("POST /api/update/signing", func(rw http.ResponseWriter, r *http.Request) {
		var b struct {
			Fingerprint string `json:"fingerprint"`
			Off         bool   `json:"off"`
		}
		d := json.NewDecoder(io.LimitReader(r.Body, 2048))
		d.DisallowUnknownFields()
		if err := d.Decode(&b); err != nil {
			fail(rw, err)
			return
		}
		if err := d.Decode(new(any)); err != io.EOF {
			fail(rw, errors.New("expected one signing selection"))
			return
		}
		if localSigningOS != "darwin" || localSigningApp() == "" || b.Off == (b.Fingerprint != "") {
			fail(rw, errors.New("choose a macOS signing identity or turn local signing off"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		var err error
		if b.Off {
			err = localSigningOff()
		} else {
			err = localSigningUse(ctx, b.Fingerprint, localSigningApp())
		}
		if err != nil {
			fail(rw, err)
			return
		}
		s, err := readSigningStatus(ctx, false)
		if err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, s)
	})
}
