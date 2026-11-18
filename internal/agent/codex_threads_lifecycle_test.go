package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

func TestCodexThreadsCapturedCallbackRemainsActiveAfterPendingRelease(t *testing.T) {
	home, _ := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, "")
	dir := filepath.Join(home, ".codex")
	const id = "019a0000-0000-7000-8000-0000000000f8"
	thread := codexThreadOn(t, dir, id, "magpie")
	packThread(t, thread)
	locks := filepath.Join(dir, "thread-writer-locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(locks, id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if held, err := filelock.TryLock(file); !held || err != nil {
		t.Fatal("lock fixture thread", held, err)
	}
	var unlock sync.Once
	defer unlock.Do(func() { filelock.Unlock(file) })
	watchCodexThreads(t)
	entered, release := make(chan struct{}), make(chan struct{})
	newCallback := make(chan struct{}, 1)
	var released sync.Once
	defer released.Do(func() { close(release) })
	codexThreadsMu.Lock()
	codexThreadsAgain = func() {
		close(entered)
		<-release
	}
	codexThreadsMu.Unlock()
	codexThreadsLater(dir, "magpie")
	codexThreadsMu.Lock()
	codexThreadsAgain = func() { newCallback <- struct{}{} }
	codexThreadsMu.Unlock()
	unlock.Do(func() { filelock.Unlock(file) })
	select {
	case <-entered:
	case <-newCallback:
		t.Fatal("existing worker called a replacement callback instead of its captured owner")
	case <-time.After(5 * time.Second):
		t.Fatal("captured callback did not run after the fixture lock was released")
	}
	codexThreadsMu.Lock()
	active, pending := codexThreadsActive, codexThreadsPending[dir+"\x00magpie"]
	codexThreadsMu.Unlock()
	if pending || active != 1 {
		t.Fatal("pending claim was not released before the still-active callback", pending, active)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observed, joined := make(chan int, 1), make(chan error, 1)
	var first sync.Once
	go func() {
		joined <- waitCodexThreadsIdle(ctx, func(active int) { first.Do(func() { observed <- active }) })
	}()
	if count := <-observed; count != 1 {
		t.Fatal("join mistook an empty pending map for a completed callback", count)
	}
	released.Do(func() { close(release) })
	if err := <-joined; err != nil {
		t.Fatal("join did not finish after the captured callback was released", err)
	}
	codexThreadsMu.Lock()
	active = codexThreadsActive
	codexThreadsMu.Unlock()
	if active != 0 {
		t.Fatal("finished worker retained an active lifecycle claim", active)
	}
}
