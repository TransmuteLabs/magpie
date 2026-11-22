package gateway

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
)

func TestPluginGatewaySignInObservation(t *testing.T) {
	for _, outcome := range []struct {
		name   string
		status int
	}{
		{"healthy_stream", http.StatusOK},
		{"canceled_lapse", http.StatusUnauthorized},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			reply := make(chan struct{})
			id := besideFake(t, "fakeco", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case started <- struct{}{}:
				case <-r.Context().Done():
					return
				}
				select {
				case <-reply:
				case <-r.Context().Done():
					return
				}
				if outcome.status == http.StatusUnauthorized {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(outcome.status)
					fmt.Fprint(w, `{"error":{"message":"invalid token"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, sse(
					`data: {"id":"c","object":"chat.completion.chunk","model":"fake-1","choices":[{"index":0,"delta":{"role":"assistant","content":"streamed from plugin"},"finish_reason":null}]}`,
					`data: {"id":"c","object":"chat.completion.chunk","model":"fake-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
					`data: [DONE]`))
			}))
			logins := filepath.Join(filepath.Dir(provider.Path()), "logins.json")
			before, err := os.ReadFile(logins)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			body := `{"model":"` + id + `/fake-1","messages":[{"role":"user","content":"hello"}],"stream":true}`
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rec := httptest.NewRecorder()
			handler := New().Handler()
			done := make(chan struct{})
			go func() {
				defer close(done)
				handler.ServeHTTP(rec, req)
			}()
			t.Cleanup(func() {
				cancel()
				<-done
			})
			select {
			case <-started:
			case <-done:
				t.Fatalf("gateway returned before reaching the plugin vendor: %d %s", rec.Code, rec.Body)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}

			unlock, held := filelock.Lock(logins+".lock", time.Second)
			if !held {
				t.Fatal("could not hold the logins lock")
			}
			defer func() { unlock() }()
			close(reply)
			if outcome.status == http.StatusUnauthorized {
				waitPluginGatewayLoginLock(t, done)
				cancel()
			}
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Errorf("gateway response waited for the externally held logins lock:\n%s", pluginGatewayLoginLockStack(make([]byte, 256<<10)))
			}
			unlock()
			unlock = func() {}
			<-done
			if outcome.status == http.StatusOK {
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "streamed from plugin") || !strings.Contains(rec.Body.String(), "data: [DONE]") {
					t.Fatalf("plugin stream = %d %s", rec.Code, rec.Body)
				}
			}
			if after, err := os.ReadFile(logins); err != nil || !bytes.Equal(after, before) {
				t.Errorf("the response changed the saved login: %v", err)
			}
		})
	}
}

func TestPluginGatewayRefusalUsesDispatchedSignIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	started, reply := make(chan struct{}, 1), make(chan struct{})
	id := besideFake(t, "fakeco", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-reply:
		case <-r.Context().Done():
			return
		}
		w.Header().Set(provider.SignInHeader, "expired")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(
			`data: {"id":"fixture","object":"chat.completion.chunk","model":"fake-1","choices":[{"index":0,"delta":{"role":"assistant","content":"fixture answer"},"finish_reason":null}]}`,
			`data: {"id":"fixture","object":"chat.completion.chunk","model":"fake-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`))
	}))
	accounts := plugin.Auths("fakeco")
	if len(accounts) != 1 {
		t.Fatalf("want one saved sign-in, got %d", len(accounts))
	}
	handler := New().Handler()
	request := func() *httptest.ResponseRecorder {
		body := `{"model":"` + id + `/fake-1","messages":[{"role":"user","content":"hello"}],"stream":true}`
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		return rec
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- request()
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-started:
	case rec := <-result:
		t.Fatalf("gateway returned before dispatch: %d %s", rec.Code, rec.Body)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	before, err := plugin.CurrentAuthEpochs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for slot, credentials := range accounts {
		if err := plugin.Restore(ctx, "fakeco", slot, credentials); err != nil {
			t.Fatal(err)
		}
		after, err := plugin.CurrentAuthEpochs(ctx)
		if err != nil || after[slot] == "" || after[slot] == before[slot] {
			t.Fatalf("restore did not replace the saved sign-in: %v", err)
		}
	}
	close(reply)
	check := func(rec *httptest.ResponseRecorder, wantLapse bool) {
		t.Helper()
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fixture answer") || rec.Header().Get(provider.SignInHeader) != "" {
			t.Fatalf("gateway response = %d %s (sign-in header %q)", rec.Code, rec.Body, rec.Header().Get(provider.SignInHeader))
		}
		logins, err := os.ReadFile(filepath.Join(filepath.Dir(provider.Path()), "logins.json"))
		if err != nil {
			t.Fatal(err)
		}
		if got := bytes.Contains(logins, []byte("sign-in has expired")); got != wantLapse {
			t.Errorf("saved sign-in lapsed = %t, want %t", got, wantLapse)
		}
	}
	select {
	case rec := <-result:
		check(rec, false)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	check(request(), true)
}

func waitPluginGatewayLoginLock(t *testing.T, done <-chan struct{}) {
	t.Helper()
	stacks := make([]byte, 256<<10)
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if stack := pluginGatewayLoginLockStack(stacks); bytes.Contains(stack, []byte("provider.pluginFetch(")) {
			return
		}
		select {
		case <-done:
			t.Fatal("gateway returned before attempting to record the lapse")
		case <-deadline.C:
			t.Fatal("gateway never reached the logins lock after the vendor's 401")
		case <-tick.C:
		}
	}
}

func pluginGatewayLoginLockStack(stacks []byte) []byte {
	for stack := range bytes.SplitSeq(stacks[:runtime.Stack(stacks, true)], []byte("\n\n")) {
		if bytes.Contains(stack, []byte("TestPluginGatewaySignInObservation.func")) && bytes.Contains(stack, []byte("filelock.lockPath(")) {
			return stack
		}
	}
	return nil
}
