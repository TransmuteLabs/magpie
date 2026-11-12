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
	invoke  func(string, string, any) (any, error)
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
func (host *agentActionHost) Invoke(_ context.Context, plugin string, method string, scope sdk.Scope, params, out any, _ func(extension.Event) error) error {
	host.method, host.scope = method, scope
	if host.invoke != nil {
		result, err := host.invoke(plugin, method, params)
		if err != nil {
			return err
		}
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, out)
	}
	request := params.(sdk.AgentPlanRequest)
	body, err := json.Marshal(sdk.AgentPlan{Files: []sdk.FileChange{{Path: "editor/config.json", Body: []byte(request.Action)}}})
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func TestExtensionAgentOwnershipSeparatesDottedIDs(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("USERPROFILE", home)
	test.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	test.Setenv("APPDATA", filepath.Join(home, "appdata"))
	first := sdk.AgentDescriptor{ID: "b.c", Name: "First", ConfigFiles: []string{"first.json"}}
	second := sdk.AgentDescriptor{ID: "c", Name: "Second", ConfigFiles: []string{"second.json"}}
	host := &agentActionHost{}
	for index, item := range []ExtensionAgent{{Plugin: "a", Agent: first}, {Plugin: "a.b", Agent: second}} {
		manifest := sdk.Manifest{Schema: 1, ID: item.Plugin, Version: "1.0.0", Runtime: "exec", Entry: "worker",
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
			Agents:   []sdk.AgentDescriptor{item.Agent}}
		if err := manifest.Validate(); err != nil {
			test.Fatalf("legitimate descriptor pair %d: %v", index, err)
		}
		host.entries = append(host.entries, extension.Installed{Manifest: manifest})
		if err := os.WriteFile(filepath.Join(home, item.Agent.ConfigFiles[0]), []byte("original-"+item.Plugin), 0o600); err != nil {
			test.Fatal(err)
		}
	}
	host.invoke = func(plugin, method string, params any) (any, error) {
		if method == sdk.MethodAgentInspect {
			request := params.(sdk.AgentInspectRequest)
			var options extensionAgentOptions
			if err := json.Unmarshal(request.Options, &options); err != nil {
				return nil, err
			}
			if plugin == "a.b" && len(options.Previous) != 0 && options.Previous[0].Path != second.ConfigFiles[0] {
				test.Error("inspection received another module's original settings")
			}
			return sdk.AgentInspectResult{Connected: true}, nil
		}
		request := params.(sdk.AgentPlanRequest)
		var settings extension.ActionSettings
		var options extensionAgentOptions
		if err := json.Unmarshal(request.Settings, &settings); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(request.Options, &options); err != nil {
			return nil, err
		}
		body := []byte("connected-" + plugin)
		if request.Action == "disconnect" {
			if len(options.Previous) != 1 {
				return nil, errors.New("missing original settings")
			}
			body = options.Previous[0].Body
		}
		return sdk.AgentPlan{Files: []sdk.FileChange{{Path: settings.Files[0].Path, Body: body}}}, nil
	}
	a := extensionAgent(home, host, ExtensionAgent{Plugin: "a", Agent: first})
	b := extensionAgent(home, host, ExtensionAgent{Plugin: "a.b", Agent: second})
	if err := a.Native.Connect(); err != nil {
		test.Fatal(err)
	}
	if b.Wired() {
		test.Fatal("connecting a/b.c adopted the independent a.b/c agent")
	}
	if err := b.Native.Connect(); err != nil {
		test.Fatal(err)
	}
	if err := b.Disconnect(); err != nil {
		test.Fatal(err)
	}
	if !a.Wired() {
		test.Fatal("disconnecting a.b/c removed a/b.c ownership")
	}
	if err := a.Disconnect(); err != nil {
		test.Fatal(err)
	}
	for _, item := range []ExtensionAgent{{Plugin: "a", Agent: first}, {Plugin: "a.b", Agent: second}} {
		body, err := os.ReadFile(filepath.Join(home, item.Agent.ConfigFiles[0]))
		if err != nil || string(body) != "original-"+item.Plugin {
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
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("USERPROFILE", home)
	test.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	test.Setenv("APPDATA", filepath.Join(home, "appdata"))
	descriptor := sdk.AgentDescriptor{ID: "editor", Name: "Editor", ConfigFiles: []string{"editor/config.json"}}
	host := &agentActionHost{entries: []extension.Installed{{Manifest: sdk.Manifest{ID: "external", Version: "1.0.0",
		Contract: sdk.FullContract(), Agents: []sdk.AgentDescriptor{descriptor}}}}}
	a := extensionAgent(home, host, ExtensionAgent{Plugin: "external", Agent: descriptor})
	if err := a.Native.Connect(); err != nil {
		test.Fatal(err)
	}
	before, err := a.Native.Disconnect()
	if err != nil {
		test.Fatal(err)
	}
	host.entries[0].Manifest.Version = "1.0.1"
	after, err := a.Native.Disconnect()
	if err != nil {
		test.Fatal(err)
	}
	// The visible file diff is identical, but another installed generation
	// must require a new confirmation.
	if before.Revision(a.ID) == after.Revision(a.ID) {
		test.Fatal("disconnect confirmation survived a module upgrade")
	}
	if err := a.Native.Execute(before); err == nil {
		test.Fatal("old native confirmation executed a different host plan")
	}
}
