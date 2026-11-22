package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	minor := 8
	if slices.Contains(modes, "rate-limited") {
		minor = sdk.FullContract().Version.Minor
	}
	manifest := map[string]any{
		"schema": 1, "id": "checkin.fixture", "version": "1.0.0", "runtime": "exec", "entry": entry,
		"contract": map[string]any{"version": map[string]int{"major": 1, "minor": minor},
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
		if _, err := saveExtensionAuth(t.Context(), manager, sdk.Scope{Plugin: "checkin.fixture", Provider: "vendor"},
			sdk.AuthResult{ID: id, Credentials: auth}); err != nil {
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
	marked := pluginCheckinMarks(t.Context(), []SubscriptionQuota{{Provider: id, User: "first"}, {Provider: id, User: "second"}})
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

func TestExtensionCheckinDeadlinePreservesPreviousMark(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	release := filepath.Join(t.TempDir(), "release")
	manager, id, trace := checkinOwnerFixtureWithService(t, started, release, "")
	key := pluginCheckinKey(id, "first")
	now := time.Now()
	previous, err := json.Marshal(map[string]WorkBuddyCheckin{
		key: {Day: wbCheckinDay(now), At: now, Outcome: CheckinInactive, Msg: "earlier confirmed result"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginCheckinPath(), previous, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	results := CheckInAccount(ctx, pluginCheckinBy(id), key)
	if _, err := os.Stat(started); err != nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("deadline did not expire during private startup: %v (%v)", ctx.Err(), err)
	}
	if got := checkinClaims(t, trace); got != 0 {
		t.Fatalf("expired preflight dispatched %d claims", got)
	}
	if len(results) != 0 {
		t.Errorf("expired preflight produced a check-in observation: %+v", results)
	}
	if got, err := os.ReadFile(pluginCheckinPath()); err != nil || !bytes.Equal(got, previous) {
		t.Errorf("expired preflight replaced stored history: %s (%v)", got, err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("released"), 0600); err != nil {
		t.Fatal(err)
	}
	results = CheckInAccount(t.Context(), pluginCheckinBy(id), key)
	if len(results) != 1 || results[0].Outcome != CheckinClaimed || results[0].Operation == nil ||
		results[0].Operation.State != "completed" || checkinClaims(t, trace) != 1 {
		t.Fatalf("expired preflight prevented the next authorized claim: %+v", results)
	}
}

func TestExtensionCheckinUsesStoredAccountWithoutWritingPolicy(t *testing.T) {
	manager, id, trace := checkinOwnerFixture(t)
	if _, err := manager.Store().SaveAccount(extensionhost.Account{
		Plugin: "checkin.fixture", Provider: "vendor", ID: "first", Auth: json.RawMessage(`{"token":"first"}`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	Changed()
	accounts := CheckinAccounts()
	if len(accounts) != 1 || accounts[0].By != pluginCheckinBy(id) {
		t.Fatalf("existing SDK account not discovered: %+v", accounts)
	}
	results := CheckInAccount(t.Context(), accounts[0].By, accounts[0].Key)
	if len(results) != 1 || results[0].Outcome != CheckinClaimed || checkinClaims(t, trace) != 1 {
		t.Fatalf("existing SDK account could not check in: %+v", results)
	}
	if _, err := os.Stat(loginsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check-in wrote routing policy for the account: %v", err)
	}
}

func TestExtensionCheckinTerminalWriteCannotReplay(t *testing.T) {
	for _, mode := range []string{"lost", "invalid", "error", "failed", "rate-limited"} {
		t.Run(mode, func(t *testing.T) {
			fixtureMode := mode
			if mode == "rate-limited" {
				fixtureMode = ""
			}
			manager, _, trace := checkinOwnerFixture(t, fixtureMode)
			if mode == "failed" || mode == "rate-limited" {
				account, err := manager.Store().GetAccount("checkin.fixture", "vendor", "first")
				if err != nil {
					t.Fatal(err)
				}
				account.Auth, _ = json.Marshal(map[string]string{
					"token": "first", "mode": mode, "until": time.Now().Add(-time.Hour).Format(time.RFC3339Nano),
				})
				if _, err := manager.Store().SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
				Changed()
			}
			accounts := CheckinAccounts()
			if len(accounts) != 1 {
				t.Fatalf("SDK account not registered: %+v", accounts)
			}
			run := func() []WorkBuddyCheckin { return CheckInAccount(t.Context(), accounts[0].By, accounts[0].Key) }
			first := run()
			if len(first) != 1 || first[0].Outcome != CheckinFailed || first[0].Operation == nil ||
				first[0].Operation.State == "not-started" {
				t.Fatalf("terminal write result: %+v", first)
			}
			if mode == "failed" && (first[0].Operation.State != "completed" || first[0].ValidUntil.IsZero()) {
				t.Fatalf("legacy failed fixture missed its saved result and expired boundary: %+v", first)
			}
			if mode == "rate-limited" && (first[0].Operation.State != "uncertain" ||
				!strings.Contains(first[0].Msg, "requires SDK API 1.20")) {
				t.Fatalf("older contract accepted the newer result: %+v", first)
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
			if err := os.Remove(pluginCheckinPath()); err != nil {
				t.Fatal(err)
			}
			retained := run()
			if len(retained) != 1 || retained[0].Outcome != CheckinFailed || retained[0].Operation == nil ||
				*retained[0].Operation != *first[0].Operation {
				t.Fatalf("terminal receipt did not survive restart and loss of its display record: %+v", retained)
			}
			if calls := checkinClaims(t, trace); calls != 1 {
				t.Fatalf("terminal write replayed across manual/generation/manager restart: %d", calls)
			}
		})
	}
}

func TestExtensionCheckinRateLimitAppliesToSchedulerAndPress(t *testing.T) {
	manager, id, trace := checkinOwnerFixture(t, "rate-limited")
	account, err := manager.Store().GetAccount("checkin.fixture", "vendor", "first")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	boundary := now.Add(40 * time.Minute).UTC()
	account.Auth, _ = json.Marshal(map[string]string{
		"token": "first", "mode": "rate-limited", "until": boundary.Format(time.RFC3339Nano),
	})
	if _, err := manager.Store().SaveAccount(account, account.Generation); err != nil {
		t.Fatal(err)
	}
	Changed()
	accounts := CheckinAccounts()
	if len(accounts) != 1 {
		t.Fatalf("SDK account not registered: %+v", accounts)
	}
	press := func() []WorkBuddyCheckin { return CheckInAccount(t.Context(), accounts[0].By, accounts[0].Key) }
	first := press()
	if len(first) != 1 || first[0].Outcome != CheckinFailed || first[0].Msg != "fixture vendor pause" ||
		!first[0].ValidUntil.Equal(boundary) || first[0].Operation == nil || first[0].Operation.State != "completed" {
		t.Fatalf("ordinary check-in did not project the completed pause: %+v", first)
	}
	providers := extensionCheckinProviders(t.Context())
	if len(providers) != 1 || providers[0].ID != id {
		t.Fatalf("scheduled check-in did not find its owner: %+v", providers)
	}
	scheduled := wbCheckinLoop{}
	during := scheduled.tick(t.Context(), providers[0].checkiner(func() time.Time { return boundary.Add(-time.Minute) }))
	if len(during) != 1 || during[0].Asked || !during[0].ValidUntil.Equal(boundary) || !scheduled.again.Equal(boundary) {
		t.Fatalf("scheduler did not keep the exact boundary: result=%+v next=%s", during, scheduled.again)
	}
	if err := os.Remove(pluginCheckinPath()); err != nil {
		t.Fatal(err)
	}
	repeated := press()
	if len(repeated) != 1 || repeated[0].Operation == nil || *first[0].Operation != *repeated[0].Operation ||
		!repeated[0].ValidUntil.Equal(boundary) || checkinClaims(t, trace) != 1 {
		t.Fatalf("manual check-in lost the durable pause after display loss: %+v", repeated)
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
	for _, change := range []string{"configuration", "account", "disabled", "removed policy", "unreadable discovery"} {
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
			case "removed policy":
				if err := os.Remove(loginsPath()); err != nil {
					t.Fatal(err)
				}
			case "unreadable discovery":
				damageExtensionDiscoverySettings(t)
			}
			results := captured[0].checkiner(time.Now).checkinNow(t.Context(), true)
			if len(results) != 1 || results[0].Outcome != CheckinFailed || checkinClaims(t, trace) != 0 {
				t.Fatalf("captured %s authority dispatched: %+v", change, results)
			}
		})
	}
}
