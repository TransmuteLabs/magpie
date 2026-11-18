package extension

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func fixtureAuthRefresh(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var auth struct {
		URL  string `json:"url"`
		Mode string `json:"mode"`
	}
	if json.Unmarshal(call.Auth, &auth) != nil {
		return nil, errors.New("invalid fixture auth")
	}
	if auth.Mode == "not-started" {
		return nil, &sdk.Error{Code: "fixture", Message: "synthetic-private-secret", Outcome: "not-started"}
	}
	if auth.Mode == "callbacks" {
		if err := stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: "refresh-fixture"}, new(sdk.StateValue)); err != nil {
			return nil, err
		}
	}
	if auth.URL == "" {
		return sdk.AuthResult{ID: call.Scope.Account, Credentials: call.Auth}, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.URL, bytes.NewReader(call.Auth))
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result sdk.AuthResult
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("malformed fixture refresh reply")
	}
	return result, nil
}

func refreshScope() sdk.Scope {
	return sdk.Scope{Plugin: "fixture", Provider: "provider", Account: "account", Generation: 1}
}

func refreshFixture(t *testing.T, handler http.HandlerFunc) (*Manager, sdk.Scope, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	path := "/sdk-refresh/" + rand.Text()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != path {
			http.NotFound(writer, request)
			return
		}
		calls.Add(1)
		handler(writer, request)
	}))
	t.Cleanup(server.Close)
	manager, _ := fixtureManager(t)
	scope := refreshScope()
	auth, _ := json.Marshal(map[string]string{"url": server.URL + path, "mode": "callbacks"})
	if _, err := manager.Store().SaveAccount(Account{
		Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: auth,
	}, 0); err != nil {
		t.Fatal(err)
	}
	return manager, scope, &calls
}

func refreshResponse(writer http.ResponseWriter) {
	_, _ = io.WriteString(writer, `{"id":"account","name":"Fixture","expiresAt":1800000000000,"credentials":{"token":"rotated-private-secret"}}`)
}

func TestRefreshConcurrentCallsPublishOneGeneration(t *testing.T) {
	manager, scope, calls := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		refreshResponse(writer)
	})
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			result, err := manager.Call(context.Background(), scope.Plugin, sdk.MethodAuthRefresh, scope, sdk.AuthRefreshRequest{}, nil)
			if err != nil {
				t.Errorf("refresh failed: %v", err)
				return
			}
			var receipt sdk.AuthResult
			if json.Unmarshal(result, &receipt) != nil || receipt.ID != scope.Account || len(receipt.Credentials) != 0 {
				t.Errorf("refresh did not return a sanitized receipt")
			}
		})
	}
	wait.Wait()
	account, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || account.Generation != 2 || calls.Load() != 1 {
		t.Fatalf("refresh did not publish exactly one exchange/generation: %d exchanges, generation %d, %v", calls.Load(), account.Generation, err)
	}
}

func TestRefreshCallerCancellationAfterExchangeStillPublishes(t *testing.T) {
	exchanged, release := make(chan struct{}), make(chan struct{})
	manager, scope, calls := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		close(exchanged)
		<-release
		refreshResponse(writer)
	})
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, err := manager.RefreshAccount(ctx, scope.Plugin, scope)
		returned <- err
	}()
	<-exchanged
	cancel()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter = %v", err)
	}
	close(release)
	manager.refreshWG.Wait()
	account, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || account.Generation != 2 || account.ExpiresAt != 1800000000000 || string(account.Auth) != `{"token":"rotated-private-secret"}` || calls.Load() != 1 {
		t.Fatalf("owned publication = generation %d, exchanges %d, error %v", account.Generation, calls.Load(), err)
	}
	receipt, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope)
	if err != nil || receipt.ID != scope.Account || len(receipt.Credentials) != 0 || calls.Load() != 1 {
		t.Fatalf("completed receipt replay: %+v %v", receipt, err)
	}
}

func TestRefreshManualReplacementOrDeletionWins(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			exchanged, release := make(chan struct{}), make(chan struct{})
			manager, scope, calls := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
				close(exchanged)
				<-release
				refreshResponse(writer)
			})
			returned := make(chan error, 1)
			go func() {
				_, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope)
				returned <- err
			}()
			<-exchanged
			if deleted {
				if err := manager.Store().DeleteAccount(scope.Plugin, scope.Provider, scope.Account, 1); err != nil {
					t.Fatal(err)
				}
			} else if _, err := manager.Store().SaveAccount(Account{
				Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
				Auth: json.RawMessage(`{"token":"manual"}`),
			}, 1); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-returned; err == nil {
				t.Fatal("stale refresh published")
			}
			account, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
			if deleted && err == nil || !deleted && (err != nil || account.Generation != 2 || string(account.Auth) != `{"token":"manual"}`) {
				t.Fatalf("manual replacement lost: generation %d, %v", account.Generation, err)
			}
			_, _ = manager.RefreshAccount(context.Background(), scope.Plugin, scope)
			if calls.Load() != 1 {
				t.Fatal("old generation was exchanged again")
			}
		})
	}
}

func TestRefreshMalformedResultsFenceReplay(t *testing.T) {
	for _, response := range []string{
		`{"id":"foreign","credentials":{"token":"synthetic-private-secret"}}`,
		`{"id":"account","pending":true,"credentials":{"token":"synthetic-private-secret"}}`,
		`{"id":"account","credentials":null}`,
		`{"id":"account","credentials":{}} trailing`,
	} {
		t.Run(response, func(t *testing.T) {
			manager, scope, calls := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.WriteString(writer, response)
			})
			_, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope)
			if err == nil || strings.Contains(err.Error(), "synthetic-private-secret") {
				t.Fatalf("malformed result was not safely refused: %v", err)
			}
			scope.Operation, scope.Session = "different-operation", "different-session"
			_, err = manager.RefreshAccount(context.Background(), scope.Plugin, scope)
			if err == nil || calls.Load() != 1 {
				t.Fatalf("unresolved replay = %d calls, %v", calls.Load(), err)
			}
			account, _ := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
			if account.Generation != 1 {
				t.Fatal("malformed result changed credentials")
			}
		})
	}
}

func TestRefreshLockCancellationAndIndependentAccount(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	unlock, err := store.lockRefresh(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	other := NewStore(root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := other.lockRefresh(ctx, scope); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock: %v", err)
	}
	otherScope := scope
	otherScope.Account = "other-account"
	release, err := other.lockRefresh(context.Background(), otherScope)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := store.StateCAS(scope, "during-refresh", 0, json.RawMessage(`1`)); err != nil {
		t.Fatalf("global store lock was retained: %v", err)
	}
}

func TestRefreshNotStartedCanUseNewHostAttempt(t *testing.T) {
	store, _ := storeTest(t)
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	if _, err := store.claimRefresh(scope, "revision", "0"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.finishRefresh(scope, nil, "not-started"); err != nil {
		t.Fatal(err)
	}
	next := scope
	next.Operation = "new-host-operation"
	if _, err := store.claimRefresh(next, "revision", "0"); err != nil {
		t.Fatal(err)
	}
	previous, err := store.Operation(scope.Plugin, scope.Operation)
	if err != nil || previous.State != "not-started" {
		t.Fatalf("historical outcome = %+v %v", previous, err)
	}
	if _, err := store.Journal(next, "completed"); err == nil {
		t.Fatal("ordinary journal modified authoritative refresh")
	}
}

func refreshChild() {
	args := os.Args[len(os.Args)-2:]
	store, scope := NewStore(args[0]), storeTestScope()
	switch args[1] {
	case "orphan", "commit":
		unlock, err := store.lockRefresh(context.Background(), scope)
		if err != nil {
			os.Exit(11)
		}
		defer unlock()
		if _, err := store.claimRefresh(scope, "revision", "0"); err != nil {
			os.Exit(12)
		}
		if args[1] == "commit" {
			if _, err := store.finishRefresh(scope, &sdk.AuthResult{
				ID: scope.Account, Credentials: json.RawMessage(`{"token":"rotated"}`),
			}, "completed"); err != nil {
				os.Exit(13)
			}
		}
		os.Exit(23)
	case "manager":
		manager := NewManager(args[0])
		scope := refreshScope()
		result, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope)
		if err != nil {
			os.Exit(14)
		}
		if err := manager.Close(context.Background()); err != nil {
			os.Exit(15)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	}
}

func TestRefreshCrashFencesOrDeduplicates(t *testing.T) {
	for _, mode := range []string{"orphan", "commit"} {
		t.Run(mode, func(t *testing.T) {
			store, root := storeTest(t)
			scope := storeTestScope()
			storeTestAccount(t, store, scope)
			command := exec.Command(os.Args[0], "--sdk-refresh-child", root, mode)
			err := command.Run()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 23 {
				t.Fatalf("crash fixture = %v", err)
			}
			unlock, err := store.lockRefresh(context.Background(), scope)
			if err != nil {
				t.Fatalf("OS did not release crashed owner: %v", err)
			}
			defer unlock()
			receipt, err := store.refreshStatus(scope)
			if mode == "commit" {
				if err != nil || receipt == nil || receipt.ID != scope.Account || len(receipt.Credentials) != 0 {
					t.Fatalf("atomic completed receipt = %+v %v", receipt, err)
				}
			} else if err == nil || receipt != nil {
				t.Fatalf("orphan dispatch was not fenced: %+v %v", receipt, err)
			}
			entry, err := store.Operation(scope.Plugin, scope.Operation)
			if err != nil || entry.State != map[string]string{"orphan": "uncertain", "commit": "completed"}[mode] {
				t.Fatalf("authoritative operation = %+v %v", entry, err)
			}
			data, _ := os.ReadFile(filepath.Join(root, "private", scope.Plugin, "accounts.json"))
			var legacy struct {
				Schema   int `json:"schema"`
				Accounts []struct {
					Account
					Deleted bool `json:"deleted,omitempty"`
				} `json:"accounts"`
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&legacy) == nil {
				t.Fatal("legacy reader accepted transaction metadata")
			}
		})
	}
}

func TestRefreshTwoProcessesExchangeOnce(t *testing.T) {
	manager, _, calls := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		refreshResponse(writer)
	})
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			body, err := exec.CommandContext(ctx, os.Args[0], "--sdk-refresh-child", manager.Namespace(), "manager").CombinedOutput()
			var result sdk.AuthResult
			if err != nil || json.Unmarshal(body, &result) != nil || result.ID != "account" || len(result.Credentials) != 0 {
				t.Errorf("child result = %q %v", body, err)
			}
		})
	}
	wait.Wait()
	if calls.Load() != 1 {
		t.Fatalf("cross-process exchanges = %d", calls.Load())
	}
}

func TestRefreshCloseJoinsOwnerAndLeavesDurableFence(t *testing.T) {
	exchanged, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	manager, scope, calls := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		close(exchanged)
		select {
		case <-request.Context().Done():
			close(canceled)
		case <-release:
		}
	})
	returned := make(chan error, 1)
	go func() {
		_, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope)
		returned <- err
	}()
	<-exchanged
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("shutdown did not release the fixture's actual HTTP request")
	}
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown did not cancel the owned RPC: %v", err)
	}
	if _, err := manager.Store().refreshStatus(scope); err == nil {
		t.Fatal("shutdown discarded unresolved dispatch fence")
	}
	next := NewManager(manager.Namespace())
	defer next.Close(context.Background())
	if _, err := next.RefreshAccount(ctx, scope.Plugin, scope); err == nil || calls.Load() != 1 {
		t.Fatalf("restart repeated canceled exchange: %d, %v", calls.Load(), err)
	}
}

func TestRefreshSelectedLeaseSurvivesCallerAndConfigReplacement(t *testing.T) {
	exchanged, release := make(chan struct{}), make(chan struct{})
	manager, scope, _ := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		close(exchanged)
		<-release
		refreshResponse(writer)
	})
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := manager.Acquire(ctx, scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	go func() {
		_, err := lease.Call(ctx, sdk.MethodAuthRefresh, scope, sdk.AuthRefreshRequest{}, nil)
		returned <- err
	}()
	<-exchanged
	version := lease.ConfigVersion()
	cancel()
	lease.Close()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("original lease waiter = %v", err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(context.Background(), scope.Plugin, version, json.RawMessage(`{"revision":"new"}`)); err != nil {
		t.Fatal(err)
	}
	close(release)
	manager.refreshWG.Wait()
	account, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || account.Generation != 2 {
		t.Fatalf("pinned old refresh did not finish: generation %d, %v", account.Generation, err)
	}
	if err := manager.Store().withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		record := records.Accounts[0].Refresh
		if record == nil || record.Config != version || record.Revision != lease.Revision() {
			t.Error("refresh switched its selected package/config")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshSymlinkAndUnreadableClaimFailClosed(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	unlock, err := store.lockRefresh(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "private", scope.Plugin, "refresh-locks", storeHash(scope.Provider, scope.Account), ".lock")
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	held, err := filelock.TryLock(file)
	file.Close()
	if held || err != nil {
		t.Fatalf("independent descriptor bypassed OS account lock: held=%v %v", held, err)
	}
	unlock()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := store.lockRefresh(context.Background(), scope); err == nil {
		t.Fatal("refresh accepted a symlink lock")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "untouched" {
		t.Fatal("symlink target changed")
	}
	accounts := filepath.Join(root, "private", scope.Plugin, "accounts.json")
	broken := []byte(`{"schema":2,"accounts":"unreadable"}`)
	if err := os.WriteFile(accounts, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.claimRefresh(scope, "revision", "0"); err == nil {
		t.Fatal("refresh overwrote an unreadable account document")
	}
	got, _ := os.ReadFile(accounts)
	if !bytes.Equal(got, broken) {
		t.Fatal("unreadable source changed")
	}
}

func TestRefreshNotStartedRPCIsSafeToIntentionallyRetry(t *testing.T) {
	manager, _ := fixtureManager(t)
	scope := refreshScope()
	if _, err := manager.Store().SaveAccount(Account{
		Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
		Auth: json.RawMessage(`{"mode":"not-started"}`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	var previous string
	for range 2 {
		_, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope)
		var outcome *OutcomeError
		if !errors.As(err, &outcome) || outcome.Outcome != "not-started" || strings.Contains(err.Error(), "synthetic-private-secret") {
			t.Fatalf("proven not-started = %v", err)
		}
		if err := manager.Store().withStore(scope.Plugin, func(root *os.Root) error {
			records, err := storeReadAccounts(root, scope.Plugin)
			if err != nil {
				return err
			}
			record := records.Accounts[0].Refresh
			if record == nil || record.State != "not-started" || record.Scope.Operation == previous {
				t.Fatal("intentional retry reused an old dispatch claim")
			}
			if previous != "" {
				entry, found, err := storeReadOperation(root, scope.Plugin, previous)
				if err != nil || !found || entry.State != "not-started" {
					t.Fatalf("previous safe attempt was not archived: %+v %v", entry, err)
				}
			}
			previous = record.Scope.Operation
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRefreshPublicationIOFailureCannotReportSuccessOrReplay(t *testing.T) {
	exchanged, release := make(chan struct{}), make(chan struct{})
	manager, scope, calls := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		close(exchanged)
		<-release
		refreshResponse(writer)
	})
	returned := make(chan error, 1)
	go func() {
		_, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope)
		returned <- err
	}()
	<-exchanged
	accounts := filepath.Join(manager.Namespace(), "private", scope.Plugin, "accounts.json")
	backup := accounts + ".failure-fixture"
	if err := os.Rename(accounts, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-returned; err == nil {
		t.Fatal("failed account publication reported success")
	}
	if err := os.Remove(accounts); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, accounts); err != nil {
		t.Fatal(err)
	}
	_, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("publication failure repeated rotation: %d %v", calls.Load(), err)
	}
	account, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || account.Generation != 1 {
		t.Fatalf("failed commit changed generation: %d %v", account.Generation, err)
	}
}

func TestRefreshExpiryValidationAndDeletion(t *testing.T) {
	store, _ := storeTest(t)
	scope := storeTestScope()
	account := Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
		Auth: json.RawMessage(`{"token":"fixture"}`), ExpiresAt: -1}
	if _, err := store.SaveAccount(account, 0); err == nil {
		t.Fatal("negative expiry was accepted")
	}
	account.ExpiresAt = 1800000000000
	saved, err := store.SaveAccount(account, 0)
	if err != nil || saved.ExpiresAt != account.ExpiresAt {
		t.Fatalf("expiry publication: %+v %v", saved, err)
	}
	if err := store.DeleteAccount(scope.Plugin, scope.Provider, scope.Account, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err == nil && records.Accounts[0].ExpiresAt != 0 {
			t.Fatal("deleted account retained credential expiry")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshDifferentAccountsCanExchangeIndependently(t *testing.T) {
	exchanged, release := make(chan struct{}), make(chan struct{})
	manager, scope, _ := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		var auth struct {
			Account string `json:"account"`
		}
		_ = json.NewDecoder(request.Body).Decode(&auth)
		if auth.Account == "" {
			close(exchanged)
			<-release
			refreshResponse(writer)
		} else {
			_, _ = io.WriteString(writer, `{"id":"other","credentials":{"token":"other-rotated"}}`)
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	other := scope
	other.Account = "other"
	account, _ := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	var auth map[string]string
	_ = json.Unmarshal(account.Auth, &auth)
	auth["account"] = other.Account
	body, _ := json.Marshal(auth)
	if _, err := manager.Store().SaveAccount(Account{
		Plugin: other.Plugin, Provider: other.Provider, ID: other.Account, Auth: body,
	}, 0); err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	go func() {
		_, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope)
		returned <- err
	}()
	<-exchanged
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, err := manager.RefreshAccount(ctx, other.Plugin, other)
	if err != nil || receipt.ID != other.Account {
		t.Fatalf("unrelated account blocked behind refresh: %+v %v", receipt, err)
	}
	close(release)
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
}

func TestRefreshFixtureCountsOnlyItsOwnExchangeEndpoint(t *testing.T) {
	manager, scope, calls := refreshFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		refreshResponse(writer)
	})
	account, _ := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	var auth struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(account.Auth, &auth); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		endpoint := auth.URL
		if method == http.MethodPost {
			endpoint += "/other"
		}
		request, _ := http.NewRequest(method, endpoint, nil)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatal("unrelated probe reached exchange handler")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unrelated probe was counted as credential rotation")
	}
	if _, err := manager.RefreshAccount(context.Background(), scope.Plugin, scope); err != nil || calls.Load() != 1 {
		t.Fatalf("actual refresh endpoint = %d calls, %v", calls.Load(), err)
	}
}
