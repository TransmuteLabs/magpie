package library

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func sdkProfileLibraryWorker(t *testing.T) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "worker")
	if runtime.GOOS == "windows" {
		filename += ".exe"
	}
	command := exec.CommandContext(t.Context(), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-p=1", "-o", filename, "../extension/testdata/actionworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build real SDK profile fixture: %v\n%s", err, output)
	}
	return filename
}

func sdkProfileLibraryInstall(t *testing.T, home, base, worker string) (*extension.Manager, sdk.AgentDescriptor) {
	t.Helper()
	descriptor := sdk.AgentDescriptor{
		ID: base + "-sdk", Name: "SDK profile for " + base, Aliases: []string{"sdk-library-" + base},
		Profile: &sdk.AgentProfile{Target: base}, ConfigFiles: []string{".sdk-library-profile.json"},
		Fields: []sdk.AgentField{{Key: "model", Label: "Model"}},
	}
	write(t, filepath.Join(home, ".sdk-library-profile.json"), `{"model":"own","note":"personal"}`)
	source := t.TempDir()
	filename := filepath.Base(worker)
	body, err := os.ReadFile(worker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, filename), body, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "fixture-library-profile", Version: "1.0.0", Runtime: "exec", Entry: filename,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
		Agents:   []sdk.AgentDescriptor{descriptor},
	}
	body, err = json.Marshal(manifest)
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
	if _, err := host.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	return host, descriptor
}

func sdkProfileLibraryAdopt(t *testing.T, descriptor sdk.AgentDescriptor) *Target {
	t.Helper()
	profile, err := agent.Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ConnectHow(); err != nil {
		t.Fatal("real SDK profile adoption:", err)
	}
	resolved, err := agent.Find(descriptor.Profile.Target)
	if err != nil || resolved.ID != descriptor.ID {
		t.Fatal("base alias did not resolve to its actual owned profile:", err)
	}
	target := targetOf(resolved)
	if target == nil || target.Agent.ID != descriptor.Profile.Target {
		t.Fatal("profile did not inherit its real base Library target")
	}
	return target
}

func sdkProfileLibraryHas(target *Target, kind string) bool {
	switch kind {
	case "instructions":
		return target.Instructions != ""
	case "mcp":
		return target.MCP != nil
	case "skills":
		return target.Skills != ""
	}
	return false
}

func TestSDKProfileTakesCanonicalIdentityAcrossLibraryFamilies(t *testing.T) {
	worker := sdkProfileLibraryWorker(t)
	for _, base := range []string{"claude", "codex", "gemini", "opencode", "mimocode", "deveco", "pi", "goose", "cursor", "copilot", "crush", "dsh"} {
		t.Run(base, func(t *testing.T) {
			home := sandbox(t)
			_, descriptor := sdkProfileLibraryInstall(t, home, base, worker)
			target := sdkProfileLibraryAdopt(t, descriptor)
			for _, query := range []string{base, descriptor.ID, descriptor.Aliases[0], descriptor.Name} {
				for _, kind := range []string{"instructions", "mcp", "skills"} {
					got, err := Takes(query, kind)
					if sdkProfileLibraryHas(target, kind) {
						if err != nil || got != target.Agent.ID {
							t.Errorf("Takes(%q,%q)=%q,%v; selected writer target is %q", query, kind, got, err, target.Agent.ID)
						}
					} else if err == nil {
						t.Errorf("Takes(%q,%q) granted a family without a real target", query, kind)
					}
				}
			}
		})
	}
}

func TestSDKProfileLibrarySelectionWritesReadsAndRemovesRealResources(t *testing.T) {
	worker := sdkProfileLibraryWorker(t)
	for _, base := range []string{"opencode", "claude", "codex", "gemini"} {
		t.Run(base, func(t *testing.T) {
			home := sandbox(t)
			_, descriptor := sdkProfileLibraryInstall(t, home, base, worker)
			target := sdkProfileLibraryAdopt(t, descriptor)
			if target.Instructions == "" || target.MCP == nil || target.Skills == "" {
				t.Fatal("fixture requires all three actual Library resource families")
			}
			write(t, target.Instructions, "Personal instructions\n")
			if err := target.MCP.put(&Server{Name: "personal", Transport: "stdio", Command: "personal-mcp", Env: map[string]string{"KEEP": "personal"}}, nil); err != nil {
				t.Fatal(err)
			}
			personalSkill := filepath.Join(target.Skills, "personal-skill", "SKILL.md")
			write(t, personalSkill, "---\nname: personal-skill\ndescription: Personal skill\n---\nPersonal content\n")
			for index, query := range []string{descriptor.ID, descriptor.Aliases[0], base, descriptor.Name} {
				t.Run(query, func(t *testing.T) {
					instructions, err := Takes(query, "instructions")
					if err != nil {
						t.Fatal(err)
					}
					shared := "Shared profile guidance"
					ok(t)(SaveInstructions(InstructionsChange{Shared: &shared, Agents: []string{instructions}}))
					if body := read(t, target.Instructions); !strings.Contains(body, shared) ||
						!strings.Contains(body, "Personal instructions") {
						t.Fatalf("Takes→SaveInstructions silently skipped its real writer or lost user text: %q", body)
					}
					servers, err := Takes(query, "mcp")
					if err != nil {
						t.Fatal(err)
					}
					ok(t)(SaveServer("", Server{Name: "sdk-owned", Transport: "stdio", Command: "fixture-mcp", Agents: []string{servers}}))
					actual, err := target.MCP.read()
					if err != nil || actual["sdk-owned"] == nil || actual["personal"] == nil ||
						actual["personal"].Command != "personal-mcp" || actual["personal"].Env["KEEP"] != "personal" {
						t.Fatal("Takes→SaveServer skipped its writer or lost the personal server:", err)
					}
					skills, err := Takes(query, "skills")
					if err != nil {
						t.Fatal(err)
					}
					name := "sdk-skill-" + strings.Repeat("x", index+1)
					ok(t)(CreateSkill(name, "SDK profile resource", []string{skills}))
					skillFile := filepath.Join(target.Skills, name, "SKILL.md")
					if body := read(t, skillFile); !strings.Contains(body, "SDK profile resource") {
						t.Fatal("Takes→CreateSkill skipped the real skills writer")
					}
					state, err := load()
					if err != nil || !slices.Equal(state.Instructions.Agents, []string{target.Agent.ID}) ||
						!slices.Equal(state.server("sdk-owned").Agents, []string{target.Agent.ID}) ||
						!slices.Equal(state.skill(name).Agents, []string{target.Agent.ID}) {
						t.Fatal("Library persisted a profile selector instead of its canonical target:", err)
					}
					ok(t)(SaveInstructions(InstructionsChange{Agents: []string{}}))
					ok(t)(RemoveServer("sdk-owned"))
					ok(t)(RemoveSkill(name))
					actual, err = target.MCP.read()
					if err != nil || actual["sdk-owned"] != nil || actual["personal"] == nil ||
						actual["personal"].Command != "personal-mcp" || actual["personal"].Env["KEEP"] != "personal" {
						t.Fatal("ordinary resource removal crossed personal server ownership:", err)
					}
					if body := read(t, target.Instructions); strings.Contains(body, shared) ||
						!strings.Contains(body, "Personal instructions") {
						t.Fatal("ordinary instruction removal lost personal text")
					}
					if _, err := os.Lstat(filepath.Dir(skillFile)); !os.IsNotExist(err) {
						t.Fatal("ordinary skills removal left a profile-owned link:", err)
					}
					if body := read(t, personalSkill); !strings.Contains(body, "Personal content") {
						t.Fatal("ordinary skills removal changed a personal skill")
					}
				})
			}
		})
	}
}

func TestSDKProfileLibraryInactiveOffAndLegacyIdentity(t *testing.T) {
	worker := sdkProfileLibraryWorker(t)
	home := sandbox(t)
	for _, kind := range []string{"instructions", "mcp", "skills"} {
		if got, err := Takes("opencode", kind); err != nil || got != "opencode" {
			t.Fatalf("ordinary legacy identity changed before SDK installation: %s %s %v", kind, got, err)
		}
	}
	host, descriptor := sdkProfileLibraryInstall(t, home, "opencode", worker)
	unchanged := read(t, filepath.Join(home, ".sdk-library-profile.json"))
	assertInactive := func() {
		t.Helper()
		for _, kind := range []string{"instructions", "mcp", "skills"} {
			if got, err := Takes("opencode", kind); err != nil || got != "opencode" {
				t.Fatalf("inactive SDK profile displaced legacy Library identity: %s %s %v", kind, got, err)
			}
			if _, err := Takes(descriptor.ID, kind); err == nil {
				t.Fatalf("inactive SDK profile gained Library authority for %s", kind)
			}
		}
	}
	assertInactive()
	if err := host.SetEnabled(t.Context(), "fixture-library-profile", false); err != nil {
		t.Fatal(err)
	}
	assertInactive()
	if got := read(t, filepath.Join(home, ".sdk-library-profile.json")); got != unchanged {
		t.Fatal("Library observation modified an unowned profile configuration")
	}
	if err := host.SetEnabled(t.Context(), "fixture-library-profile", true); err != nil {
		t.Fatal(err)
	}
	sdkProfileLibraryAdopt(t, descriptor)
	if err := host.SetEnabled(t.Context(), "fixture-library-profile", false); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"instructions", "mcp", "skills"} {
		if got, err := Takes(descriptor.ID, kind); err != nil || got != "opencode" {
			t.Fatalf("known off-profile lost the independent base Library owner: %s %s %v", kind, got, err)
		}
	}
	recovery, err := agent.Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Disconnect(); err != nil {
		t.Fatal("off-profile recovery failed:", err)
	}
	assertInactive()
	if got := read(t, filepath.Join(home, ".sdk-library-profile.json")); got != unchanged {
		t.Fatal("profile recovery failed to preserve its user-owned preimage")
	}
}
