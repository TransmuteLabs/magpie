package extension

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestAccountTaskBindingCannotBorrowAcceptedForeground(t *testing.T) {
	manifest := componentFixtureManifest()
	manifest.Contract.Version.Minor = 9
	manifest.Contract.Capabilities = []sdk.Capability{sdk.ModuleRPC, sdk.OutboundExecutor, sdk.AccountAuth, sdk.UsageReporter}
	manifest.Actions = append(manifest.Actions, sdk.ActionDescriptor{
		ID: "primary.write", Effect: "write", InputSchema: json.RawMessage(`{}`),
	})
	manifest.Bindings = append(manifest.Bindings, sdk.BindingDescriptor{
		ID: "primary.write", From: sdk.PrimaryComponent, Action: "primary.write",
	})
	for _, providerID := range []string{"vendor", "other"} {
		var provider sdk.ProviderDescriptor
		if err := json.Unmarshal([]byte(`{"id":"`+providerID+`","name":"Fixture","protocols":["chat"]}`), &provider); err != nil {
			t.Fatal(err)
		}
		for _, component := range []string{sdk.PrimaryComponent, "helper"} {
			action := providerID + "." + component + ".paid"
			manifest.Actions = append(manifest.Actions, sdk.ActionDescriptor{
				ID: action, Component: component, Effect: "write", InputSchema: json.RawMessage(`{}`),
			})
			manifest.Bindings = append(manifest.Bindings, sdk.BindingDescriptor{
				ID: action, From: sdk.PrimaryComponent, Action: action,
			})
			provider.AccountTasks = append(provider.AccountTasks, sdk.AccountTaskDescriptor{ID: component, Action: action})
			if component == "helper" {
				manifest.Services[1].Methods = append(manifest.Services[1].Methods, action)
			}
		}
		manifest.Providers = append(manifest.Providers, provider)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	fixture := simpleComponentFixture(t, manifest)
	if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
		t.Fatal(err)
	}
	store := fixture.group.manager.store
	account, err := store.SaveAccount(Account{
		Plugin: manifest.ID, Provider: "vendor", ID: "alice", Auth: json.RawMessage(`{"token":"synthetic"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{
		Plugin: account.Plugin, Provider: account.Provider, Account: account.ID,
		Generation: account.Generation, Operation: "ordinary-foreground", Attempt: 1,
	}
	if _, err := store.Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.Journal(scope, "accepted")
	if err != nil {
		t.Fatal(err)
	}
	parent := fixture.group.withAuthority(t.Context(), sdk.PrimaryComponent, "write")
	input := json.RawMessage(`{"task":"primary","key":"forged","job":"opaque-template","dueAt":1794656400000,"model":"m1"}`)
	for _, bindingID := range []string{"primary.write", "controller.write", "vendor.primary.paid", "vendor.helper.paid", "other.primary.paid", "other.helper.paid"} {
		binding, _ := manifest.ResolveBinding(bindingID)
		action, _ := manifest.ResolveAction(binding.Action)
		paid := bindingID != "primary.write" && bindingID != "controller.write"
		for _, nested := range []bool{false, true} {
			name := bindingID
			call := parent
			if nested {
				name += "/nested-task"
				call = context.WithValue(call, accountTaskExecutionKey{}, &accountTaskExecution{
					scope: scope, task: sdk.AccountTaskDescriptor{ID: "primary", Action: action.ID},
				})
			}
			t.Run(name, func(t *testing.T) {
				entered := false
				fixture.action = func(ctx context.Context, invocation componentInvocation, consume func(Event) error) (json.RawMessage, error) {
					if invocation.request.Action != action.ID || invocation.scope != scope {
						t.Fatal("binding reached another action or scope", invocation)
					}
					entered = true
					return json.RawMessage(`{"outcome":"completed"}`), nil
				}
				result, err := fixture.group.callBinding(call, sdk.PrimaryComponent, bindingID, scope, account.Auth, input)
				if !paid {
					if err != nil || !entered || string(result) != `{"outcome":"completed"}` {
						t.Fatal("ordinary declared write control was rejected", err, entered, string(result))
					}
					return
				}
				var outcome *OutcomeError
				if err == nil || entered || !errors.As(err, &outcome) || outcome.Outcome != "not-started" {
					t.Fatalf("declared paid task reached component binding without its own scheduled admission: entered=%v err=%v", entered, err)
				}
			})
		}
	}
	if persisted, err := store.Operation(scope.Plugin, scope.Operation); err != nil || persisted != accepted {
		t.Fatalf("component binding changed its foreground receipt: %+v %v", persisted, err)
	}
}
