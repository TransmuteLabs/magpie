package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionAccountObservationSharesOnlyTheNextScan(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	empty, err := extensionhost.AccountStamp()
	if err != nil {
		t.Fatal(err)
	}
	store := extensionhost.NewStore(filepath.Dir(extensionhost.CatalogPath()))
	if _, err := store.SaveAccount(extensionhost.Account{Plugin: "fixture", Provider: "vendor",
		ID: "alice", Auth: json.RawMessage(`{"access":"fixture"}`)}, 0); err != nil {
		t.Fatal(err)
	}
	account, err := extensionhost.AccountStamp()
	if err != nil || account.Equal(empty) {
		t.Fatalf("account fixture did not change the stamp: %v", err)
	}
	for _, attempt := range []struct {
		name             string
		scanError        error
		repairedRevision extensionAccountFiles
	}{
		{name: "sharing", repairedRevision: 3},
		{name: "persistent_error", scanError: errors.New("metadata unavailable"), repairedRevision: 4},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var observations accountFileObservations
				var reads atomic.Int64
				firstFinished, secondFinished := make(chan struct{}), make(chan struct{})
				finishFirst := sync.OnceFunc(func() { close(firstFinished) })
				finishSecond := sync.OnceFunc(func() { close(secondFinished) })
				defer finishFirst()
				defer finishSecond()
				repaired := false
				read := func() (extensionhost.AccountFileStamp, error) {
					switch reads.Add(1) {
					case 1:
						<-firstFinished
						return empty, nil
					case 2:
						<-secondFinished
						if attempt.scanError == nil {
							return account, nil
						}
					}
					if !repaired && attempt.scanError != nil {
						return extensionhost.AccountFileStamp{}, attempt.scanError
					}
					return empty, nil
				}
				start := func() <-chan extensionAccountFiles {
					result := make(chan extensionAccountFiles, 1)
					go func() { result <- observations.observe(read) }()
					return result
				}
				first := start()
				synctest.Wait()
				leader := start()
				synctest.Wait()
				waiting := make([]<-chan extensionAccountFiles, 16)
				for index := range waiting {
					waiting[index] = start()
				}
				synctest.Wait()
				if got := reads.Load(); got != 1 {
					t.Fatalf("started %d scans before the first finished", got)
				}
				finishFirst()
				synctest.Wait()
				if got := reads.Load(); got != 2 {
					t.Fatalf("queued hold did not start a fresh scan, reads=%d", got)
				}
				later := start()
				synctest.Wait()
				finishSecond()
				if firstRevision, leaderRevision := <-first, <-leader; firstRevision != 1 || leaderRevision != 2 {
					t.Fatalf("first and queued observations got revisions %d and %d", firstRevision, leaderRevision)
				}
				batched := 0
				for _, waiter := range waiting {
					if observed := <-waiter; observed == 2 {
						batched++
					}
				}
				if batched != len(waiting) {
					t.Errorf("%d of %d queued holds got the second scan's revision", batched, len(waiting))
				}
				if observed := <-later; observed != 3 {
					t.Errorf("hold arriving during the second scan observed revision %d, want 3", observed)
				}
				if got := reads.Load(); got != 3 {
					t.Errorf("read %d stamps, want three fresh scans", got)
				}
				repaired = true
				if serial := <-start(); serial != attempt.repairedRevision || reads.Load() != 4 {
					t.Errorf("first serial hold after repair: revision=%d reads=%d, want %d and 4",
						serial, reads.Load(), attempt.repairedRevision)
				}
				if repeat := <-start(); repeat != attempt.repairedRevision || reads.Load() != 5 {
					t.Errorf("next serial hold with unchanged stamp: revision=%d reads=%d, want %d and 5",
						repeat, reads.Load(), attempt.repairedRevision)
				}
			})
		})
	}
}

func TestExtensionAccountObservationErrorInvalidatesHeldCatalog(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	originalHeld := heldFor
	heldFor = time.Hour
	t.Cleanup(func() { heldFor = originalHeld })
	release := Hold()
	defer release()
	builds := 0
	build := func() int { builds++; return builds }
	const key = "account-observation-error"
	if got := heldOf(key, build); got != 1 {
		t.Fatalf("initial build = %d, want 1", got)
	}

	private := filepath.Join(filepath.Dir(extensionhost.CatalogPath()), "private")
	if err := os.MkdirAll(filepath.Dir(private), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(private, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	releaseFailed := Hold()
	defer releaseFailed()
	if got := heldOf(key, build); got != 2 {
		t.Fatalf("metadata error left build %d, want 2", got)
	}
	if err := os.Remove(private); err != nil {
		t.Fatal(err)
	}
	releaseRepaired := Hold()
	defer releaseRepaired()
	if got := heldOf(key, build); got != 3 {
		t.Fatalf("metadata recovery left build %d, want 3", got)
	}
}

func TestExtensionAccountObservationDoesNotHoldCatalogLock(t *testing.T) {
	reading := make(chan struct{})
	unblock := make(chan struct{})
	finished := make(chan func())
	go func() {
		finished <- holdWithAccounts(func() extensionAccountFiles {
			close(reading)
			<-unblock
			return extensionAccountFilesNow()
		})
	}()
	<-reading
	available := held.TryLock()
	if available {
		held.Unlock()
	}
	close(unblock)
	release := <-finished
	release()
	if !available {
		t.Fatal("account metadata I/O holds the global catalog mutex")
	}
}

func TestExtensionAccountLateObservationCannotRestoreOldCatalog(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	read := make(chan struct{})
	resume := make(chan struct{})
	finished := make(chan func())
	go func() {
		finished <- holdWithAccounts(func() extensionAccountFiles {
			previous := extensionAccountFilesNow()
			close(read)
			<-resume
			return previous
		})
	}()
	<-read
	private := filepath.Join(filepath.Dir(extensionhost.CatalogPath()), "private", "fixture")
	if err := os.MkdirAll(private, 0o700); err != nil {
		close(resume)
		(<-finished)()
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "accounts.json"), []byte(`{}`), 0o600); err != nil {
		close(resume)
		(<-finished)()
		t.Fatal(err)
	}
	current := extensionAccountFilesNow()
	second := make(chan func())
	go func() {
		second <- holdWithAccounts(func() extensionAccountFiles { return current })
	}()
	select {
	case release := <-second:
		defer release()
	case <-time.After(10 * time.Second):
		close(resume)
		(<-finished)()
		(<-second)()
		t.Fatal("newer account observation could not publish while an old reader was blocked")
	}
	builds := 0
	build := func() int { builds++; return builds }
	if got := heldOf("late-account-observation", build); got != 1 {
		t.Fatal("new snapshot was not built")
	}
	close(resume)
	release := <-finished
	defer release()
	if got := heldOf("late-account-observation", build); got != 1 {
		t.Fatal("late old metadata discarded the newer published catalog")
	}
}

func TestExtensionManualReplacementInvalidatesHeldDiscovery(t *testing.T) {
	const peerRoot = "MAGPIE_TEST_EXTENSION_HOLD_PEER_ROOT"
	const peerPlugin = "MAGPIE_TEST_EXTENSION_HOLD_PEER_PLUGIN"
	if root := os.Getenv(peerRoot); root != "" {
		store := extensionhost.NewStore(root)
		id := os.Getenv(peerPlugin)
		account, err := store.GetAccount(id, "vendor", "alice")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteAccount(id, "vendor", "alice", account.Generation); err != nil {
			t.Fatal(err)
		}
		return
	}
	host := fixtureExtensionProvider(t, true)
	plugin.UseCached(nil)
	root := filepath.Dir(extensionhost.CatalogPath())
	originalStore := host.store
	host.store = extensionhost.NewStore(root)
	host.namespace = root
	for _, id := range []string{"alice", "bob"} {
		account, err := originalStore.GetAccount(host.manifest.ID, "vendor", id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := host.store.SaveAccount(account, 0); err != nil {
			t.Fatal(err)
		}
	}
	originalHeld, originalKept := heldFor, keptFor
	heldFor, keptFor = time.Hour, time.Hour
	t.Cleanup(func() { heldFor, keptFor = originalHeld, originalKept })
	initial := extensionAccounts()[0]
	cacheKey := initial.Account.extension.modelCatalogKey()
	if _, err := initial.Account.extension.writeModelCatalog(t.Context(), sdk.ModelsResult{Models: []sdk.Model{{ID: "old-generation-only"}}}, extensionhost.VersionedValue{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { extensionModelCache.Delete(cacheKey) })
	snapshot := func() (Provider, Stamp, []string) {
		t.Helper()
		defer Hold()()
		var selected Provider
		for _, candidate := range All() {
			if candidate.ID == initial.ID {
				selected = candidate
				break
			}
		}
		if selected.Account == nil {
			t.Fatal("SDK provider was not discovered")
		}
		var models []string
		for _, entry := range Catalog() {
			if strings.HasPrefix(entry.ID, initial.ID+"/") {
				models = append(models, entry.Model)
			}
		}
		return selected, StampNow(), models
	}
	old, previous, models := snapshot()
	oldScope, _ := old.ExtensionScope()
	if oldScope.Generation != 1 || !slices.Contains(models, "old-generation-only") {
		t.Fatal("initial generation was not held")
	}
	foreign := extensionhost.NewStore(root)
	accountPath := filepath.Join(root, "private", host.manifest.ID, "accounts.json")
	accountFile, err := os.Open(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	previousInfo, err := accountFile.Stat()
	accountFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	account := extensionhost.Account{Plugin: host.manifest.ID, Provider: "vendor", ID: "alice",
		Auth: json.RawMessage(`{"access":"changed-alice"}`)}
	if _, err := foreign.SaveAccount(account, oldScope.Generation); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(accountPath, time.Now(), previousInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	replacedInfo, err := os.Stat(accountPath)
	if err != nil || os.SameFile(previousInfo, replacedInfo) ||
		previousInfo.Size() != replacedInfo.Size() || previousInfo.Mode() != replacedInfo.Mode() ||
		!previousInfo.ModTime().Equal(replacedInfo.ModTime()) {
		t.Fatal("replacement fixture did not preserve metadata on a new inode", err)
	}
	current, next, models := snapshot()
	currentScope, _ := current.ExtensionScope()
	if currentScope.Generation != 2 {
		t.Fatalf("separate-store replacement left held discovery at G%d; want G2", currentScope.Generation)
	}
	if previous == next || slices.Contains(models, "old-generation-only") {
		t.Fatal("account replacement retained the old model/source snapshot")
	}
	if err := old.Account.extension.check(context.Background()); err == nil {
		t.Fatal("old binding adopted a manually replaced generation")
	}
	if captured, _ := old.ExtensionScope(); captured.Generation != 1 {
		t.Fatal("old binding was advanced without a refresh receipt")
	}
	host.call = func(_ context.Context, method string, scope sdk.Scope, _ any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderExecute || scope.Generation != 2 {
			return nil, fmt.Errorf("fresh work used a stale binding: %s %+v", method, scope)
		}
		saved, err := host.store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
		if err != nil || string(saved.Auth) != string(account.Auth) {
			return nil, fmt.Errorf("fresh scope selected old credentials")
		}
		if err := consume(extensionhost.Event{Name: "headers", Data: json.RawMessage(`{"status":204}`)}); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"status":204,"outcome":"completed"}`), nil
	}
	request, _ := http.NewRequest(http.MethodPost, current.Chat+"/chat/completions",
		strings.NewReader(`{"model":"m1"}`))
	response, err := current.Do(http.DefaultClient, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	release := Hold()
	defer release()
	All()
	if _, err := foreign.SaveAccount(account, currentScope.Generation); err != nil {
		t.Fatal(err)
	}
	overlapping, _, _ := snapshot()
	overlappingScope, _ := overlapping.ExtensionScope()
	if overlappingScope.Generation != 3 {
		t.Fatal("a new request overlapping an old hold did not discover the replacement generation")
	}
	beforeInPlace, err := extensionhost.AccountStamp()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	replaced := bytes.Replace(contents, []byte(`"generation":3`), []byte(`"generation":4`), 1)
	if bytes.Equal(contents, replaced) || len(contents) != len(replaced) {
		t.Fatal("in-place replacement fixture did not change the generation at constant size")
	}
	if err := os.WriteFile(accountPath, replaced, info.Mode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(accountPath, time.Now(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	afterInPlace, err := extensionhost.AccountStamp()
	if err != nil || !beforeInPlace.Equal(afterInPlace) {
		t.Fatal("in-place replacement did not preserve the optimistic metadata stamp", err)
	}
	optimistic, _, _ := snapshot()
	optimisticScope, _ := optimistic.ExtensionScope()
	if optimisticScope.Generation != 3 {
		t.Fatal("fixture did not retain the old optimistic catalog")
	}
	ctx, attempt := WithExtensionAttempt(context.Background())
	request, _ = http.NewRequestWithContext(ctx, http.MethodPost, optimistic.Chat+"/chat/completions",
		strings.NewReader(`{"model":"m1"}`))
	if err := optimistic.Sign(ctx, request, Chat, nil); err == nil || attempt.Outcome(false) == nil {
		t.Fatal("unchanged metadata permitted stale generation execution or retry patience")
	}
	fresh, beforeSignOut, _ := snapshot()
	freshScope, _ := fresh.ExtensionScope()
	if freshScope.Generation != 4 {
		t.Fatal("stale selection did not invalidate the undetected in-place replacement")
	}
	if original, _ := optimistic.ExtensionScope(); original.Generation != 3 {
		t.Fatal("stale binding adopted the in-place manual successor")
	}
	peerCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := proc.CommandContext(peerCtx, os.Args[0], "-test.run=^TestExtensionManualReplacementInvalidatesHeldDiscovery$", "-test.count=1")
	command.Env = append(os.Environ(), peerRoot+"="+root, peerPlugin+"="+host.manifest.ID)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("peer could not sign out: %v\n%s", err, output)
	}
	remaining, afterSignOut, _ := snapshot()
	if scope, _ := remaining.ExtensionScope(); scope.Account != "bob" || afterSignOut == beforeSignOut {
		t.Fatalf("a new overlapping hold missed the peer logout: %+v", scope)
	}
	if err := fresh.Account.extension.check(t.Context()); err == nil {
		t.Fatal("the provider selected before peer logout can still use the revoked account")
	}
}

func TestExtensionRevokedSelectionKeepsPriorDurableOutcome(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	selected := extensionAccounts()[0]
	scope, _ := selected.ExtensionScope()
	scope.Operation, scope.Attempt = "completed-before-replacement", 1
	if _, err := host.store.Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if _, err := host.store.Journal(scope, "completed"); err != nil {
		t.Fatal(err)
	}
	ctx, attempt := WithExtensionAttempt(context.Background())
	extensionBegin(ctx, scope)
	extensionCompleted(ctx)
	if _, err := extensionhost.NewStore(host.namespace).SaveAccount(extensionhost.Account{
		Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
		Auth: json.RawMessage(`{"access":"replacement"}`),
	}, scope.Generation); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, selected.Chat+"/chat/completions",
		strings.NewReader(`{"model":"m1"}`))
	if err := selected.Sign(ctx, request, Chat, nil); err == nil {
		t.Fatal("replaced binding was not fenced")
	}
	var outcome *extensionhost.OutcomeError
	if !errors.As(attempt.Outcome(true), &outcome) || outcome.Outcome != "completed" {
		t.Fatal("selection failure discarded an earlier durable completion")
	}
	moduleID, operation := attempt.Operation()
	if moduleID != scope.Plugin || operation != scope.Operation {
		t.Fatal("selection failure replaced the original operation identity")
	}
	entry, err := host.store.Operation(scope.Plugin, scope.Operation)
	if err != nil || entry.Scope != scope || entry.State != "completed" {
		t.Fatal("manual replacement altered the durable old operation", err)
	}
}

func TestExtensionRevokedSelectionIsTerminalBeforeDispatch(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	selected := extensionAccounts()[0]
	scope, _ := selected.ExtensionScope()
	foreign := extensionhost.NewStore(host.namespace)
	if _, err := foreign.SaveAccount(extensionhost.Account{Plugin: scope.Plugin, Provider: scope.Provider,
		ID: scope.Account, Auth: json.RawMessage(`{"access":"replacement"}`)}, scope.Generation); err != nil {
		t.Fatal(err)
	}
	dispatched := false
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		dispatched = true
		return nil, fmt.Errorf("revoked selection reached the executor")
	}
	ctx, attempt := WithExtensionAttempt(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, selected.Chat+"/chat/completions",
		strings.NewReader(`{"model":"m1"}`))
	err := selected.Sign(ctx, request, Chat, nil)
	var revoked *sdk.Error
	if !errors.As(err, &revoked) || revoked.Code != "stale_generation" || dispatched {
		t.Fatalf("old selection did not fail closed before dispatch: %v", err)
	}
	outcome := attempt.Outcome(false)
	if outcome == nil {
		t.Fatal("revoked selection remains eligible for generic retry/patience instead of terminal rejection")
	}
	var terminal *sdk.Error
	if !errors.As(outcome, &terminal) || terminal.Code != "stale_generation" ||
		terminal.Outcome == "accepted" || terminal.Outcome == "uncertain" || terminal.Outcome == "completed" {
		t.Fatal("a revoked pre-dispatch selection was mislabeled as an accepted business write")
	}
	if captured, _ := selected.ExtensionScope(); captured.Generation != scope.Generation {
		t.Fatal("manual replacement was adopted without a refresh receipt")
	}
}

func TestExtensionRevokedSelectionPreservesStartedOutcomes(t *testing.T) {
	for _, state := range []string{"accepted", "uncertain", "completed", "replay"} {
		t.Run(state, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			selected := extensionAccounts()[0]
			scope, _ := selected.ExtensionScope()
			scope.Operation, scope.Attempt = "started-before-replacement", 1
			if _, err := host.store.Journal(scope, "dispatched"); err != nil {
				t.Fatal(err)
			}
			durable := state
			if state == "replay" {
				durable = "completed"
			}
			if _, err := host.store.Journal(scope, durable); err != nil {
				t.Fatal(err)
			}
			ctx, attempt := WithExtensionAttempt(context.Background())
			extensionBegin(ctx, scope)
			var prior error
			switch state {
			case "accepted":
				extensionAccepted(ctx)
			case "completed":
				extensionCompleted(ctx)
			case "uncertain":
				prior = &extensionhost.OutcomeError{Outcome: "uncertain"}
				extensionOutcome(ctx, prior)
			case "replay":
				prior = &sdk.Error{Code: "no_replay", Message: "Already completed.", Outcome: "completed"}
				extensionOutcome(ctx, prior)
			}
			if _, err := extensionhost.NewStore(host.namespace).SaveAccount(extensionhost.Account{
				Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
				Auth: json.RawMessage(`{"access":"replacement"}`),
			}, scope.Generation); err != nil {
				t.Fatal(err)
			}
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, selected.Chat+"/chat/completions",
				strings.NewReader(`{"model":"m1"}`))
			if err := selected.Sign(ctx, request, Chat, nil); err == nil {
				t.Fatal("replaced binding was not rejected")
			}
			result := attempt.Outcome(true)
			if prior != nil {
				if result != prior {
					t.Fatal("selection failure replaced the earlier terminal error")
				}
			} else {
				var outcome *extensionhost.OutcomeError
				if !errors.As(result, &outcome) || outcome.Outcome != state {
					t.Fatalf("selection failure downgraded %s: %v", state, result)
				}
			}
			moduleID, operation := attempt.Operation()
			if moduleID != scope.Plugin || operation != scope.Operation {
				t.Fatal("selection failure changed the original operation identity")
			}
			entry, err := host.store.Operation(scope.Plugin, scope.Operation)
			if err != nil || entry.State != durable || entry.Scope != scope {
				t.Fatal("selection failure changed the durable old operation", err)
			}
			if _, err := host.store.Journal(scope, "dispatched"); err == nil {
				t.Fatal("selection failure permitted replay of started work")
			}
		})
	}
}
