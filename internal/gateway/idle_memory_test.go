package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestUnservedGatewayDoesNotArmIdleMemory(t *testing.T) {
	fresh(t)
	server := New()
	server.idle.rest = time.Hour
	t.Cleanup(server.idle.stop)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/unknown", nil))
	if response.Code != http.StatusNotFound || server.Busy().Last.IsZero() {
		t.Fatalf("POST did not pass through the counted gateway handler: status %d, busy %+v", response.Code, server.Busy())
	}
	if idleMemoryArmed(server) {
		t.Fatal("a handler without a serving gateway armed idle memory reclamation")
	}
}

func TestStoppedGatewayCancelsIdleMemory(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:0")
	server := New()
	server.idle.rest = time.Hour
	t.Cleanup(server.idle.stop)
	stop, done := listening(t, server)
	waitStopped := sync.OnceFunc(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("gateway failed: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("gateway did not stop")
		}
	})
	t.Cleanup(waitStopped)

	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Post("http://"+server.Addr()+"/v1/unknown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("POST status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	waitFor(t, "idle memory timer", func() bool { return idleMemoryArmed(server) })

	waitStopped()
	if idleMemoryArmed(server) {
		t.Error("the stopped gateway kept its idle memory timer")
	}
	server.idle.stop()
	server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/unknown", nil))
	if idleMemoryArmed(server) {
		t.Error("a late request armed idle memory reclamation after shutdown")
	}
}

func idleMemoryArmed(server *Server) bool {
	server.idle.mu.Lock()
	defer server.idle.mu.Unlock()
	return server.idle.due != nil
}

// Once the gateway rests, what its requests left in the heap goes back to
// the system, once per rest, and never while a request is in flight
// (#3472).
func TestRestingGatewayHandsMemoryBack(t *testing.T) {
	fresh(t)
	server := New()
	synctest.Test(t, func(t *testing.T) {
		defer server.idle.start(t.Context())()
		var released atomic.Int32
		server.idle.release = func() { released.Add(1) }
		hold, held := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() { close(hold) })
		defer unblock()
		handler := server.counted(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/hold" {
				close(held)
				<-hold
			}
		}))
		post := func(path string) {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
		}
		wantReleased := func(want int32) {
			t.Helper()
			synctest.Wait()
			if got := released.Load(); got != want {
				t.Fatalf("released %d times, want %d", got, want)
			}
		}

		post("/work")
		time.Sleep(idleRelease / 2)
		post("/work")
		time.Sleep(idleRelease / 2)
		wantReleased(0)
		time.Sleep(idleRelease / 2)
		wantReleased(1)

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if idleMemoryArmed(server) {
			t.Fatal("a GET armed a release")
		}

		post("/work")
		go post("/hold")
		<-held
		time.Sleep(idleRelease)
		wantReleased(1)
		unblock()
		synctest.Wait()
		time.Sleep(idleRelease)
		wantReleased(2)
		time.Sleep(idleRelease)
		wantReleased(2)
	})
}

func TestIdleReleaseDiesDuringGatewayDrain(t *testing.T) {
	for _, mode := range []string{"cancel", "handoff"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server := &Server{}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				defer server.idle.start(ctx)()
				var released atomic.Int32
				server.idle.release = func() { released.Add(1) }
				server.armRelease()
				if !idleMemoryArmed(server) {
					t.Fatal("the serving gateway did not arm idle reclamation")
				}
				if mode == "cancel" {
					cancel()
				} else {
					server.HandOff()
				}
				time.Sleep(idleRelease)
				synctest.Wait()
				server.armRelease()
				if idleMemoryArmed(server) || released.Load() != 0 {
					t.Fatalf("the draining gateway kept idle reclamation: armed=%t, releases=%d", idleMemoryArmed(server), released.Load())
				}
			})
		})
	}
}
