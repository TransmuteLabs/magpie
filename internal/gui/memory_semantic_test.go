package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/memory"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestMemorySemanticWindowUsesTheLinkedSwitch(t *testing.T) {
	config, cache := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("XDG_CACHE_HOME", cache)
	path := filepath.Join(appdir.Config(), "memory-semantic.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"on":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	memorySemanticRoutes(mux)
	read := func(method, body string) sdk.MemorySemanticStatus {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), method, "/api/memory/semantic", strings.NewReader(body)))
		var status sdk.MemorySemanticStatus
		if response.Code != http.StatusOK {
			t.Fatalf("%s semantic status: %d %s", method, response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	if status := read(http.MethodGet, ""); !status.On || status.Installed || status.Size == 0 {
		t.Fatalf("window lost the saved switch or model status: %+v", status)
	}
	if status := read(http.MethodPost, `{"on":false}`); status.On || memory.Semantic().On() {
		t.Fatalf("window disable did not reach the Memory owner: %+v", status)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/memory/rank", strings.NewReader(`{"q":"publish"}`)))
	var rank struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rank); response.Code != http.StatusOK || err != nil || rank.IDs != nil {
		t.Fatalf("disabled rank returned %d %s (%v)", response.Code, response.Body.String(), err)
	}
}
