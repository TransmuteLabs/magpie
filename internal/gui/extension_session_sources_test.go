package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/sessions"
)

func TestExtensionSessionSourceNativeOrdinaryAPI(t *testing.T) {
	distribution := os.Getenv("MAGPIE_SOURCE_TEST_DIST")
	if distribution == "" {
		t.Skip("requires explicitly built synthetic native Source 1.6 fixture")
	}
	home := sandboxHome(t)
	if err := sessions.SetReads(nil, map[string]bool{"fixture-source": true}); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = manager.Close(ctx)
		sessions.Reset()
	})
	installed, err := manager.Install(t.Context(), distribution)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".fixture-source", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"sess","title":"Source API","cwd":"/work/source","at":"2026-11-13T00:00:00Z","role":"user","text":"source API words","input":5}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "sess.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	sessionRoutes(mux, folderOnly{})
	for _, check := range []struct{ path, expected string }{
		{"/api/sessions?limit=20", `"agent":"fixture-source"`},
		{"/api/sessions/one?key=fixture-source:sess", `"id":"sess"`},
		{"/api/sessions/transcript?agent=fixture-source&id=sess", "source API words"},
		{"/api/sessions/markdown?agent=fixture-source&id=sess", "source API words"},
		{"/api/sessions/sources", `"errors":[]`},
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest("GET", check.path, nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), check.expected) {
			t.Fatal("ordinary source API failed", check.path, response.Code, response.Body.String())
		}
	}
	if err := manager.SetEnabled(t.Context(), installed.Manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "/api/sessions/transcript?agent=fixture-source&id=sess", nil))
	if response.Code == 200 {
		t.Fatal("ordinary transcript API retained disabled source authority")
	}
}
