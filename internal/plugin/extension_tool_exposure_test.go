package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionToolExposureListAndCall(t *testing.T) {
	fake := extensionToolsFixture(t)
	fake.entries[0].Manifest.Contract.Version = sdk.APIVersion{Major: 1, Minor: 2}
	fake.entries[0].Manifest.Tools = []sdk.ToolDescriptor{
		{Name: "legacy", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "public", Exposure: "shared", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "private", Exposure: "local-owner", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	for _, test := range []struct {
		name  string
		ctx   context.Context
		owner bool
	}{
		{"unknown", t.Context(), false},
		{"owner", extension.WithLocalOwner(t.Context(), true), true},
		{"revoked", extension.WithLocalOwner(extension.WithLocalOwner(t.Context(), true), false), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			groups, err := Tools(test.ctx)
			if err != nil || len(groups) != 1 {
				t.Fatalf("contextual SDK list: %+v, %v", groups, err)
			}
			var names []string
			for _, tool := range groups[0].Tools {
				names = append(names, tool.Name)
			}
			want := "legacy,public"
			if test.owner {
				want += ",private"
			}
			if strings.Join(names, ",") != want {
				t.Fatalf("tools=%v, want %s", names, want)
			}
			for _, name := range []string{"legacy", "public", "private"} {
				before := fake.calls
				_, err := RunExtensionTool(test.ctx, "extension:tool.module", name,
					json.RawMessage(`{"origin":"local","localOwner":true,"_meta":{"origin":"local"}}`))
				allowed := name != "private" || test.owner
				if (err == nil) != allowed || (fake.calls == before+1) != allowed {
					t.Fatalf("%s tool=%s err=%v calls=%d->%d", test.name, name, err, before, fake.calls)
				}
			}
		})
	}
	if names := ToolNames()["extension:tool.module"]; len(names) != 3 {
		t.Fatalf("owner configuration inventory lost descriptors: %v", names)
	}
	if count, err := ToolsOn(); err != nil || count != 1 {
		t.Fatalf("owner-only inventory stopped the tool point: %d, %v", count, err)
	}
}

func TestExtensionToolExposureRechecksCurrentManifest(t *testing.T) {
	fake := extensionToolsFixture(t)
	ctx := t.Context()
	groups, err := Tools(ctx)
	if err != nil || len(groups) != 1 || len(groups[0].Tools) == 0 {
		t.Fatalf("shared tool was not listed: %+v, %v", groups, err)
	}
	// A caller may retain a previously shared name across a manifest update.
	fake.entries[0].Manifest.Tools[0].Exposure = "local-owner"
	if _, err := RunExtensionTool(ctx, groups[0].Spec, "echo", json.RawMessage(`{}`)); err == nil || fake.calls != 0 {
		t.Fatalf("stale shared name bypassed the new policy: %v, calls=%d", err, fake.calls)
	}
	if _, err := RunExtensionTool(extension.WithLocalOwner(ctx, true), groups[0].Spec, "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	fake.entries[0].Manifest.Tools[0].Exposure = "unknown"
	if _, err := RunExtensionTool(extension.WithLocalOwner(ctx, true), groups[0].Spec, "echo", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown exposure acquired owner authority")
	}
	fake.entries[0].Manifest.Tools[0].Exposure = ""
	if _, err := RunExtensionTool(ctx, groups[0].Spec, "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatal("default shared legacy behavior changed:", err)
	}
}
