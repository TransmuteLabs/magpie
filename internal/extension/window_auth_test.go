package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func authWindowFixture(t *testing.T, native bool, config authfixture.Config) (*Manager, string, string) {
	t.Helper()
	manifest := authfixture.Manifest()
	directory := t.TempDir()
	args := []string{"build", "-mod=readonly", "-o", filepath.Join(directory, manifest.Entry), "./testdata/authworker"}
	if native {
		if runtime.GOOS == "windows" {
			t.Skip("native authentication fixture needs a tested C toolchain")
		}
		manifest.Runtime, manifest.Entry = "native", "primary.so"
		args = []string{"build", "-mod=readonly", "-buildmode=c-shared", "-o", filepath.Join(directory, manifest.Entry), "./testdata/authnative"}
	}
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), args...)
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build auth fixture: %v %s", err, output)
	}
	body, _ := json.Marshal(manifest)
	for name, data := range map[string][]byte{
		sdk.ManifestFile: body,
		"package.json":   []byte(`{"name":"window-auth","magpie":{"page":{"title":"Sign in","file":"page.html"}}}`),
		"page.html":      []byte("<!doctype html><html><body>Auth fixture</body></html>"),
	} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewManager(t.TempDir())
	entry, err := manager.Install(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	values, _ := json.Marshal(config)
	settings, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", values)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return manager, manifest.ID, windowRevision(entry.Revision, settings.Version)
}

func authWindowContext(ctx context.Context, request string) context.Context {
	ctx = WithWindowAuthParent(ctx, "parent-frame")
	ctx = WithModuleRequestID(ctx, request)
	return WithWindowAuthPolicy(ctx, func(ctx context.Context, _ sdk.Scope) (context.Context, error) { return ctx, ctx.Err() })
}

func authWindowReady(t *testing.T, manager *Manager, id string, view sdk.WindowAuthFlowView) {
	t.Helper()
	if _, err := manager.store.updateWindowAuth(id, view.Flow, func(record *windowAuthRecord, _ *storeAccounts) error {
		record.Next = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func authWindowTrace(t *testing.T, path string, method string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(body, []byte(`"method":"`+method+`"`))
}

func TestWindowAuthOwnerPollProjectionThrottleAndPublication(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker", true: "native"}[native], func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "trace")
			manager, id, revision := authWindowFixture(t, native, authfixture.Config{Trace: trace})
			ctx := authWindowContext(t.Context(), "start-intent")
			view, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{Method: "browser"})
			if err != nil || view.State != "waiting" || view.UserCode != "SAFE-CODE" {
				t.Fatalf("start: %+v %v", view, err)
			}
			raw, _ := json.Marshal(view)
			if bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte("continuation")) {
				t.Fatalf("start exposed private auth material: %s", raw)
			}
			if duplicate, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{Method: "browser"}); err != nil || duplicate.Flow != view.Flow {
				t.Fatal("start intent was dispatched twice:", err)
			}
			if _, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{Method: "device"}); err == nil {
				t.Fatal("changed method reused a start intent")
			}
			if _, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{Method: "browser", Input: json.RawMessage(`{"changed":true}`)}); err == nil {
				t.Fatal("changed input reused a start intent")
			}
			if _, err := manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", view.Flow); err != nil {
				t.Fatal(err)
			}
			if authWindowTrace(t, trace, "finish") != 0 {
				t.Fatal("early poll reached authentication backend")
			}
			authWindowReady(t, manager, id, view)
			var writers sync.WaitGroup
			for range 8 {
				writers.Add(1)
				go func() {
					defer writers.Done()
					completed, err := manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", view.Flow)
					if err != nil || completed.State != "complete" || completed.Account == nil || completed.Account.Generation != "1" {
						t.Errorf("finish: %+v %v", completed, err)
					}
				}()
			}
			writers.Wait()
			if authWindowTrace(t, trace, "start") != 1 || authWindowTrace(t, trace, "finish") != 1 {
				t.Fatal("auth exchange was replayed")
			}
			body, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range bytes.Split(bytes.TrimSpace(body), []byte("\n")) {
				var event struct {
					PID int `json:"pid"`
				}
				if json.Unmarshal(line, &event) != nil || event.PID <= 0 || (event.PID == os.Getpid()) != native {
					t.Fatal("authentication runtime identity did not match its declared native/worker mode")
				}
			}
			account, err := manager.Store().GetAccount(id, "vendor", "chosen")
			if err != nil || account.Generation != 1 || !bytes.Contains(account.Auth, []byte("PRIVATE-CREDENTIAL")) {
				t.Fatal("account publication:", err)
			}
		})
	}
}

func TestWindowAuthPollReturnsDurableResultAfterOwnerLeaves(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "auth.trace")
	manager, id, revision := authWindowFixture(t, false, authfixture.Config{Trace: trace})
	ctx, cancel := context.WithTimeout(authWindowContext(t.Context(), "start-intent"), 6*time.Second)
	defer cancel()
	start, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{Method: "browser"})
	if err != nil || start.State != "waiting" {
		t.Fatalf("start: %s %v", start.State, err)
	}
	authWindowReady(t, manager, id, start)
	manager.mu.Lock()
	owner := manager.windowAuthOwners[start.Flow]
	manager.mu.Unlock()
	if owner == nil {
		t.Fatal("authentication owner missing")
	}
	read := make(chan struct{})
	resume := make(chan struct{})
	policyCalls := 0
	stale := WithWindowAuthPolicy(ctx, func(current context.Context, _ sdk.Scope) (context.Context, error) {
		policyCalls++
		if policyCalls == 2 {
			close(read)
			select {
			case <-resume:
			case <-current.Done():
			}
		}
		return current, current.Err()
	})
	type result struct {
		view sdk.WindowAuthFlowView
		err  error
	}
	outcome := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		view, err := manager.CoreAuthFinish(stale, id, revision, "window:page", "signin", start.Flow)
		outcome <- result{view, err}
	}()
	defer func() {
		cancel()
		<-finished
	}()
	select {
	case <-read:
	case <-ctx.Done():
		t.Fatal("the first poll did not capture its pre-completion receipt")
	}
	completed, err := manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", start.Flow)
	if err != nil || completed.State != "complete" || completed.Account == nil {
		t.Fatalf("completion: %s %v", completed.State, err)
	}
	select {
	case <-owner.done:
	case <-ctx.Done():
		t.Fatal("the authentication owner did not leave")
	}
	close(resume)
	select {
	case returned := <-outcome:
		if returned.err != nil || returned.view.State != "complete" || returned.view.Account == nil ||
			returned.view.Account.ID != completed.Account.ID || returned.view.Account.Generation != completed.Account.Generation {
			t.Fatalf("poll after durable completion returned %s: %v", returned.view.State, returned.err)
		}
	case <-ctx.Done():
		t.Fatal("the first poll did not finish")
	}
	if authWindowTrace(t, trace, "finish") != 1 {
		t.Fatal("the poll replayed the auth exchange")
	}
}

func TestWindowAuthOwnerRejectsForeignFramesMethodsAndGrants(t *testing.T) {
	manager, id, revision := authWindowFixture(t, false, authfixture.Config{})
	ctx := authWindowContext(t.Context(), "start")
	for _, test := range []struct{ module, ref, part, binding, method string }{
		{"foreign", revision, "window:page", "signin", "device"},
		{id, "old", "window:page", "signin", "device"},
		{id, revision, "window:panel:0", "signin", "device"},
		{id, revision, "window:page", "ungranted", "device"},
		{id, revision, "window:page", "signin", "ungranted"},
	} {
		if _, err := manager.CoreAuthStart(ctx, test.module, test.ref, test.part, test.binding, sdk.WindowAuthStartOptions{Method: test.method}); err == nil {
			t.Fatal("foreign auth grant was accepted:", test)
		}
	}
	view, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{Method: "device"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CoreAuthFinish(WithWindowAuthParent(ctx, "foreign-frame"), id, revision, "window:page", "signin", view.Flow); err == nil {
		t.Fatal("another frame finished the selected flow")
	}
	if _, err := manager.CoreAuthStart(authWindowContext(t.Context(), "selected"), id, revision, "window:page", "signin",
		sdk.WindowAuthStartOptions{Method: "device", Account: &sdk.AccountSelection{ID: "missing", ExpectedGeneration: "1"}}); err == nil {
		t.Fatal("unknown selected account was accepted")
	}
}

func TestWindowAuthOwnerRevokesPublicationDuringExchange(t *testing.T) {
	for _, change := range []string{"off", "config", "cancel", "delete", "caller-disconnect"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			config := authfixture.Config{Started: filepath.Join(dir, "started"), Release: filepath.Join(dir, "release")}
			manager, id, revision := authWindowFixture(t, false, config)
			ctx := authWindowContext(t.Context(), "start")
			options := sdk.WindowAuthStartOptions{Method: "device"}
			if change == "delete" {
				if _, err := manager.Store().SaveAccount(Account{Plugin: id, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{}`)}, 0); err != nil {
					t.Fatal(err)
				}
				options.Account = &sdk.AccountSelection{ID: "chosen", ExpectedGeneration: "1"}
			}
			view, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", options)
			if err != nil {
				t.Fatal(err)
			}
			authWindowReady(t, manager, id, view)
			caller, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := manager.CoreAuthFinish(caller, id, revision, "window:page", "signin", view.Flow)
				done <- err
			}()
			deadline := time.After(5 * time.Second)
			for {
				if _, err := os.Stat(config.Started); err == nil {
					break
				}
				select {
				case <-deadline:
					t.Fatal("backend exchange never started")
				case <-time.After(time.Millisecond):
				}
			}
			switch change {
			case "off":
				if err := manager.SetEnabled(t.Context(), id, false); err != nil {
					t.Fatal(err)
				}
			case "config":
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), id, "1", json.RawMessage(`{}`)); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				if _, err := manager.CoreAuthCancel(ctx, id, revision, "window:page", "signin", view.Flow); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := manager.Store().DeleteAccount(id, "vendor", "chosen", 1); err != nil {
					t.Fatal(err)
				}
			case "caller-disconnect":
				cancel()
			}
			if err := os.WriteFile(config.Release, []byte("release"), 0o600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if change == "caller-disconnect" && !errors.Is(err, context.Canceled) {
					t.Fatal("caller did not detach:", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("backend did not settle")
			}
			for {
				record, err := manager.store.windowAuthRecord(id, view.Flow)
				if err == nil && (record.Phase == "complete" || record.Phase == "uncertain") {
					if (record.Phase == "complete") != (change == "caller-disconnect") {
						t.Fatalf("changed authority published an account: %s -> %s", change, record.Phase)
					}
					break
				}
				select {
				case <-deadline:
					t.Fatal("owned exchange did not persist a receipt")
				case <-time.After(time.Millisecond):
				}
			}
			accounts, err := manager.Store().Accounts(id, "vendor")
			expected := 0
			if change == "caller-disconnect" {
				expected = 1
			}
			if err != nil || len(accounts) != expected {
				t.Fatalf("wrong credential publication after %s: %d %v", change, len(accounts), err)
			}
		})
	}
}

func TestWindowAuthOwnerInteropWithExistingTransactionalRefresh(t *testing.T) {
	dir := t.TempDir()
	config := authfixture.Config{Trace: filepath.Join(dir, "trace"),
		RefreshStarted: filepath.Join(dir, "refresh-started"), RefreshRelease: filepath.Join(dir, "refresh-release")}
	manager, id, revision := authWindowFixture(t, false, config)
	if _, err := manager.Store().SaveAccount(Account{Plugin: id, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{"token":"OLD"}`)}, 0); err != nil {
		t.Fatal(err)
	}
	ctx := authWindowContext(t.Context(), "start")
	var finishing atomic.Bool
	var observed sync.Once
	attempted := make(chan struct{})
	ctx = WithWindowAuthPolicy(ctx, func(ctx context.Context, _ sdk.Scope) (context.Context, error) {
		if finishing.Load() {
			observed.Do(func() { close(attempted) })
		}
		return ctx, ctx.Err()
	})
	view, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{
		Method: "device", Account: &sdk.AccountSelection{ID: "chosen", ExpectedGeneration: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	authWindowReady(t, manager, id, view)
	refreshDone := make(chan error, 1)
	go func() {
		_, err := manager.RefreshAccount(t.Context(), id, sdk.Scope{Plugin: id, Provider: "vendor", Account: "chosen", Generation: 1})
		refreshDone <- err
	}()
	deadline := time.After(10 * time.Second)
	for {
		if _, err := os.Stat(config.RefreshStarted); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("existing refresh owner did not enter its vendor transaction")
		case <-time.After(time.Millisecond):
		}
	}
	finishing.Store(true)
	finishDone := make(chan error, 1)
	go func() {
		_, err := manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", view.Flow)
		finishDone <- err
	}()
	select {
	case <-attempted:
	case <-deadline:
		t.Fatal("broker finish did not reach its captured policy owner")
	}
	if err := os.WriteFile(config.RefreshRelease, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	for index, done := range []chan error{refreshDone, finishDone} {
		select {
		case err := <-done:
			if index == 0 && err != nil {
				t.Fatal("existing refresh owner failed:", err)
			}
		case <-deadline:
			t.Fatal("refresh/broker lock order did not settle")
		}
	}
	account, err := manager.Store().GetAccount(id, "vendor", "chosen")
	if err != nil || account.Generation != 2 || string(account.Auth) != `{"token":"PRIVATE-REFRESHED"}` {
		t.Fatalf("broker overwrote refresh successor: generation %d, error %v", account.Generation, err)
	}
	if authWindowTrace(t, config.Trace, "refresh") != 1 || authWindowTrace(t, config.Trace, "finish") != 0 {
		t.Fatal("broker exchanged a code under a stale generation")
	}
}

func TestWindowAuthOwnerDoesNotExposeVendorErrors(t *testing.T) {
	manager, id, revision := authWindowFixture(t, false, authfixture.Config{Fail: true})
	ctx := authWindowContext(t.Context(), "start")
	view, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{Method: "device"})
	if err != nil {
		t.Fatal(err)
	}
	authWindowReady(t, manager, id, view)
	view, err = manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", view.Flow)
	raw, _ := json.Marshal(view)
	if err != nil && strings.Contains(err.Error(), "PRIVATE") || bytes.Contains(raw, []byte("PRIVATE")) || view.State != "uncertain" {
		t.Fatalf("backend auth failure reached UI: %s %v", raw, err)
	}
}
