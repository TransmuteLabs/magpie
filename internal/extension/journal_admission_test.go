package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestLeaseDispatchWaitRespectsDeadline(t *testing.T) {
	manager, scope := usageManager(t)
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	lock, err := os.OpenFile(filepath.Join(manager.Namespace(), "private", ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if held, err := filelock.TryShared(lock); err != nil || !held {
		t.Fatalf("fixture could not hold the shared Store lock: %v", err)
	}

	scope.Operation, scope.Attempt = "deadline-before-journal-dispatch", 1
	request := sdk.ExecuteRequest{Protocol: "chat", Model: "model", Body: json.RawMessage(`{}`)}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := lease.Call(ctx, sdk.MethodProviderExecute, scope, request, nil)
		done <- err
	}()
	select {
	case err = <-done:
		filelock.Unlock(lock)
	case <-time.After(time.Second):
		t.Error("dispatch claim continued waiting on the Store after the deadline")
		filelock.Unlock(lock)
		err = <-done
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("dispatch claim error = %v, want deadline", err)
	}

	store := NewStore(manager.Namespace())
	entry, err := store.Operation(scope.Plugin, scope.Operation)
	var rpcErr *sdk.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != "not_found" {
		t.Errorf("canceled dispatch claim left a receipt: %+v, %v", entry, err)
	}

	retry, finish := context.WithTimeout(t.Context(), 5*time.Second)
	defer finish()
	accepted := false
	if _, err := lease.Call(retry, sdk.MethodProviderExecute, scope, request, func(event Event) error {
		if event.Name == "accepted" {
			entry, err := store.OperationContext(retry, scope.Plugin, scope.Operation)
			if err != nil || entry.Scope != scope || entry.State != "accepted" {
				return fmt.Errorf("receipt at acceptance = %+v, %v", entry, err)
			}
			accepted = true
		}
		return nil
	}); err != nil {
		t.Fatal("live dispatch of the same intent:", err)
	}
	if !accepted {
		t.Fatal("live dispatch did not deliver acceptance")
	}
	if entry, err := store.Operation(scope.Plugin, scope.Operation); err != nil || entry.Scope != scope || entry.State != "completed" {
		t.Fatalf("terminal receipt = %+v, %v", entry, err)
	}
}

func TestLeaseAcceptedDispatchRecordsCancellation(t *testing.T) {
	manager, scope := usageManager(t)
	scope.Operation, scope.Attempt = "canceled-after-acceptance", 1
	request := sdk.ExecuteRequest{Protocol: "chat", Model: "model", Body: json.RawMessage(`{}`)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := manager.Call(ctx, scope.Plugin, sdk.MethodProviderExecute, scope, request, func(event Event) error {
		if event.Name == "accepted" {
			cancel()
		}
		return nil
	})
	var outcome *OutcomeError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &outcome) || outcome.Outcome != "uncertain" {
		t.Fatalf("cancel after acceptance returned %v, want uncertain cancellation", err)
	}
	store := NewStore(manager.Namespace())
	if entry, err := store.Operation(scope.Plugin, scope.Operation); err != nil || entry.Scope != scope || entry.State != "uncertain" {
		t.Fatalf("terminal receipt = %+v, %v", entry, err)
	}
	_, err = manager.Call(t.Context(), scope.Plugin, sdk.MethodProviderExecute, scope, request, nil)
	storeTestError(t, err, "operation_conflict")
}
