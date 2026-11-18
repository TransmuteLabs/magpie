package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sessions"
)

func TestSessionsMCPReadRetainsCallerCancellation(t *testing.T) {
	fresh(t)
	claude := filepath.Join(os.Getenv("HOME"), ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	sessions.Reset()
	t.Cleanup(sessions.Reset)
	path := filepath.Join(claude, "projects", "-work-app", "held.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"privatecancellationword"},"cwd":"/work/app","sessionId":"held","uuid":"u1"}`,
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := lanGuard(New().Handler())
	text, failed := sessionsCall(t, handler, sessionsReadTool, `{"session":"claude:held"}`)
	if failed || !strings.Contains(text, "privatecancellationword") {
		t.Fatalf("ordinary read fixture: failed=%v text=%q", failed, text)
	}
	for _, find := range []string{"", "privatecancellationword"} {
		t.Run("find="+find, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_session","arguments":{"session":"claude:held","find":"` + find + `"}}}`
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			request := httptest.NewRequest(http.MethodPost, SessionsMCPPath, strings.NewReader(body)).WithContext(ctx)
			request.RemoteAddr = "127.0.0.1:50000"
			request.Host = "127.0.0.1:3425"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			reply := readMCP(t, response)
			if reply.Error != nil || len(reply.Result.Content) != 1 {
				t.Fatalf("MCP cancellation reply: %+v", reply)
			}
			text := reply.Result.Content[0].Text
			if !reply.Result.IsError || !strings.Contains(text, context.Canceled.Error()) ||
				strings.Contains(text, "privatecancellationword") {
				t.Fatalf("canceled MCP read published history: failed=%v text=%q", reply.Result.IsError, text)
			}
		})
	}
}
