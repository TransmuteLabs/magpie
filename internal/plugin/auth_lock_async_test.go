package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAuthLockWaitDoesNotFreezeTheHost(t *testing.T) {
	sandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	abs, err := filepath.Abs("testdata/renew/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), []byte(`{"renewco":{"type":"api","key":"test-source"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stopOwner := startSavedAuthOwner(t, ctx)
	defer stopOwner()

	lock := AuthPath() + ".lock"
	old := time.Now().Add(-11 * time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	bunWrite := make(chan error, 1)
	go func() {
		bunWrite <- host.call(ctx, "import", map[string]any{
			"provider": "renewco", "key": "renewco#bun",
			"auth": map[string]any{"type": "api", "key": "test-bun"},
		}, nil)
	}()
	goWrite := make(chan error, 1)
	go func() {
		goWrite <- EditAuth(func(all map[string]json.RawMessage) (bool, error) {
			all["renewco#go"] = json.RawMessage(`{"type":"api","key":"test-go"}`)
			return true, nil
		})
	}()
	defer func() {
		stopOwner()
		cancel()
		for _, write := range []chan error{bunWrite, goWrite} {
			if write != nil {
				<-write
			}
		}
	}()
	poll, stopPoll := context.WithTimeout(ctx, time.Second)
	err = host.call(poll, "gatewayStates", nil, nil)
	stopPoll()
	if err != nil {
		t.Fatalf("unrelated Bun RPC stalled while auth owner was live: %v", err)
	}
	select {
	case err := <-bunWrite:
		bunWrite = nil
		t.Fatalf("Bun writer finished while the saved-auth owner was live: %v", err)
	case err := <-goWrite:
		goWrite = nil
		t.Fatalf("Go writer finished while the saved-auth owner was live: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	stopOwner()
	if err := <-bunWrite; err != nil {
		bunWrite = nil
		t.Fatalf("Bun write after owner death: %v", err)
	}
	bunWrite = nil
	if err := <-goWrite; err != nil {
		goWrite = nil
		t.Fatalf("Go write after owner death: %v", err)
	}
	goWrite = nil
	for _, key := range []string{"renewco", "renewco#go", "renewco#bun"} {
		if authFile(t)[key] == nil {
			t.Fatalf("missing account %q after publishing both changes", key)
		}
	}
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy reservation left behind: %v", err)
	}
}

func startSavedAuthOwner(t *testing.T, ctx context.Context) func() {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSavedAuthLockOwner$")
	cmd.Env = append(os.Environ(), "MAGPIE_TEST_SAVED_AUTH_OWNER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = stdin.Close()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || ready != "holding saved auth\n" {
		t.Fatalf("saved-auth subprocess did not acquire its lock: %q, %v", ready, err)
	}
	return stop
}

func TestSavedAuthLockOwner(t *testing.T) {
	if os.Getenv("MAGPIE_TEST_SAVED_AUTH_OWNER") != "1" {
		return
	}
	err := WithSavedAuth(t.Context(), "renewco", "renewco", func(json.RawMessage) error {
		fmt.Println("holding saved auth")
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuthWriteRespectsAnOlderWritersReservation(t *testing.T) {
	sandbox(t)
	if err := os.MkdirAll(filepath.Dir(AuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	lock := AuthPath() + ".lock"
	if err := os.WriteFile(lock, []byte("old owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-11 * time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	before := []byte(`{"renewco":{"type":"api","key":"test-source"}}`)
	if err := os.WriteFile(AuthPath(), before, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := SignOut(ctx, "renewco", "renewco"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sign-out with the older writer's reservation: %v", err)
	}
	if recorded, err := os.ReadFile(lock); err != nil || string(recorded) != "old owner" {
		t.Fatalf("older writer lost its reservation: %v", err)
	}
	if recorded, err := os.ReadFile(AuthPath()); err != nil || string(recorded) != string(before) {
		t.Fatalf("credentials changed while the older writer held its reservation: %v", err)
	}
	abandoned := lock + ".abandoned"
	if err := os.Rename(lock, abandoned); err != nil {
		t.Fatal(err)
	}
	if err := SignOut(t.Context(), "renewco", "renewco"); err != nil {
		t.Fatal(err)
	}
	if len(authFile(t)) != 0 {
		t.Fatal("sign-out after offline recovery kept the account")
	}
	if recorded, err := os.ReadFile(abandoned); err != nil || string(recorded) != "old owner" {
		t.Fatalf("the moved reservation changed during recovery: %v", err)
	}
}

func TestAuthStoreRejectsAnUpdateFromAnOlderSnapshot(t *testing.T) {
	sandbox(t)
	path := AuthPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"renewco":{"type":"api","key":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := editHostAuth(t.Context(), path, authStoreRequest{ID: 1})
	if err != nil || before.Snapshot == nil {
		t.Fatalf("reading the auth snapshot failed: %v", err)
	}
	if err := SignOut(t.Context(), "renewco", "renewco"); err != nil {
		t.Fatal(err)
	}
	stale, err := editHostAuth(t.Context(), path, authStoreRequest{
		ID: 2, Version: before.Snapshot.Version,
		Records: json.RawMessage(`{"renewco":{"type":"api","key":"stale-renewal"}}`),
	})
	if err != nil || !stale.Retry {
		t.Fatalf("an auth update from the old snapshot was accepted: %v", err)
	}
	if len(authFile(t)) != 0 {
		t.Fatal("the account returned after it was signed out")
	}
}

func TestAuthWriteFailsWithoutAnOSLock(t *testing.T) {
	sandbox(t)
	path := AuthPath()
	if err := os.MkdirAll(path+".write.lock", 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte(`{"renewco":{"type":"api","key":"test-source"}}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SignOut(t.Context(), "renewco", "renewco"); err == nil {
		t.Fatal("sign-out succeeded without an OS writer lock")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("credentials changed while the OS lock was unavailable: %v", err)
	}
}
