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

func TestLinkedAgentLibraryTargets(t *testing.T) {
	home := sandbox(t)
	opencode := filepath.Join(home, "open code")
	deveco := filepath.Join(home, "dev eco")
	mimo := filepath.Join(home, "mi mo")
	omp := filepath.Join(home, ".omp", "profiles", "work", "agent")
	t.Setenv("OPENCODE_CONFIG_DIR", opencode)
	t.Setenv("DEVECO_CONFIG_DIR", deveco)
	t.Setenv("MIMOCODE_HOME", mimo)
	t.Setenv("OMP_PROFILE", "work")
	holdCLI(t, home, "omp")
	setOmpVersion(t, "11.0.0")
	write(t, filepath.Join(omp, "config.yml"), "{}\n")

	for _, fixture := range []struct {
		id, directory, file, format, note string
		claudeSkills, shared              bool
	}{
		{"opencode", opencode, "opencode.jsonc", "opencode", "opencode-claude", true, false},
		{"deveco", deveco, "config.json", "opencode", "opencode-claude", true, false},
		{"mimocode", filepath.Join(mimo, "config"), "mimocode.json", "opencode", "", false, false},
		{"omp", omp, "mcp.json", "omp", "", false, true},
	} {
		t.Run(fixture.id, func(t *testing.T) {
			mcp := filepath.Join(fixture.directory, fixture.file)
			write(t, mcp, "{}\n")
			target := targetByID(fixture.id)
			if target == nil || target.Instructions != filepath.Join(fixture.directory, "AGENTS.md") ||
				target.MCP == nil || target.MCP.Path != mcp || target.MCP.Format != pluginMCP[fixture.format] ||
				target.Skills != filepath.Join(fixture.directory, "skills") {
				t.Fatalf("wrong Library target for %s: %+v", fixture.id, target)
			}
			if target.Note != fixture.note || slices.Contains(target.SkillsAlso, "claude") != fixture.claudeSkills ||
				target.SharedToo != fixture.shared || target.Copy || target.Store != "" {
				t.Fatalf("wrong Library policy for %s: %+v", fixture.id, target)
			}

			format := pluginMCP[fixture.format]
			delete(pluginMCP, fixture.format)
			t.Cleanup(func() { pluginMCP[fixture.format] = format })
			if target := targetByID(fixture.id); target != nil {
				t.Fatalf("ordinary target bypassed the SDK's unknown MCP format rejection: %+v", target)
			}
		})
	}
}

func TestSDKAgentLibraryTargetUsesOrdinaryOwners(t *testing.T) {
	home := sandbox(t)
	holdCLI(t, home, "sdk-editor")
	source := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-p=1",
		"-o", filepath.Join(source, entry), "../extension/testdata/actionworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agent fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "fixture-library", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
		Agents: []sdk.AgentDescriptor{{
			ID: "sdk-editor", Name: "SDK editor", Bin: "sdk-editor", ConfigFiles: []string{".sdk-editor/config.json"},
			Fields:  []sdk.AgentField{{Key: "model", Label: "Model"}},
			Library: &sdk.AgentLibrary{Instructions: ".sdk-editor/AGENTS.md", MCP: ".sdk-editor/mcp.json", MCPFormat: "claude", Skills: ".sdk-editor/skills"},
		}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".sdk-editor/config.json"), `{"model":"own"}`)
	write(t, filepath.Join(home, ".sdk-editor/AGENTS.md"), "Personal guidance\n")
	write(t, filepath.Join(home, ".sdk-editor/mcp.json"), `{"mcpServers":{"mine":{"command":"mine"}},"user":true}`)
	host := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := host.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := host.Install(ctx, source); err != nil {
		t.Fatal(err)
	}
	a, err := agent.Find("sdk-editor")
	if err != nil {
		t.Fatal(err)
	}
	target := targetOf(a)
	if target == nil || target.MCP == nil || target.MCP.Path != filepath.Join(home, ".sdk-editor/mcp.json") ||
		target.Skills != filepath.Join(home, ".sdk-editor/skills") {
		t.Fatal("SDK agent did not reuse the ordinary Library target")
	}
	shared := "Shared SDK guidance"
	ok(t)(SaveInstructions(InstructionsChange{Shared: &shared, Agents: []string{"sdk-editor"}}))
	if text := read(t, target.Instructions); !strings.Contains(text, shared) || !strings.Contains(text, "Personal guidance") {
		t.Fatal("ordinary instructions writer lost user text or SDK target")
	}
	ok(t)(SaveServer("", Server{Name: "owned", Transport: "stdio", Command: "fixture-server", Agents: []string{"sdk-editor"}}))
	servers, err := target.MCP.read()
	if err != nil || servers["owned"] == nil || servers["mine"] == nil {
		t.Fatal("ordinary MCP owner failed to preserve existing servers", err)
	}
	ok(t)(CreateSkill("sdk-skill", "SDK skill fixture", []string{"sdk-editor"}))
	if _, err := os.Stat(filepath.Join(target.Skills, "sdk-skill", "SKILL.md")); err != nil {
		t.Fatal("ordinary skill owner did not reach SDK target:", err)
	}
	ok(t)(ServerAgents("owned", nil))
	servers, err = target.MCP.read()
	if err != nil || servers["owned"] != nil || servers["mine"] == nil {
		t.Fatal("ordinary MCP removal changed user ownership", err)
	}
}

func TestSDKAgentLibraryFormatRegistryMatchesOwner(t *testing.T) {
	for format := range pluginMCP {
		manifest := sdk.Manifest{
			Schema: 1, ID: "fixture", Version: "1.0.0", Runtime: "exec", Entry: "worker",
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
			Agents: []sdk.AgentDescriptor{{
				ID: "agent", Name: "Agent", Library: &sdk.AgentLibrary{MCP: ".agent/mcp.json", MCPFormat: format},
			}},
		}
		if err := manifest.Validate(); err != nil {
			t.Fatalf("existing Library format %s was omitted from SDK metadata: %v", format, err)
		}
	}
}
