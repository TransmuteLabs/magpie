package library

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The market's counts of who uses each server (gugediyige666 on X: a
// ranking by installs): the day's report names a library server by its
// market id only when the market lists it from what ships with magpie and
// an agent is given it. A server of the user's own, one no agent has, and
// one that names no package are never named, nor anything of a server but
// its id.
func TestPublicServers(t *testing.T) {
	listed := readPopular([]byte(`[
		{"name":"io.github.big/one","packages":[{"registryType":"npm","identifier":"@big/one"}]},
		{"name":"io.github.big/again","packages":[{"registryType":"npm","identifier":"@big/one"}]},
		{"name":"io.github.far/away","remotes":[{"type":"streamable-http","url":"https://far.dev/mcp"}]},
		{"name":"io.github.no/agent","packages":[{"registryType":"npm","identifier":"no-agent"}]}
	]`))
	if len(listed) < 3 {
		t.Fatalf("the fixture lists %d servers", len(listed))
	}
	l := &Library{MCP: []*Server{
		{Name: "secret-name", Command: "npx", Args: []string{"-y", "@big/one@latest"}, Env: map[string]string{"TOKEN": "sk-1"}, Agents: []string{"claude"}},
		{Name: "twice", Command: "npx", Args: []string{"@big/one"}, Agents: []string{"codex"}},
		{Name: "far", Transport: "http", URL: "https://FAR.dev/mcp/", Agents: []string{"claude"}},
		{Name: "idle", Command: "npx", Args: []string{"no-agent"}},
		{Name: "mine", Transport: "http", URL: "https://intranet.corp/mcp?key=abc", Agents: []string{"claude"}},
		{Name: "bare", Command: "npx", Agents: []string{"claude"}},
	}}
	listed = append(listed, MarketServer{ID: "bare-one", server: Server{Command: "npx"}})
	got := l.publicServers(listed)
	if want := []string{"io.github.big/one", "io.github.far/away"}; !slices.Equal(got, want) {
		t.Fatalf("named %q, want %q", got, want)
	}
	for _, id := range got {
		for _, leak := range []string{"secret", "sk-1", "intranet", "abc", "twice"} {
			if strings.Contains(id, leak) {
				t.Errorf("%q carries %q", id, leak)
			}
		}
	}
}

func TestPublicServersDistinguishBuiltinCommands(t *testing.T) {
	listed := []MarketServer{memoryServer(), searchServer()}
	library := Library{MCP: []*Server{
		{Name: "private-helper", Command: "/opt/acme/internal-helper", Args: []string{"mcp", "memory"}, Agents: []string{"claude"}},
		{Name: "search", Command: `C:\Tools\magpie-windows-amd64.exe`, Args: []string{"mcp", "search"}, Agents: []string{"codex"}},
	}}
	if got := library.publicServers(listed); !slices.Equal(got, []string{searchServerName}) {
		t.Fatalf("named %q, want only %q", got, searchServerName)
	}
}

func TestPublicServersKeepsCommandIdentityThroughSync(t *testing.T) {
	for _, test := range []struct {
		name, command string
		present, want bool
	}{
		{name: "private helper present", command: "magpie-private-helper", present: true},
		{name: "private helper missing", command: "magpie-private-helper"},
		{name: "portable", command: "magpie", want: true},
		{name: "macOS", command: "Magpie.app/Contents/MacOS/magpie", present: true, want: true},
		{name: "Windows launcher", command: `C:\Tools\magpie.com`, want: true},
		{name: "Windows download", command: `C:\Downloads\magpie-windows-amd64 (1).exe`, want: true},
		{name: "Linux CLI", command: "/old/bin/magpie-cli-linux-arm64", want: true},
		{name: "development build present", command: "magpie-dev", present: true, want: true},
		{name: "development backend from another computer", command: "/old/magpie-dev-backend", want: true},
		{name: "renamed current build", command: "team-proxy", present: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := sandbox(t)
			exe := filepath.Join(home, "bin", "team-proxy")
			write(t, exe, "")
			thisMagpie(t, exe)
			command := test.command
			if test.present || test.name == "private helper missing" {
				command = filepath.Join(home, test.command)
			}
			if test.name == "renamed current build" {
				command = exe
			}
			if test.present {
				write(t, command, "")
			}
			seedLibrary(t, Server{Name: memoryServerName, Transport: "stdio", Command: command, Args: []string{"mcp", "memory"}, Agents: []string{"claude"}})
			if got := slices.Contains(PublicServers(), memoryServerName); got != test.want {
				t.Errorf("counted as %s = %t, want %t", memoryServerName, got, test.want)
			}
			if got := storedCommand(t, memoryServerName); got != command {
				t.Errorf("telemetry changed saved command to %q, want %q", got, command)
			}
			ok(t)(Sync())
			if got := slices.Contains(PublicServers(), memoryServerName); got != test.want {
				t.Errorf("after sync, counted as %s = %t, want %t", memoryServerName, got, test.want)
			}
			if got := storedCommand(t, memoryServerName); !test.want && got != command {
				t.Errorf("sync changed the user's command to %q, want %q", got, command)
			}
			portable := command
			if test.want {
				portable = magpieCommand
			}
			bundle, err := Collect()
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.MCP) != 1 {
				t.Fatalf("carried %d servers, want one", len(bundle.MCP))
			}
			if got := bundle.MCP[0].Command; got != portable {
				t.Errorf("carry named the command %q, want %q", got, portable)
			}
			otherExe := filepath.Join(home, "bin", "other-magpie-build")
			write(t, otherExe, "")
			thisMagpie(t, otherExe)
			ok(t)(Put(bundle))
			restored := command
			if test.want {
				restored = otherExe
			}
			if got := storedCommand(t, memoryServerName); got != restored {
				t.Errorf("restored command %q, want %q", got, restored)
			}
		})
	}
}

// What ships with magpie: every featured and popular server's id is one
// the site takes (site/pluginstats.js's validServer), so none is dropped
// there or names more than a catalog id.
func TestPublicServerIDsAreCatalogIDs(t *testing.T) {
	ok := func(id string) bool {
		if id == "" || len(id) > 200 {
			return false
		}
		parts := strings.Split(id, "/")
		if len(parts) > 2 {
			return false
		}
		for _, p := range parts {
			if p == "" || !(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z' || p[0] >= '0' && p[0] <= '9') {
				return false
			}
			for _, c := range p {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
					return false
				}
			}
		}
		return true
	}
	for _, m := range append(slices.Clone(featured), popularServers()...) {
		if !ok(m.ID) {
			t.Errorf("market id %q is not one the site takes", m.ID)
		}
	}
}
