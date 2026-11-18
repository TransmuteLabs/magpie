package profile

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func sdkProfileFields(t *testing.T, descriptors ...sdk.AgentDescriptor) []*agent.Agent {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata"))
	t.Setenv("SHELL", "/bin/bash")
	source := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	command := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-p=1", "-o", filepath.Join(source, entry), "../extension/testdata/actionworker")
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOPROXY=off", "GOSUMDB=off")
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build SDK field fixture: %v\n%s", err, raw)
	}
	// The fixture worker is addressed by absolute path. Ordinary clients on
	// the test machine must not become part of this profile's snapshot.
	t.Setenv("PATH", t.TempDir())
	for i := range descriptors {
		d := &descriptors[i]
		d.Name, d.Bin = d.ID, d.ID
		d.ConfigFiles = []string{"." + d.ID + ".json"}
		d.Launch = &sdk.AgentLaunch{}
	}
	manifest := sdk.Manifest{Schema: 1, ID: "profile.fields", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}}, Agents: descriptors}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	host := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := host.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := host.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	var agents []*agent.Agent
	for _, d := range descriptors {
		a, err := agent.Find(d.ID)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, a)
	}
	return agents
}

func TestSDKProfileRoundtripDottedField(t *testing.T) {
	a := sdkProfileFields(t, sdk.AgentDescriptor{ID: "profile-probe", Fields: []sdk.AgentField{{Key: "model.reasoning", Label: "Reasoning model"}}})[0]
	if err := a.Apply("model.reasoning", "magpie/first"); err != nil {
		t.Fatal(err)
	}
	if err := Save("dotted", Profile{Fields: Fields()}); err != nil {
		t.Fatal(err)
	}
	saved, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := saved["dotted"]
	if err := a.Apply("model.reasoning", "magpie/second"); err != nil {
		t.Fatal(err)
	}
	if InUse(p, []*agent.Agent{a}, nil) {
		t.Fatal("changed dotted field still marked in use")
	}
	groups := DetailsIn(p, []*agent.Agent{a})
	if len(groups) != 1 || groups[0].ID != a.ID || len(groups[0].Fields) != 1 || groups[0].Fields[0].Key != "model.reasoning" || groups[0].Fields[0].Value != "magpie/first" {
		t.Fatalf("dotted field details lost its owner: %+v", groups)
	}
	n, err := ApplyFields(p.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Field("model.reasoning").Get(); n != 1 || got != "magpie/first" {
		t.Fatalf("saved SDK field not restored: changed=%d value=%q", n, got)
	}
	if !InUse(p, []*agent.Agent{a}, nil) {
		t.Fatal("restored dotted field not in use")
	}
	// Before explicit keys existed, this single installed owner is still recoverable.
	if _, err := ApplyFields(map[string]string{a.ID + ".model.reasoning": "magpie/legacy"}); err != nil || a.Field("model.reasoning").Get() != "magpie/legacy" {
		t.Fatal("unambiguous legacy dotted field did not restore", err)
	}
}

func TestSDKProfileAmbiguousLegacyFieldRefusesWrites(t *testing.T) {
	agents := sdkProfileFields(t,
		sdk.AgentDescriptor{ID: "profile-probe", Fields: []sdk.AgentField{{Key: "model.reasoning", Label: "Reasoning model"}}},
		sdk.AgentDescriptor{ID: "profile-probe.model", Fields: []sdk.AgentField{{Key: "reasoning", Label: "Reasoning"}}})
	for i, a := range agents {
		if err := a.Apply(a.Fields[0].Key, []string{"magpie/first", "magpie/second"}[i]); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := Fields()
	if len(snapshot) != 2 {
		t.Fatalf("two distinct field addresses collided: %v", snapshot)
	}
	for _, a := range agents {
		if err := a.Apply(a.Fields[0].Key, "magpie/changed"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := ApplyFields(snapshot); err != nil || n != 2 {
		t.Fatalf("explicit snapshot failed to restore both fields: %d %v", n, err)
	}
	for i, a := range agents {
		if got := a.Fields[0].Get(); got != []string{"magpie/first", "magpie/second"}[i] {
			t.Fatalf("wrong owner restored: %s=%s", a.ID, got)
		}
	}
	legacy := Profile{Fields: map[string]string{"profile-probe.model.reasoning": "magpie/wrong"}}
	if n, err := ApplyFields(legacy.Fields); err == nil || !strings.Contains(err.Error(), "ambiguous") || n != 0 {
		t.Fatalf("ambiguous legacy field was not refused before writing: %d %v", n, err)
	}
	before := provider.ModelListOf(agents[0].ID)
	legacy.Models = map[string]provider.ModelList{agents[0].ID: {Only: true, Picked: []string{"magpie/hidden-by-failed-profile"}}}
	if applied, err := Apply(legacy); err == nil || !strings.Contains(err.Error(), "ambiguous") || applied.Changed != 0 {
		t.Fatalf("ambiguous profile changed models before resolving fields: %+v %v", applied, err)
	}
	if after := provider.ModelListOf(agents[0].ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("ambiguous profile changed the model list: before=%+v after=%+v", before, after)
	}
	legacy.Models = nil
	for i, a := range agents {
		if got := a.Fields[0].Get(); got != []string{"magpie/first", "magpie/second"}[i] {
			t.Fatalf("ambiguous address changed %s to %s", a.ID, got)
		}
	}
	if InUse(legacy, agents, nil) {
		t.Fatal("ambiguous legacy profile marked in use")
	}
	groups := DetailsIn(legacy, agents)
	if len(groups) != 1 || !strings.Contains(groups[0].Name, "ambiguous") || groups[0].ID == agents[0].ID || groups[0].ID == agents[1].ID {
		t.Fatalf("ambiguous details chose an owner: %+v", groups)
	}
}

func TestSDKProfileAbsentCandidateDoesNotResolveAmbiguousLegacyField(t *testing.T) {
	agents := sdkProfileFields(t,
		sdk.AgentDescriptor{ID: "profile-probe", Fields: []sdk.AgentField{{Key: "model.reasoning", Label: "Reasoning model"}}},
		sdk.AgentDescriptor{ID: "profile-probe.model", Fields: []sdk.AgentField{{Key: "reasoning", Label: "Reasoning"}}})
	if err := agents[0].Apply("model.reasoning", "magpie/first"); err != nil {
		t.Fatal(err)
	}
	if !agents[0].Detected() || agents[1].Detected() {
		t.Fatal("fixture must have one detected agent and another declared but absent")
	}
	p := Profile{Fields: map[string]string{"profile-probe.model.reasoning": "magpie/first"}}
	if n, err := ApplyFields(p.Fields); err == nil || !strings.Contains(err.Error(), "ambiguous") || n != 0 {
		t.Fatalf("absent candidate let legacy address choose an owner: %d %v", n, err)
	}
	if InUse(p, agents[:1], nil) {
		t.Fatal("absent candidate made ambiguous legacy address in use")
	}
	groups := DetailsIn(p, agents)
	if len(groups) != 1 || !strings.Contains(groups[0].Name, "ambiguous") {
		t.Fatalf("Details disagrees with apply/in-use: %+v", groups)
	}
}
