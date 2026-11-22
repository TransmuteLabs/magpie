package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func usageChild() {
	args := os.Args[len(os.Args)-2:]
	var scope sdk.Scope
	if json.Unmarshal([]byte(args[1]), &scope) != nil {
		os.Exit(11)
	}
	if _, err := NewStore(args[0]).InvalidateUsage(scope); err != nil {
		os.Exit(12)
	}
}

func TestUsageEpochCrossProcessFencesExistingStore(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	before, _ := store.UsageStamp(scope)
	raw, _ := json.Marshal(scope)
	if body, err := exec.Command(os.Args[0], "--sdk-usage-child", root, string(raw)).CombinedOutput(); err != nil {
		t.Fatalf("foreign host invalidation = %v %q", err, body)
	}
	after, err := store.UsageStamp(scope)
	if err != nil || after.Revision != before.Revision+1 || !after.Dirty {
		t.Fatalf("existing process missed durable epoch: %+v %v", after, err)
	}
	if _, err := store.AcknowledgeUsage(scope, before.Revision); err == nil {
		t.Fatal("pre-mutation reader acknowledged after a foreign process invalidation")
	}
	if err := store.UsageReady(scope); err == nil {
		t.Fatal("foreign mutation admitted inference")
	}
}

func usageManager(t *testing.T) (*Manager, sdk.Scope) {
	t.Helper()
	return usageManagerVersion(t, 4)
}

func usageManagerVersion(t *testing.T, minor int, configure ...func(*sdk.Manifest)) (*Manager, sdk.Scope) {
	t.Helper()
	path := fixturePackage(t)
	raw, _ := os.ReadFile(filepath.Join(path, sdk.ManifestFile))
	var manifest sdk.Manifest
	_ = json.Unmarshal(raw, &manifest)
	manifest.Contract.Version.Minor = minor
	manifest.Compatibility = &sdk.Compatibility{HostAPI: "^1." + strconv.Itoa(minor)}
	manifest.Actions = append(manifest.Actions, sdk.ActionDescriptor{
		ID: "fixture.auth-read", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	for _, option := range configure {
		option(&manifest)
	}
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(path, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	scope := refreshScope()
	storeTestAccount(t, manager.Store(), scope)
	return manager, scope
}

func TestUsageEpochFencesInferenceUntilMatchedFreshAcknowledgment(t *testing.T) {
	manager, scope := usageManager(t)
	before, _ := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if _, err := manager.Call(context.Background(), scope.Plugin, sdk.MethodToolCall, scope,
		sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"usage-invalidate"}`)}, nil); err != nil {
		t.Fatal(err)
	}
	stamp, err := manager.Store().UsageStamp(scope)
	if err != nil || stamp.Revision != 1 || !stamp.Dirty {
		t.Fatalf("invalidation = %+v %v", stamp, err)
	}
	after, _ := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("usage invalidation changed credentials, generation or expiry")
	}
	if _, err := manager.Call(context.Background(), scope.Plugin, sdk.MethodProviderExecute, scope,
		sdk.ExecuteRequest{Protocol: "chat", Model: "model", Body: []byte(`{}`)}, nil); err == nil {
		t.Fatal("dirty account admitted inference")
	}
	if _, err := manager.Store().AcknowledgeUsage(scope, 0); err == nil {
		t.Fatal("pre-mutation acknowledgment reopened inference")
	}
	if _, err := manager.Call(context.Background(), scope.Plugin, sdk.MethodProviderUsage, scope, sdk.UsageRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.Store().UsageReady(scope); err != nil {
		t.Fatal("matching fresh acknowledgment did not reopen admission:", err)
	}
	if _, err := manager.Call(context.Background(), scope.Plugin, sdk.MethodProviderExecute, scope,
		sdk.ExecuteRequest{Protocol: "chat", Model: "model", Body: []byte(`{}`)}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUsageRPCDoesNotAcknowledgeAfterCancellationOrPolicyChange(t *testing.T) {
	for _, test := range []string{"cancellation", "account discovery off", "module reenabled"} {
		t.Run(test, func(t *testing.T) {
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			manager, scope := usageManager(t)
			store := manager.Store()
			before, err := store.InvalidateUsage(scope)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := os.OpenFile(filepath.Join(manager.Namespace(), "private", ".lock"), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			defer filelock.Unlock(lock)
			replied, resume := make(chan struct{}), make(chan struct{})
			checks := 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = context.WithValue(ctx, windowAdmissionKey{}, func(context.Context) error {
				checks++
				if checks != 2 {
					return nil
				}
				held, err := filelock.TryShared(lock)
				if err != nil {
					return err
				}
				if !held {
					return errors.New("fixture could not hold the acknowledgment lock")
				}
				close(replied)
				if test == "module reenabled" {
					select {
					case <-resume:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			})
			done := make(chan error, 1)
			go func() {
				_, err := manager.Call(ctx, scope.Plugin, sdk.MethodProviderUsage, scope, sdk.UsageRequest{}, nil)
				done <- err
			}()
			select {
			case <-replied:
			case err := <-done:
				t.Fatalf("usage did not reach acknowledgment: %v", err)
			case <-time.After(5 * time.Second):
				cancel()
				filelock.Unlock(lock)
				<-done
				t.Fatal("usage result did not arrive")
			}
			switch test {
			case "cancellation":
				cancel()
				select {
				case err = <-done:
				case <-time.After(time.Second):
					t.Error("canceled usage RPC still waited for its acknowledgment lock")
					filelock.Unlock(lock)
					err = <-done
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("usage RPC returned %v, want cancellation", err)
				}
			case "account discovery off":
				t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
				filelock.Unlock(lock)
				if err := <-done; err == nil {
					t.Error("usage RPC acknowledged after account discovery was disabled")
				}
				t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			case "module reenabled":
				if err := manager.SetEnabled(t.Context(), scope.Plugin, false); err == nil {
					err = manager.SetEnabled(t.Context(), scope.Plugin, true)
					if err != nil {
						t.Error(err)
					}
				} else {
					t.Error(err)
				}
				filelock.Unlock(lock)
				close(resume)
				if err := <-done; err == nil {
					t.Error("usage RPC acknowledged for a superseded enable generation")
				}
			}
			filelock.Unlock(lock)
			after, err := store.UsageStamp(scope)
			if err != nil || after != before {
				t.Fatalf("rejected usage RPC changed the dirty revision: before=%+v after=%+v err=%v", before, after, err)
			}
			if _, err := manager.Call(t.Context(), scope.Plugin, sdk.MethodProviderUsage, scope, sdk.UsageRequest{}, nil); err != nil {
				t.Fatal(err)
			}
			if err := store.UsageReady(scope); err != nil {
				t.Fatal("a fresh read could not acknowledge:", err)
			}
		})
	}
}

func TestUsageRPCCleanReplyCannotOutliveInvalidation(t *testing.T) {
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager, scope := usageManager(t)
	peer := NewStore(manager.Namespace())
	admissions := 0
	ctx := context.WithValue(t.Context(), windowAdmissionKey{}, func(context.Context) error {
		admissions++
		if admissions != 2 {
			return nil
		}
		_, err := peer.InvalidateUsage(scope)
		return err
	})
	_, err := manager.Call(ctx, scope.Plugin, sdk.MethodProviderUsage, scope, sdk.UsageRequest{}, nil)
	var stale *sdk.Error
	if !errors.As(err, &stale) || stale.Code != "usage_stale" {
		t.Fatalf("clean reply after invalidation = %v, want usage_stale", err)
	}
	stamp, err := manager.Store().UsageStamp(scope)
	if err != nil || stamp.Revision != 1 || !stamp.Dirty {
		t.Fatalf("late reply changed the new revision: %+v %v", stamp, err)
	}
	if _, err := manager.Call(t.Context(), scope.Plugin, sdk.MethodProviderUsage, scope, sdk.UsageRequest{}, nil); err != nil {
		t.Fatal("fresh usage read did not acknowledge the new revision:", err)
	}
}

func TestUsageRPCRejectsInvalidTimestampsBeforeAcknowledgment(t *testing.T) {
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager, scope := usageManagerVersion(t, 4, func(manifest *sdk.Manifest) {
		manifest.Args = []string{"--sdk-raw-usage"}
	})
	store := manager.Store()
	for _, scenario := range []string{"reset-out-of-range", "null-reset", "null-window-as-of", "null-result-as-of"} {
		t.Run(scenario, func(t *testing.T) {
			before, err := store.InvalidateUsage(scope)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			badScope := scope
			badScope.Session = scenario
			if _, err := manager.Call(ctx, scope.Plugin, sdk.MethodProviderUsage, badScope, sdk.UsageRequest{}, nil); err == nil {
				t.Error("usage RPC accepted an invalid timestamp")
			}
			if after, err := store.UsageStamp(scope); err != nil || after != before {
				t.Fatalf("invalid timestamp cleared the dirty revision: before=%+v after=%+v err=%v", before, after, err)
			}
			if _, err := manager.Call(ctx, scope.Plugin, sdk.MethodProviderUsage, scope, sdk.UsageRequest{}, nil); err != nil {
				t.Fatal("usage RPC rejected the last valid reset millisecond:", err)
			}
			if after, err := store.UsageStamp(scope); err != nil || after.Dirty || after.Revision != before.Revision {
				t.Fatalf("valid reset did not acknowledge the dirty revision: %+v %v", after, err)
			}
		})
	}
}

func TestUsageRPCCleanReadSharesStoreLock(t *testing.T) {
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager, scope := usageManager(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	lock, err := os.OpenFile(filepath.Join(manager.Namespace(), "private", ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	held, err := filelock.TryShared(lock)
	if err != nil || !held {
		t.Fatalf("fixture could not hold the shared Store lock: %v", err)
	}
	defer filelock.Unlock(lock)
	var result sdk.UsageResult
	if err := manager.Invoke(ctx, scope.Plugin, sdk.MethodProviderUsage, scope, sdk.UsageRequest{}, &result, nil); err != nil {
		t.Fatal("clean quota read needed the writer lock:", err)
	}
	if len(result.Windows) != 1 || result.Windows[0].Used != 12 || result.Revision != "0" {
		t.Fatalf("unexpected clean quota result: %+v", result)
	}
}

func TestUsageExecutionPreflightStopsAtDeadline(t *testing.T) {
	manager, scope := usageManager(t)
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	unlock, held := filelock.Lock(filepath.Join(manager.Namespace(), "private", ".lock"), 0)
	if !held {
		t.Fatal("fixture could not hold the Store lock")
	}
	defer unlock()
	scope.Operation, scope.Attempt = "deadline-without-dispatch", 1
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := lease.Call(ctx, sdk.MethodProviderExecute, scope,
			sdk.ExecuteRequest{Protocol: "chat", Model: "model", Body: json.RawMessage(`{}`)}, nil)
		done <- err
	}()
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Error("usage preflight continued waiting on Store after the deadline")
		unlock()
		err = <-done
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("usage preflight error = %v, want deadline", err)
	}
	unlock()
	if _, err := manager.Store().Operation(scope.Plugin, scope.Operation); err == nil {
		t.Fatal("cancelled preflight dispatched inference")
	}
}

func TestUsageInvalidationReceiptReadRespectsCallbackDeadline(t *testing.T) {
	manager, scope := usageManager(t)
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	scope.Operation, scope.Attempt = "usage-invalidation-deadline", 1
	store := manager.Store()
	if _, err := store.Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Journal(scope, "accepted")
	if err != nil {
		t.Fatal(err)
	}
	authority := hostAuthority{manifest: lease.worker.entry.Manifest, method: sdk.MethodToolCall}
	call := lease.group.withAuthority(t.Context(), sdk.PrimaryComponent, "write")
	ctx, cancel := context.WithTimeout(call, 200*time.Millisecond)
	defer cancel()
	store.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := manager.invalidateUsage(ctx, scope, json.RawMessage(`{}`), authority)
		done <- err
	}()
	select {
	case err := <-done:
		store.mu.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("receipt lookup returned %v, want deadline", err)
		}
	case <-time.After(1200 * time.Millisecond):
		store.mu.Unlock()
		<-done
		t.Fatal("receipt lookup waited for the Store after callback deadline")
	}
	if current, err := store.Operation(scope.Plugin, scope.Operation); err != nil || current != receipt {
		t.Fatalf("canceled callback changed the durable receipt: %+v, %v", current, err)
	}
	if stamp, err := store.UsageStamp(scope); err != nil || stamp.Revision != 0 || stamp.Dirty {
		t.Fatalf("canceled callback changed usage: %+v, %v", stamp, err)
	}
	if _, err := manager.invalidateUsage(call, scope, json.RawMessage(`{}`), authority); err != nil {
		t.Fatal(err)
	}
	if stamp, err := store.UsageStamp(scope); err != nil || stamp.Revision != 1 || !stamp.Dirty {
		t.Fatalf("live callback did not invalidate usage: %+v, %v", stamp, err)
	}
}

type usageInvalidationProbeContext struct {
	context.Context
	observe func()
}

func (ctx usageInvalidationProbeContext) Err() error {
	err := ctx.Context.Err()
	if err == nil {
		ctx.observe()
	}
	return err
}

func TestUsageInvalidationCommitsUnderItsReceiptLock(t *testing.T) {
	manager, scope := usageManager(t)
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	scope.Operation, scope.Attempt = "usage-invalidation-transaction", 1
	store := manager.Store()
	if _, err := store.Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Journal(scope, "accepted")
	if err != nil {
		t.Fatal(err)
	}

	authority := hostAuthority{manifest: lease.worker.entry.Manifest, method: sdk.MethodToolCall}
	call := lease.group.withAuthority(t.Context(), sdk.PrimaryComponent, "write")
	ctx, cancel := context.WithCancel(call)
	defer cancel()
	enteredStore := false
	probe := usageInvalidationProbeContext{Context: ctx, observe: func() {
		if !store.mu.TryLock() {
			enteredStore = true
			return
		}
		store.mu.Unlock()
		if enteredStore {
			cancel()
		}
	}}
	result, err := manager.invalidateUsage(probe, scope, json.RawMessage(`{}`), authority)
	stamp, stampErr := store.UsageStamp(scope)
	if stampErr != nil {
		t.Fatal(stampErr)
	}
	if ctx.Err() != nil {
		if !errors.Is(err, context.Canceled) || stamp.Revision != 0 || stamp.Dirty {
			t.Fatalf("callback invalidated usage after cancellation between Store transactions: %v, %+v", err, stamp)
		}
		t.Fatal("callback released the Store before publishing its accepted invalidation")
	}
	if !enteredStore || err != nil || result != (sdk.UsageInvalidateResult{Revision: "1"}) ||
		stamp.Revision != 1 || !stamp.Dirty {
		t.Fatalf("accepted callback did not publish one invalidation under its receipt lock: %v, %v, %+v", result, err, stamp)
	}
	if current, err := store.Operation(scope.Plugin, scope.Operation); err != nil || current != receipt {
		t.Fatalf("usage invalidation changed the operation receipt: %+v, %v", current, err)
	}
}

func TestUsageEpochIsSelectedDurableAndSurvivesAuthRefresh(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	other := scope
	other.Account = "other"
	storeTestAccount(t, store, other)
	for range 2 {
		if _, err := NewStore(root).InvalidateUsage(scope); err != nil {
			t.Fatal(err)
		}
	}
	stamp, _ := store.UsageStamp(scope)
	unchanged, _ := store.UsageStamp(other)
	if stamp.Revision != 2 || !stamp.Dirty || unchanged.Revision != 0 || unchanged.Dirty {
		t.Fatalf("durable selected metadata = %+v / %+v", stamp, unchanged)
	}
	if _, err := store.claimRefresh(scope, "revision", "0"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.finishRefresh(scope, &sdk.AuthResult{ID: scope.Account, Credentials: json.RawMessage(`{"token":"new"}`)}, "completed"); err != nil {
		t.Fatal(err)
	}
	scope.Generation++
	stamp, err := NewStore(root).UsageStamp(scope)
	if err != nil || stamp.Revision != 2 || !stamp.Dirty {
		t.Fatalf("refresh erased usage fence: %+v %v", stamp, err)
	}
}

func TestBusinessCancelHasSeparateDurableIntent(t *testing.T) {
	manager, scope := usageManager(t)
	target := scope
	target.Operation, target.Attempt = "execution-target", 1
	if _, err := manager.Store().Journal(target, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Store().Journal(target, "completed"); err != nil {
		t.Fatal(err)
	}
	before, _ := manager.Store().Operation(scope.Plugin, target.Operation)
	scope.Operation, scope.Attempt = "cancel-intent", 1
	var result sdk.OperationResult
	if err := manager.Invoke(context.Background(), scope.Plugin, sdk.MethodOperationCancel, scope,
		sdk.OperationRequest{OperationID: target.Operation}, &result, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := manager.Store().Operation(scope.Plugin, target.Operation)
	intent, _ := manager.Store().Operation(scope.Plugin, scope.Operation)
	if !reflect.DeepEqual(before, after) || intent.State != "completed" || result.OperationID != target.Operation {
		t.Fatalf("cancel confused target and intent: %+v %+v", result, intent)
	}
	if _, err := manager.Call(context.Background(), scope.Plugin, sdk.MethodOperationCancel, target,
		sdk.OperationRequest{OperationID: target.Operation}, nil); err == nil {
		t.Fatal("original execution journal was reused as cancel intent")
	}
	if _, err := manager.Call(context.Background(), scope.Plugin, sdk.MethodOperationCancel, scope,
		sdk.OperationRequest{OperationID: "another"}, nil); err == nil {
		t.Fatal("cancel intent was replayed")
	}
}

func TestBusinessCancelLegacyControlRemainsUnjournaled(t *testing.T) {
	manager, scope := usageManagerVersion(t, 3)
	scope.Operation, scope.Attempt = "legacy-target", 1
	var result sdk.OperationResult
	if err := manager.Invoke(context.Background(), scope.Plugin, sdk.MethodOperationCancel, scope,
		sdk.OperationRequest{OperationID: scope.Operation}, &result, nil); err != nil {
		t.Fatal(err)
	}
	if result.OperationID != scope.Operation || result.Outcome != "canceled" {
		t.Fatalf("legacy cancel changed: %+v", result)
	}
	if _, err := manager.Store().Operation(scope.Plugin, scope.Operation); err == nil {
		t.Fatal("legacy control was turned into a new write")
	}
}

func TestBusinessCancelRejectsEqualIntentWithoutExistingJournal(t *testing.T) {
	scope := storeTestScope()
	if err := validateCancelIntent(sdk.OperationRequest{OperationID: scope.Operation}, scope); err == nil {
		t.Fatal("business cancel accepted the target as its intent")
	}
}

func TestUsageInvalidationRequiresAcceptedDeclaredWrite(t *testing.T) {
	manager, scope := usageManager(t)
	lease, err := manager.Acquire(context.Background(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	scope.Operation, scope.Attempt = "mutation", 1
	ctx := context.WithValue(context.Background(), hostAuthorityKey{}, hostAuthority{
		manifest: lease.worker.entry.Manifest, method: sdk.MethodModuleCall,
	})
	write := lease.group.withAuthority(ctx, sdk.PrimaryComponent, "write")
	read := lease.group.withAuthority(ctx, sdk.PrimaryComponent, "read")
	if _, err := manager.hostCall(write, scope, sdk.MethodUsageInvalidate, json.RawMessage(`{}`)); err == nil {
		t.Fatal("unaccepted mutation invalidated usage")
	}
	if _, err := manager.Store().Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Store().Journal(scope, "accepted"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.hostCall(read, scope, sdk.MethodUsageInvalidate, json.RawMessage(`{}`)); err == nil {
		t.Fatal("declared read invalidated usage")
	}
	if _, err := manager.hostCall(read, scope, sdk.MethodStateCAS,
		json.RawMessage(`{"key":"read","value":1}`)); err == nil {
		t.Fatal("API 1.4 read mutated host state")
	}
	if _, err := manager.hostCall(write, scope, sdk.MethodUsageInvalidate, json.RawMessage(`{"account":"other"}`)); err == nil {
		t.Fatal("callback accepted an account selector")
	}
	stale := scope
	stale.Generation++
	if _, err := manager.hostCall(write, stale, sdk.MethodUsageInvalidate, json.RawMessage(`{}`)); err == nil {
		t.Fatal("stale scope invalidated usage")
	}
	if _, err := manager.hostCall(write, scope, sdk.MethodUsageInvalidate, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestUsageEpochNonfreshResultsCannotAcknowledge(t *testing.T) {
	manager, scope := usageManager(t)
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	stamp, err := manager.Store().InvalidateUsage(scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"revision":"0","windows":[{"name":"quota","used":0}]}`,
		`{"revision":"1","readDeferred":true,"windows":[{"name":"quota","used":0}]}`,
		`{"revision":"1","asOf":1,"windows":[{"name":"quota","used":0}]}`,
		`{"revision":"1","windows":[{"name":"quota","unknown":true}]}`,
		`{"revision":"1","windows":[{"name":"quota","used":0,"aside":true}]}`,
		`{"revision":"1","windows":[{"name":"quota","used":0,"error":"unavailable"}]}`,
	} {
		_ = lease.finishUsageRead(t.Context(), scope, &stamp, json.RawMessage(raw))
		if err := manager.Store().UsageReady(scope); err == nil {
			t.Fatalf("nonfresh read reopened inference: %s", raw)
		}
	}
	if _, err := manager.Store().InvalidateUsage(scope); err != nil {
		t.Fatal(err)
	}
	if err := lease.finishUsageRead(t.Context(), scope, &stamp,
		json.RawMessage(`{"revision":"1","windows":[{"name":"quota","used":0}]}`)); err == nil {
		t.Fatal("late matching response acknowledged a newer mutation")
	}
	if err := manager.Store().UsageReady(scope); err == nil {
		t.Fatal("late read reopened inference")
	}
}

func TestUsageEpochAdmissionRechecksAfterOptimisticRead(t *testing.T) {
	store, _ := storeTest(t)
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	if err := store.UsageReady(scope); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InvalidateUsage(scope); err != nil {
		t.Fatal(err)
	}
	scope.Operation, scope.Attempt = "inference-after-mutation", 1
	if _, err := store.journal(t.Context(), scope, "dispatched", true); err == nil {
		t.Fatal("admission used its pre-mutation optimistic read")
	}
	if _, err := store.Operation(scope.Plugin, scope.Operation); err == nil {
		t.Fatal("rejected inference created a dispatch receipt")
	}
}

func TestUsageEpochConcurrentInvalidationsAreNotLost(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	results := make(chan error, 12)
	for range cap(results) {
		go func() {
			_, err := NewStore(root).InvalidateUsage(scope)
			results <- err
		}()
	}
	for range cap(results) {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	stamp, err := store.UsageStamp(scope)
	if err != nil || stamp.Version() != strconv.Itoa(cap(results)) || !stamp.Dirty {
		t.Fatalf("lost invalidation: %+v %v", stamp, err)
	}
}

func TestUsageEpochExhaustionAndCorruptionFailClosed(t *testing.T) {
	store, _ := storeTest(t)
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	if _, err := store.InvalidateUsage(scope); err != nil {
		t.Fatal(err)
	}
	if err := store.withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		records.Accounts[0].Usage = &usageState{Revision: ^uint64(0)}
		return storeWrite(root, "accounts.json", records, storeFileLimit)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InvalidateUsage(scope); err == nil {
		t.Fatal("exhausted revision wrapped")
	}
	if err := store.withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		records.Accounts[0].Usage = &usageState{Revision: 1, Acknowledged: 2}
		return storeWrite(root, "accounts.json", records, storeFileLimit)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UsageReady(scope); err == nil {
		t.Fatal("corrupt acknowledgement admitted inference")
	}
}

func TestUsageEpochMalformedMetadataCannotEraseDirtyFence(t *testing.T) {
	for _, invalid := range []string{
		`{}`,
		`{"revision":1}`,
		`{"acknowledged":0}`,
		`{"revision":null,"acknowledged":0}`,
		`{"revision":0,"acknowledged":null}`,
		`{"revision":0.0,"acknowledged":0}`,
		`{"revision":"0","acknowledged":0}`,
		`{"revision":0,"acknowledged":0,"extra":true}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			store, _ := storeTest(t)
			scope := storeTestScope()
			storeTestAccount(t, store, scope)
			if _, err := store.InvalidateUsage(scope); err != nil {
				t.Fatal(err)
			}
			if err := store.withStore(scope.Plugin, func(root *os.Root) error {
				records, err := storeReadAccounts(root, scope.Plugin)
				if err != nil {
					return err
				}
				raw, err := json.Marshal(records.Accounts[0])
				if err != nil {
					return err
				}
				var account map[string]json.RawMessage
				if err := json.Unmarshal(raw, &account); err != nil {
					return err
				}
				account["usage"] = json.RawMessage(invalid)
				return storeWrite(root, "accounts.json", map[string]any{
					"schema": 3, "accounts": []any{account},
				}, storeFileLimit)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.UsageStamp(scope); err == nil {
				t.Fatal("malformed metadata returned a clean usage stamp")
			}
			if err := store.UsageReady(scope); err == nil {
				t.Fatal("malformed metadata erased the inference barrier")
			}
			if _, err := store.journal(t.Context(), scope, "dispatched", true); err == nil {
				t.Fatal("malformed metadata admitted inference")
			}
		})
	}
}

func TestUsage14AuthContinuationCASIsNotAFinancialWrite(t *testing.T) {
	manager, scope := usageManager(t)
	account := scope
	scope.Account, scope.Generation = "", 0
	scope.Session, scope.Operation, scope.Attempt = "stateful-auth", "auth-control", 1
	var start sdk.AuthStartResult
	if err := manager.Invoke(context.Background(), scope.Plugin, sdk.MethodAuthStart, scope,
		sdk.AuthStartRequest{Method: "stateful"}, &start, nil); err != nil {
		t.Fatal("auth.start could not persist its polling continuation:", err)
	}
	if _, err := manager.Call(context.Background(), scope.Plugin, sdk.MethodModuleCall, scope,
		sdk.ModuleCallRequest{Action: "fixture.auth-read", Input: json.RawMessage(`{}`)}, nil); err == nil {
		t.Fatal("same worker's read action inherited auth state mutation authority")
	}
	escaped, err := manager.Store().StateGet(scope, "auth.read-escape")
	if err != nil || escaped.Version != 0 {
		t.Fatal("same worker's read action persisted unauthorized state")
	}
	var finish sdk.AuthResult
	if err := manager.Invoke(context.Background(), scope.Plugin, sdk.MethodAuthFinish, scope,
		sdk.AuthFinishRequest{Code: "stateful", Continuation: start.Continuation}, &finish, nil); err != nil {
		t.Fatal("auth.finish could not settle its polling continuation:", err)
	}
	continuation, err := manager.Store().StateGet(scope, "auth.continuation")
	if err != nil || continuation.Version != 2 || string(continuation.Value) != `"finished"` {
		t.Fatal("pre-account auth continuation did not settle")
	}
	account.Session, account.Operation, account.Attempt = scope.Session, scope.Operation, scope.Attempt
	if _, err := manager.Store().StateCAS(account, "auth.continuation", 0, json.RawMessage(`"finished"`)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Call(context.Background(), scope.Plugin, sdk.MethodAuthLogout, account,
		sdk.AuthLogoutRequest{}, nil); err != nil {
		t.Fatal("auth.logout could not clean up private state:", err)
	}
	current, err := manager.Store().StateGet(account, "auth.continuation")
	if err != nil || current.Version != 2 || string(current.Value) != `"logged-out"` {
		t.Fatalf("auth continuation did not traverse its private state: %+v %v", current, err)
	}
	if _, err := manager.Store().Operation(scope.Plugin, scope.Operation); err == nil {
		t.Fatal("auth continuation was converted into a financial write receipt")
	}
}

func TestUsage14AuthContinuationCannotBypassRecallOrInvalidateUsage(t *testing.T) {
	manager, scope := usageManager(t)
	lease, err := manager.Acquire(context.Background(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	scope.Operation, scope.Attempt = "auth-control", 1
	if _, err := manager.Store().Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Store().Journal(scope, "accepted"); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{sdk.MethodAuthStart, sdk.MethodAuthFinish, sdk.MethodAuthLogout} {
		ctx := context.WithValue(context.Background(), hostAuthorityKey{}, hostAuthority{
			manifest: lease.worker.entry.Manifest, method: method,
		})
		ctx = lease.group.withAuthority(ctx, sdk.PrimaryComponent, "read")
		if _, err := manager.hostCall(ctx, scope, sdk.MethodUsageInvalidate, json.RawMessage(`{}`)); err == nil {
			t.Fatal("auth control gained entitlement mutation authority:", method)
		}
		recall := context.WithValue(ctx, gatewayReadOnlyKey{}, true)
		if _, err := manager.hostCall(recall, scope, sdk.MethodStateCAS,
			json.RawMessage(`{"key":"continuation","value":1}`)); err == nil {
			t.Fatal("auth control bypassed the read-only recall marker:", method)
		}
		root := scope
		root.Provider, root.Account, root.Generation = "", "", 0
		if _, err := manager.hostCall(ctx, root, sdk.MethodStateCAS,
			json.RawMessage(`{"key":"module.config","value":{}}`)); err == nil {
			t.Fatal("auth continuation bypassed root module configuration ownership:", method)
		}
		stale := scope
		stale.Generation++
		if _, err := manager.hostCall(ctx, stale, sdk.MethodStateCAS,
			json.RawMessage(`{"key":"continuation","value":1}`)); err == nil {
			t.Fatal("auth continuation bypassed selected account generation:", method)
		}
		authority := ctx.Value(hostAuthorityKey{}).(hostAuthority)
		authority.manifest.Contract.Capabilities = []sdk.Capability{sdk.ModuleRPC}
		withoutAuth := context.WithValue(ctx, hostAuthorityKey{}, authority)
		if _, err := manager.hostCall(withoutAuth, scope, sdk.MethodStateCAS,
			json.RawMessage(`{"key":"continuation","value":1}`)); err == nil {
			t.Fatal("undeclared auth method gained private state mutation authority")
		}
	}
}
