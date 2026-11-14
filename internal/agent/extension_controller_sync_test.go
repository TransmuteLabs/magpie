package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionAgentNoopSyncKeepsOwnedBindingAndAdmittedRead(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.Fields = []sdk.AgentField{{Key: "model", Label: "Model"}}
	descriptor.Launch.Model = "--model"
	host, _ := launchAgentFixture(t, descriptor)
	owned, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owned.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	lease, err := host.Acquire(t.Context(), "fixture.agent")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	admitted, err := protocolAgentBinding(t.Context(), lease, descriptor.ID)
	if err != nil || admitted.Model == "" {
		t.Fatal("fixture did not admit an owned native read", err)
	}
	before, err := os.ReadFile(extensionBindingPath())
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(extensionBindingPath())
	if err != nil {
		t.Fatal(err)
	}
	for repeat := 0; repeat < 3; repeat++ {
		if err := owned.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(extensionBindingPath())
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(extensionBindingPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("no-op catalog synchronization republished the durable ownership binding")
	}
	if err := admitted.Check(t.Context()); err != nil {
		t.Fatal("no-op catalog synchronization revoked an admitted native read", err)
	}
	if err := owned.Field("model").Set("magpie/changed-model"); err != nil {
		t.Fatal(err)
	}
	if err := admitted.Check(t.Context()); err == nil {
		t.Fatal("a real model change retained the old native account authority")
	}
}

func TestExtensionAgentNoopSyncStillConsumesCheckedPlan(t *testing.T) {
	descriptor := launchAgentDescriptor()
	host, home := launchAgentFixture(t, descriptor)
	owned, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owned.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	controller := &extensionController{
		home: home, host: host,
		item: ExtensionAgent{Plugin: "fixture.agent", Agent: descriptor}, bin: descriptor.Bin,
	}
	before, err := os.ReadFile(extensionBindingPath())
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := controller.prepare(t.Context(), "sync", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := controller.apply(ctx, cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled no-op synchronization was admitted", err)
	}
	plan, err := controller.prepare(t.Context(), "sync", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.action.Preview().Files) != 0 {
		t.Fatal("fixture unexpectedly planned file effects")
	}
	if err := controller.apply(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(extensionBindingPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("checked no-op synchronization changed the durable binding", err)
	}
	if err := controller.apply(t.Context(), plan); !errors.Is(err, extension.ErrActionChanged) {
		t.Fatal("no-op synchronization reused a consumed action plan", err)
	}
}
