package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestZCodeResetUsageRouteNamesRemoteOwnerAndWindow(t *testing.T) {
	sandboxHome(t)
	var mu sync.Mutex
	var paths []string
	var received []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (r.URL.Path != provider.RemoteZCodeAccountPath+"/scoped" && r.URL.Path != provider.RemoteZCodeResetPath+"/scoped") {
			t.Errorf("unexpected remote request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		paths, received = append(paths, r.URL.Path), append(received, body)
		mu.Unlock()
		if !strings.Contains(r.Header.Get("Authorization"), "fixture-key") && r.Header.Get("x-api-key") != "fixture-key" {
			t.Error("gateway key was not sent to remote owner")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == provider.RemoteZCodeAccountPath+"/scoped" {
			_, _ = w.Write([]byte(`{"context":{"scope":"scope-a","generation":"epoch-a","origin":"https://zcode.z.ai","family":"zai","userId":"user-a","kind":"individual"},"data":{"accountSelector":"b@example.com"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"source":"zcode","available":true,"pending":"unconfirmed","pendingWindow":"week"}`))
	}))
	t.Cleanup(server.Close)
	if err := provider.Save(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Key: "fixture-key", Chat: server.URL}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	usageRoutes(mux, folderOnly{})
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/usage/zcode-account", bytes.NewBufferString(body)))
		return w
	}
	for _, body := range []string{
		`{"provider":"office/zcode","operation":"reset.use","params":{"window":"week"},"scope":"scope-a","generation":"epoch-a"}`,
		`{"provider":"office/zcode","account":"b@example.com","operation":"reset.use","params":{"window":"month"},"scope":"scope-a","generation":"epoch-a"}`,
		`{"provider":"office/zcode","account":"b@example.com","operation":"reset.use","params":{"window":"week"}}`,
		`{"provider":"office/zcode","account":"b@example.com","operation":"reset.resolve","params":{"window":"week"},"scope":"scope-a","generation":"epoch-a"}`,
	} {
		if w := post(body); w.Code < 400 {
			t.Errorf("invalid action accepted: %d %s", w.Code, w.Body.String())
		}
	}
	w := post(`{"provider":"office/zcode","account":"b@example.com","operation":"reset.use","params":{"window":"week"},"scope":"scope-a","generation":"epoch-a"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"pending":"unconfirmed"`) || !strings.Contains(w.Body.String(), `"context":`) || strings.Contains(w.Body.String(), "fixture-key") {
		t.Fatalf("remote response: %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 || paths[0] != provider.RemoteZCodeAccountPath+"/scoped" || paths[1] != provider.RemoteZCodeResetPath+"/scoped" {
		t.Fatalf("remote paths=%v bodies=%+v", paths, received)
	}
	for _, body := range received {
		if body["provider"] != "zcode" || body["account"] != "b@example.com" || body["scope"] != "scope-a" || body["generation"] != "epoch-a" {
			t.Fatalf("remote owner received %+v", body)
		}
	}
	if received[1]["window"] != "week" {
		t.Fatalf("remote reset window=%v", received[1]["window"])
	}
}
