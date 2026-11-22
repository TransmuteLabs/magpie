package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/testenv"
)

type communityQuotaFixture struct {
	logins   []savedLogin
	auth     map[string]map[string]string
	quotas   []SubscriptionQuota
	bindings map[string]communityAllowanceBinding
}

func newCommunityQuotaFixture(tb testing.TB, count int) communityQuotaFixture {
	tb.Helper()
	testenv.SetHome(tb, tb.TempDir())
	tb.Setenv("MAGPIE_NO_ACCOUNTS", "")
	tb.Setenv("XDG_CONFIG_HOME", tb.TempDir())
	manager := extensionhost.NewManager(tb.TempDir())
	previous := providerExtensionHost
	providerExtensionHost = func() extensionProviderHost { return manager }
	tb.Cleanup(func() {
		providerExtensionHost = previous
		if err := manager.Close(context.Background()); err != nil {
			tb.Error(err)
		}
	})
	const agent = "allowance-owner-fixture"
	fixture := communityQuotaFixture{
		logins:   make([]savedLogin, count),
		auth:     make(map[string]map[string]string, count),
		quotas:   make([]SubscriptionQuota, count),
		bindings: make(map[string]communityAllowanceBinding, count),
	}
	for index := range count {
		user := fmt.Sprintf("user-%d", index)
		slot := fmt.Sprintf("%s#%d", agent, index)
		login := savedLogin{Agent: agent, User: user, Home: slot, On: true}
		login.ID = loginID(login)
		fixture.logins[index] = login
		fixture.auth[slot] = map[string]string{"type": "api", "_magpieAuthEpoch": "first"}
		selected := pluginUsageSelection{Agent: agent, User: user, Slot: slot, LoginID: login.ID, AuthEpoch: "first"}
		fixture.quotas[index] = SubscriptionQuota{Provider: agent, User: user, pluginUsage: &selected}
		fixture.bindings[user] = communityAllowanceBinding{provider: agent, selected: selected}
	}
	writeCommunityQuotaFixture(tb, loginsPath(), fixture.logins)
	writeCommunityQuotaFixture(tb, plugin.AuthPath(), fixture.auth)
	return fixture
}

func writeCommunityQuotaFixture(tb testing.TB, path string, value any) {
	tb.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		tb.Fatal(err)
	}
}

func TestCommunityQuotaLargeBatchMatchesCurrentLogin(t *testing.T) {
	fixture := newCommunityQuotaFixture(t, 40)
	original := fixture.logins[0]
	check := func(t *testing.T, wantTarget bool) {
		t.Helper()
		current := subscriptionQuotaCheck(t.Context(), fixture.quotas...)
		owners := extensionSubscriptionOwners{}
		bindings := currentCommunityAllowanceBindings(t.Context(), fixture.bindings, &owners)
		for index, quota := range fixture.quotas {
			want := index != 0 || wantTarget
			if got := current(quota); got != want {
				t.Errorf("quota for %s kept = %t, want %t", quota.User, got, want)
			}
			if _, got := bindings[quota.User]; got != want {
				t.Errorf("routing binding for %s kept = %t, want %t", quota.User, got, want)
			}
		}
	}
	for _, test := range []struct {
		name   string
		change func(*savedLogin)
	}{
		{"agent", func(login *savedLogin) { login.Agent = "other" }},
		{"stable ID", func(login *savedLogin) { login.ID = "replaced" }},
		{"login epoch", func(login *savedLogin) { login.PluginObservations.Epoch = "replaced" }},
		{"user", func(login *savedLogin) { login.User = "other" }},
		{"hidden", func(login *savedLogin) { login.Hidden = "removed" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			conflict := original
			test.change(&conflict)
			writeCommunityQuotaFixture(t, loginsPath(), append([]savedLogin{conflict}, fixture.logins...))
			check(t, true)
			writeCommunityQuotaFixture(t, loginsPath(), append([]savedLogin{conflict, conflict}, fixture.logins[1:]...))
			check(t, false)
			writeCommunityQuotaFixture(t, loginsPath(), append([]savedLogin{conflict}, fixture.logins[1:]...))
			check(t, false)
		})
	}
	t.Run("missing login slot", func(t *testing.T) {
		writeCommunityQuotaFixture(t, loginsPath(), fixture.logins[1:])
		check(t, false)
	})
	writeCommunityQuotaFixture(t, loginsPath(), fixture.logins)
	t.Run("replaced auth epoch", func(t *testing.T) {
		fixture.auth[original.Home]["_magpieAuthEpoch"] = "second"
		writeCommunityQuotaFixture(t, plugin.AuthPath(), fixture.auth)
		check(t, false)
	})
	t.Run("missing auth slot", func(t *testing.T) {
		delete(fixture.auth, original.Home)
		writeCommunityQuotaFixture(t, plugin.AuthPath(), fixture.auth)
		check(t, false)
	})
	t.Run("legacy ID and normalized user", func(t *testing.T) {
		legacy := original
		legacy.ID, legacy.User = "", " USER-0 "
		fixture.auth[original.Home] = map[string]string{"type": "api", "_magpieAuthEpoch": "first"}
		writeCommunityQuotaFixture(t, plugin.AuthPath(), fixture.auth)
		writeCommunityQuotaFixture(t, loginsPath(), append([]savedLogin{legacy}, fixture.logins[1:]...))
		check(t, true)
	})
}
