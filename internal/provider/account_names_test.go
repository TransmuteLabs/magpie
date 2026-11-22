package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestEmptyQuotaNamesDoNotWaitForSDKStore(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	unlock, locked := filelock.Lock(filepath.Join(host.Namespace(), "private", ".lock"), time.Second)
	if !locked {
		t.Fatal("could not lock the SDK Store")
	}
	synctest.Test(t, func(t *testing.T) {
		done := make(chan []SubscriptionQuota, 1)
		go func() { done <- withAccountNames(nil) }()
		synctest.Wait()
		select {
		case cards := <-done:
			unlock()
			if len(cards) != 0 {
				t.Errorf("naming an empty snapshot published %d cards", len(cards))
			}
		default:
			unlock()
			<-done
			t.Error("naming an empty snapshot waited for the SDK Store")
		}
	})
}

// #1515: a plugin's signed-in account (ZCode's, through
// opencode-zcode-auth) takes a name of the user's own. It is magpie's,
// in providers.json: plugin-auth.json and logins.json are not written.
func TestAccountNameOfAPluginAccount(t *testing.T) {
	bun := testenv.Bun(t)
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("../plugin/testdata/fake/index.js")
	if _, err := plugin.Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	k, err := plugin.Import(ctx, "fakeco", map[string]any{"type": "oauth", "refresh": "r", "access": "a", "expires": 9e15, "accountId": "31****36@qq.com"})
	if err != nil {
		t.Fatal(err)
	}
	saveLogins(t, savedLogin{Agent: "plugin:fakeco", User: "31****36@qq.com", Home: k, On: true})
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	id := PluginID("fakeco")
	read := func(p string) []byte { b, _ := os.ReadFile(p); return b }
	auth, logins := read(plugin.AuthPath()), read(loginsPath())
	if len(auth) == 0 || len(logins) == 0 {
		t.Fatalf("no sign-in to keep: auth %d bytes, logins %d", len(auth), len(logins))
	}

	if err := SetAccountName(id, "31****36@QQ.com", "  某某·GLM\n高级版 "); err != nil {
		t.Fatal(err)
	}
	if got := AccountNameOf(id, "31****36@qq.com"); got != "某某·GLM 高级版" {
		t.Fatalf("name %q", got)
	}
	if !bytes.Equal(read(plugin.AuthPath()), auth) || !bytes.Equal(read(loginsPath()), logins) {
		t.Fatal("naming an account wrote to its sign-in")
	}
	// on its card, as every surface reads it
	qs := withAccountNames([]SubscriptionQuota{{Provider: id, User: "31****36@qq.com"}})
	if qs[0].Alias != "某某·GLM 高级版" || !qs[0].Named {
		t.Fatalf("card alias %q, the user's own %v", qs[0].Alias, qs[0].Named)
	}
	// and on what the vendor's name is kept by: the Usage page's Accounts,
	// a gateway key's accounts, Routing's pools (magpie-community/plugins#310:
	// a renamed Kiro account read by its old name on every request after)
	if got := AccountShownOf(id, "31****36@qq.com"); got != "某某·GLM 高级版" {
		t.Fatalf("shown as %q", got)
	}
	if got := AccountShownOf(id, "nobody@qq.com"); got != "nobody@qq.com" {
		t.Fatalf("an unnamed account shown as %q", got)
	}
	var shown []string
	for _, v := range AccountNames() {
		if strings.HasPrefix(v, id+"/") {
			shown = append(shown, v)
		}
	}
	if len(shown) != 1 || shown[0] != id+"/某某·GLM 高级版" {
		t.Fatalf("a gateway key's accounts read %q", shown)
	}
	for _, bad := range []struct{ ref, name string }{
		{"nobody@qq.com", "x"},
		{"31****36@qq.com", strings.Repeat("名", MaxAccountName+1)},
	} {
		if err := SetAccountName(id, bad.ref, bad.name); err == nil {
			t.Fatalf("%q named %q", bad.ref, bad.name)
		}
	}
	// a key provider's keys are named in their own rows
	if err := Save(Provider{ID: "zhipu-team", Name: "某某·GLM高级版", Chat: "https://open.bigmodel.cn/api/coding/paas/v4", Key: "k", KeyName: "张三"}); err != nil {
		t.Fatal(err)
	}
	if err := SetAccountName("zhipu-team", "张三", "x"); err == nil {
		t.Fatal("a key provider took an account's name")
	}
	if err := SetAccountName(id, "31****36@qq.com", ""); err != nil {
		t.Fatal(err)
	}
	if got := AccountNameOf(id, "31****36@qq.com"); got != "" {
		t.Fatalf("cleared name %q", got)
	}
}

func TestUsageAccountNameFollowsSubscriptionOwner(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	const providerID, user, sdkID = "devin", "existing-account", "sdk-account"
	const communityName, sdkName, alias = "Community account", "SDK account", "Work"
	host.manifest.Providers[0].ID = providerID
	host.manifest.Contract.Capabilities = append(host.manifest.Contract.Capabilities, sdk.AccountAuth)
	if _, err := host.store.SaveAccount(extensionhost.Account{Plugin: host.manifest.ID, Provider: providerID, ID: sdkID,
		Name: sdkName, Auth: json.RawMessage(`{}`)}, 0); err != nil {
		t.Fatal(err)
	}
	const spec = "@magpie-community/opencode-devin-auth"
	if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings.Dir(), "plugins.json"), []byte(`{"plugins":[{"spec":"`+spec+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin.UseCached([]plugin.Provider{{ID: providerID, Spec: spec, Name: "Community Devin"}})
	t.Cleanup(func() { plugin.UseCached(nil) })
	loginUsageCache.Lock()
	previous := loginUsageCache.m
	loginUsageCache.m = map[string]loginUsageEntry{
		"plugin:" + providerID + "/" + user: {q: SubscriptionQuota{User: communityName}},
	}
	loginUsageCache.Unlock()
	t.Cleanup(func() {
		loginUsageCache.Lock()
		loginUsageCache.m = previous
		loginUsageCache.Unlock()
	})

	dir := filepath.Join(host.namespace, "private", ".saved-signins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := struct {
		extensionhost.SubscriptionOwner
		GrantRevision string            `json:"grantRevision"`
		Digests       map[string]string `json:"digests"`
		Tickets       []string          `json:"tickets"`
	}{
		SubscriptionOwner: extensionhost.SubscriptionOwner{
			Module: host.manifest.ID, Provider: providerID, PublicID: providerID, Revision: "fixture-owner",
			Accounts: []extensionhost.SubscriptionAccount{{Key: "saved-key", ID: sdkID, User: user, LoginID: "saved-login"}},
		},
		GrantRevision: strings.Repeat("a", 64),
		Digests:       map[string]string{"saved-key": strings.Repeat("b", 64)},
		Tickets:       []string{"fixture-ticket"},
	}
	setOwner := func(active bool) {
		t.Helper()
		owner.Active = active
		body, err := json.Marshal([]any{owner})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "subscription-owners.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
		Changed()
	}
	check := func(want string, named bool) {
		t.Helper()
		rows := []SubscriptionQuota{{Provider: providerID, User: user, Seat: "Plan seat"}}
		card := withAccountNames(rows)[0]
		if card.Provider != providerID || card.User != user || card.Alias != want || card.Named != named || rows[0].Alias != "" {
			t.Fatalf("want Usage label %q (user alias: %t), got %+v", want, named, card)
		}
	}
	check(communityName, false)
	setOwner(true)
	check(sdkName, false)
	if err := SetAccountName(providerID, user, alias); err != nil {
		t.Fatal(err)
	}
	check(alias, true)
	if err := SetAccountName(providerID, user, ""); err != nil {
		t.Fatal(err)
	}
	setOwner(false)
	check(communityName, false)
}
