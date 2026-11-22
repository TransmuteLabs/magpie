package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

type allowanceOwnerReadHost struct {
	*extensionhost.Manager
	reads atomic.Int32
}

func (host *allowanceOwnerReadHost) Store() *extensionhost.Store {
	host.reads.Add(1)
	return host.Manager.Store()
}

func TestWarmCommunityAllowanceReadsOwnerOnce(t *testing.T) {
	for _, count := range []int{1, 10, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			host := &allowanceOwnerReadHost{Manager: discoveryCatalogManager(t)}
			providerExtensionHost = func() extensionProviderHost { return host }
			startWithNoAllowancesRead(t)
			const agent = "allowance-owner-fixture"
			seedCommunityAllowance(t, agent, count)
			ownersPath := filepath.Join(host.Namespace(), "private", ".saved-signins", "subscription-owners.json")
			writeFile(t, ownersPath, []extensionhost.SubscriptionOwner{})

			allowances := Allowances(agent)
			if user := allowances["user-0"]; len(allowances) != count || len(user) != 1 || user[0].Used != 42 {
				t.Fatalf("warm Allowances did not keep %d valid readings: %+v", count, allowances)
			}
			if got := host.reads.Swap(0); got != 1 {
				t.Errorf("warm Allowances made %d owner snapshot reads, want 1", got)
			}
			if at, failed, ok := UsageRead(agent, "user-0"); at.IsZero() || failed || !ok {
				t.Fatalf("warm UsageRead did not keep the valid reading: %v %t %t", at, failed, ok)
			}
			if got := host.reads.Load(); got != 1 {
				t.Errorf("warm UsageRead made %d owner snapshot reads, want 1", got)
			}
		})
	}
}

func TestWarmCommunityAllowanceChecksCurrentAuthority(t *testing.T) {
	const agent = "allowance-owner-fixture"
	for _, test := range []struct {
		name       string
		change     func(*testing.T, string)
		wantOthers int
	}{
		{"reauth", func(t *testing.T, _ string) {
			writeFile(t, plugin.AuthPath(), map[string]map[string]string{
				agent + "#0": {"type": "api", "_magpieAuthEpoch": "second"},
				agent + "#1": {"type": "api", "_magpieAuthEpoch": "first"},
			})
		}, 1},
		{"discovery off", func(t *testing.T, _ string) {
			set := settings.Load()
			set.NoAccountDiscovery = true
			if err := settings.Save(set); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"unreadable owners", func(t *testing.T, root string) {
			dir := filepath.Join(root, "private", ".saved-signins")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "subscription-owners.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := discoveryCatalogManager(t)
			startWithNoAllowancesRead(t)
			seedCommunityAllowance(t, agent, 2)
			before := Allowances(agent)
			if len(before) != 2 {
				t.Fatalf("initial allowances = %+v", before)
			}
			test.change(t, manager.Namespace())
			if at, _, ok := UsageRead(agent, "user-0"); ok || !at.IsZero() {
				t.Errorf("UsageRead retained a revoked reading: %v", at)
			}
			after := Allowances(agent)
			if len(after) != test.wantOthers || len(after["user-0"]) != 0 {
				t.Errorf("Allowances after change = %+v", after)
			}
			if len(before) != 2 {
				t.Errorf("previously returned allowances were mutated: %+v", before)
			}
		})
	}
}

func TestWarmCommunityAllowanceChecksCurrentPublicSDKOwner(t *testing.T) {
	for _, usage := range []bool{false, true} {
		name := "Allowances"
		if usage {
			name = "UsageRead"
		}
		t.Run(name, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			startWithNoAllowancesRead(t)
			const agent = "allowance-owner-fixture"
			seedCommunityAllowance(t, agent, 2)
			defer Hold()()
			if SDKSubscriptionOwns(agent) {
				t.Fatal("fixture started with an SDK owner")
			}
			usageTestSubscriptionOwner(t, host, agent)
			if usage {
				if at, _, ok := UsageRead(agent, "user-0"); ok || !at.IsZero() {
					t.Errorf("cutover retained a Community reading: %v", at)
				}
			} else if after := Allowances(agent); len(after) != 0 {
				t.Errorf("cutover retained Community allowances: %+v", after)
			}
			usedCache.Lock()
			binding := usedCache.extensions[agent]["alice"]
			usedCache.Unlock()
			if binding == "" {
				t.Error("routing did not select the current SDK account under the transferred public alias")
			}
		})
	}
}

func seedCommunityAllowance(t *testing.T, agent string, count int) {
	t.Helper()
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	now := time.Now()
	saved := make([]savedLogin, 0, count)
	auth := make(map[string]map[string]string, count)
	bindings := make(map[string]communityAllowanceBinding, count)
	allowances := make(map[string]Allowance, count)
	for index := range count {
		user, slot := fmt.Sprintf("user-%d", index), fmt.Sprintf("%s#%d", agent, index)
		login := savedLogin{Agent: agent, User: user, Home: slot, On: true}
		saved = append(saved, login)
		auth[slot] = map[string]string{"type": "api", "_magpieAuthEpoch": "first"}
		selected := pluginUsageSelection{Agent: agent, User: user, Slot: slot, LoginID: loginID(login), AuthEpoch: "first"}
		bindings[user] = communityAllowanceBinding{provider: agent, selected: selected}
		allowances[user] = Allowance{{Used: 42}}
	}
	saveLogins(t, saved...)
	writeFile(t, plugin.AuthPath(), auth)
	usedCache.Lock()
	if usedCache.community == nil {
		usedCache.community = map[string]map[string]communityAllowanceBinding{}
	}
	usedCache.community[agent] = bindings
	usedCache.m[agent], usedCache.at[agent] = allowances, now
	usedCache.loading[agent] = make(chan struct{})
	if usedCache.read == nil {
		usedCache.read = map[string]readState{}
	}
	usedCache.read[agent+"/user-0"] = readState{at: now}
	usedCache.Unlock()
	t.Cleanup(func() {
		usedCache.Lock()
		delete(usedCache.community, agent)
		delete(usedCache.extensions, agent)
		delete(usedCache.read, agent+"/user-0")
		usedCache.Unlock()
	})
}
