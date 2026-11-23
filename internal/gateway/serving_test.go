package gateway

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/testenv"
)

// A Kubernetes rolling update runs the new pod beside the old one on one
// volume, each gateway on a port of its own. The new one answers requests
// at once, but the work only the serving magpie does (renewing sign-ins,
// check-ins, WhileServing) waits until the old one's gateway ends: two
// never do it at once. Without the serving lock both did.
func TestServingWorkWaitsForTheOtherMagpie(t *testing.T) {
	fresh(t)
	oldRetry := servingRetry
	servingRetry = 10 * time.Millisecond
	t.Cleanup(func() { servingRetry = oldRetry })
	var working atomic.Int32
	var most atomic.Int32
	var began atomic.Int32
	oldWork := WhileServing
	WhileServing = append([]func(context.Context){func(ctx context.Context) {
		began.Add(1)
		n := working.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		<-ctx.Done()
		working.Add(-1)
	}}, oldWork...)
	t.Cleanup(func() { WhileServing = oldWork })

	t.Setenv("MAGPIE_ADDR", "127.0.0.1:0")
	old := New()
	stopOld, oldDone := listening(t, old)
	defer stopOld()
	waitFor(t, "the first gateway's work to begin", func() bool { return began.Load() == 1 })

	t.Setenv("MAGPIE_ADDR", "127.0.0.1:0")
	next := New()
	stopNext, nextDone := listening(t, next)
	defer stopNext()
	next.lnMu.Lock()
	addr := next.ln.Addr().String()
	next.lnMu.Unlock()
	// the new one answers while the old one serves
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the second gateway answered %d while the first served", resp.StatusCode)
	}
	// many retries' worth: its work hasn't begun
	time.Sleep(30 * servingRetry)
	if n := began.Load(); n != 1 {
		t.Fatalf("the serving work began %d times with the first gateway still serving", n)
	}

	stopOld()
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second gateway's work to begin once the first ended", func() bool { return began.Load() == 2 })
	if m := most.Load(); m != 1 {
		t.Fatalf("the serving work ran in %d gateways at once", m)
	}
	stopNext()
	if err := <-nextDone; err != nil {
		t.Fatal(err)
	}
}

// A gateway that ends while it waits for the lock never does the work,
// and lets go of nothing it doesn't hold.
func TestServingWaitEndsWithItsGateway(t *testing.T) {
	fresh(t)
	oldRetry := servingRetry
	servingRetry = 10 * time.Millisecond
	t.Cleanup(func() { servingRetry = oldRetry })
	unlock, held := filelock.Lock(servingLockPath(), 0)
	if !held {
		t.Skip("no file locks here")
	}
	var began atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	release := holdServing(ctx, servingLockPath(), func() func() {
		began.Add(1)
		return nil
	})
	time.Sleep(10 * servingRetry)
	cancel()
	release()
	unlock()
	time.Sleep(10 * servingRetry)
	if n := began.Load(); n != 0 {
		t.Fatalf("the work began %d times in a gateway that ended waiting", n)
	}
	// and the lock is free for the next
	again := holdServing(context.Background(), servingLockPath(), func() func() {
		began.Add(1)
		return nil
	})
	defer again()
	if n := began.Load(); n != 1 {
		t.Fatalf("a free lock: the work began %d times, want at once", n)
	}
}

func TestServingReleaseJoinsOwnedWork(t *testing.T) {
	fresh(t)
	stopping := make(chan struct{})
	finish := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(finish) })
	path := servingLockPath()
	release := holdServing(t.Context(), path, func() func() {
		return func() {
			close(stopping)
			<-finish
		}
	})
	released := make(chan struct{})
	go func() {
		defer close(released)
		release()
	}()
	defer func() {
		unblock()
		<-released
	}()
	select {
	case <-stopping:
	case <-released:
		t.Fatal("the serving lock was released before its owned work stopped")
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not reach the owned work")
	}
	if unlock, held := filelock.Lock(path, 0); held {
		unlock()
		t.Fatal("a second gateway acquired the serving lock during shutdown")
	}
	unblock()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not release the serving lock")
	}
	if unlock, held := filelock.Lock(path, 0); held {
		unlock()
	} else {
		t.Fatal("the serving lock stayed held after shutdown")
	}
}

func TestServingCommunityListingWaitsForOwnership(t *testing.T) {
	bun := testenv.Bun(t)
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:0")
	t.Setenv("MAGPIE_BUN", bun)
	t.Cleanup(plugin.Settle)
	oldRetry := servingRetry
	servingRetry = 10 * time.Millisecond
	t.Cleanup(func() { servingRetry = oldRetry })

	path := servingLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, held := filelock.Lock(path, 0)
	if !held {
		t.Fatal("could not acquire the other gateway's serving lock")
	}
	release := sync.OnceFunc(unlock)
	defer release()
	spec, err := filepath.Abs("../plugin/testdata/fake/index.js")
	if err != nil {
		t.Fatal(err)
	}
	listPath := plugin.ListPath()
	writePlugins := func(data string) {
		t.Helper()
		if err := os.WriteFile(listPath, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		filememo.Forget()
	}
	writePlugins(`{"plugins":[]}`)
	plugin.UseCached(nil)
	stop, done := listening(t, New())
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	writePlugins(`{"plugins":[{"spec":` + strconv.Quote(spec) + `}]}`)
	plugin.Cached()
	plugin.Refreshed()
	if plugin.Running() {
		t.Fatal("the waiting gateway started a host to list providers")
	}
	if _, listed := plugin.ListedAt(); listed {
		t.Fatal("the waiting gateway published the provider list")
	}

	release()
	waitFor(t, "the new owner to publish the provider list", func() bool {
		_, listed := plugin.ListedAt()
		return listed
	})
	ps, known := plugin.Listed()
	if !known || len(ps) != 1 || ps[0].ID != "fakeco" {
		t.Fatalf("the new owner's provider list: %+v, known %v", ps, known)
	}
}
