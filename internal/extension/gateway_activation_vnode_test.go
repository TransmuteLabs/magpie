//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package extension

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
	"golang.org/x/sys/unix"
)

func TestActivationVnodeFollowsFirstConfig(t *testing.T) {
	root := t.TempDir()
	const id = "fixture"
	state := filepath.Join(root, "private", id, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	watch := newActivationWatch(root)
	watch.open(nil)
	t.Cleanup(watch.close)
	if watch.vnode == nil {
		t.Fatal("fixture watcher unavailable")
	}
	watch.begin()
	watch.followConfig(id)
	watch.finish()

	if err := os.WriteFile(filepath.Join(state, "unrelated"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if waitActivationVnode(t, watch) {
		t.Fatal("an unrelated file scheduled reconciliation")
	}
	name := filepath.Join(state, storeStateName(sdk.Scope{Plugin: id}, moduleConfigKey))
	if err := os.WriteFile(name, []byte(`{"version":"1","value":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitActivationVnode(t, watch) {
		t.Fatal("the first module config did not schedule reconciliation")
	}
}

func TestActivationVnodeBoundsAndReleasesDescriptors(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < activationWatchLimit+32; index++ {
		name := filepath.Join(root, fmt.Sprintf("unrelated-%d", index))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := filepath.Join(root, "catalog.json")
	if err := os.WriteFile(catalog, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	watch := newActivationWatch(root)
	watch.open(nil)
	t.Cleanup(watch.close)
	native, ok := watch.vnode.(*kqueueActivation)
	if !ok {
		t.Fatal("fixture watcher unavailable")
	}
	watch.begin()
	watch.finish()
	if len(native.files) != 3 || native.files[catalog].file == nil {
		t.Fatalf("opened %d paths, want the catalog, root and parent only", len(native.files))
	}
	for index := 0; index < activationWatchLimit; index++ {
		id := fmt.Sprintf("fixture-%d", index)
		state := filepath.Join(root, "private", id, "state")
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		watch.followConfig(id)
	}
	watch.finish()
	if len(native.files) != activationWatchLimit {
		t.Fatalf("opened %d paths, want the %d-path bound", len(native.files), activationWatchLimit)
	}
	fds := []int{native.queue, int(native.reader.Fd()), int(native.writer.Fd())}
	for _, entry := range native.files {
		fds = append(fds, entry.fd)
	}
	watch.close()
	select {
	case <-native.done:
	default:
		t.Fatal("watcher closed before joining its reader")
	}
	for _, fd := range fds {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
			t.Fatalf("descriptor %d survived watcher closure: %v", fd, err)
		}
	}
}

func waitActivationVnode(t *testing.T, watch *activationWatch) bool {
	t.Helper()
	select {
	case _, open := <-watch.vnodeChanges():
		if !open {
			t.Fatal("fixture watcher closed")
		}
		return watch.vnodeRelevant()
	case <-time.After(gatewayActivationFallback / 2):
		t.Fatal("fixture did not observe a filesystem change")
		return false
	}
}
