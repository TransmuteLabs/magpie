package extension

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

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
	manifest := sdk.Manifest{Contract: sdk.FullContract()}
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
		_ = manager.finishUsageRead(manifest, scope, &stamp, json.RawMessage(raw))
		if err := manager.Store().UsageReady(scope); err == nil {
			t.Fatalf("nonfresh read reopened inference: %s", raw)
		}
	}
	if _, err := manager.Store().InvalidateUsage(scope); err != nil {
		t.Fatal(err)
	}
	if err := manager.finishUsageRead(manifest, scope, &stamp,
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
	if _, err := store.journal(scope, "dispatched", true); err == nil {
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
			if _, err := store.journal(scope, "dispatched", true); err == nil {
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
