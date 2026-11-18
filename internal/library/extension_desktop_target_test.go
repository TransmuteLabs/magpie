package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/agent"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKDesktopProfileSingleLibraryTarget(t *testing.T) {
	worker := sdkProfileLibraryWorker(t)
	home := sandbox(t)
	_, d := sdkProfileLibraryInstall(t, home, "claude-desktop", worker)
	// Make the existing library-only desktop entry detected in this temp home.
	for _, app := range apps() {
		if app.ID == "claude-desktop" {
			write(t, app.Path, `{"mcpServers":{}}`)
		}
	}
	before := 0
	for _, target := range Targets() {
		if target.Agent.ID == "claude-desktop" {
			before++
		}
	}
	if before != 1 {
		t.Fatalf("unadopted profile changed ordinary desktop targets: %d", before)
	}
	a, err := agent.Find(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	targets := Targets()
	n := 0
	for _, target := range targets {
		if target.Agent.ID == "claude-desktop" {
			n++
			t.Logf("desktop writer MCP=%s", filepath.Clean(target.MCP.Path))
		}
	}
	if n != 1 {
		t.Fatalf("one active desktop profile produced %d writers under the same Library identity", n)
	}
}

func TestSDKDesktopProfileWritesOnlySelectedLibraryTarget(t *testing.T) {
	worker := sdkProfileLibraryWorker(t)
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "blocked"}[blocked], func(t *testing.T) {
			home := sandbox(t)
			host, d := sdkProfileLibraryInstall(t, home, "claude-desktop", worker)
			original := ""
			originalBody := `{"mcpServers":{"personal":{"command":"personal-mcp"}},"note":"keep"}`
			for _, app := range apps() {
				if app.ID == "claude-desktop" {
					original = app.Path
					write(t, original, originalBody)
				}
			}
			entries, err := host.List()
			if err != nil || len(entries) != 1 {
				t.Fatal(err)
			}
			source := t.TempDir()
			if err := os.CopyFS(source, os.DirFS(filepath.Join(host.Namespace(), filepath.FromSlash(entries[0].Directory)))); err != nil {
				t.Fatal(err)
			}
			m := entries[0].Manifest.Clone()
			m.Version = "1.0.1"
			d.Library = &sdk.AgentLibrary{MCP: ".profile-specific/mcp.json", MCPFormat: "claude"}
			m.Agents = []sdk.AgentDescriptor{d}
			raw, _ := json.Marshal(m)
			if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := host.Install(t.Context(), source); err != nil {
				t.Fatal(err)
			}
			a, err := agent.Find(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.ConnectHow(); err != nil {
				t.Fatal(err)
			}
			if blocked {
				write(t, filepath.Join(home, ".profile-specific"), "not a directory")
			}
			canonical, err := Takes(d.ID, "mcp")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := SaveServer("", Server{Name: "sdk-owned", Transport: "stdio", Command: "fixture-server", Agents: []string{canonical}}); err != nil {
				t.Fatal(err)
			}
			if !blocked && !strings.Contains(read(t, filepath.Join(home, ".profile-specific/mcp.json")), "sdk-owned") {
				t.Fatal("profile target did not get server")
			}
			if read(t, original) != originalBody {
				t.Fatal("profile-specific MCP selection changed the original desktop config outside the selected Library target")
			}
			count := 0
			for _, target := range Targets() {
				if target.Agent.ID == "claude-desktop" {
					count++
				}
			}
			expected := 1
			if blocked {
				expected = 0
			}
			if count != expected {
				t.Fatalf("canonical desktop targets=%d, want %d", count, expected)
			}
		})
	}
}

// A canonical Library identity is not permission to clean a different path
// when the selected profile's directory becomes unavailable.
func TestSDKDesktopProfileCleanupKeepsSelectedTarget(t *testing.T) {
	worker := sdkProfileLibraryWorker(t)
	operations := []struct {
		name string
		run  func(string) (*Result, error)
	}{
		{"remove", func(string) (*Result, error) { return RemoveServer("sdk-owned") }},
		{"remove-many", func(string) (*Result, error) { return RemoveServers([]string{"sdk-owned"}) }},
		{"untick", func(string) (*Result, error) { return ServerAgents("sdk-owned", nil) }},
		{"untick-many", func(id string) (*Result, error) {
			return SomeServersAgents([]string{"sdk-owned"}, []string{id}, false)
		}},
	}
	for _, operation := range operations {
		for _, blocked := range []bool{false, true} {
			t.Run(operation.name+"/"+map[bool]string{false: "available", true: "blocked"}[blocked], func(t *testing.T) {
				home := sandbox(t)
				host, descriptor := sdkProfileLibraryInstall(t, home, "claude-desktop", worker)
				original := ""
				originalBody := `{"mcpServers":{"sdk-owned":{"command":"user-original-mcp"}},"note":"keep"}`
				for _, app := range apps() {
					if app.ID == "claude-desktop" {
						original = app.Path
						write(t, original, originalBody)
					}
				}
				entries, err := host.List()
				if err != nil || len(entries) != 1 {
					t.Fatal("fixture module", entries, err)
				}
				source := t.TempDir()
				if err := os.CopyFS(source, os.DirFS(filepath.Join(host.Namespace(), filepath.FromSlash(entries[0].Directory)))); err != nil {
					t.Fatal(err)
				}
				manifest := entries[0].Manifest.Clone()
				manifest.Version = "1.0.1"
				descriptor.Library = &sdk.AgentLibrary{MCP: ".profile-specific/mcp.json", MCPFormat: "claude"}
				manifest.Agents = []sdk.AgentDescriptor{descriptor}
				raw, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(source, sdk.ManifestFile), string(raw))
				if _, err := host.Install(t.Context(), source); err != nil {
					t.Fatal(err)
				}
				profile, err := agent.Find(descriptor.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := profile.ConnectHow(); err != nil {
					t.Fatal(err)
				}
				canonical, err := Takes(descriptor.ID, "mcp")
				if err != nil {
					t.Fatal(err)
				}
				ok(t)(SaveServer("", Server{Name: "sdk-owned", Transport: "stdio", Command: "fixture-server", Agents: []string{canonical}}))
				selected := filepath.Join(home, ".profile-specific/mcp.json")
				if !strings.Contains(read(t, selected), "sdk-owned") || read(t, original) != originalBody {
					t.Fatal("fixture did not write only to its selected target")
				}
				directory := filepath.Dir(selected)
				if blocked {
					if err := os.Rename(directory, directory+".saved"); err != nil {
						t.Fatal(err)
					}
					write(t, directory, "not a directory")
				}
				ok(t)(operation.run(canonical))
				if got := read(t, original); got != originalBody {
					t.Fatalf("cleanup changed the unrelated original config: got %s want %s", got, originalBody)
				}
				view, err := Read(nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, target := range view.Agents {
					if target.ID == canonical && (blocked || target.MCP != selected) {
						t.Fatalf("view substituted a different cleanup target: %+v", target)
					}
				}
				if blocked {
					if err := os.Remove(directory); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(directory+".saved", directory); err != nil {
						t.Fatal(err)
					}
					ok(t)(Sync())
				}
				if strings.Contains(read(t, selected), "sdk-owned") {
					t.Fatal("selected output was not cleaned when it became available")
				}
				if read(t, original) != originalBody {
					t.Fatal("retry changed the unrelated original config")
				}
			})
		}
	}
}
