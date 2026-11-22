package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func installAgentActionFixture(t *testing.T, descriptor sdk.AgentDescriptor) *extension.Manager {
	t.Helper()
	minor := 0
	if descriptor.Aliases != nil || descriptor.UA != nil || descriptor.Launch != nil || descriptor.Profile != nil || descriptor.Library != nil {
		minor = 5
	}
	return installAgentWorkerFixture(t, descriptor, "../extension/testdata/actionworker", minor)
}

func installAgentWorkerFixture(t *testing.T, descriptor sdk.AgentDescriptor, worker string, minor int) *extension.Manager {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	host, source := prepareAgentWorkerFixture(t, ctx, descriptor, worker, minor)
	if _, err := host.Install(ctx, source); err != nil {
		t.Fatal(err)
	}
	return host
}

func prepareAgentWorkerFixture(t *testing.T, ctx context.Context, descriptor sdk.AgentDescriptor, worker string, minor int) (*extension.Manager, string) {
	t.Helper()
	home := t.TempDir()
	for _, env := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(env, home)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata"))
	source := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-p=1", "-o",
		filepath.Join(source, entry), worker)
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build action fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: 1, ID: "fixture.agent", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: minor}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
		Agents:   []sdk.AgentDescriptor{descriptor}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
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
	return host, source
}

func TestExtensionAgentMetadataOrdinaryDiscovery(t *testing.T) {
	installAgentActionFixture(t, sdk.AgentDescriptor{
		ID: "fixture-metadata", Name: "Metadata fixture", Bin: "fixture-editor",
		ConfigFiles: []string{".fixture/config.json"}, Aliases: []string{"fixture-alias"},
		UA: []string{"Fixture-Agent/"}, Fields: []sdk.AgentField{{Key: "model", Label: "Model"}},
		Library: &sdk.AgentLibrary{Instructions: ".fixture/AGENTS.md", MCP: ".fixture/mcp.json", MCPFormat: "claude", Skills: ".fixture/skills"},
	})
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, ".fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".fixture/config.json"), []byte(`{"model":"own"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Find("fixture-alias")
	if err != nil || a.ID != "fixture-metadata" || len(a.UA) != 1 || a.UA[0] != "fixture-agent/" {
		t.Fatalf("ordinary identity metadata was lost: %+v %v", a, err)
	}
	target, places, sdkOwned := a.SDKLibraryTarget()
	mcp := filepath.Join(home, ".fixture", "mcp.json")
	if !sdkOwned || target != a || places == nil || places.MCP != mcp {
		t.Fatal("ordinary agent did not expose typed Library metadata")
	}
	places.MCP = "changed"
	_, fresh, _ := a.SDKLibraryTarget()
	if fresh.MCP != mcp {
		t.Fatal("Library caller mutated the agent's installed declaration")
	}
}

func TestExtensionAgentMetadataCannotStealBuiltinAlias(t *testing.T) {
	installAgentActionFixture(t, sdk.AgentDescriptor{
		ID: "fixture-collision", Name: "Metadata fixture", ConfigFiles: []string{".fixture.json"}, Aliases: []string{"claude"},
	})
	if _, err := Find("fixture-collision"); err == nil {
		t.Fatal("SDK alias replaced a built-in identity")
	}
	if a, err := Find("claude"); err != nil || a.ID != "claude" || a.extensionMetadata != nil {
		t.Fatal("built-in lookup changed without explicit profile adoption")
	}
}

func TestExtensionAgentOrdinaryLifecycle(t *testing.T) {
	host := installAgentActionFixture(t, sdk.AgentDescriptor{ID: "fixture-editor", Name: "Fixture editor", Bin: "fixture-editor",
		ConfigFiles: []string{".fixture/config.json"}, Fields: []sdk.AgentField{{Key: "model", Label: "model"}}})
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".fixture", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"model":"own","note":"keep"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	all := All()
	a, err := FindIn(all, "fixture-editor")
	if err != nil || a.Native == nil || a.Field("model") == nil || !a.Detected() {
		t.Fatalf("installed descriptor absent from ordinary All: %+v %v", a, err)
	}
	if _, err := Find("fixture-editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := FindIn(local(), "fixture-editor"); err != nil {
		t.Fatal(err)
	}
	if a.Wired() {
		t.Fatal("unconnected fixture is wired")
	}
	if _, err := a.ConnectHow(); err != nil || !a.Wired() {
		t.Fatalf("ordinary connect: %v", err)
	}
	if err := a.Apply("model", "magpie/test-model"); err != nil || a.Values()["model"] != "magpie/test-model" {
		t.Fatalf("ordinary Apply/Values: %v", err)
	}
	// A new Agent object must retain ownership and the original preimage.
	a, err = Find("fixture-editor")
	if err != nil || !a.Wired() {
		t.Fatalf("ordinary rediscovery lost connection: %v", err)
	}
	before, _ := os.ReadFile(path)
	preview, err := a.Native.Disconnect()
	if err != nil || len(preview.Files) != 1 {
		t.Fatalf("ordinary native disconnect preview: %+v %v", preview, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("disconnect preview wrote config")
	}
	// Execution rejects edits arriving after preview.
	var values map[string]string
	if err := json.Unmarshal(before, &values); err != nil {
		t.Fatal(err)
	}
	values["note"] = "edited by user"
	edited, _ := json.Marshal(values)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Native.Execute(preview); err == nil {
		t.Fatal("stale disconnect preview overwrote a user edit")
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	values = nil
	if err := json.Unmarshal(body, &values); err != nil {
		t.Fatal(err)
	}
	if values["model"] != "own" || values["note"] != "edited by user" || values["gateway"] != "" || a.Wired() || !a.Disconnected() {
		t.Fatalf("ordinary disconnect did not restore owned keys: %s", body)
	}
	if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Find("fixture-editor"); err == nil {
		t.Fatal("disabled extension still registered")
	}
}

func TestExtensionAgentOrdinaryPreservesManualWiring(t *testing.T) {
	installAgentActionFixture(t, sdk.AgentDescriptor{ID: "fixture-editor", Name: "Fixture editor",
		ConfigFiles: []string{".fixture.json"}, Fields: []sdk.AgentField{{Key: "model", Label: "model"}}})
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".fixture.json")
	original := []byte(`{"gateway":"manual","model":"magpie/manual","note":"keep"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Find("fixture-editor")
	if err != nil {
		t.Fatal(err)
	}
	if a.Wired() {
		t.Fatal("manual wiring was adopted without a user action")
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if !bytes.Equal(body, original) {
		t.Fatalf("manual wiring changed: %s", body)
	}
}

func TestExtensionAgentOrdinaryDisconnectWithoutStashPublication(t *testing.T) {
	descriptor := sdk.AgentDescriptor{
		ID: "fixture-editor", Name: "Fixture editor", ConfigFiles: []string{".fixture.json"},
		Fields: []sdk.AgentField{{Key: "model", Label: "Model"}},
	}
	installAgentActionFixture(t, descriptor)
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".fixture.json")
	if err := os.WriteFile(path, []byte(`{"model":"own","note":"original"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	agent, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	var wired map[string]string
	if err != nil || json.Unmarshal(raw, &wired) != nil || wired["gateway"] == "" {
		t.Fatalf("connect did not publish gateway settings: %s (%v)", raw, err)
	}
	forget("sdk.agent.fixture.agent:" + descriptor.ID)
	wired["note"] = "user edit after connect"
	edited, err := json.Marshal(wired)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edited, 0o640); err != nil {
		t.Fatal(err)
	}
	agent, err = Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Disconnect(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	var restored map[string]string
	if err != nil || json.Unmarshal(raw, &restored) != nil || restored["model"] != "own" ||
		restored["gateway"] != "" || restored["key"] != "" || restored["note"] != wired["note"] {
		t.Fatalf("rediscovered disconnect did not restore its owned settings: %s (%v)", raw, err)
	}
}

func TestExtensionAgentOrdinaryRecoversInterruptedConfigTransition(t *testing.T) {
	const fixtureIntent = "MAGPIE_TEST_AGENT_CONFIG_TRANSITION"
	const fixtureHome = "MAGPIE_TEST_AGENT_CONFIG_TRANSITION_HOME"
	const interrupted = 86
	descriptor := sdk.AgentDescriptor{
		ID: "fixture-editor", Name: "Fixture editor", ConfigFiles: []string{".fixture.json"},
		Fields: []sdk.AgentField{{Key: "model", Label: "Model"}},
	}
	if intent := os.Getenv(fixtureIntent); intent != "" {
		home := os.Getenv(fixtureHome)
		if home == "" || !filepath.IsAbs(home) {
			t.Fatal("interruption fixture has no isolated home")
		}
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		t.Setenv("APPDATA", filepath.Join(home, "appdata"))
		host := extension.Default()
		controller := newExtensionController(home, host, ExtensionAgent{Plugin: "fixture.agent", Agent: descriptor}, nil)
		plan, err := controller.prepare(t.Context(), intent, "model", "magpie/fixture")
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := acquireAgentRuntime(t.Context(), host, "fixture.agent", descriptor)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.lease.Close()
		opts, err := json.Marshal(extensionAgentOptions{Gateway: "http://127.0.0.1:3425", Key: "magpie-fixture-editor",
			Previous: plan.before.Before, Field: "model", Value: "magpie/fixture"})
		if err != nil {
			t.Fatal(err)
		}
		settings := filepath.Join(home, descriptor.ConfigFiles[0])
		initial, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		policy := controller.policy(plan.before)
		policy.RevalidateInputs = func(_ context.Context, _ map[string]fs.FileInfo) error {
			current, err := os.ReadFile(settings)
			if err == nil && !bytes.Equal(current, initial) {
				os.Exit(interrupted)
			}
			return err
		}
		plan.action, err = extension.NewActions(runtime).AgentPlan(t.Context(), "fixture.agent", sdk.Scope{Operation: "agent." + intent},
			sdk.AgentPlanRequest{ID: descriptor.ID, Action: intent, Options: opts}, policy)
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("controller returned instead of stopping after publication: %v", controller.apply(t.Context(), plan))
	}

	for _, intent := range []string{"connect", "set", "disconnect"} {
		t.Run(intent, func(t *testing.T) {
			host := installAgentActionFixture(t, descriptor)
			home, _ := os.UserHomeDir()
			settings := filepath.Join(home, descriptor.ConfigFiles[0])
			before := []byte(`{"model":"own","note":"original"}`)
			if err := os.WriteFile(settings, before, 0o640); err != nil {
				t.Fatal(err)
			}
			readSettings := func() []byte {
				t.Helper()
				raw, err := os.ReadFile(settings)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			note := "original"
			if intent != "connect" {
				agent, err := Find(descriptor.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := agent.ConnectHow(); err != nil {
					t.Fatal(err)
				}
				var values map[string]string
				if err := json.Unmarshal(readSettings(), &values); err != nil {
					t.Fatal(err)
				}
				note = "user edit before transition"
				values["note"] = note
				edited, err := json.Marshal(values)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(settings, edited, 0o640); err != nil {
					t.Fatal(err)
				}
			}
			program, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, program, "-test.run=^TestExtensionAgentOrdinaryRecoversInterruptedConfigTransition$")
			child.Env = append(os.Environ(), fixtureIntent+"="+intent, fixtureHome+"="+home)
			child.WaitDelay = 2 * time.Second
			output, err := child.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != interrupted {
				t.Fatalf("transition did not stop after publication: %v\n%s", err, output)
			}
			published := readSettings()
			ledger, err := extensionBindingRead()
			record := ledger.Records[extensionBindingKey(home, "fixture.agent", descriptor.ID)]
			if err != nil || record.Phase != "pending" || len(record.Before) != 1 || !bytes.Equal(record.Before[0].Body, before) ||
				len(record.Applied) != 1 || !bytes.Equal(record.Applied[0].Body, published) {
				t.Fatalf("interrupted %s lost its original or published snapshots: %+v %v", intent, record, err)
			}
			controller := newExtensionController(home, host, ExtensionAgent{Plugin: "fixture.agent", Agent: descriptor}, nil)
			if err := controller.recover(t.Context(), record.Revision-1); !errors.Is(err, extension.ErrActionChanged) ||
				!bytes.Equal(readSettings(), published) {
				t.Fatalf("stale recovery attempted to restore a different transition: %v", err)
			}
			if intent == "disconnect" {
				if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
					t.Fatal(err)
				}
			}
			agent, err := Find(descriptor.ID)
			if err != nil || agent.Native.Read().Provider != "invalid" {
				t.Fatalf("interrupted %s did not surface in ordinary discovery: %v", intent, err)
			}
			changed := []byte(`{"note":"new unknown edit"}`)
			if err := os.WriteFile(settings, changed, 0o640); err != nil {
				t.Fatal(err)
			}
			if err := agent.Disconnect(); err == nil || !bytes.Equal(readSettings(), changed) {
				t.Fatal("disconnect recovery overwrote an edit made after interruption")
			}
			if err := os.WriteFile(settings, published, 0o640); err != nil {
				t.Fatal(err)
			}
			if err := agent.Disconnect(); err != nil {
				t.Fatalf("ordinary disconnect could not recover interrupted %s: %v", intent, err)
			}
			var restored map[string]string
			if err := json.Unmarshal(readSettings(), &restored); err != nil || restored["model"] != "own" ||
				restored["gateway"] != "" || restored["key"] != "" || restored["note"] != note || !agent.Disconnected() {
				t.Fatalf("recovery discarded original settings or user edits: %s (%v)", readSettings(), err)
			}
		})
	}
}

func TestExtensionAgentOrdinaryRejectsBuiltinCollision(t *testing.T) {
	installAgentActionFixture(t, sdk.AgentDescriptor{ID: "claude", Name: "Fake Claude",
		ConfigFiles: []string{".fixture.json"}})
	a, err := Find("claude")
	if err != nil || a.Name == "Fake Claude" {
		t.Fatalf("extension replaced a builtin: %+v %v", a, err)
	}
	if PluginErrors()["fixture.agent"] == "" {
		t.Fatal("collision was not reported")
	}
}

func TestExtensionAgentOrdinaryDiscoveryDoesNotCreateOwnershipFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata"))
	if len(All()) == 0 {
		t.Fatal("ordinary discovery returned no agents")
	}
	for _, path := range []string{extensionBindingPath(), filepath.Join(filepath.Dir(extensionBindingPath()), "sdk-agent-locks")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("read-only ordinary discovery created ownership state", path, err)
		}
	}
}
