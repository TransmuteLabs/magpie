package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func checkinOwnerFixture(t *testing.T, modes ...string) (*extensionhost.Manager, string, string) {
	t.Helper()
	return checkinOwnerFixtureWithService(t, "", "", modes...)
}

func checkinOwnerFixtureWithService(t *testing.T, started, release string, modes ...string) (*extensionhost.Manager, string, string) {
	t.Helper()
	claudeHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager := extensionhost.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
		if current := extensionhost.Default(); current != manager {
			if err := current.Close(ctx); err != nil {
				t.Error(err)
			}
		}
	})
	directory := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-o", filepath.Join(directory, entry), "../extension/testdata/checkinworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build checkin fixture: %v\n%s", err, output)
	}
	action := map[string]any{"id": "daily.claim", "effect": "write", "inputSchema": map[string]any{"type": "object"}}
	manifest := map[string]any{
		"schema": 1, "id": "checkin.fixture", "version": "1.0.0", "runtime": "exec", "entry": entry,
		"contract": map[string]any{"version": map[string]int{"major": 1, "minor": 8},
			"capabilities": []string{"outbound-executor", "account-auth", "module-rpc"}},
		"providers": []any{map[string]any{"id": "vendor", "name": "Fixture", "protocols": []string{"chat"},
			"checkin": map[string]any{"action": "daily.claim", "recheck": true}}},
		"actions": []any{action},
	}
	if started != "" {
		action["component"] = "helper"
		manifest["services"] = []sdk.ServiceDescriptor{{ID: "helper", Runtime: "exec", Entry: entry,
			Args: []string{"--checkin-helper"}, Contract: sdk.ServiceContract{ID: "checkin.helper", Version: 1},
			Methods: []string{"daily.claim"}, Start: "lazy"}}
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "events.jsonl")
	config := map[string]any{"trace": trace}
	if started != "" {
		config["mode"], config["started"], config["release"] = "hold-service", started, release
	}
	raw, _ = json.Marshal(config)
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), "checkin.fixture", "0", raw); err != nil {
		t.Fatal(err)
	}
	for position, mode := range modes {
		id := []string{"first", "second"}[position]
		auth, _ := json.Marshal(map[string]string{"token": id, "mode": mode, "displayName": "same display name"})
		if _, err := manager.Store().SaveAccount(extensionhost.Account{
			Plugin: "checkin.fixture", Provider: "vendor", ID: id, Auth: auth,
		}, 0); err != nil {
			t.Fatal(err)
		}
	}
	Changed()
	return manager, ExtensionProviderID("checkin.fixture", "vendor"), trace
}

func checkinClaims(t *testing.T, trace string) int {
	t.Helper()
	raw, err := os.ReadFile(trace)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Count(string(raw), `"event":"claim"`)
}

func TestExtensionCheckinUsesOrdinaryOwners(t *testing.T) {
	manager, id, trace := checkinOwnerFixture(t, "", "")
	accounts := CheckinAccounts()
	if len(accounts) != 2 || accounts[0].Key == accounts[1].Key || accounts[0].By != pluginCheckinBy(id) {
		t.Fatalf("ordinary owner did not register exact SDK accounts: %+v", accounts)
	}
	if !HasPluginCheckin() || len(PluginCheckins()) != 1 || PluginCheckins()[0].On {
		t.Fatal("SDK check-in missing from settings or not default-off")
	}
	loop := pluginCheckiner{now: time.Now, only: func(id string) bool { return PluginCheckinOn(settings.Load(), id) }}
	if results := loop.checkinNow(t.Context(), false); len(results) != 0 || manager.Running("checkin.fixture") {
		t.Fatalf("default-off automatic check-in dispatched: %+v", results)
	}
	leaveOut(t, accounts[0].Key)
	results := CheckInAccount(t.Context(), accounts[0].By, accounts[0].Key)
	if len(results) != 1 || results[0].Outcome != CheckinClaimed || results[0].Credit != 12.5 || !results[0].Asked {
		t.Fatalf("ordinary manual SDK check-in: %+v", results)
	}
	CheckInAccount(t.Context(), accounts[0].By, accounts[0].Key)
	if checkinClaims(t, trace) != 1 {
		t.Fatal("manual replayed a completed claim")
	}
	if len(PluginCheckins()[0].Checkins) != 1 || len(settings.Load().CheckinOff) != 1 {
		t.Fatal("ordinary check-in display or account exclusion was lost")
	}
	marked := pluginCheckinMarks([]SubscriptionQuota{{Provider: id, User: "first"}, {Provider: id, User: "second"}})
	if len(marked) != 2 || marked[0].CheckinKey == marked[1].CheckinKey || marked[0].Checkin == nil ||
		marked[0].Checkin.Operation == nil || marked[0].Checkin.Operation.State != "completed" || marked[1].Checkin != nil {
		t.Fatalf("ordinary cards lost exact account receipts: %+v", marked)
	}
	if err := os.Rename(pluginCheckinPath(), pluginCheckinPath()+".preserved"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	results = CheckInAccount(t.Context(), accounts[0].By, accounts[0].Key)
	if len(results) != 1 || results[0].Outcome != CheckinClaimed || checkinClaims(t, trace) != 1 ||
		results[0].Operation == nil || results[0].Operation.Scope.Operation != marked[0].Checkin.Operation.Scope.Operation {
		t.Fatalf("lost display record replayed a durable claim: %+v", results)
	}
}

func TestExtensionCheckinTerminalWriteCannotReplay(t *testing.T) {
	for _, mode := range []string{"lost", "invalid", "error"} {
		t.Run(mode, func(t *testing.T) {
			manager, _, trace := checkinOwnerFixture(t, mode)
			accounts := CheckinAccounts()
			if len(accounts) != 1 {
				t.Fatalf("SDK account not registered: %+v", accounts)
			}
			run := func() []WorkBuddyCheckin { return CheckInAccount(t.Context(), accounts[0].By, accounts[0].Key) }
			if results := run(); len(results) != 1 || results[0].Outcome != CheckinFailed {
				t.Fatalf("terminal write result: %+v", results)
			}
			run()
			account, err := manager.Store().GetAccount("checkin.fixture", "vendor", "first")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Store().SaveAccount(account, account.Generation); err != nil {
				t.Fatal(err)
			}
			Changed()
			if err := manager.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			run()
			if calls := checkinClaims(t, trace); calls != 1 {
				t.Fatalf("terminal write replayed across manual/generation/manager restart: %d", calls)
			}
		})
	}
}

func TestExtensionCheckinRechecksKnownNonclaimsWithDistinctIntents(t *testing.T) {
	for _, mode := range []string{"not-started", "done", "inactive"} {
		t.Run(mode, func(t *testing.T) {
			_, _, trace := checkinOwnerFixture(t, mode)
			account := CheckinAccounts()[0]
			first := CheckInAccount(t.Context(), account.By, account.Key)
			second := CheckInAccount(t.Context(), account.By, account.Key)
			if len(first) != 1 || len(second) != 1 || first[0].Operation == nil || second[0].Operation == nil ||
				first[0].Operation.Scope.Operation == second[0].Operation.Scope.Operation {
				t.Fatalf("ordinary intentional retry reused its execution receipt: %+v %+v", first, second)
			}
			want := 2
			if mode == "not-started" {
				want = 0
				if first[0].Operation.State != "not-started" || second[0].Operation.State != "not-started" {
					t.Fatalf("definite pre-write refusal was lost: %+v %+v", first, second)
				}
			}
			if got := checkinClaims(t, trace); got != want {
				t.Fatalf("known %s recheck: claims=%d want=%d", mode, got, want)
			}
		})
	}
}

func TestExtensionCheckinPreservesSelectedPolicyAndOwnerDay(t *testing.T) {
	manager, id, trace := checkinOwnerFixture(t, "", "")
	setProxies(t, id, "http://127.0.0.1:18880", map[string]string{"first": "direct", "second": "http://127.0.0.1:18881"})
	projections := extensionCheckinProviders(t.Context())
	if len(projections) != 1 {
		t.Fatalf("check-in projection: %+v", projections)
	}
	held := netproxy.Hold(t.Context(), errors.New("fixture network hold"))
	if results := CheckInAccount(held, pluginCheckinBy(id), projections[0].accounts[0].key); len(results) != 1 ||
		results[0].Outcome != CheckinFailed || checkinClaims(t, trace) != 0 || manager.Running("checkin.fixture") {
		t.Fatalf("held check-in dispatched: %+v", results)
	}
	now := time.Date(2029, 3, 4, 16, 15, 0, 0, time.UTC)
	if err := SetOff(id, true); err != nil {
		t.Fatal(err)
	}
	logins, err := readLoginsFile()
	if err != nil {
		t.Fatal(err)
	}
	for index := range logins {
		if logins[index].Agent == id && logins[index].User == "first" {
			logins[index].Paused = true
		}
	}
	if err := writeLogins(logins); err != nil {
		t.Fatal(err)
	}
	runner := projections[0].checkiner(func() time.Time { return now })
	if results := runner.checkinNow(t.Context(), false); len(results) != 0 || checkinClaims(t, trace) != 0 {
		t.Fatalf("automatic check-in ignored provider Off: %+v", results)
	}
	results := runner.checkinNow(t.Context(), true)
	if len(results) != 2 || results[0].Outcome != CheckinClaimed || results[1].Outcome != CheckinClaimed {
		t.Fatalf("manual check-in with provider Off and routing pause: %+v", results)
	}
	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	proxies := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event struct {
			Event   string                    `json:"event"`
			Scope   sdk.Scope                 `json:"scope"`
			Request sdk.AccountCheckinRequest `json:"request"`
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Event != "claim" {
			continue
		}
		if event.Request.Day != wbCheckinDay(now) {
			t.Fatalf("adapter inferred its own day: %+v", event)
		}
		proxies[event.Scope.Account] = event.Request.Proxy
	}
	if len(proxies) != 2 || proxies["first"] != "direct" || proxies["second"] != "http://127.0.0.1:18881" {
		t.Fatalf("selected account proxies changed: %+v", proxies)
	}
}

func TestExtensionCheckinRefusesCapturedReplacedAuthority(t *testing.T) {
	for _, change := range []string{"configuration", "account", "disabled"} {
		t.Run(change, func(t *testing.T) {
			manager, _, trace := checkinOwnerFixture(t, "")
			captured := extensionCheckinProviders(t.Context())
			if len(captured) != 1 {
				t.Fatal("check-in owner missing")
			}
			switch change {
			case "configuration":
				config, err := manager.ModuleConfig(t.Context(), "checkin.fixture")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), "checkin.fixture", config.Version, config.Values); err != nil {
					t.Fatal(err)
				}
			case "account":
				account, err := manager.Store().GetAccount("checkin.fixture", "vendor", "first")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.Store().SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := manager.SetEnabled(t.Context(), "checkin.fixture", false); err != nil {
					t.Fatal(err)
				}
			}
			results := captured[0].checkiner(time.Now).checkinNow(t.Context(), true)
			if len(results) != 1 || results[0].Outcome != CheckinFailed || checkinClaims(t, trace) != 0 {
				t.Fatalf("captured %s authority dispatched: %+v", change, results)
			}
		})
	}
}
