package provider

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/steady"
)

func TestPluginUsageRejectsPeerCredentialCommitWithoutLoginWrite(t *testing.T) {
	const peerSlot = "MAGPIE_TEST_PLUGIN_REAUTH_SLOT"
	if slot := os.Getenv(peerSlot); slot != "" {
		t.Cleanup(stopPluginHost)
		t.Cleanup(plugin.Settle)
		currentSync := steady.Sync
		t.Cleanup(func() { steady.Sync = currentSync })
		failedLoginSync := false
		path := loginsPath()
		steady.Sync = func(file *os.File) error {
			if stagedWriteFor(file, path) {
				failedLoginSync = true
				return errors.New("fixture refused final login metadata sync")
			}
			return currentSync(file)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
		defer cancel()
		saved, err := plugin.Import(ctx, "fakeco", map[string]any{
			"type": "oauth", "refresh": "signed-in-again", "access": "replacement", "expires": 9e15, "accountId": "usage@fake",
		})
		if err != nil || saved != slot {
			t.Fatalf("replacement sign-in did not reuse the selected slot: %v", err)
		}
		PluginSignedIn(plugin.Saved{Provider: "fakeco", Account: saved})
		if !failedLoginSync {
			t.Fatal("fixture never reached the metadata write after committing credentials")
		}
		return
	}

	for _, name := range []string{"warm", "late"} {
		t.Run(name, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			reply, seeded := startWalletEditorRead(t, ctx, login.Agent, requests)
			reply <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","balance":"$100","windows":[{"name":"Funds","balance":100},{"name":"Reserve","balance":50}]}`}
			checkWalletUsage(t, readWalletEditorResult(t, seeded)[login.User], 100, 50, 0, 0)
			selected := loginOf(t, readLogins(), login.Agent, login.User)
			beforeLogins, err := os.ReadFile(loginsPath())
			if err != nil {
				t.Fatal(err)
			}
			beforeAuth, err := os.ReadFile(plugin.AuthPath())
			if err != nil {
				t.Fatal(err)
			}
			beforeWallet, err := os.ReadFile(walletSpendPath())
			if err != nil {
				t.Fatal(err)
			}
			var late <-chan walletEditorResult
			if name == "late" {
				AskUsage()
				reply, late = startWalletEditorRead(t, ctx, login.Agent, requests)
			}
			keep := filememo.Hold()
			defer keep()
			if _, err := plugin.CurrentAuthEpochs(ctx); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := proc.CommandContext(ctx, executable, "-test.run=^TestPluginUsageRejectsPeerCredentialCommitWithoutLoginWrite$")
			cmd.Env = append(os.Environ(), peerSlot+"="+selected.Home)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("peer sign-in: %v\n%s", err, output)
			}
			if after, err := os.ReadFile(plugin.AuthPath()); err != nil || bytes.Equal(after, beforeAuth) {
				t.Fatalf("peer did not commit replacement credentials: %v", err)
			}
			if after, err := os.ReadFile(loginsPath()); err != nil || !bytes.Equal(after, beforeLogins) {
				t.Fatalf("fixture did not preserve original login metadata: %v", err)
			}
			var quotas map[string]SubscriptionQuota
			if late != nil {
				reply <- pluginUsageReply{http.StatusOK, `{"plan":"previous sign-in","signIn":"expired","balance":"$80","windows":[{"name":"Funds","balance":80}]}`}
				quotas = readWalletEditorResult(t, late)
			} else {
				cards := CachedCardsContext(ctx, time.Now())
				card := cardOf(cards, loginProvider(login), login.User)
				if card == nil {
					t.Fatal("cached cards omitted the selected account")
				}
				quotas = map[string]SubscriptionQuota{login.User: *card}
			}
			checkDeferredWallet(t, login, quotas[login.User], errPluginUsageAccountChanged.Error())
			if after, err := os.ReadFile(loginsPath()); err != nil || !bytes.Equal(after, beforeLogins) {
				t.Errorf("the superseded reply changed login metadata: %v", err)
			}
			if after, err := os.ReadFile(walletSpendPath()); err != nil || !bytes.Equal(after, beforeWallet) {
				t.Errorf("the superseded reply changed wallet spend: %v", err)
			}
			keep()
			if _, err := plugin.Providers(ctx); err != nil {
				t.Fatal(err)
			}
			Changed()
			AskUsage()
			reply, current := startWalletEditorRead(t, ctx, login.Agent, requests)
			reply <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","balance":"$100","windows":[{"name":"Funds","balance":100},{"name":"Reserve","balance":50}]}`}
			checkWalletUsage(t, readWalletEditorResult(t, current)[login.User], 100, 50, 0, 0)
		})
	}
}

func TestPluginSignInCompletionUsesCommittedCredentials(t *testing.T) {
	login, _ := startPluginUsageFixture(t)
	row := loginOf(t, readLogins(), login.Agent, login.User)
	old, err := plugin.APIKey(t.Context(), "fakeco", 0, nil, "first-fixture-key", row.Home)
	if err != nil {
		t.Fatal(err)
	}
	current, err := plugin.APIKey(t.Context(), "fakeco", 0, nil, "second-fixture-key", row.Home)
	if err != nil {
		t.Fatal(err)
	}
	if old.Account != row.Home || current.Account != row.Home || old.AuthEpoch == "" || current.AuthEpoch == old.AuthEpoch {
		t.Fatal("the two actual sign-ins did not update the selected credential slot")
	}
	row.Lapsed, row.Hidden = "current sign-in has expired", "fixture-hidden"
	saveLogins(t, row)
	clearPluginLapse(old)
	if got := loginOf(t, readLogins(), login.Agent, login.User); got.Lapsed != row.Lapsed || got.Hidden != row.Hidden {
		t.Fatalf("older sign-in changed the current login: lapse=%q hidden=%q", got.Lapsed, got.Hidden)
	}
	clearPluginLapse(current)
	if got := loginOf(t, readLogins(), login.Agent, login.User); got.Lapsed != "" || got.Hidden != "" {
		t.Fatalf("current sign-in did not restore the account: lapse=%q hidden=%q", got.Lapsed, got.Hidden)
	}
}
