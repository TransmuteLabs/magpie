package library

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func sdkGrokHookFixture(t *testing.T, worker, home string, library sdk.AgentLibrary) sdk.AgentDescriptor {
	t.Helper()
	descriptor := sdk.AgentDescriptor{
		ID: "grok-app-sdk", Name: "Grok App SDK fixture",
		Profile: &sdk.AgentProfile{Target: "grok-app"}, ConfigFiles: []string{".sdk-grok-config.json"},
		Library: &library,
	}
	write(t, filepath.Join(home, descriptor.ConfigFiles[0]), `{"personal":"kept"}`)
	source := t.TempDir()
	entry := filepath.Base(worker)
	body, err := os.ReadFile(worker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, entry), body, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "fixture-library-grok", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
		Agents:   []sdk.AgentDescriptor{descriptor},
	}
	body, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(source, sdk.ManifestFile), string(body))
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
	return descriptor
}

const sdkGrokBaseHook = "model = \"test\"\n[[hooks.Stop]]\n  [[hooks.Stop.hooks]]\n  type = \"command\"\n  command = \"base-hook\"\n"
const sdkGrokOtherHook = "\n[[hooks.Stop]]\n  [[hooks.Stop.hooks]]\n  type = \"command\"\n  command = \"keep-me\"\n"
const sdkGrokCustomFile = ".sdk-grok/config.toml"

func sdkGrokHookHome(t *testing.T) (string, string) {
	t.Helper()
	home := sandbox(t)
	app := filepath.Join(home, "grok-app-data")
	t.Setenv("GROK_APP_HOME", app)
	baseHookFile := filepath.Join(app, "agent-home", "config.toml")
	write(t, baseHookFile, sdkGrokBaseHook)
	return home, baseHookFile
}

func TestSDKGrokAppLibraryReadWithoutCompatibleMCP(t *testing.T) {
	worker := sdkProfileLibraryWorker(t)
	for _, format := range []string{"", "claude"} {
		t.Run("format="+format, func(t *testing.T) {
			home, baseHookFile := sdkGrokHookHome(t)
			places := sdk.AgentLibrary{Skills: ".sdk-grok/skills"}
			if format != "" {
				places.MCP, places.MCPFormat = ".sdk-grok/mcp.json", format
				write(t, filepath.Join(home, places.MCP), "{}\n")
			}
			sdkProfileLibraryAdopt(t, sdkGrokHookFixture(t, worker, home, places))
			view, err := Read(nil)
			if err != nil {
				t.Fatal(err)
			}
			if index := slices.IndexFunc(view.HookAgents, func(row HookAgentView) bool { return row.ID == "grok-app" }); index < 0 {
				t.Fatal("Library did not list the adopted Grok App")
			} else if row := view.HookAgents[index]; row.File != "" || row.No == "" {
				t.Errorf("Library offered unsupported hooks: %+v", row)
			}
			if f := foundBy(view.FoundHooks, "grok-app", "base-hook"); f != nil {
				t.Fatalf("explicit SDK target fell back to the original config: %+v", f)
			}
			if got := read(t, baseHookFile); got != sdkGrokBaseHook {
				t.Fatal("Library read changed the app's original config")
			}
		})
	}
}

func TestFoundHookKeyDoesNotFollowSDKGrokAppAcrossFiles(t *testing.T) {
	worker := sdkProfileLibraryWorker(t)
	home, baseHookFile := sdkGrokHookHome(t)
	base := foundBy(foundHooks(t), "grok-app", "base-hook")
	if base == nil || base.File != baseHookFile {
		t.Fatalf("fixture lacks the original Grok App hook: %+v", base)
	}
	customHookFile := filepath.Join(home, sdkGrokCustomFile)
	before := sdkGrokBaseHook + sdkGrokOtherHook
	write(t, customHookFile, before)
	descriptor := sdkGrokHookFixture(t, worker, home, sdk.AgentLibrary{MCP: sdkGrokCustomFile, MCPFormat: "grok"})
	target := sdkProfileLibraryAdopt(t, descriptor)
	if target.MCP == nil || target.MCP.Path != customHookFile {
		t.Fatalf("the adopted profile did not select its alternate TOML file: %+v", target.MCP)
	}
	selected := foundBy(foundHooks(t), "grok-app", "base-hook")
	if selected == nil || selected.File != customHookFile || selected.Hook == nil {
		t.Fatalf("fixture lacks the adopted Grok App hook: %+v", selected)
	}
	if selected.Key == base.Key {
		t.Error("the original file's hook key is reused for the SDK profile's alternate file")
	}
	if _, err := ImportHooks([]string{base.Key}); err == nil {
		t.Error("the stale key imported a hook from another file")
	}
	if _, err := RemoveFoundHooks([]string{base.Key}); err == nil || read(t, customHookFile) != before {
		t.Fatalf("stale original key affected another file's hook: err %v, now %q", err, read(t, customHookFile))
	}
	ok(t)(ImportHooks([]string{selected.Key}))
	if f := foundBy(foundHooks(t), "grok-app", "base-hook"); f != nil || read(t, customHookFile) != before {
		t.Fatalf("fresh import failed to claim the unchanged selected hook: %+v", f)
	}
	other := foundBy(foundHooks(t), "grok-app", "keep-me")
	if other == nil || other.File != customHookFile {
		t.Fatalf("missing other hook in adopted file: %+v", other)
	}
	ok(t)(RemoveFoundHooks([]string{other.Key}))
	if got := read(t, customHookFile); strings.TrimSpace(got) != strings.TrimSpace(sdkGrokBaseHook) {
		t.Errorf("fresh removal changed the imported hook or settings: %q", got)
	}
	if !slices.Contains(keptCopies(t, "config.toml"), before) {
		t.Error("fresh removal did not back up the selected file")
	}
	if read(t, baseHookFile) != sdkGrokBaseHook {
		t.Fatal("the original file changed after profile adoption")
	}
}

func TestSDKGrokAppHookClaimsStayWithSelectedFile(t *testing.T) {
	worker := sdkProfileLibraryWorker(t)
	for _, kind := range []string{"recorded-file", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			home, baseHookFile := sdkGrokHookHome(t)
			base := foundBy(foundHooks(t), "grok-app", "base-hook")
			if base == nil || base.Hook == nil {
				t.Fatalf("fixture lacks the original importable Grok App hook: %+v", base)
			}
			ok(t)(ImportHooks([]string{base.Key}))
			library := mustLoad(t)
			name := library.Hooks[0].Name
			if kind == "legacy" {
				library.Applied["grok-app"].Hooks[0].File = ""
				if err := library.save(); err != nil {
					t.Fatal(err)
				}
			}
			customHookFile := filepath.Join(home, sdkGrokCustomFile)
			write(t, customHookFile, sdkGrokBaseHook)
			descriptor := sdkGrokHookFixture(t, worker, home, sdk.AgentLibrary{MCP: sdkGrokCustomFile, MCPFormat: "grok"})
			sdkProfileLibraryAdopt(t, descriptor)
			if f := foundBy(foundHooks(t), "grok-app", "base-hook"); f == nil || f.File != customHookFile {
				t.Errorf("original claim hid a personal hook at the alternate path: %+v", f)
			}
			ok(t)(RemoveHooks([]string{name}))
			if got := read(t, customHookFile); got != sdkGrokBaseHook {
				t.Errorf("removing the original claim changed the personal file: %q", got)
			}
			if got := read(t, baseHookFile); got != sdkGrokBaseHook {
				t.Errorf("removing the original claim wrote to the inactive file: %q", got)
			}
			profile, err := agent.Find(descriptor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := profile.Disconnect(); err != nil {
				t.Fatal(err)
			}
			ok(t)(Sync())
			got := read(t, baseHookFile)
			if kind == "legacy" {
				if got != sdkGrokBaseHook {
					t.Errorf("ambiguous legacy claim changed the base file: %q", got)
				}
				base := foundBy(foundHooks(t), "grok-app", "base-hook")
				if base == nil || base.File != baseHookFile {
					t.Fatalf("legacy hook couldn't be reclaimed: %+v", base)
				}
				ok(t)(ImportHooks([]string{base.Key}))
				ok(t)(RemoveHooks([]string{name}))
				got = read(t, baseHookFile)
			}
			if strings.TrimSpace(got) != `model = "test"` {
				t.Errorf("cleanup did not remove the file-bound claim: %q", got)
			}
			if got := read(t, customHookFile); got != sdkGrokBaseHook {
				t.Errorf("base cleanup touched the alternate personal file: %q", got)
			}
		})
	}
}
