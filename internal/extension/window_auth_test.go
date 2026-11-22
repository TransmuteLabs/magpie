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
	"github.com/yetone/magpie/internal/filelock"
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
			replaceRuntimeConfigAtSameVersion(t, manager, id, `"fail":false`, `"fail":true `)
			replayed, err := manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", view.Flow)
			if err != nil || replayed.State != "complete" || replayed.Account == nil || replayed.Account.Generation != "1" ||
				authWindowTrace(t, trace, "finish") != 1 {
				t.Fatalf("changed values hid or replayed a durable receipt: %s %v", replayed.State, err)
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
	for _, change := range []string{"off", "config", "same CAS configuration", "reordered configuration", "cancel", "delete", "caller-disconnect"} {
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
			case "same CAS configuration":
				replaceRuntimeConfigAtSameVersion(t, manager, id, `"fail":false`, `"fail":true `)
			case "reordered configuration":
				replaceRuntimeConfigAtSameVersion(t, manager, id, `"refreshStarted":"","refreshRelease":""`, `"refreshRelease":"","refreshStarted":""`)
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
					if (record.Phase == "complete") != (change == "caller-disconnect" || change == "reordered configuration") {
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
			if change == "caller-disconnect" || change == "reordered configuration" {
				expected = 1
			}
			if err != nil || len(accounts) != expected {
				t.Fatalf("wrong credential publication after %s: %d %v", change, len(accounts), err)
			}
		})
	}
}

func TestWindowAuthFinishRechecksSameCASConfigurationAfterSlotWait(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	manager, id, revision := authWindowFixture(t, false, authfixture.Config{Trace: trace})
	ctx, cancel := context.WithTimeout(authWindowContext(t.Context(), "queued-finish"), 5*time.Second)
	defer cancel()
	view, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{Method: "device"})
	if err != nil || view.State != "waiting" {
		t.Fatalf("start: %s %v", view.State, err)
	}
	authWindowReady(t, manager, id, view)
	manager.mu.Lock()
	owner := manager.windowAuthOwners[view.Flow]
	manager.mu.Unlock()
	if owner == nil {
		t.Fatal("waiting flow lost its owner")
	}
	client := owner.lease.client
	slots := client.slots
	for range cap(slots) {
		slots <- struct{}{}
	}
	release := sync.OnceFunc(func() {
		for range cap(slots) {
			<-slots
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", view.Flow)
	}()
	defer func() {
		cancel()
		release()
		<-done
	}()
	for len(client.regularSlots) == 0 {
		select {
		case <-done:
			t.Fatal("finish returned before it could wait for a slot")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if record, err := manager.store.windowAuthRecord(id, view.Flow); err != nil || record.Phase != "polling" {
		t.Fatalf("finish did not reach the worker wait: %s %v", record.Phase, err)
	}
	replaceRuntimeConfigAtSameVersion(t, manager, id, `"fail":false`, `"fail":true `)
	release()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := authWindowTrace(t, trace, "finish"); got != 0 {
		t.Errorf("revoked window flow reached the worker %d time(s)", got)
	}
	accounts, err := manager.Store().Accounts(id, "vendor")
	if err != nil || len(accounts) != 0 {
		t.Fatalf("revoked flow published %d accounts: %v", len(accounts), err)
	}
}

func TestTransferredWindowAuthFinishRechecksConfigAfterRollbackFenceWait(t *testing.T) {
	for _, change := range []string{"same CAS replacement", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "trace")
			manager, id, revision := authWindowFixture(t, false, authfixture.Config{Trace: trace})
			ctx, cancel := context.WithTimeout(authWindowContext(t.Context(), "transferred-finish"), 10*time.Second)
			defer cancel()
			ctx = WithSubscriptionOwnerRevision(ctx, "transfer-revision")
			owner, _ := SubscriptionOwnerRevision(ctx)
			if err := withTestSavedSignIns(manager.store, ctx, func(dir *os.Root) error {
				return storeWrite(dir, subscriptionOwnersFile, []subscriptionRecord{{
					SubscriptionOwner: SubscriptionOwner{
						Module: id, Provider: "vendor", PublicID: "legacy-vendor", Revision: owner, Active: true,
						Accounts: []SubscriptionAccount{{Key: "vendor", ID: "chosen", User: "alice", LoginID: "alice-login"}},
					},
					GrantRevision: strings.Repeat("a", 64),
					Digests:       map[string]string{"vendor": strings.Repeat("b", 64)},
					Tickets:       []string{"transferred-ticket"},
					Generations:   []uint64{1},
				}}, storeFileLimit)
			}); err != nil {
				t.Fatal(err)
			}
			account, err := manager.Store().SaveAccount(Account{
				Plugin: id, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{"token":"fixture"}`),
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			view, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{
				Method: "device", Account: &sdk.AccountSelection{ID: "chosen", ExpectedGeneration: "1"},
			})
			if err != nil || view.State != "waiting" {
				t.Fatalf("start: %s %v", view.State, err)
			}
			authWindowReady(t, manager, id, view)
			manager.mu.Lock()
			client := manager.windowAuthOwners[view.Flow].lease.client
			manager.mu.Unlock()
			slots := client.slots
			for range cap(slots) {
				slots <- struct{}{}
			}
			unblockSlots := sync.OnceFunc(func() {
				for range cap(slots) {
					<-slots
				}
			})
			lock, err := os.OpenFile(filepath.Join(manager.Namespace(), "private", ".lock"), os.O_RDWR, 0)
			if err != nil {
				unblockSlots()
				t.Fatal(err)
			}
			defer lock.Close()
			var result sdk.WindowAuthFlowView
			var finishErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				result, finishErr = manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", view.Flow)
			}()
			unblockStore := sync.OnceFunc(func() { filelock.Unlock(lock) })
			defer func() {
				unblockStore()
				unblockSlots()
				cancel()
				<-done
			}()
			wait := func(stage string, ready func() bool) {
				t.Helper()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for !ready() {
					select {
					case <-done:
						t.Fatalf("finish returned before %s: %s %v", stage, result.State, finishErr)
					case <-ctx.Done():
						t.Fatalf("wait for %s: %v", stage, ctx.Err())
					case <-tick.C:
					}
				}
			}
			wait("worker-slot wait", func() bool { return len(client.regularSlots) != 0 })
			if held, err := filelock.TryShared(lock); err != nil || !held {
				t.Fatalf("hold the peer Store reader: %v", err)
			}
			unblockSlots()
			wait("rollback fence's exclusive Store wait", func() bool {
				if !manager.store.mu.TryLock() {
					return true
				}
				manager.store.mu.Unlock()
				return false
			})
			wantState, wantCalls, wantGeneration := "complete", 1, account.Generation+1
			if change == "same CAS replacement" {
				replaceRuntimeConfigAtSameVersion(t, manager, id, `"fail":false`, `"fail":true `)
				wantState, wantCalls, wantGeneration = "uncertain", 0, account.Generation
			}
			unblockStore()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if calls := authWindowTrace(t, trace, "finish"); calls != wantCalls {
				t.Errorf("finish worker calls = %d, want %d", calls, wantCalls)
			}
			if finishErr != nil || result.State != wantState {
				t.Fatalf("finish: %s %v, want %s", result.State, finishErr, wantState)
			}
			current, err := manager.Store().GetAccount(id, "vendor", "chosen")
			if err != nil || current.Generation != wantGeneration {
				t.Fatalf("account generation = %d, want %d: %v", current.Generation, wantGeneration, err)
			}
			if change == "unchanged" {
				replay, err := manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", view.Flow)
				if err != nil || replay.State != "complete" || replay.Account == nil || result.Account == nil ||
					replay.Account.Generation != result.Account.Generation || authWindowTrace(t, trace, "finish") != 1 {
					t.Fatalf("finish did not replay its completed receipt: %s %v", replay.State, err)
				}
			}
		})
	}
}

func TestWindowAuthFinishRechecksConfigAfterPublicationWait(t *testing.T) {
	for _, change := range []string{"same CAS replacement", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			config := authfixture.Config{
				Trace: filepath.Join(dir, "trace"), Started: filepath.Join(dir, "started"), Release: filepath.Join(dir, "release"),
			}
			manager, id, revision := authWindowFixture(t, false, config)
			original, err := manager.Store().SaveAccount(Account{
				Plugin: id, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{"token":"original"}`),
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(authWindowContext(t.Context(), "publication-wait"), 8*time.Second)
			defer cancel()
			view, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", sdk.WindowAuthStartOptions{
				Method: "device", Account: &sdk.AccountSelection{ID: original.ID, ExpectedGeneration: "1"},
			})
			if err != nil || view.State != "waiting" {
				t.Fatalf("start: %s %v", view.State, err)
			}
			authWindowReady(t, manager, id, view)
			lock, err := os.OpenFile(filepath.Join(manager.Namespace(), "private", ".lock"), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			var result sdk.WindowAuthFlowView
			var finishErr error
			done := make(chan struct{})
			unblockStore := sync.OnceFunc(func() { filelock.Unlock(lock) })
			go func() {
				defer close(done)
				result, finishErr = manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", view.Flow)
			}()
			defer func() {
				unblockStore()
				cancel()
				<-done
			}()
			wait := func(stage string, ready func() bool) {
				t.Helper()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for !ready() {
					select {
					case <-done:
						t.Fatalf("finish returned before %s: %s %v", stage, result.State, finishErr)
					case <-ctx.Done():
						t.Fatalf("wait for %s: %v", stage, ctx.Err())
					case <-tick.C:
					}
				}
			}
			wait("worker exchange", func() bool {
				_, err := os.Stat(config.Started)
				return err == nil
			})
			if held, err := filelock.TryShared(lock); err != nil || !held {
				t.Fatalf("hold the peer Store reader: %v", err)
			}
			if err := os.WriteFile(config.Release, []byte("release"), 0o600); err != nil {
				t.Fatal(err)
			}
			wait("exclusive publication wait", func() bool {
				if !manager.store.mu.TryLock() {
					return true
				}
				manager.store.mu.Unlock()
				return false
			})
			if change == "same CAS replacement" {
				replaceRuntimeConfigAtSameVersion(t, manager, id, `"fail":false`, `"fail":true `)
			}
			unblockStore()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			current, err := manager.Store().GetAccount(id, "vendor", original.ID)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := manager.store.windowAuthRecord(id, view.Flow)
			if err != nil {
				t.Fatal(err)
			}
			if calls := authWindowTrace(t, config.Trace, "finish"); calls != 1 {
				t.Fatalf("worker finish calls = %d, want 1", calls)
			}
			if change == "same CAS replacement" {
				if finishErr != nil || result.State != "uncertain" || result.Account != nil || receipt.Phase != "uncertain" || receipt.Receipt != nil ||
					current.Generation != original.Generation || !bytes.Equal(current.Auth, original.Auth) {
					t.Fatalf("replacement published a stale result: state=%s phase=%s generation=%d err=%v", result.State, receipt.Phase, current.Generation, finishErr)
				}
				return
			}
			if finishErr != nil || result.State != "complete" || result.Account == nil || receipt.Receipt == nil || *result.Account != *receipt.Receipt ||
				current.Generation != original.Generation+1 || string(current.Auth) != `{"token":"PRIVATE-CREDENTIAL"}` {
				t.Fatalf("unchanged configuration lost its publication: state=%s phase=%s generation=%d err=%v", result.State, receipt.Phase, current.Generation, finishErr)
			}
			replay, err := manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", view.Flow)
			if err != nil || replay.State != "complete" || replay.Account == nil || *replay.Account != *result.Account || authWindowTrace(t, config.Trace, "finish") != 1 {
				t.Fatalf("finish did not replay its completed receipt: %s %v", replay.State, err)
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
