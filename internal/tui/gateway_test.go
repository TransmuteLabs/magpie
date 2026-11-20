package tui

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/gateway"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal(what)
}

// With no magpie serving, the terminal serves the gateway while it is open
// and lets go as it quits (gnayiab on X: ECONNREFUSED in WSL with only
// magpie tui).
func TestTUIServesTheGatewayWhenNoneDoes(t *testing.T) {
	t.Setenv("MAGPIE_ADDR", freeAddr(t))
	if gateway.Running() {
		t.Fatal("a gateway answers before the terminal started")
	}
	ctx, stop := context.WithCancel(context.Background())
	wait := serveGateway(ctx)
	defer func() { stop(); wait() }()
	waitFor(t, "no gateway while the terminal is open", gateway.Running)
	if !gw.here.Load() {
		t.Fatal("not marked as served here")
	}
	if s := gatewayStatus(); !strings.Contains(s, gateway.URL()) || !strings.Contains(s, "served while magpie tui is open") {
		t.Fatalf("status %q", s)
	}
	stop()
	wait()
	waitFor(t, "the gateway still answers after the terminal quit", func() bool { return !gateway.Running() })
	if gw.here.Load() || gatewayStatus() != "" {
		t.Fatalf("still marked as served here: %q", gatewayStatus())
	}
}

// A magpie that has the gateway keeps it; the terminal takes it up once
// that one is gone.
func TestTUILeavesTheGatewayToAnotherAndTakesItUp(t *testing.T) {
	var probes atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"magpie"}`)
		probes.Add(1)
	}))
	defer other.Close()
	t.Setenv("MAGPIE_ADDR", other.Listener.Addr().String())
	old := gatewayWatch
	gatewayWatch = 100 * time.Millisecond
	defer func() { gatewayWatch = old }()

	ctx, stop := context.WithCancel(context.Background())
	wait := serveGateway(ctx)
	defer func() { stop(); wait() }()
	waitFor(t, "the terminal did not look again while the other gateway was serving", func() bool {
		return probes.Load() >= 2
	})
	if gw.here.Load() || gatewayStatus() != "" {
		t.Fatalf("took the gateway another magpie serves: %q", gatewayStatus())
	}

	other.Close()
	waitFor(t, "the terminal didn't take the gateway up once the other was gone", func() bool {
		return gw.here.Load() && gateway.Running()
	})
}
