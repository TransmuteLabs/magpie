package extension

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestModelCatalogPublicationComparesCurrentSelection(t *testing.T) {
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager, installed := fixtureManager(t)
	peer := NewManager(manager.Namespace())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := peer.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx := WithSubscriptionOwnerRevision(t.Context(), "")
	entry, config, err := manager.ActiveModuleConfig(ctx, installed.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := sdk.Scope{Plugin: entry.Manifest.ID}
	account, err := manager.Store().SaveAccount(Account{
		Plugin: state.Plugin, Provider: "provider", ID: "reader", Auth: json.RawMessage(`{"key":"fixture"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation}
	key := ModelCatalogPrefix + "fixture"
	original := json.RawMessage(`{"catalog":"original"}`)
	newer := json.RawMessage(`{"catalog":"newer"}`)
	root, err := manager.PublishModelCatalog(ctx, entry, config.Version, scope, key, 0, original)
	if err != nil || root.Version != 1 {
		t.Fatalf("initial root: %+v, %v", root, err)
	}
	if root, err := manager.PublishModelCatalog(ctx, entry, config.Version, scope, key, root.Version, original); err != nil || root.Version != 1 {
		t.Fatalf("unchanged result rewrote the root: %+v, %v", root, err)
	}
	winner, err := peer.PublishModelCatalog(ctx, entry, config.Version, scope, key, root.Version, newer)
	if err != nil || winner.Version != 2 {
		t.Fatalf("peer root: %+v, %v", winner, err)
	}
	if _, err := manager.PublishModelCatalog(ctx, entry, config.Version, scope, key, root.Version, original); err == nil {
		t.Fatal("stale identical response ignored the winning root")
	}
	if err := peer.SetEnabled(ctx, state.Plugin, false); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetEnabled(ctx, state.Plugin, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PublishModelCatalog(ctx, entry, config.Version, scope, key, winner.Version, original); err == nil {
		t.Fatal("old refresh published after the same package was re-enabled")
	}
	current, err := manager.Store().StateGetContext(ctx, state, key)
	if err != nil || current.Version != winner.Version || string(current.Value) != string(newer) {
		t.Fatalf("a rejected publication changed the durable root: %+v, %v", current, err)
	}
	entry, config, err = manager.ActiveModuleConfig(ctx, state.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	if account, err = peer.Store().SaveAccount(account, scope.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PublishModelCatalog(ctx, entry, config.Version, scope, key, current.Version, original); err == nil {
		t.Fatal("old account generation published a model root")
	}
	scope.Generation = account.Generation
	if current, err = manager.PublishModelCatalog(ctx, entry, config.Version, scope, key, current.Version, original); err != nil ||
		current.Version != winner.Version+1 || string(current.Value) != string(original) {
		t.Fatalf("current selection could not publish: %+v, %v", current, err)
	}
}

func TestProviderPublicationKeepsCommittedResultAfterCancellation(t *testing.T) {
	manager, installed := fixtureManager(t)
	ctx, cancel := context.WithCancel(WithSubscriptionOwnerRevision(t.Context(), ""))
	defer cancel()
	entry, config, err := manager.ActiveModuleConfig(ctx, installed.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: entry.Manifest.ID}
	key := ModelCatalogPrefix + "canceled"
	value := json.RawMessage(`{"published":true}`)
	var written VersionedValue
	err = manager.withProviderPublication(ctx, entry, config.Version, scope, func(root *os.Root) error {
		var err error
		written, err = compareStoreState(ctx, root, scope, key, 0, value, true)
		cancel()
		return err
	})
	current, readErr := manager.Store().StateGetContext(t.Context(), scope, key)
	if err != nil || ctx.Err() == nil || written.Version != 1 || readErr != nil ||
		current.Version != written.Version || string(current.Value) != string(value) {
		t.Fatalf("committed root reported an unsuccessful publication: written=%+v err=%v durable=%+v err=%v",
			written, err, current, readErr)
	}
}

func TestModelCatalogPublicationChecksCurrentAccountDiscovery(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager, installed := fixtureManager(t)
	ctx := WithSubscriptionOwnerRevision(t.Context(), "")
	entry, config, err := manager.ActiveModuleConfig(ctx, installed.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := sdk.Scope{Plugin: entry.Manifest.ID}
	account, err := manager.Store().SaveAccount(Account{
		Plugin: state.Plugin, Provider: "provider", ID: "reader", Auth: json.RawMessage(`{"key":"fixture"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation}
	original, late := json.RawMessage(`{"catalog":"original"}`), json.RawMessage(`{"catalog":"late"}`)
	const allowed = `{"noAccountDiscovery":false}`
	for _, test := range []struct {
		name, policy string
		sameStamp    bool
		backup       bool
	}{
		{name: "disabled", policy: `{"noAccountDiscovery":true}`},
		{name: "same-stamp-replacement", policy: `{"noAccountDiscovery":true }`, sameStamp: true},
		{name: "corrupt-with-backup", policy: `{broken`, backup: true},
		{name: "blank", policy: " \n\t"},
		{name: "null", policy: `null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			key := ModelCatalogPrefix + test.name
			active := settings.Path()
			catalogTestWrite(t, active, []byte(allowed), 0o600)
			stamp := time.Now().Add(-time.Minute).Truncate(time.Second)
			if err := os.Chtimes(active, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			defer filememo.Hold()()
			if settings.AccountDiscoveryOff() {
				t.Fatal("fixture started with account discovery off")
			}
			root, err := manager.PublishModelCatalog(ctx, entry, config.Version, scope, key, 0, original)
			if err != nil {
				t.Fatal(err)
			}
			if test.backup {
				catalogTestWrite(t, lastgood.Bak(active), []byte(allowed), 0o600)
			}
			replacement := filepath.Join(filepath.Dir(active), "external-settings.json")
			catalogTestWrite(t, replacement, []byte(test.policy), 0o600)
			if test.sameStamp {
				if len(test.policy) != len(allowed) {
					t.Fatal("replacement settings differ in size")
				}
				if err := os.Chtimes(replacement, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(replacement, active); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.PublishModelCatalog(ctx, entry, config.Version, scope, key, root.Version, late); err == nil {
				t.Error("late discovery published under stale or unreadable settings")
			}
			current, err := manager.Store().StateGetContext(t.Context(), state, key)
			if err != nil || current.Version != root.Version || string(current.Value) != string(original) {
				t.Fatalf("disallowed refresh replaced the durable root: %+v, %v", current, err)
			}
			if _, err := manager.PublishModelCatalog(ctx, entry, config.Version, state, key, root.Version, late); err != nil {
				t.Fatalf("account discovery policy denied public models: %v", err)
			}
		})
	}
}
