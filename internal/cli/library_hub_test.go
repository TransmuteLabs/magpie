package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

// magpie library mcp router on|off (#2679): on, the agent is given magpie's
// one MCP server in place of the library's; off, each server's own again.
// Any other word is refused, and writes nothing. "hub", its name before,
// still works.
func TestLibraryMCPHub(t *testing.T) {
	h := t.TempDir()
	for k, v := range map[string]string{"HOME": h, "USERPROFILE": h, "XDG_CONFIG_HOME": filepath.Join(h, ".config"), "CLAUDE_CONFIG_DIR": "", "CODEX_HOME": "", "APPDATA": "", "LOCALAPPDATA": ""} {
		t.Setenv(k, v)
	}
	if err := os.MkdirAll(filepath.Join(h, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".claude/settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(h, ".claude.json"))
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		var out []string
		for k := range c.MCPServers {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	if err := libraryCmd([]string{"library", "mcp", "add", "fs", "npx", "-y", "fs", "agents=claude"}); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"fs"}) {
		t.Fatalf("before: %v", got)
	}
	if err := libraryCmd([]string{"library", "mcp", "router", "on"}); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"magpie"}) {
		t.Fatalf("router on: %v", got)
	}
	if err := libraryCmd([]string{"library", "mcp", "router", "yes"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("router yes: %v", err)
	}
	if got := names(); !slices.Equal(got, []string{"magpie"}) {
		t.Fatalf("after a word it doesn't know: %v", got)
	}
	// a server kept direct (#4382) is its own entry again, beside no hub
	// entry once no server is behind it; shared, behind the router again
	// (said with "hub", its name before)
	if err := libraryCmd([]string{"library", "mcp", "router", "fs", "direct"}); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"fs"}) {
		t.Fatalf("fs direct: %v", got)
	}
	if err := libraryCmd([]string{"library", "mcp", "router", "fs", "solo"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("router fs solo: %v", err)
	}
	if err := libraryCmd([]string{"library", "mcp", "hub", "fs", "shared"}); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"magpie"}) {
		t.Fatalf("fs shared: %v", got)
	}
	if err := libraryCmd([]string{"library", "mcp", "hub", "off"}); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"fs"}) {
		t.Fatalf("hub off: %v", got)
	}
}

func TestLibraryMCPAccountUnbind(t *testing.T) {
	h := t.TempDir()
	for k, v := range map[string]string{"HOME": h, "USERPROFILE": h, "XDG_CONFIG_HOME": filepath.Join(h, ".config"), "CLAUDE_CONFIG_DIR": "", "CODEX_HOME": "", "APPDATA": "", "LOCALAPPDATA": ""} {
		t.Setenv(k, v)
	}
	if err := libraryCmd([]string{"library", "mcp", "add", "official", "https://example.test/mcp"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(settings.Dir(), "library.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	server := doc["mcp"].([]any)[0].(map[string]any)
	server["pluginAccount"] = map[string]any{"provider": "zcode", "account": "slot", "scope": "verified-scope"}
	b, _ = json.Marshal(doc)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := libraryCmd([]string{"library", "mcp", "account", "official", "bind", "zcode", "slot", "--unknown"}); err == nil {
		t.Fatal("an unknown flag was accepted as a bind command")
	}
	if err := libraryCmd([]string{"library", "mcp", "account", "official", "unbind"}); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(path)
	doc = nil
	if err != nil || json.Unmarshal(b, &doc) != nil {
		t.Fatalf("reading library after unbind: %v", err)
	}
	server = doc["mcp"].([]any)[0].(map[string]any)
	if _, ok := server["pluginAccount"]; ok {
		t.Fatalf("unbind left the account binding: %s", b)
	}
}

func TestLibraryMCPAccountBind(t *testing.T) {
	bun := testenv.Bun(t)
	h := t.TempDir()
	for k, v := range map[string]string{"HOME": h, "USERPROFILE": h, "XDG_CONFIG_HOME": filepath.Join(h, ".config"),
		"XDG_CACHE_HOME": filepath.Join(h, ".cache"), "CLAUDE_CONFIG_DIR": filepath.Join(h, ".claude"), "MAGPIE_BUN": bun,
		"CODEX_HOME": "", "APPDATA": "", "LOCALAPPDATA": ""} {
		t.Setenv(k, v)
	}
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p := filepath.Join(t.TempDir(), "fixture.mjs")
	src := `export const Fixture = async () => ({
  config: async c => { c.provider = c.provider || {}; c.provider.fixture = {
    name: "Fixture", npm: "@ai-sdk/openai-compatible", api: "https://fixture.invalid",
    models: {"one": {name: "One", limit: {context: 1000, output: 100}}}
  }},
  auth: {
    provider: "fixture", methods: [{type: "api", label: "Fixture"}],
    accountOperations: ["mcp.headers"],
    accountPreflight: ({params}) => {
      if (params.url !== "https://trusted.invalid/mcp") throw new Error("untrusted")
    },
    account: async (getAuth, _provider, input) => {
      const auth = await getAuth()
      if (auth.key !== "fixture-secret" || input.operation !== "mcp.headers") throw new Error("wrong account")
      return {context: {scope: "fixture-scope", generation: "fixture-epoch"},
              data: {headers: {Authorization: "Bearer fixture-secret"}}}
    }
  }
})`
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Add(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	account, err := plugin.Import(ctx, "fixture", map[string]any{"type": "api", "key": "fixture-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := libraryCmd([]string{"library", "mcp", "add", "official", "https://trusted.invalid/mcp"}); err != nil {
		t.Fatal(err)
	}
	if err := libraryCmd([]string{"library", "mcp", "account", "official", "bind", "fixture", account}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(settings.Dir(), "library.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"scope": "fixture-scope"`) || strings.Contains(string(b), "fixture-secret") {
		t.Fatalf("binding did not preserve only nonsecret identity: %s", b)
	}
	if err := libraryCmd([]string{"library", "mcp", "account", "official", "unbind"}); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(path)
	if err != nil || strings.Contains(string(b), "pluginAccount") {
		t.Fatalf("unbind left the binding: %s %v", b, err)
	}
}
