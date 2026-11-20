package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
)

func TestSDKObserversImportDoesNotRegister(t *testing.T) {
	for _, observer := range Observers() {
		switch observer.(type) {
		case *extensionLedgerObserver, *extensionRequestObserver:
			t.Fatal("importing usage registered an SDK observer without a serving owner")
		}
	}
}

func TestSDKObserverDiscoveryStopsWithListener(t *testing.T) {
	root := t.TempDir()
	manager := extensionhost.NewManager(root)
	unlock, held := filelock.Lock(filepath.Join(root, "catalog.lock"), time.Second)
	if !held {
		t.Fatal("could not hold the catalog lock")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		stop := ServeExtensionLedgerObservers(ctx, manager)
		stop()
	}()
	defer func() {
		cancel()
		unlock()
		<-done
	}()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		extensionLedgerOwners.Lock()
		owner := extensionLedgerOwners.byManager[manager]
		extensionLedgerOwners.Unlock()
		if owner != nil {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("observer discovery did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled listener waited for the catalog lock")
	}
}
