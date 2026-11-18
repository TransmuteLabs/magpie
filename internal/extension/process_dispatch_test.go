package extension

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestOwnedProcessOrdinaryDispatchKeepsHostAuthorityAndFiniteParent(t *testing.T) {
	manifest := sdk.Manifest{
		ID: "fixture",
		Contract: sdk.Contract{
			Version:      sdk.APIVersion{Major: 1, Minor: 10},
			Capabilities: []sdk.Capability{sdk.AgentProcess, sdk.ModuleRPC},
		},
		Processes: []sdk.AgentProcessRecipe{{ID: "run", Agent: "agent", Argv: []string{"app-server", "--stdio"}}},
		Actions:   []sdk.ActionDescriptor{{ID: "run", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	lease := &Lease{worker: &managedWorker{entry: Installed{Manifest: manifest}}}
	scope := sdk.Scope{Plugin: "fixture", Operation: "foreground", Attempt: 1}
	action := sdk.ActionDescriptor{ID: "run", Effect: "write"}
	sentinel := errors.New("host resolver barrier")
	calls := 0
	processAgentResolver.Lock()
	previous := processAgentResolver.resolve
	processAgentResolver.resolve = func(ctx context.Context, actual *Lease, recipe sdk.AgentProcessRecipe) (OwnedProcessLaunch, error) {
		deadline, finite := ctx.Deadline()
		if actual != lease || !finite || time.Until(deadline) > 30*time.Minute ||
			recipe.ID != "run" || recipe.Agent != "agent" || !slices.Equal(recipe.Argv, manifest.Processes[0].Argv) {
			t.Error("ordinary dispatch changed host parent, finite lifetime or literal declaration")
		}
		calls++
		recipe.Argv[0] = "changed-private-copy"
		return OwnedProcessLaunch{}, sentinel
	}
	processAgentResolver.Unlock()
	defer func() {
		processAgentResolver.Lock()
		processAgentResolver.resolve = previous
		processAgentResolver.Unlock()
	}()
	ctx := context.WithoutCancel(t.Context())
	selected, _, err := lease.callDeclaredProcess(ctx, scope, action, nil, nil)
	if !selected || !errors.Is(err, sentinel) || calls != 1 || manifest.Processes[0].Argv[0] != "app-server" {
		t.Fatal("ordinary foreground action lost its checked host resolver", selected, calls, err)
	}
	for _, test := range []struct {
		name   string
		ctx    context.Context
		scope  sdk.Scope
		action sdk.ActionDescriptor
	}{
		{name: "read-parent", ctx: context.WithValue(ctx, gatewayReadOnlyKey{}, true), scope: scope, action: action},
		{name: "component-parent", ctx: context.WithValue(ctx, componentAuthorityKey{}, componentAuthority{}), scope: scope, action: action},
		{name: "callback-parent", ctx: context.WithValue(ctx, hostAuthorityKey{}, hostAuthority{}), scope: scope, action: action},
		{name: "ledger-parent", ctx: withOwnCallParent(ctx, sdk.MethodModuleCall), scope: scope, action: action},
		{name: "scheduled-parent", ctx: context.WithValue(ctx, accountTaskParentKey{}, &accountTaskParent{}), scope: scope, action: action},
		{name: "scheduled-execution", ctx: context.WithValue(ctx, accountTaskExecutionKey{}, &accountTaskExecution{}), scope: scope, action: action},
		{name: "no-intent", ctx: ctx, scope: sdk.Scope{Plugin: "fixture"}, action: action},
		{name: "read-action", ctx: ctx, scope: scope, action: sdk.ActionDescriptor{ID: "run", Effect: "read"}},
		{name: "service-action", ctx: ctx, scope: scope, action: sdk.ActionDescriptor{ID: "run", Effect: "write", Component: "worker"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := calls
			selected, _, err := lease.callDeclaredProcess(test.ctx, test.scope, test.action, nil, nil)
			var outcome *OutcomeError
			if !selected || !errors.As(err, &outcome) || outcome.Outcome != "not-started" || calls != before {
				t.Fatal("non-foreground authority reached process resolution", selected, calls, err)
			}
		})
	}
	for _, provider := range []sdk.ProviderDescriptor{
		{ID: "checkin", Checkin: &sdk.AccountCheckinDescriptor{Action: action.ID}},
		{ID: "scheduled", AccountTasks: []sdk.AccountTaskDescriptor{{ID: "paid", Action: action.ID}}},
	} {
		t.Run(provider.ID, func(t *testing.T) {
			before := calls
			lease.worker.entry.Manifest.Providers = []sdk.ProviderDescriptor{provider}
			defer func() { lease.worker.entry.Manifest.Providers = nil }()
			selected, _, err := lease.callDeclaredProcess(ctx, scope, action, nil, nil)
			var outcome *OutcomeError
			if !selected || !errors.As(err, &outcome) || outcome.Outcome != "not-started" || calls != before {
				t.Fatal("background action declaration borrowed foreground process resolution", selected, calls, err)
			}
		})
	}
	before := calls
	selected, _, err = lease.callDeclaredProcess(ctx, scope, sdk.ActionDescriptor{ID: "other", Effect: "write"}, nil, nil)
	if selected || err != nil || calls != before {
		t.Fatal("ordinary neighboring action acquired process policy", selected, calls, err)
	}
}
