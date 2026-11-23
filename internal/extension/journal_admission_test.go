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

func TestManagerCloseWaitsForProviderReceipt(t *testing.T) {
	manager, scope := usageManager(t)
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	scope.Operation, scope.Attempt = "close-after-acceptance", 1
	request := sdk.ExecuteRequest{Protocol: "chat", Model: "model", Body: json.RawMessage(`{}`)}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	callCtx, cancelCall := context.WithCancel(ctx)
	accepted, finished := make(chan struct{}), make(chan struct{})
	var callErr error
	go func() {
		defer close(finished)
		_, callErr = lease.Call(callCtx, sdk.MethodProviderExecute, scope, request, func(event Event) error {
			if event.Name == "accepted" {
				close(accepted)
				<-callCtx.Done()
				return callCtx.Err()
			}
			return nil
		})
	}()
	defer func() {
		cancelCall()
		<-finished
	}()
	select {
	case <-accepted:
	case <-finished:
		t.Fatalf("provider call ended before acceptance: %v", callErr)
	case <-ctx.Done():
		t.Fatal("provider call did not reach acceptance")
	}

	closeDeadline := func() {
		t.Helper()
		wait, stop := context.WithTimeout(ctx, 250*time.Millisecond)
		defer stop()
		if err := manager.Close(wait); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("close with provider receipt still pending = %v, want deadline", err)
		}
	}
	closeDeadline()
	select {
	case <-lease.client.done:
	case <-ctx.Done():
		t.Fatal("close did not stop the worker while the provider receipt was pending")
	}
	closeDeadline()

	store := NewStore(manager.Namespace())
	if entry, err := store.OperationContext(ctx, scope.Plugin, scope.Operation); err != nil || entry.Scope != scope || entry.State != "accepted" {
		t.Fatalf("receipt before releasing the operation owner = %+v, %v", entry, err)
	}
	late := scope
	late.Operation = "preheld-lease-after-close"
	_, err = lease.Call(ctx, sdk.MethodProviderExecute, late, request, nil)
	var outcome *OutcomeError
	if !errors.As(err, &outcome) || outcome.Outcome != "not-started" {
		t.Errorf("pre-held lease called after close = %v, want not-started", err)
	}
	_, err = store.OperationContext(ctx, late.Plugin, late.Operation)
	storeTestError(t, err, "not_found")

	cancelCall()
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("close after releasing the operation owner: %v", err)
	}
	if entry, err := store.OperationContext(ctx, scope.Plugin, scope.Operation); err != nil || entry.Scope != scope || entry.State != "uncertain" {
		t.Fatalf("receipt after close = %+v, %v", entry, err)
	}
	<-finished
	outcome = nil
	if !errors.Is(callErr, context.Canceled) || !errors.As(callErr, &outcome) || outcome.Outcome != "uncertain" {
		t.Errorf("accepted owner returned %v, want uncertain cancellation", callErr)
	}
}
