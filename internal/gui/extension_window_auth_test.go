package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionWindowAuthBrokerHTTP(t *testing.T) {
	manifest := authfixture.Manifest()
	directory := t.TempDir()
	build := exec.CommandContext(t.Context(), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(directory, manifest.Entry), "../extension/testdata/authworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build auth fixture: %v %s", err, output)
	}
	raw, _ := json.Marshal(manifest)
	for name, data := range map[string][]byte{
		sdk.ManifestFile: raw,
		"package.json":   []byte(`{"name":"window-auth","magpie":{"page":{"title":"Auth","file":"page.html"}}}`),
		"page.html":      []byte("<!doctype html><html><body>Auth</body></html>"),
	} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pagePlugins(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager := extension.NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	previous := extensionWindows
	extensionWindows = func() extensionWindowHost { return manager }
	t.Cleanup(func() { extensionWindows = previous })
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	handler := pageMux()
	listed := do(handler, httptest.NewRequest(http.MethodGet, "/api/plugin-pages", nil))
	var pages []pluginPage
	if json.Unmarshal(listed.Body.Bytes(), &pages) != nil {
		t.Fatal("page metadata is unavailable")
	}
	ref := ""
	for _, page := range pages {
		if page.Name == manifest.ID {
			ref = page.Ref
		}
	}
	if ref == "" || manager.Running(manifest.ID) {
		t.Fatal("auth page metadata failed or started a worker")
	}
	post := func(value any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(value)
		response := do(handler, httptest.NewRequest(http.MethodPost, "/api/plugin-pages/auth", bytes.NewReader(raw)))
		if bytes.Contains(response.Body.Bytes(), []byte("PRIVATE")) || bytes.Contains(response.Body.Bytes(), []byte("continuation")) {
			t.Fatal("private auth material reached the browser")
		}
		return response
	}
	start := map[string]any{"ref": ref, "binding": "signin", "parent": "FRAME", "request": "FRAME:start",
		"operation": "start", "options": map[string]any{"method": "device", "input": map[string]any{"provider": "foreign", "auth": "business-only"}}}
	t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
	if response := post(start); response.Code == http.StatusOK {
		t.Fatal("account discovery policy allowed sign-in")
	}
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	var waiting sdk.WindowAuthFlowView
	response := post(start)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &waiting) != nil || waiting.State != "waiting" {
		t.Fatalf("auth start: status %d %s", response.Code, response.Body.String())
	}
	for _, operation := range []string{"refresh", "logout", "provider.execute", "unknown"} {
		if response := post(map[string]any{"ref": ref, "binding": "signin", "parent": "FRAME", "operation": operation, "flow": waiting.Flow}); response.Code == http.StatusOK {
			t.Fatal("authentication route exposed arbitrary methods:", operation)
		}
	}
	if response := post(map[string]any{"ref": ref, "binding": "signin", "parent": "OTHER", "operation": "finish", "flow": waiting.Flow}); response.Code == http.StatusOK {
		t.Fatal("another frame used the authorization flow")
	}
	if response := post(map[string]any{"ref": ref, "binding": "signin", "parent": "FRAME", "operation": "finish", "flow": waiting.Flow,
		"scope": map[string]string{"provider": "foreign"}}); response.Code == http.StatusOK {
		t.Fatal("page-supplied account scope was accepted")
	}
	deadline := time.After(10 * time.Second)
	for {
		response := post(map[string]any{"ref": ref, "binding": "signin", "parent": "FRAME", "operation": "finish", "flow": waiting.Flow})
		var view sdk.WindowAuthFlowView
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &view) != nil {
			t.Fatalf("auth finish: %d %s", response.Code, response.Body.String())
		}
		if view.State == "complete" {
			if view.Account == nil || view.Account.ID != "chosen" || view.Account.Generation != "1" {
				t.Fatal("HTTP completion did not publish a sanitized account")
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("bounded host polling did not complete")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := manager.Store().GetAccount(manifest.ID, "vendor", "chosen"); err != nil {
		t.Fatal("HTTP success had no committed account:", err)
	}
}
