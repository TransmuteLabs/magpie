package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionAgentNoopSyncCosmeticCatalogKeepsAuthority(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.Fields = []sdk.AgentField{{Key: "model", Label: "Model"}}
	descriptor.Launch.Model = "--model"
	host, _ := launchAgentFixture(t, descriptor)
	replaceAgentFixture(t, host, descriptor, "1.0.1", 9)
	if err := provider.Save(provider.Provider{ID: "noop", Name: "No-op fixture", Chat: "http://127.0.0.1:1/v1",
		Key: "fixture", Models: []string{"selected"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("noop", "http://127.0.0.1:1/v1", []catalog.Model{
		{ID: "selected", Context: 32768, Output: 4096, Efforts: []string{"low", "high"}, DefaultEffort: "low"},
	}); err != nil {
		t.Fatal(err)
	}
	owned, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owned.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	if err := owned.Apply("model", "magpie/noop/selected"); err != nil {
		t.Fatal(err)
	}
	lease, err := host.Acquire(t.Context(), "fixture.agent")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	admitted, err := protocolAgentBinding(t.Context(), lease, descriptor.ID)
	if err != nil || admitted.Model != "magpie/noop/selected" {
		t.Fatalf("fixture did not admit its selected model authority: %+v (%v)", admitted, err)
	}
	before, err := os.ReadFile(extensionBindingPath())
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(extensionBindingPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Renamed model", "Another display name"} {
		if err := provider.SetModelName("noop/selected", label); err != nil {
			t.Fatal(err)
		}
		if err := owned.Sync(); err != nil {
			t.Fatal(err)
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
			t.Fatal("cosmetic catalog synchronization republished durable ownership authority")
		}
		if err := admitted.Check(t.Context()); err != nil {
			t.Fatal("cosmetic catalog synchronization revoked an admitted native read", err)
		}
	}
	if err := provider.SetModelOutput("noop/selected", 8192); err != nil {
		t.Fatal(err)
	}
	if err := owned.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := admitted.Check(t.Context()); err == nil {
		t.Fatal("a real output capability change retained old native authority")
	}
	fresh, err := protocolAgentBinding(t.Context(), lease, descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.SetModelEfforts("noop/selected", []string{"high"}); err != nil {
		t.Fatal(err)
	}
	if err := owned.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Check(t.Context()); err == nil {
		t.Fatal("a real effort capability change retained old native authority")
	}
}

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
	replaceAgentFixture(t, host, descriptor, "1.0.1", 9)
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

func TestExtensionAgentNoopSyncCatalogAuthorityComparison(t *testing.T) {
	first := json.RawMessage(`[{"value":"magpie/p/m","ref":"p/m","label":"Old","note":"Old provider","icon":"old","icons":["old"],"group":"Old section","groupIcon":"old","context":32768,"maxOutput":4096,"efforts":["low","high"],"defaultEffort":"low","fast":true}]`)
	cosmetic := json.RawMessage(`[{"value":"magpie/p/m","ref":"p/m","label":"New","note":"New provider","icon":"new","icons":[],"group":"New section","groupIcon":"new","context":32768,"maxOutput":4096,"efforts":["low","high"],"defaultEffort":"low","fast":true}]`)
	if !sameExtensionModelAuthority(first, cosmetic) {
		t.Fatal("display-only catalog facts changed model authority")
	}
	if !sameExtensionModelAuthority(
		json.RawMessage(`[{"value":"a"},{"value":"b"}]`),
		json.RawMessage(`[{"value":"b"},{"value":"a"}]`)) {
		t.Fatal("display ordering changed unchanged model authority")
	}
	for _, change := range []struct {
		field string
		value json.RawMessage
	}{
		{"value", json.RawMessage(`"magpie/p/other"`)},
		{"ref", json.RawMessage(`"p/other"`)},
		{"context", json.RawMessage(`65536`)},
		{"maxOutput", json.RawMessage(`8192`)},
		{"efforts", json.RawMessage(`[]`)},
		{"efforts", json.RawMessage(`null`)},
		{"defaultEffort", json.RawMessage(`"high"`)},
		{"fast", json.RawMessage(`false`)},
		{"futureCapability", json.RawMessage(`true`)},
	} {
		t.Run(change.field+"/"+string(change.value), func(t *testing.T) {
			var models []map[string]json.RawMessage
			if err := json.Unmarshal(first, &models); err != nil {
				t.Fatal(err)
			}
			models[0][change.field] = change.value
			changed, err := json.Marshal(models)
			if err != nil {
				t.Fatal(err)
			}
			if sameExtensionModelAuthority(first, changed) {
				t.Fatal("semantic model capability change retained old authority", change.field)
			}
		})
	}
	for _, pair := range [][2]string{
		{`[{"value":"a","efforts":null}]`, `[{"value":"a","efforts":[]}]`},
		{`[{"value":"a"}]`, `[]`},
		{`[{"value":"a"}]`, `[{"value":"a"},{"value":"a"}]`},
		{`null`, `[]`},
		{`[`, `[]`},
	} {
		if sameExtensionModelAuthority(json.RawMessage(pair[0]), json.RawMessage(pair[1])) {
			t.Fatal("unknown or changed model authority became a cosmetic no-op", pair)
		}
	}
}
