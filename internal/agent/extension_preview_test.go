package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestExtensionAgentPreviewKeepsOwnershipLocksOutOfLinkedConfig(t *testing.T) {
	source, destination := t.TempDir(), filepath.Join(t.TempDir(), "config")
	for _, name := range []string{"sdk-agent-locks", "plugins"} {
		if err := os.MkdirAll(filepath.Join(source, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "sdk-agent-locks", "held.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := []byte(`{"schema":1,"records":{}}`)
	if err := os.WriteFile(filepath.Join(source, "sdk-agent-bindings.json"), ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyTop(source, destination, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "sdk-agent-locks")); !os.IsNotExist(err) {
		t.Fatal("preview inherited another configuration root's ownership locks", err)
	}
	if copied, err := os.ReadFile(filepath.Join(destination, "sdk-agent-bindings.json")); err != nil || string(copied) != string(ledger) {
		t.Fatal("preview discarded durable SDK ownership data with the ephemeral locks", err)
	}
	if runtime.GOOS != "windows" {
		if linked, err := os.Readlink(filepath.Join(destination, "plugins")); err != nil || linked != filepath.Join(source, "plugins") {
			t.Fatal("preview stopped reusing its read-only legacy plugin catalog", linked, err)
		}
	}
}

func TestExtensionAgentPreviewUsesPrivateLocksWithoutChangingLiveOwnership(t *testing.T) {
	home, _ := codexHome(t, "", "model = \"gpt-5.4\"\n")
	client := codex(home)
	if err := client.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	path := client.Path
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lockRoot := filepath.Join(filepath.Dir(extensionBindingPath()), "sdk-agent-locks")
	keys := func() []string {
		entries, err := os.ReadDir(lockRoot)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}
	prior := keys()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	changes, err := DisconnectPreview(client, executable)
	if err != nil || len(changes) == 0 {
		t.Fatal("ordinary file-agent preview could not acquire private sandbox locks", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) || !slices.Equal(prior, keys()) {
		t.Fatal("preview changed live config or published its sandbox lock keys", err)
	}
}
