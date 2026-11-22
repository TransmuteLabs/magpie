package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestAccountReplacementTerminalPublication(t *testing.T) {
	for _, mode := range []string{"success", "manual successor", "admission revoked", "prepared selection"} {
		t.Run(mode, func(t *testing.T) {
			manager, _ := fixtureManager(t)
			account, err := manager.Store().SaveAccount(Account{
				Plugin: "fixture", Provider: "provider", ID: "account", Auth: json.RawMessage(`{"token":"old"}`),
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			scope := sdk.Scope{
				Plugin: "fixture", Provider: "provider", Account: account.ID, Generation: account.Generation,
				Session: "replace-session", Operation: "replace-operation", Attempt: 1,
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			lease, err := manager.Acquire(ctx, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			var revoked atomic.Bool
			var publicationContext context.Context
			ctx = WithToolAccountCheck(ctx, manager.Namespace(), lease.Revision(), lease.ConfigVersion(), lease.worker.entry.EnableGeneration,
				func(current context.Context) error {
					if revoked.Load() {
						return errors.New("selected policy was revoked")
					}
					selected := AccountPublicationScope(current, manager.Namespace(), scope)
					if selected.Generation != scope.Generation {
						publicationContext = current
						if selected.Generation != account.Generation+1 {
							return errors.New("publication granted an unrelated successor")
						}
						for _, field := range []string{"plugin", "provider", "account", "generation", "session", "operation", "attempt"} {
							foreign := scope
							switch field {
							case "plugin":
								foreign.Plugin = "other"
							case "provider":
								foreign.Provider = "other"
							case "account":
								foreign.Account = "other"
							case "generation":
								foreign.Generation += 2
							case "session":
								foreign.Session = "other"
							case "operation":
								foreign.Operation = "other"
							case "attempt":
								foreign.Attempt++
							}
							if AccountPublicationScope(current, manager.Namespace(), foreign) != foreign {
								return errors.New("publication granted a different selection")
							}
						}
						if AccountPublicationScope(current, "other-namespace", scope) != scope {
							return errors.New("publication crossed its host namespace")
						}
						foreign := context.WithValue(current, hostAuthorityKey{}, hostAuthority{
							invocation: &invocationAuthority{scope: scope},
						})
						if AccountPublicationScope(foreign, manager.Namespace(), scope) != scope {
							return errors.New("publication crossed its invocation")
						}
						canceled, stop := context.WithCancel(current)
						stop()
						if AccountPublicationScope(canceled, manager.Namespace(), scope) != scope {
							return errors.New("publication survived cancellation")
						}
					}
					if mode == "prepared selection" {
						current, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
						if err != nil || current.Generation != selected.Generation {
							return errors.New("prepared account generation changed")
						}
					}
					return nil
				})
			var replacements atomic.Int32
			lease.client.hostCall = func(current context.Context, wire sdk.Scope, method string, raw json.RawMessage) (any, error) {
				value, err := manager.hostCall(current, wire, method, raw)
				if err != nil || method != sdk.MethodAccountReplace {
					return value, err
				}
				replacements.Add(1)
				if wire != scope {
					return nil, fmt.Errorf("replacement changed wire scope: %+v", wire)
				}
				if AccountPublicationScope(current, manager.Namespace(), wire) != wire {
					return nil, errors.New("replacement granted successor callback authority")
				}
				for _, callback := range []struct {
					method string
					params string
				}{
					{sdk.MethodStateCAS, `{"key":"after-replace","version":"0","value":true}`},
					{sdk.MethodNetworkProxy, `{"url":"https://203.0.113.40/endpoint"}`},
					{sdk.MethodAccountReplace, `{"credentials":{"token":"again"}}`},
				} {
					if _, err := manager.hostCall(current, wire, callback.method, json.RawMessage(callback.params)); err == nil {
						return nil, fmt.Errorf("replacement revived old callback authority: %s", callback.method)
					}
				}
				switch mode {
				case "manual successor":
					saved, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
					if err != nil {
						return nil, err
					}
					saved.Auth = json.RawMessage(`{"token":"manual"}`)
					if _, err := manager.Store().SaveAccount(saved, saved.Generation); err != nil {
						return nil, err
					}
				case "admission revoked":
					revoked.Store(true)
				}
				return value, nil
			}
			result, err := lease.Call(ctx, sdk.MethodToolCall, scope, sdk.ToolRequest{
				ID: "tool", Arguments: json.RawMessage(`{"action":"replace","value":{"token":"new"}}`),
			}, nil)
			wantState := "completed"
			if mode == "success" || mode == "prepared selection" {
				var replacement sdk.AccountReplaceResult
				if err != nil || json.Unmarshal(result, &replacement) != nil ||
					replacement.ID != account.ID || replacement.Generation != account.Generation+1 {
					t.Fatalf("accepted replacement lost its terminal result: %s, %v", result, err)
				}
				if publicationContext == nil {
					t.Fatal("result skipped the shared publication authority")
				}
				for _, retained := range []context.Context{publicationContext, context.WithoutCancel(publicationContext)} {
					if AccountPublicationScope(retained, manager.Namespace(), scope) != scope {
						t.Fatal("successor authority survived final publication")
					}
				}
			} else {
				wantState = "uncertain"
				var outcome *OutcomeError
				if len(result) != 0 || !errors.As(err, &outcome) || outcome.Outcome != wantState {
					t.Fatalf("revoked replacement published success: %s, %v", result, err)
				}
			}
			if replacements.Load() != 1 {
				t.Fatalf("replacement callback count: %d", replacements.Load())
			}
			record, err := manager.Store().Operation(scope.Plugin, scope.Operation)
			if err != nil || record.Scope != scope || record.State != wantState {
				t.Fatalf("terminal journal changed original authority: %+v, %v", record, err)
			}
			saved, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
			if err != nil {
				t.Fatal(err)
			}
			replay := scope
			replay.Generation = saved.Generation
			if _, err := lease.Call(ctx, sdk.MethodToolCall, replay, sdk.ToolRequest{
				ID: "tool", Arguments: json.RawMessage(`{"action":"replace","value":{"token":"replay"}}`),
			}, nil); err == nil {
				t.Fatal("successor replayed the original operation")
			}
			if replacements.Load() != 1 {
				t.Fatal("replay reached the replacement callback")
			}
			if _, err := lease.Call(ctx, sdk.MethodToolCall, scope, sdk.ToolRequest{
				ID: "tool", Arguments: json.RawMessage(`{"action":"auth"}`),
			}, nil); err == nil {
				t.Fatal("replacement revived the original generation")
			}
		})
	}
}

func TestAccountReplacementProofIsInvocationLocal(t *testing.T) {
	manager, _ := fixtureManager(t)
	account, err := manager.Store().SaveAccount(Account{
		Plugin: "fixture", Provider: "provider", ID: "account", Auth: json.RawMessage(`{"token":"old"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	lease, err := manager.Acquire(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	lease.client.hostCall = func(current context.Context, wire sdk.Scope, method string, raw json.RawMessage) (any, error) {
		value, err := manager.hostCall(current, wire, method, raw)
		if method == sdk.MethodNetworkProxy && err == nil {
			close(started)
			select {
			case <-release:
			case <-current.Done():
				return nil, current.Err()
			}
		}
		return value, err
	}
	type response struct {
		result json.RawMessage
		err    error
	}
	done := make(chan response, 1)
	scope := sdk.Scope{
		Plugin: "fixture", Provider: "provider", Account: account.ID, Generation: account.Generation,
		Operation: "overlapping-operation", Attempt: 1,
	}
	go func() {
		result, err := lease.Call(ctx, sdk.MethodToolCall, scope, sdk.ToolRequest{
			ID: "tool", Arguments: json.RawMessage(`{"action":"proxy","key":"https://203.0.113.40/endpoint"}`),
		}, nil)
		done <- response{result, err}
	}()
	joined := false
	defer func() {
		unblock()
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("overlapping invocation did not join")
			}
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("overlapping invocation did not reach its callback")
	}
	replacement := scope
	replacement.Operation = "replacement-operation"
	if _, err := lease.Call(ctx, sdk.MethodToolCall, replacement, sdk.ToolRequest{
		ID: "tool", Arguments: json.RawMessage(`{"action":"replace","value":{"token":"new"}}`),
	}, nil); err != nil {
		t.Fatalf("replacement lost its own terminal result: %v", err)
	}
	unblock()
	select {
	case got := <-done:
		joined = true
		var outcome *OutcomeError
		if len(got.result) != 0 || !errors.As(got.err, &outcome) || outcome.Outcome != "uncertain" {
			t.Fatalf("another invocation borrowed replacement authority: %s, %v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("overlapping invocation did not return")
	}
	record, err := manager.Store().Operation(scope.Plugin, scope.Operation)
	if err != nil || record.Scope != scope || record.State != "uncertain" {
		t.Fatalf("overlapping invocation lost its original fence: %+v, %v", record, err)
	}
}
