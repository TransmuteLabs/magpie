package extension

import (
	"context"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

func executorFixtureLease(t *testing.T) *Lease {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return &Lease{
		manager: NewManager(t.TempDir()), ctx: ctx, cancel: cancel,
		worker: &managedWorker{entry: Installed{Manifest: sdk.Manifest{
			ID: "fixture.owner", Contract: sdk.Contract{
				Version:      sdk.APIVersion{Major: 1, Minor: 4},
				Capabilities: []sdk.Capability{sdk.InboundCodec, sdk.OutboundExecutor},
			},
		}}},
	}
}

func TestExecutorConstraintRequiresAnOwnedNegotiatedLiveLease(t *testing.T) {
	ctx := WithInboundLease(t.Context())
	if _, err := WithExecutorPolicy(ctx, codec.SameModuleExecutor); err == nil {
		t.Fatal("unbound native input granted executor authority")
	}
	if _, err := WithExecutorPolicy(ctx, "another-module"); err == nil {
		t.Fatal("arbitrary executor selector was accepted")
	}
	lease := executorFixtureLease(t)
	pinInboundLease(ctx, lease)
	pinned, err := WithExecutorPolicy(ctx, codec.SameModuleExecutor)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckExecutor(pinned, "fixture.owner", lease.manager.Namespace()); err != nil {
		t.Fatal(err)
	}
	for _, target := range [][2]string{{"", ""}, {"another.module", lease.manager.Namespace()}, {"fixture.owner", "another-host"}} {
		if err := CheckExecutor(pinned, target[0], target[1]); err == nil {
			t.Fatalf("native-only request escaped to %v", target)
		}
	}
	if err := CheckExecutor(t.Context(), "", ""); err != nil {
		t.Fatal("ordinary requests acquired a restriction", err)
	}
	inherited, err := WithExecutorPolicy(pinned, "")
	if err != nil || CheckExecutor(inherited, "", "") == nil {
		t.Fatal("an empty nested policy removed the original restriction", err)
	}
	nested := WithInboundLease(pinned)
	pinInboundLease(nested, executorFixtureLease(t))
	if _, err := WithExecutorPolicy(nested, codec.SameModuleExecutor); err == nil {
		t.Fatal("a nested request replaced the original executor lease")
	}
	if _, err := WithExecutorPolicy(nested, ""); err == nil {
		t.Error("an empty nested policy accepted another inbound lease")
	}
	if err := CheckExecutor(nested, "fixture.owner", lease.manager.Namespace()); err == nil {
		t.Error("executor admission ignored the active inbound lease")
	}
	lease.closed.Store(true)
	if err := CheckExecutor(pinned, "fixture.owner", lease.manager.Namespace()); err == nil {
		t.Fatal("closed native lease retained executor authority")
	}
}

func TestExecutorConstraintRejectsLegacyAndInboundOnlyModules(t *testing.T) {
	for _, scenario := range []string{"legacy", "inbound-only", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			lease := executorFixtureLease(t)
			switch scenario {
			case "legacy":
				lease.worker.entry.Manifest.Contract.Version.Minor = 3
			case "inbound-only":
				lease.worker.entry.Manifest.Contract.Capabilities = []sdk.Capability{sdk.InboundCodec}
			case "canceled":
				lease.cancel()
			}
			ctx := WithInboundLease(t.Context())
			pinInboundLease(ctx, lease)
			if _, err := WithExecutorPolicy(ctx, codec.SameModuleExecutor); err == nil {
				t.Fatal("unavailable executor authority was accepted")
			}
		})
	}
}
