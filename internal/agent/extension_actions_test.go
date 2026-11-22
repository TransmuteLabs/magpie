package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type agentActionHost struct {
	entries []extension.Installed
	err     error
	scope   sdk.Scope
	method  string
}

func (host *agentActionHost) List() ([]extension.Installed, error) { return host.entries, host.err }
func (host *agentActionHost) Store() *extension.Store              { return nil }
func (host *agentActionHost) Manifest(plugin string) (sdk.Manifest, error) {
	for _, entry := range host.entries {
		if entry.Manifest.ID == plugin && !entry.Disabled {
			return entry.Manifest, nil
		}
	}
	return sdk.Manifest{}, errors.New("not installed")
}
func (host *agentActionHost) Invoke(_ context.Context, _ string, method string, scope sdk.Scope, params, out any, _ func(extension.Event) error) error {
	host.method, host.scope = method, scope
	request := params.(sdk.AgentPlanRequest)
	body, err := json.Marshal(sdk.AgentPlan{Files: []sdk.FileChange{{Path: "editor/config.json", Body: []byte(request.Action)}}})
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func TestExtensionAgentOwnershipSeparatesDottedIDs(test *testing.T) {
	first := sdk.AgentDescriptor{ID: "b.c", Name: "First", ConfigFiles: []string{"first.json"}}
	second := sdk.AgentDescriptor{ID: "c", Name: "Second", ConfigFiles: []string{"second.json"}}
	host, source := prepareAgentWorkerFixture(test, test.Context(), first, "../extension/testdata/actionworker", 0)
	home, _ := os.UserHomeDir()
	manifestPath := filepath.Join(source, sdk.ManifestFile)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		test.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		test.Fatal(err)
	}
	items := []ExtensionAgent{{Plugin: "a", Agent: first}, {Plugin: "a.b", Agent: second}}
	for _, item := range items {
		manifest.ID, manifest.Agents = item.Plugin, []sdk.AgentDescriptor{item.Agent}
		raw, err := json.Marshal(manifest)
		if err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
			test.Fatal(err)
		}
		if _, err := host.Install(test.Context(), source); err != nil {
			test.Fatal(err)
		}
		body := []byte(`{"gateway":"original-` + item.Plugin + `"}`)
		if err := os.WriteFile(filepath.Join(home, item.Agent.ConfigFiles[0]), body, 0o600); err != nil {
			test.Fatal(err)
		}
	}
	firstAgent, err := Find(first.ID)
	if err != nil {
		test.Fatal(err)
	}
	secondAgent, err := Find(second.ID)
	if err != nil {
		test.Fatal(err)
	}
	if err := firstAgent.Native.Connect(); err != nil {
		test.Fatal(err)
	}
	if secondAgent.Wired() {
		test.Fatal("connecting a/b.c adopted the independent a.b/c agent")
	}
	if err := secondAgent.Native.Connect(); err != nil {
		test.Fatal(err)
	}
	if err := secondAgent.Disconnect(); err != nil {
		test.Fatal(err)
	}
	if !firstAgent.Wired() {
		test.Fatal("disconnecting a.b/c removed a/b.c ownership")
	}
	if err := firstAgent.Disconnect(); err != nil {
		test.Fatal(err)
	}
	for _, item := range items {
		body, err := os.ReadFile(filepath.Join(home, item.Agent.ConfigFiles[0]))
		var values map[string]string
		if err != nil || json.Unmarshal(body, &values) != nil || values["gateway"] != "original-"+item.Plugin || values["key"] != "" {
			test.Fatalf("%s restored another agent's preimage: %q %v", item.Plugin, body, err)
		}
	}
}

func TestExtensionAgentDiscoveryAndPlan(test *testing.T) {
	host := &agentActionHost{entries: []extension.Installed{
		{Manifest: sdk.Manifest{ID: "external", Version: "1.0.0", Contract: sdk.FullContract(), Agents: []sdk.AgentDescriptor{{ID: "editor", Name: "Editor"}}}},
		{Manifest: sdk.Manifest{ID: "off", Contract: sdk.FullContract(), Agents: []sdk.AgentDescriptor{{ID: "hidden", Name: "Hidden"}}}, Disabled: true},
	}}
	agents, err := ExtensionAgents(host)
	if err != nil || len(agents) != 1 || agents[0].Plugin != "external" || agents[0].Agent.ID != "editor" {
		test.Fatalf("discovery: %+v, %v", agents, err)
	}
	root := test.TempDir()
	policy := extension.ActionPolicy{Root: root, Paths: []string{"editor/config.json"}, BackupDir: filepath.Join(test.TempDir(), "backups")}
	scope := sdk.Scope{Plugin: "external", Session: "parent", Operation: "operation", Attempt: 3}
	plan, err := PlanExtension(context.Background(), host, "external", scope, sdk.AgentPlanRequest{ID: "editor", Action: "connect"}, policy)
	if err != nil {
		test.Fatal(err)
	}
	if host.scope != scope || host.method != sdk.MethodAgentPlan {
		test.Fatalf("wiring lost parent authority: %+v, %s", host.scope, host.method)
	}
	if _, err := os.Stat(filepath.Join(root, "editor")); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("planning wrote an agent config")
	}
	if result, err := plan.Execute(context.Background(), plan.Preview().Revision); err != nil || !result.Applied {
		test.Fatalf("host execution: %+v, %v", result, err)
	}
	body, err := os.ReadFile(filepath.Join(root, "editor", "config.json"))
	if err != nil || string(body) != "connect" {
		test.Fatalf("agent config: %q, %v", body, err)
	}
}

func TestExtensionAgentDiscoveryFailsClosed(test *testing.T) {
	host := &agentActionHost{err: errors.New("catalog unreadable")}
	if _, err := ExtensionAgents(host); err == nil {
		test.Fatal("unreadable catalog was treated as empty")
	}
	host.err = nil
	host.entries = []extension.Installed{
		{Manifest: sdk.Manifest{ID: "first", Contract: sdk.FullContract(), Agents: []sdk.AgentDescriptor{{ID: "shared"}}}},
		{Manifest: sdk.Manifest{ID: "second", Contract: sdk.FullContract(), Agents: []sdk.AgentDescriptor{{ID: "shared"}}}},
	}
	if _, err := ExtensionAgents(host); err == nil {
		test.Fatal("ambiguous agent ownership was accepted")
	}
}

func TestExtensionAgentConfirmationBindsManifest(test *testing.T) {
	descriptor := sdk.AgentDescriptor{ID: "fixture-editor", Name: "Editor", ConfigFiles: []string{"editor/config.json"}}
	host := installAgentActionFixture(test, descriptor)
	agent, err := Find(descriptor.ID)
	if err != nil {
		test.Fatal(err)
	}
	if err := agent.Native.Connect(); err != nil {
		test.Fatal(err)
	}
	before, err := agent.Native.Disconnect()
	if err != nil {
		test.Fatal(err)
	}
	replaceAgentFixture(test, host, descriptor, "1.0.1", 0)
	after, err := agent.Native.Disconnect()
	if err != nil {
		test.Fatal(err)
	}
	// The visible file diff is identical, but another installed generation
	// must require a new confirmation.
	if before.Revision(agent.ID) == after.Revision(agent.ID) {
		test.Fatal("disconnect confirmation survived a module upgrade")
	}
	if err := agent.Native.Execute(before); err == nil {
		test.Fatal("old native confirmation executed a different host plan")
	}
}
