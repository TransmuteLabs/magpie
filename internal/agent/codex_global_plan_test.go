package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
)

func TestCodexGlobalConnectAndSyncKeepProviderChoices(t *testing.T) {
	const project = "[projects.\"/work\"]\ntrust_level = \"trusted\"\n"
	const config = "model = \"gpt-5.5\"\nmodel_reasoning_effort = \"xhigh\"\n\n" +
		"[ model_providers . magpie ]\n" +
		"base_url = \"http://127.0.0.1:1/v1\"\n" +
		"requires_openai_auth = \"false\"\n" +
		"capabilities = { remote_compaction = \"unsupported\" }\n\n" +
		"[model_providers.magpie.http_headers]\n" +
		"x-custom = \"kept\"\n\n" + project
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, config)
	cx := codex(home)
	checkProvider := func() {
		t.Helper()
		data := read()
		for _, wanted := range []string{
			`wire_api = "responses"`, `"x-openai-actor-authorization" = "magpie"`,
			`x-custom = "kept"`, project,
		} {
			if !strings.Contains(data, wanted) {
				t.Errorf("missing %q in config:\n%s", wanted, data)
			}
		}
		if strings.Contains(data, `base_url = "http://127.0.0.1:1/v1"`) {
			t.Errorf("obsolete provider URL in config:\n%s", data)
		}
		if _, capability := asyncOf(t, data); capability != "unsupported" {
			t.Errorf("user capability was changed: %v", capability)
		}
	}
	if err := os.Chmod(cx.Path, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(cx.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cx.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	checkProvider()
	for _, wanted := range []string{`model = "gpt-5.5"`, `model_reasoning_effort = "xhigh"`, `requires_openai_auth = false`} {
		if data := read(); !strings.Contains(data, wanted) {
			t.Errorf("Connect changed %q:\n%s", wanted, data)
		}
	}
	if err := edit.SetTOMLKey(cx.Path, "model_providers.magpie", "requires_openai_auth", true); err != nil {
		t.Fatal(err)
	}
	codexUsedUp = func() bool { return true }
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if data := read(); strings.Contains(data, "requires_openai_auth") {
		t.Fatalf("held account retained native auth:\n%s", data)
	}
	codexUsedUp = func() bool { return false }
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	table, err := edit.GetTOMLTable(cx.Path, "model_providers.magpie")
	if err != nil || table["requires_openai_auth"] != "true" {
		t.Fatalf("available account did not regain native auth: %v, %v", table, err)
	}
	checkProvider()
	if info, err := os.Stat(cx.Path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("config mode = %o, want %o", info.Mode().Perm(), before.Mode().Perm())
	}
}
