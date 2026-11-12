package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/steady"
)

func authLeaseHost(t *testing.T) (*host, context.Context) {
	t.Helper()
	sandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	h, err := get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return h, ctx
}

func authLeaseAcquiring(h *host) bool {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	return h.authAcquiring
}

func authLeaseToken(h *host) string {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	return h.authToken
}

func authLeaseIdle(h *host) bool {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	return !h.authAcquiring && h.authUnlock == nil && !h.authCommitting
}

func authLeaseImport(h *host, ctx context.Context, key string) error {
	return h.call(ctx, "import", map[string]any{
		"provider": "fakeco", "key": key,
		"auth": map[string]any{"type": "api", "key": key},
	}, nil)
}

func authLeaseReleaseHolder(t *testing.T, cmd *exec.Cmd, in io.Writer) {
	t.Helper()
	if _, err := io.WriteString(in, "release\n"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("holder: %v", err)
	}
}

// A complete Bun host waits on a separate Go process, but its RPC heartbeat
// remains live. Once released, the host commits over the fresh snapshot.
func TestAuthLeaseHostWaitKeepsHeartbeatAndUpdates(t *testing.T) {
	h, ctx := authLeaseHost(t)
	cmd, in := authPublicHolder(t)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(AuthPath()+".flock", old, old); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- authLeaseImport(h, ctx, "fakeco#host") }()
	waitFor(t, "host acquisition", func() bool { return authLeaseAcquiring(h) })
	beat, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := h.call(beat, "gatewayStates", nil, nil); err != nil {
		t.Fatalf("host heartbeat while waiting: %v", err)
	}
	select {
	case err := <-result:
		t.Fatalf("host changed auth while holder lived: %v", err)
	default:
	}
	authLeaseReleaseHolder(t, cmd, in)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host did not complete after release")
	}
	all, err := AuthEntries()
	if err != nil || len(all["parked"]) == 0 || len(all["fakeco#host"]) == 0 {
		t.Fatalf("updates after release = %v, %v", all, err)
	}
}

// A draining host must continue reading Core's lease replies. Closing stdin
// first strands the queued import even when the holder soon releases.
func TestAuthLeaseStopDrainsPendingChange(t *testing.T) {
	h, ctx := authLeaseHost(t)
	cmd, in := authPublicHolder(t)
	result := make(chan error, 1)
	go func() { result <- authLeaseImport(h, ctx, "fakeco#draining") }()
	waitFor(t, "host acquisition", func() bool { return authLeaseAcquiring(h) })
	written := make(chan error, 1)
	if err := h.enqueueDone(ctx, map[string]string{"method": "shutdown"}, written); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown control was not written")
	}
	stopping := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		close(stopping)
		h.stop()
		close(stopped)
	}()
	<-stopping
	time.Sleep(100 * time.Millisecond)
	authLeaseReleaseHolder(t, cmd, in)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("pending change failed during graceful stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending change did not finish during graceful stop")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("host did not stop after draining")
	}
	all, err := AuthEntries()
	if err != nil || len(all["parked"]) == 0 || len(all["fakeco#draining"]) == 0 {
		t.Fatalf("graceful stop lost a change: %v, %v", all, err)
	}
}

func TestAuthLeaseCancelledAcquisitionDoesNotWrite(t *testing.T) {
	h, ctx := authLeaseHost(t)
	cmd, in := authPublicHolder(t)
	callCtx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { result <- authLeaseImport(h, callCtx, "fakeco#cancelled") }()
	waitFor(t, "host acquisition", func() bool { return authLeaseAcquiring(h) })
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled import: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled import did not return")
	}
	authLeaseReleaseHolder(t, cmd, in)
	waitFor(t, "cancelled acquisition cleanup", func() bool { return authLeaseIdle(h) })
	all, err := AuthEntries()
	if err != nil || len(all["parked"]) == 0 || len(all["fakeco#cancelled"]) != 0 {
		t.Fatalf("cancelled acquisition wrote: %v, %v", all, err)
	}
}

func TestAuthLeaseAcquisitionTimesOutWithoutWriting(t *testing.T) {
	h, ctx := authLeaseHost(t)
	cmd, in := authPublicHolder(t)
	start := time.Now()
	if err := authLeaseImport(h, ctx, "fakeco#timedout"); err == nil {
		t.Fatal("host saved without acquiring the held lock")
	}
	if elapsed := time.Since(start); elapsed > filelock.Wait+5*time.Second {
		t.Fatalf("bounded acquisition took %v", elapsed)
	}
	authLeaseReleaseHolder(t, cmd, in)
	all, err := AuthEntries()
	if err != nil || len(all["parked"]) == 0 || len(all["fakeco#timedout"]) != 0 {
		t.Fatalf("timed-out acquisition wrote: %v, %v", all, err)
	}
}

func TestAuthLeaseHostDiesBeforeAcquisition(t *testing.T) {
	h, ctx := authLeaseHost(t)
	cmd, in := authPublicHolder(t)
	result := make(chan error, 1)
	go func() { result <- authLeaseImport(h, ctx, "fakeco#orphan-acquire") }()
	waitFor(t, "host acquisition", func() bool { return authLeaseAcquiring(h) })
	h.kill()
	waitFor(t, "host exit observed", func() bool {
		h.authMu.Lock()
		defer h.authMu.Unlock()
		return h.authExited
	})
	authLeaseReleaseHolder(t, cmd, in)
	waitFor(t, "orphaned acquisition cleanup", func() bool { return authLeaseIdle(h) })
	_, done := authLeaseContender(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquisition granted after host death was not released")
	}
	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("dead host call did not end")
	}
	all, err := AuthEntries()
	if err != nil || len(all["fakeco#orphan-acquire"]) != 0 {
		t.Fatalf("dead host wrote after acquisition: %v, %v", all, err)
	}
}

func TestAuthLeaseLockFailureDoesNotWrite(t *testing.T) {
	h, ctx := authLeaseHost(t)
	if err := os.MkdirAll(filepath.Dir(AuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory at the lock path makes opening the OS lock fail even when
	// the test runs with privileges that bypass ordinary permission bits.
	if err := os.Mkdir(AuthPath()+".flock", 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := EditAuth(func(map[string]json.RawMessage) (bool, error) { called = true; return true, nil }); err == nil || called {
		t.Fatalf("Go edit did not fail closed: called=%v err=%v", called, err)
	}
	if err := authLeaseImport(h, ctx, "fakeco#refused"); err == nil {
		t.Fatal("host import succeeded without an OS lock")
	}
	if _, err := os.Stat(AuthPath()); !os.IsNotExist(err) {
		t.Fatalf("failed acquisition wrote auth: %v", err)
	}
}

func TestAuthLeaseGoSignOutFailsClosed(t *testing.T) {
	Settle()
	authPublicProbeHome(t)
	t.Cleanup(Settle)
	if err := os.MkdirAll(filepath.Dir(AuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"fakeco":{"type":"api","key":"keep"}}`)
	if err := os.WriteFile(AuthPath(), original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(AuthPath()+".flock", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SignOut(context.Background(), "fakeco", ""); err == nil {
		t.Fatal("Go sign-out succeeded without the OS lock")
	}
	if got, _ := os.ReadFile(AuthPath()); !bytes.Equal(got, original) {
		t.Fatalf("failed sign-out changed auth: %s", got)
	}
}

// The plugin uses the real client.auth.set expected-value API. A mismatch
// must leave the store unchanged, and a subsequent match must commit.
func TestAuthLeaseHostExpectedCASAndLockFailure(t *testing.T) {
	for _, deny := range []bool{false, true} {
		name := "conflict and commit"
		if deny {
			name = "lock failure"
		}
		t.Run(name, func(t *testing.T) {
			sandbox(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := os.MkdirAll(filepath.Dir(AuthPath()), 0o700); err != nil {
				t.Fatal(err)
			}
			original := []byte(`{"leasecas":{"type":"api","key":"original"}}`)
			if err := os.WriteFile(AuthPath(), original, 0o600); err != nil {
				t.Fatal(err)
			}
			if deny {
				if err := os.Mkdir(AuthPath()+".flock", 0o700); err != nil {
					t.Fatal(err)
				}
			}
			fixture := filepath.Join(t.TempDir(), "index.js")
			source := `export const LeaseCAS = async ({ client }) => ({
  config: async (config) => {
    config.provider = config.provider ?? {}
    config.provider.leasecas = { name: "Lease CAS", models: {} }
  },
  auth: {
    provider: "leasecas",
    methods: [],
    loader: async (getAuth) => {
      const was = await getAuth()
      try {
        const conflict = await client.auth.set({ path: { id: "leasecas" }, expected: { ...was, key: "wrong" }, body: { ...was, key: "wrong-write" } })
        const success = await client.auth.set({ path: { id: "leasecas" }, expected: was, body: { ...was, key: "new" } })
        return { apiKey: JSON.stringify({ conflict: conflict.data, success: success.data }) }
      } catch (e) {
        return { apiKey: "error:" + String(e?.message ?? e) }
      }
    },
  },
})
`
			if err := os.WriteFile(fixture, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Add(ctx, fixture); err != nil {
				t.Fatal(err)
			}
			h, err := get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var answer struct {
				APIKey string `json:"apiKey"`
			}
			if err := h.call(ctx, "load", map[string]any{"provider": "leasecas", "account": "leasecas"}, &answer); err != nil {
				t.Fatal(err)
			}
			if deny {
				if !strings.HasPrefix(answer.APIKey, "error:") {
					t.Fatalf("client.auth.set did not report the lock failure: %q", answer.APIKey)
				}
				if got, _ := os.ReadFile(AuthPath()); !bytes.Equal(got, original) {
					t.Fatalf("failed client.auth.set changed auth: %s", got)
				}
				return
			}
			var result struct {
				Conflict struct {
					Saved    bool `json:"saved"`
					Conflict bool `json:"conflict"`
				} `json:"conflict"`
				Success struct {
					Saved    bool `json:"saved"`
					Conflict bool `json:"conflict"`
				} `json:"success"`
			}
			if err := json.Unmarshal([]byte(answer.APIKey), &result); err != nil {
				t.Fatalf("CAS answer %q: %v", answer.APIKey, err)
			}
			if result.Conflict.Saved || !result.Conflict.Conflict || !result.Success.Saved || result.Success.Conflict {
				t.Fatalf("CAS outcomes = %+v", result)
			}
			all, err := AuthEntries()
			if err != nil || !bytes.Contains(all["leasecas"], []byte(`"new"`)) {
				t.Fatalf("matching CAS did not publish: %v, %v", all, err)
			}
		})
	}
}

func authLeaseAcquireForTest(t *testing.T, h *host, id int64) string {
	t.Helper()
	h.dispatch(message{AuthLock: &authLockFrame{ID: id, Op: "acquire"}})
	waitFor(t, "lease grant", func() bool { return authLeaseToken(h) != "" })
	return authLeaseToken(h)
}

func authLeaseContender(t *testing.T) (<-chan struct{}, <-chan error) {
	t.Helper()
	entered, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- EditAuth(func(m map[string]json.RawMessage) (bool, error) {
			close(entered)
			m["contender"] = json.RawMessage(`{"type":"api","key":"c"}`)
			return true, nil
		})
	}()
	return entered, done
}

func TestAuthLeaseWrongOwnerAndStaleTokenCannotRelease(t *testing.T) {
	h1, ctx := authLeaseHost(t)
	h2, _, err := startOn(ctx, os.Getenv("MAGPIE_BUN"))
	if err != nil {
		t.Fatal(err)
	}
	defer h2.stop()
	first := authLeaseAcquireForTest(t, h1, 1001)
	h1.dispatch(message{AuthLock: &authLockFrame{ID: 1002, Op: "abort", Token: first}})
	waitFor(t, "first lease release", func() bool { return authLeaseIdle(h1) })
	second := authLeaseAcquireForTest(t, h1, 1003)
	if first == second {
		t.Fatal("consecutive grants had the same opaque token")
	}
	entered, done := authLeaseContender(t)
	h2.dispatch(message{AuthLock: &authLockFrame{ID: 1004, Op: "abort", Token: second}})
	h1.dispatch(message{AuthLock: &authLockFrame{ID: 1005, Op: "abort", Token: first}})
	select {
	case <-entered:
		t.Fatal("wrong owner or stale token released the current lease")
	case <-time.After(300 * time.Millisecond):
	}
	h1.dispatch(message{AuthLock: &authLockFrame{ID: 1006, Op: "abort", Token: second}})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("current owner's release did not let the contender write")
	}
}

func TestAuthLeaseHostDeathAndRecycleReleaseIdleGrant(t *testing.T) {
	for _, how := range []string{"death", "recycle"} {
		t.Run(how, func(t *testing.T) {
			h, _ := authLeaseHost(t)
			authLeaseAcquireForTest(t, h, 2001)
			if how == "death" {
				h.kill()
			} else {
				h.recycle()
			}
			select {
			case <-h.dead:
			case <-time.After(5 * time.Second):
				t.Fatal("host did not exit")
			}
			_, done := authLeaseContender(t)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("idle lease survived host exit")
			}
		})
	}
}

// Pause the real Go publisher inside the file flush. Host death must not
// release its lock before this already-accepted commit finishes.
func TestAuthLeaseHostDeathDuringGoCommit(t *testing.T) {
	h, ctx := authLeaseHost(t)
	if err := EditAuth(func(m map[string]json.RawMessage) (bool, error) {
		m["seed"] = json.RawMessage(`{"type":"api","key":"seed"}`)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	syncFile := steady.Sync
	var parkOnce sync.Once
	steady.Sync = func(f *os.File) error {
		if strings.HasPrefix(filepath.Base(f.Name()), ".plugin-auth.json.") && !strings.HasPrefix(filepath.Base(f.Name()), ".plugin-auth.json.bak.") {
			parkOnce.Do(func() {
				close(entered)
				<-resume
			})
		}
		return syncFile(f)
	}
	t.Cleanup(func() { steady.Sync = syncFile })
	result := make(chan error, 1)
	go func() { result <- authLeaseImport(h, ctx, "fakeco#committing") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("Go publication did not reach its flush")
	}
	h.kill()
	waitFor(t, "host exit observed", func() bool {
		h.authMu.Lock()
		defer h.authMu.Unlock()
		return h.authExited
	})
	contenderEntered, done := authLeaseContender(t)
	select {
	case <-contenderEntered:
		close(resume)
		t.Fatal("host death released a Go commit in progress")
	case <-time.After(300 * time.Millisecond):
	}
	close(resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("contender did not enter after Go publication finished")
	}
	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("dead host call did not end")
	}
	all, err := AuthEntries()
	if err != nil || len(all["seed"]) == 0 || len(all["fakeco#committing"]) == 0 || len(all["contender"]) == 0 {
		t.Fatalf("commit/contender lost an update: %v, %v", all, err)
	}
}

func authLeaseReply(t *testing.T, f *fakeHost, id int64) map[string]any {
	t.Helper()
	var reply map[string]any
	waitFor(t, "auth lock response", func() bool {
		for _, wire := range f.wrote() {
			frame, _ := wire["authLock"].(map[string]any)
			if frame != nil && frame["id"] == float64(id) {
				reply = frame
				return true
			}
		}
		return false
	})
	return reply
}

func TestAuthLeaseReseedRejectsMultipleAccounts(t *testing.T) {
	authPublicProbeHome(t)
	if err := os.MkdirAll(filepath.Dir(AuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), zeroedAuth, 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeHost(t)
	f.authPath = AuthPath()
	t.Cleanup(f.endAuthLease)
	f.line(map[string]any{"authLock": map[string]any{"id": 1, "op": "acquire"}})
	if reply := authLeaseReply(t, f, 1); reply["error"] == nil || reply["token"] != nil {
		t.Fatalf("ordinary acquisition of unreadable auth = %v", reply)
	}
	f.line(map[string]any{"authLock": map[string]any{"id": 2, "op": "acquire", "reseed": true}})
	grant := authLeaseReply(t, f, 2)
	token, _ := grant["token"].(string)
	if token == "" {
		t.Fatalf("no explicit reseed lease: %v", grant)
	}
	f.line(map[string]any{"authLock": map[string]any{
		"id": 3, "op": "commit", "token": token,
		"all": map[string]any{
			"a": map[string]any{"type": "api", "key": "a"},
			"b": map[string]any{"type": "api", "key": "b"},
		},
	}})
	if reply := authLeaseReply(t, f, 3); reply["error"] == nil {
		t.Fatalf("reseed of multiple accounts was accepted: %v", reply)
	}
	if got, _ := os.ReadFile(AuthPath()); !bytes.Equal(got, zeroedAuth) {
		t.Fatal("rejected reseed overwrote unreadable auth")
	}
	if kept, _ := filepath.Glob(AuthPath() + ".bad-*"); len(kept) != 0 {
		t.Fatalf("rejected reseed unexpectedly created a backup: %v", kept)
	}
}

func TestAuthLeaseRejectsExternalChangeBeforeCommit(t *testing.T) {
	authPublicProbeHome(t)
	if err := EditAuth(func(m map[string]json.RawMessage) (bool, error) {
		m["original"] = json.RawMessage(`{"type":"api","key":"old"}`)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	f := newFakeHost(t)
	f.authPath = AuthPath()
	t.Cleanup(f.endAuthLease)
	f.line(map[string]any{"authLock": map[string]any{"id": 1, "op": "acquire"}})
	grant := authLeaseReply(t, f, 1)
	token, _ := grant["token"].(string)
	if token == "" {
		t.Fatalf("no lease: %v", grant)
	}
	manual := []byte(`{"manual":{"type":"api","key":"new"}}`)
	if err := os.WriteFile(AuthPath(), manual, 0o600); err != nil {
		t.Fatal(err)
	}
	f.line(map[string]any{"authLock": map[string]any{"id": 2, "op": "commit", "token": token,
		"all": map[string]any{"original": map[string]any{"type": "api", "key": "stale"}}}})
	reply := authLeaseReply(t, f, 2)
	if reply["error"] == nil {
		t.Fatalf("external edit was not rejected: %v", reply)
	}
	if got, _ := os.ReadFile(AuthPath()); !bytes.Equal(got, manual) {
		t.Fatalf("stale proposal overwrote manual change: %s", got)
	}
	if err := EditAuth(func(m map[string]json.RawMessage) (bool, error) { return false, nil }); err != nil {
		t.Fatalf("rejected commit did not release its lease: %v", err)
	}
}

// The child is the real Go owner of a real Bun host. The plugin intercepts
// the host's commit frame before it is sent, after the callback ran. Killing
// the Go owner releases its flock; resuming the orphan must never write.
func TestAuthLeaseParentDeathBeforeCommit(t *testing.T) {
	if os.Getenv("MAGPIE_AUTH_PARENT_PROBE") == "child" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := Add(ctx, os.Getenv("MAGPIE_AUTH_PARENT_FIXTURE")); err != nil {
			t.Fatal(err)
		}
		h, err := get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("MAGPIE_AUTH_PARENT_PID"), []byte(strconv.Itoa(h.cmd.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = authLeaseImport(h, ctx, "fakeco#orphan")
		return
	}
	sandbox(t)
	if err := EditAuth(func(m map[string]json.RawMessage) (bool, error) {
		m["seed"] = json.RawMessage(`{"type":"api","key":"seed"}`)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	marker, release, pidPath := filepath.Join(dir, "parked"), filepath.Join(dir, "release"), filepath.Join(dir, "host.pid")
	fixture := filepath.Join(dir, "index.js")
	original, err := os.ReadFile("testdata/fake/index.js")
	if err != nil {
		t.Fatal(err)
	}
	patch := `import fs from "node:fs"
const realStringify = JSON.stringify.bind(JSON)
let parkedCommit = false
JSON.stringify = function(value, ...args) {
  if (!parkedCommit && value?.authLock?.op === "commit") {
    parkedCommit = true
    fs.writeFileSync(process.env.MAGPIE_AUTH_PARENT_MARKER, String(process.pid))
    const cell = new Int32Array(new SharedArrayBuffer(4))
    while (!fs.existsSync(process.env.MAGPIE_AUTH_PARENT_RELEASE)) Atomics.wait(cell, 0, 0, 20)
    fs.writeFileSync(process.env.MAGPIE_AUTH_PARENT_MARKER + ".resumed", "yes")
    setTimeout(() => fs.writeFileSync(process.env.MAGPIE_AUTH_PARENT_MARKER + ".after", "yes"), 50)
  }
  return realStringify(value, ...args)
}
`
	if err := os.WriteFile(fixture, append([]byte(patch), original...), 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestAuthLeaseParentDeathBeforeCommit$")
	child.Env = append(os.Environ(), "MAGPIE_AUTH_PARENT_PROBE=child", "MAGPIE_AUTH_PARENT_FIXTURE="+fixture,
		"MAGPIE_AUTH_PARENT_MARKER="+marker, "MAGPIE_AUTH_PARENT_RELEASE="+release, "MAGPIE_AUTH_PARENT_PID="+pidPath)
	var log bytes.Buffer
	child.Stdout, child.Stderr = &log, &log
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
		if b, err := os.ReadFile(pidPath); err == nil {
			if pid, err := strconv.Atoi(string(b)); err == nil && living(pid) {
				endPID(pid)
			}
		}
	})
	pid := readPID(t, marker)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if err := EditAuth(func(m map[string]json.RawMessage) (bool, error) {
		m["newer"] = json.RawMessage(`{"type":"api","key":"newer"}`)
		return true, nil
	}); err != nil {
		t.Fatalf("writer after parent death: %v; child log: %s", err, &log)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		waitFor(t, "orphan resumed after parent death", func() bool {
			_, err := os.Stat(marker + ".resumed")
			return err == nil
		})
	}
	// Unix does not promise to kill the child with its parent. Let its
	// event loop resume after the attempted commit, then end it explicitly.
	waitFor(t, "orphan's commit attempt or exit", func() bool {
		_, err := os.Stat(marker + ".after")
		return err == nil || !living(pid)
	})
	endPID(pid)
	waitFor(t, "orphan cleanup", func() bool { return !living(pid) })
	all, err := AuthEntries()
	if err != nil || len(all["seed"]) == 0 || len(all["newer"]) == 0 || len(all["fakeco#orphan"]) != 0 {
		t.Fatalf("orphan wrote after parent death: %v, %v", all, err)
	}
}
