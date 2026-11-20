package extension

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type actionManifestBarrier struct {
	*actionTestRuntime
	seen chan struct{}
	once sync.Once
}

func (runtime *actionManifestBarrier) Manifest(plugin string) (sdk.Manifest, error) {
	manifest, err := runtime.actionTestRuntime.Manifest(plugin)
	runtime.once.Do(func() { close(runtime.seen) })
	return manifest, err
}

func TestActionsRecheckModuleAuthorityAfterWaitingForWriteLock(t *testing.T) {
	actions, host, policy := actionFixture(t)
	plan := actionTestPlan(t, actions, policy)
	root, err := os.OpenRoot(policy.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	unlock, err := actionLock(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	defer release.Do(unlock)
	barrier := &actionManifestBarrier{actionTestRuntime: host, seen: make(chan struct{})}
	actions.host = barrier
	done := make(chan error, 1)
	go func() {
		_, err := plan.Execute(t.Context(), plan.Preview().Revision)
		done <- err
	}()
	<-barrier.seen
	host.mu.Lock()
	host.manifest.Version = "2.0.0"
	host.mu.Unlock()
	release.Do(unlock)
	if err := <-done; !errors.Is(err, ErrActionChanged) {
		t.Fatal("waiting action wrote under replaced module authority", err)
	}
	for _, name := range []string{"a.json", "b.json"} {
		if raw := actionTestRead(t, filepath.Join(policy.Root, name)); string(raw) != "before-"+name {
			t.Fatal("module replaced while action waited still changed files", name)
		}
	}
}

func TestActionsModuleReplacementDuringWriteRollsBackBeforeNextFile(t *testing.T) {
	actions, host, policy := actionFixture(t)
	host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
		return sdk.AgentPlan{Files: []sdk.FileChange{
			{Path: "a.json", Body: []byte("after-a")},
			{Path: "b.json", Body: []byte("after-b")},
		}}, nil
	}
	plan := actionTestPlan(t, actions, policy)
	written := []string{}
	actions.write = func(root *os.Root, name string, raw []byte, mode fs.FileMode, check func() error) (fs.FileInfo, error) {
		written = append(written, name)
		info, err := actionWriteChecked(root, name, raw, mode, check)
		if err != nil {
			return info, err
		}
		host.mu.Lock()
		host.manifest.Version = "2.0.0"
		host.mu.Unlock()
		return info, nil
	}
	if _, err := plan.Execute(t.Context(), plan.Preview().Revision); !errors.Is(err, ErrActionChanged) {
		t.Fatal("module replacement during action was ignored", err)
	}
	if len(written) != 1 || written[0] != "a.json" {
		t.Fatal("action wrote another file after module authority changed", written)
	}
	for _, name := range []string{"a.json", "b.json"} {
		if raw := actionTestRead(t, filepath.Join(policy.Root, name)); string(raw) != "before-"+name {
			t.Fatal("replacement did not roll back an owned preimage", name)
		}
	}
}
