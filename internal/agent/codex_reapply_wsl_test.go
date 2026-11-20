package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
)

func codexWSLReapply(t *testing.T) (*Agent, func() string, string, string) {
	t.Helper()
	const oldHost, newHost = "172.28.96.1", "172.29.0.1"
	old, next := "http://"+oldHost+":"+gateway.Port(), "http://"+newHost+":"+gateway.Port()
	fakeReach(t, map[string]bool{gateway.URL(): true, next: true})
	wsl.Lock()
	seen := wsl.seen
	wsl.seen = map[string]*distro{"Ubuntu": {Name: "Ubuntu", Gateway: newHost, Was: []string{oldHost}}}
	wsl.Unlock()
	t.Cleanup(func() { wsl.Lock(); wsl.seen = seen; wsl.Unlock() })

	home, read := codexAt(t, old)
	cx := codex(home)
	if drift := driftSettled(t, cx); drift == nil || drift.Kind != "unreachable" || drift.Move != next {
		t.Fatalf("expected WSL address drift, got %+v", drift)
	}
	return cx, read, old, next
}

func TestCodexReapplyWSLPreservesConcurrentEdit(t *testing.T) {
	codex, read, old, next := codexWSLReapply(t)
	native, err := edit.Target(codex.Path)
	if err != nil {
		t.Fatal(err)
	}
	before := read()
	concurrent := strings.NewReplacer(
		old+gateway.CodexPath, "https://user.example/codex",
		fmt.Sprintf("experimental_bearer_token = %q", gateway.Token), `experimental_bearer_token = "dev-token"`,
	).Replace(before) + "\n# a concurrent Codex edit\n"
	injected := false
	err = edit.Filtered(func(path string, proposed []byte) []byte {
		if path == native && !injected && strings.Contains(string(proposed), next+gateway.CodexPath) {
			injected = true
			writeFile(t, path, concurrent)
		}
		return proposed
	}, codex.Reapply)
	if !injected || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("Reapply did not refuse stale configuration: injected=%t, err=%v", injected, err)
	}
	if got := read(); got != concurrent {
		t.Errorf("address migration overwrote the independent URL or key:\n%s", got)
	}
}

func TestCodexReapplyWSLPublishesBothAddressesTogether(t *testing.T) {
	codex, read, old, next := codexWSLReapply(t)
	native, err := edit.Target(codex.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(read(), old, next)
	var writes []string
	err = edit.Filtered(func(path string, proposed []byte) []byte {
		if path == native {
			writes = append(writes, string(proposed))
		}
		return proposed
	}, codex.Reapply)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || writes[0] != want || read() != want {
		t.Fatalf("expected one publication of both addresses, got %d writes:\n%s", len(writes), read())
	}
}

func TestCodexMoveGatewayPreservesUnownedTOML(t *testing.T) {
	const old, next = "http://172.28.96.1:3425", "http://172.29.0.1:3425"
	const foreign = "https://user.example/v1"
	baseURL := old + "/v1"
	for _, test := range []struct {
		name, table string
		move        bool
	}{
		{"explicit-table", fmt.Sprintf("[model_providers.magpie]\nbase_url = %q # table URL\nexperimental_bearer_token = %q\n", baseURL, gateway.Token), true},
		{"quoted-table", fmt.Sprintf("[model_providers.\"magpie\"]\nbase_url = %q # table URL\nexperimental_bearer_token = %q\n", baseURL, gateway.Token), false},
		{"foreign-token", fmt.Sprintf("[model_providers.magpie]\nbase_url = %q\nexperimental_bearer_token = \"user-token\"\n", baseURL), false},
		{"foreign-url", fmt.Sprintf("[model_providers.magpie]\nbase_url = %q\nexperimental_bearer_token = %q\n", foreign, gateway.Token), false},
		{"dotted-table", fmt.Sprintf("model_providers.magpie.base_url = %q\nmodel_providers.magpie.experimental_bearer_token = %q\n", baseURL, gateway.Token), false},
		{"inline-table", fmt.Sprintf("model_providers.magpie = { base_url = %q, experimental_bearer_token = %q }\n", baseURL, gateway.Token), false},
		{"array-table", fmt.Sprintf("[[model_providers.magpie]]\nbase_url = %q\nexperimental_bearer_token = %q\n", baseURL, gateway.Token), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "config.toml")
			root := fmt.Sprintf("  openai_base_url = %q # primary URL\n\n", old+gateway.CodexPath)
			other := fmt.Sprintf("\n[model_providers.other]\nbase_url = %q\n", baseURL)
			windowsBytes := func(text string) string { return "\ufeff" + strings.ReplaceAll(text, "\n", "\r\n") }
			if err := os.WriteFile(config, []byte(windowsBytes(root+test.table+other)), 0o600); err != nil {
				t.Fatal(err)
			}
			table := test.table
			if test.move {
				table = strings.Replace(table, baseURL, next+"/v1", 1)
			}
			want := windowsBytes(strings.Replace(root, old, next, 1) + table + other)
			if err := codexMoveGateway(config, old+"/", next+"/"); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(config); err != nil || string(got) != want {
				t.Errorf("Reapply changed TOML spelling or an unowned value: got %q, want %q, err=%v", got, want, err)
			}
		})
	}
}
