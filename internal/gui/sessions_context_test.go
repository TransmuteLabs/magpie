package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/sessions"
)

func TestSessionReadsHonorRequestCancellation(t *testing.T) {
	home := sandboxHome(t)
	sessions.Reset()
	t.Cleanup(sessions.Reset)
	const id = "11111111-2222-3333-4444-555555555555"
	project := filepath.Join(home, ".claude", "projects", "-work-app")
	files := map[string]string{
		id + ".jsonl": `{"type":"user","message":{"role":"user","content":"private session content"},"uuid":"u1","timestamp":"2026-09-20T10:00:00.000Z","cwd":"/work/app","sessionId":"` + id + `"}` + "\n",
		filepath.Join(id, "subagents", "agent-a1.jsonl"): `{"isSidechain":true,"agentId":"a1","type":"user","message":{"role":"user","content":"private subagent content"},"uuid":"s1","timestamp":"2026-09-20T10:01:00.000Z","cwd":"/work/app","sessionId":"` + id + `"}` + "\n",
	}
	for name, content := range files {
		path := filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	sessionRoutes(mux, folderOnly{})
	query := "?agent=claude&id=" + id
	for _, path := range []string{"/api/sessions/transcript" + query, "/api/sessions/transcript" + query + "&sub=a1", "/api/sessions/markdown" + query, "/api/sessions/export" + query} {
		t.Run(path, func(t *testing.T) {
			method := http.MethodGet
			if strings.HasPrefix(path, "/api/sessions/export") {
				method = http.MethodPost
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			request := httptest.NewRequestWithContext(ctx, method, path, nil)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "private") {
				t.Fatalf("canceled session read published content: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
