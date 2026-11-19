//go:build !windows

package cli

import (
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSignalWaitsForGatewayShutdown(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			died := deaths(t, sig)
			entered, release := make(chan os.Signal, 1), make(chan struct{})
			var once sync.Once
			unhold := func() { once.Do(func() { close(release) }) }
			finish := stopGatewayOnSignalAs(func(received os.Signal) {
				entered <- received
				<-release
			})
			defer finish()
			defer unhold()
			profileStopped := make(chan struct{})
			t.Cleanup(endProbesOnSignal(func() { close(profileStopped) }))
			send(t, sig)
			select {
			case got := <-entered:
				if got != sig {
					t.Fatalf("gateway drained for %v, want %v", got, sig)
				}
			case got := <-died:
				t.Fatalf("died of %v before draining the gateway and saving its sessions", got)
			case <-time.After(5 * time.Second):
				t.Fatal("signal never reached gateway shutdown")
			}
			select {
			case got := <-died:
				t.Fatalf("died of %v while gateway shutdown was held", got)
			case <-profileStopped:
				t.Fatal("profile stopped before the gateway drained")
			default:
			}

			returned := make(chan struct{})
			go func() { finish(); close(returned) }()
			select {
			case <-returned:
				t.Fatal("command returned while the signal was still draining")
			case <-time.After(20 * time.Millisecond):
			}
			unhold()
			diesOf(t, died, sig)
			select {
			case <-profileStopped:
			default:
				t.Fatal("died before stopping the profile")
			}
			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				t.Fatal("command did not join completed signal shutdown")
			}
		})
	}
}
