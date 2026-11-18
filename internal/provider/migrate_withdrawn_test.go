package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/plugin"
)

// A withdrawn plugin (Antigravity's, owner 11-15) is still installed for
// the users who have it: a move of their own built-in accounts onto it, and
// keepMovedCurrent bringing a moved user's up to its min. Adopt, which signs
// in to it with no account to bring, is a new user's install, and turned
// away.
func TestWithdrawnPluginKeptForItsUsers(t *testing.T) {
	claudeHome(t)
	fakeMover(t)
	const pkg = "@magpie-community/opencode-antigravity-auth"
	movers["fakeco"].pkg, movers["fakeco"].min = pkg, "0.2.1"
	list := plugin.ListPath()
	var said []error
	installPlugin = func(ctx context.Context, p, _ string) error {
		err := plugin.RefuseWithdrawn(ctx, p)
		said = append(said, err)
		return err
	}
	oldProviders, oldRemove := pluginProviders, removePlugin
	pluginProviders = func(context.Context) ([]plugin.Provider, error) { return nil, errors.New("stop here") }
	removePlugin = func(context.Context, string) error { return nil }
	t.Cleanup(func() { pluginProviders, removePlugin = oldProviders, oldRemove })
	ctx := context.Background()

	if err := Adopt(ctx, "fakeco"); err == nil || len(said) != 1 || said[0] == nil {
		t.Fatalf("Adopt with no account = %v (install said %v), want it turned away", err, said)
	}

	loginsMu.Lock()
	if err := writeLogins([]savedLogin{fakeLogin("a@fake", "r-a", true, true)}); err != nil {
		loginsMu.Unlock()
		t.Fatal(err)
	}
	loginsMu.Unlock()
	said = nil
	_ = Move(ctx, "fakeco")
	if len(said) != 1 || said[0] != nil {
		t.Fatalf("a move of the user's own account: install said %v, want it allowed", said)
	}

	said = nil
	if err := setMigration("fakeco", func(m *Migration) { m.State = MovePlugin }); err != nil {
		t.Fatal(err)
	}
	if pluginListed(pkg) {
		t.Fatal("absent maintenance fixture already lists the package")
	}
	keepMovedCurrent(ctx)
	if len(said) != 0 {
		t.Fatalf("a migration record reinstalled an absent withdrawn package: install said %v", said)
	}

	if err := os.MkdirAll(filepath.Dir(list), 0o700); err != nil {
		t.Fatal(err)
	}
	installed := []byte("{\"plugins\":[{\"spec\":\"" + pkg + "@latest\"}]}\n")
	if err := os.WriteFile(list, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := plugin.Load().Plugins
	if len(entries) != 1 || plugin.PackageName(entries[0].Spec) != pkg || entries[0].Off {
		t.Fatalf("maintenance fixture must list one enabled target: %+v", entries)
	}
	keepMovedCurrent(ctx)
	if len(said) != 1 || said[0] != nil {
		t.Fatalf("keepMovedCurrent: install said %v, want it allowed", said)
	}
}
