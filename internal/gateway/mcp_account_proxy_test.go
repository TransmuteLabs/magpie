package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/mcpauth/mcpauthtest"
)

// A stale or late OAuth record cannot bypass a current account binding.
func TestMCPProxyRejectsBoundAccount(t *testing.T) {
	fresh(t)
	f := mcpauthtest.New(t)
	f.SignIn(t, "neon")
	old := MCPAccountBound
	t.Cleanup(func() { MCPAccountBound = old })
	var state atomic.Int32
	MCPAccountBound = func(name string) (bool, error) {
		if name != "neon" {
			t.Errorf("looked up %q", name)
		}
		switch state.Load() {
		case 1:
			return true, nil
		case 2:
			return false, errors.New("library unreadable")
		default:
			return false, nil
		}
	}
	h := lanGuard(New().Handler())
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/mcp/neon", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
		r.RemoteAddr, r.Host = here, "127.0.0.1:3425"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call(); w.Code != http.StatusOK || len(f.Calls) != 1 {
		t.Fatalf("unbound relay changed: %d, calls %v", w.Code, f.Calls)
	}
	state.Store(1)
	if w := call(); w.Code != http.StatusForbidden || len(f.Calls) != 1 {
		t.Fatalf("bound server reached stale OAuth relay: %d, calls %v", w.Code, f.Calls)
	}
	state.Store(2)
	if w := call(); w.Code != http.StatusServiceUnavailable || len(f.Calls) != 1 {
		t.Fatalf("unreadable binding failed open: %d, calls %v", w.Code, f.Calls)
	}
	state.Store(0)
	if w := call(); w.Code != http.StatusOK || len(f.Calls) != 2 {
		t.Fatalf("unbind did not restore the ordinary relay: %d, calls %v", w.Code, f.Calls)
	}
}
