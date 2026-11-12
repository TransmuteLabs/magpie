package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/steady"
)

// replaceProviderFixture deliberately replaces a whole test fixture while
// preserving the guarded store's snapshot of the actual current source.
func replaceProviderFixture(desired file) error {
	current, err := read()
	if err != nil {
		return err
	}
	desired.source = current.source
	return store(desired)
}

func zcodeScopedAuth(t *testing.T, name, kind string) map[string]any {
	t.Helper()
	state := map[string]any{"version": 1, "site": "zai", "uid": "user-1", "kind": kind}
	if kind == "team" {
		state["org"], state["project"], state["product"] = "org", "project", "product"
	}
	refresh, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"type": "oauth", "accountId": name, "uid": "user-1", "refresh": string(refresh), "accountEpoch": "epoch-1"}
}

func TestZCodeAppLabelRequiresMatchingFamilyAndUID(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{
		"zcode":   {"type": "oauth", "accountId": "saved A", "refresh": `{"source":"zcode","site":"zai","uid":"a","kind":"individual"}`},
		"zcode#2": {"type": "oauth", "accountId": "saved B", "refresh": `{"source":"zcode","site":"bigmodel","uid":"a","kind":"individual"}`},
		"zcode#3": {"type": "oauth", "accountId": "old", "refresh": `{"source":"zcode","site":"zai"}`},
		"zcode#4": {"type": "oauth", "accountId": "no family", "refresh": `{"source":"zcode","uid":"a"}`},
	})
	old := zcodeAppLabel
	t.Cleanup(func() { zcodeAppLabel = old })
	zcodeAppLabel = func() zcodeAppIdentity {
		return zcodeAppIdentity{family: "zai", uid: "a", user: "current app", kind: "individual"}
	}
	pp := plugin.Provider{ID: "zcode", Accounts: []plugin.Account{{Key: "zcode"}, {Key: "zcode#2"}, {Key: "zcode#3"}, {Key: "zcode#4"}}}
	_, names := pluginOwnNames(pp)
	if len(names) != 1 || names["zcode"] != "current app" {
		t.Fatalf("labels across families or without UID: %v", names)
	}
	zcodeAppLabel = func() zcodeAppIdentity {
		return zcodeAppIdentity{family: "zai", uid: "other", user: "different account", kind: "individual"}
	}
	_, names = pluginOwnNames(pp)
	if len(names) != 0 {
		t.Fatalf("different account was labelled as the app user: %v", names)
	}
}

func TestZCodeAppLabelReadsActiveNormalizedProfile(t *testing.T) {
	claudeHome(t)
	path := zcodeCredentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	store := map[string]string{
		"oauth:active_provider":    "bigmodel",
		"oauth:bigmodel:user_info": `{"id":"bm-uid","displayName":"BigModel user"}`,
		"oauth:zai:user_info":      `{"user_id":"zai-uid","name":"other family"}`,
		"zcodejwttoken":            "session",
	}
	b, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	app := zcodeAppLabel()
	if app.family != "bigmodel" || app.uid != "bm-uid" || app.user != "BigModel user" {
		t.Fatalf("active profile = %+v", app)
	}
}

func TestZCodeChinaAppLabelFollowsOnlyMatchingSelection(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{
		"zcode":       {"type": "oauth", "accountId": "old name", "uid": "cn-user", "refresh": `{"source":"zcode","site":"bigmodel","uid":"cn-user","kind":"individual","accountIdentity":"bm-user"}`},
		"zcode#other": {"type": "oauth", "accountId": "other name", "uid": "cn-user", "refresh": `{"source":"zcode","site":"zai","uid":"cn-user","kind":"individual"}`},
	})
	path := zcodeCredentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	store := map[string]string{
		"oauth:active_provider":  "zai_cn",
		"oauth:zai_cn:user_info": `{"user_id":"cn-user","name":"Current China user"}`,
		"oauth:linked_bigmodel":  `{"userId":"cn-user","customerId":"bm-user","accessToken":"business"}`,
		"oauth:session_revision": "revision-1",
		"zcodejwttoken":          "session",
	}
	b, _ := json.Marshal(store)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	setting := filepath.Join(home, ".zcode", "v2", "setting.json")
	if err := os.MkdirAll(filepath.Dir(setting), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setting, []byte(`{"providerFamilyDomain":"bigmodel","providerFamilyConnectionSelections":{"bigmodel":{"kind":"individual-coding-plan"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pp := plugin.Provider{ID: "zcode", Accounts: []plugin.Account{{Key: "zcode"}, {Key: "zcode#other"}}}
	_, names := pluginOwnNames(pp)
	if len(names) != 1 || names["zcode"] != "Current China user" {
		t.Fatalf("China app names the wrong selection: %v", names)
	}
}

func TestZCodeRenameMovesOwnedPolicyAndTrayOnlyAfterReadableSettings(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{"zcode": zcodeScopedAuth(t, "Alice", "individual")})
	pp, ok := PluginOf("zcode")
	if !ok {
		t.Fatal("missing ZCode")
	}
	logins := pluginLogins(pp)
	if len(logins) != 1 || logins[0].User != "Alice" || logins[0].pluginIdentity == "" {
		t.Fatalf("initial account: %+v", logins)
	}
	old := accountKey(logins[0].User)
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	f.Providers = append(f.Providers, Provider{ID: "zcode",
		AccountProxies: map[string]string{old: "direct"}, AccountModels: map[string][]string{old: {"GLM-5.3"}},
		AccountCaps: map[string]int{old: 70}, AccountWindowCaps: map[string]map[string]int{old: {"week": 60}},
		AccountWindowAmounts: map[string]map[string]float64{old: {"month": 42}},
		AccountConcurrency:   map[string]int{old: 2}, AccountWeights: map[string]int{old: 3},
		AccountNames: map[string]string{old: "Work"},
	})
	if err := store(f); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{TrayUsages: []string{"zcode|Alice"}, TrayUsageEvery: 5}); err != nil {
		t.Fatal(err)
	}
	card := quotaForLogin(SubscriptionQuota{Provider: "zcode", User: "Alice", Windows: []QuotaWindow{{Name: "Week", Used: 20}}}, logins[0].Login)
	noteQuotaHistory([]SubscriptionQuota{card}, time.Now())
	if err := plugin.EditAuth(func(auth map[string]json.RawMessage) (bool, error) {
		var value map[string]any
		if err := json.Unmarshal(auth["zcode"], &value); err != nil {
			return false, err
		}
		value["accountId"] = "Alicia"
		auth["zcode"], _ = json.Marshal(value)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(settings.Path())
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"trayUsages":`)
	if err := os.WriteFile(settings.Path(), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	pp, _ = PluginOf("zcode")
	if got := pluginLogins(pp); len(got) != 0 {
		t.Fatalf("unreadable settings let the pending account route: %+v", got)
	}
	if choices, ok := ZCodeAccountChoices(); !ok || len(choices) != 1 || choices[0].Error == "" {
		t.Fatalf("pending rename was not surfaced: %+v, available=%v", choices, ok)
	}
	if after, err := os.ReadFile(settings.Path()); err != nil || string(after) != string(bad) {
		t.Fatalf("unreadable settings changed: %q %v", after, err)
	}
	if err := os.WriteFile(settings.Path(), good, 0o600); err != nil {
		t.Fatal(err)
	}
	pp, _ = PluginOf("zcode")
	got := pluginLogins(pp)
	if len(got) != 1 || got[0].User != "Alicia" || got[0].pluginIdentity != logins[0].pluginIdentity {
		t.Fatalf("same selection did not rename: %+v", got)
	}
	f, err = read()
	if err != nil {
		t.Fatal(err)
	}
	var saved Provider
	for _, p := range f.Providers {
		if p.ID == "zcode" {
			saved = p
		}
	}
	key := accountKey("Alicia")
	if saved.AccountProxies[key] != "direct" || len(saved.AccountModels[key]) != 1 || saved.AccountCaps[key] != 70 ||
		saved.AccountWindowCaps[key]["week"] != 60 || saved.AccountWindowAmounts[key]["month"] != 42 ||
		saved.AccountConcurrency[key] != 2 || saved.AccountWeights[key] != 3 || saved.AccountNames[key] != "Work" {
		t.Fatalf("renamed policy: %+v", saved)
	}
	if got := settings.Load(); len(got.TrayUsages) != 1 || got.TrayUsages[0] != "zcode|Alicia" || got.TrayUsageEvery != 5 {
		t.Fatalf("renamed tray: %+v", got)
	}
	if history := QuotaHistories(time.Time{}, "zcode", "Alicia"); len(history) != 1 {
		t.Fatalf("same-scope history did not follow the name: %+v", history)
	}
}

func TestZCodeRemovedTwinDoesNotStealTheOtherPolicy(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{
		"zcode":      zcodeScopedAuth(t, "Alice", "individual"),
		"zcode#team": zcodeScopedAuth(t, "Alice", "team"),
	})
	pp, _ := PluginOf("zcode")
	rows := pluginLogins(pp)
	var team string
	for _, row := range rows {
		if row.acct.Key == "zcode#team" {
			team = row.User
		}
	}
	if team == "" || team == "Alice" {
		t.Fatalf("team was not disambiguated: %+v", rows)
	}
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	f.Providers = append(f.Providers, Provider{ID: "zcode", AccountProxies: map[string]string{"alice": "http://old.example:8080", accountKey(team): "direct"}})
	if err := store(f); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{TrayUsages: []string{"zcode|" + team}}); err != nil {
		t.Fatal(err)
	}
	if err := plugin.EditAuth(func(auth map[string]json.RawMessage) (bool, error) {
		delete(auth, "zcode")
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	pp, _ = PluginOf("zcode")
	rows = pluginLogins(pp)
	if len(rows) != 1 || rows[0].User != team || ProxyOfLogin("zcode", rows[0].User) != "direct" {
		t.Fatalf("remaining selection inherited another policy: %+v", rows)
	}
	if got := settings.Load(); len(got.TrayUsages) != 1 || got.TrayUsages[0] != "zcode|"+team {
		t.Fatalf("remaining selection lost its tray: %+v", got.TrayUsages)
	}
}

func TestZCodeReplacementScopeDoesNotInheritOldSelection(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{"zcode": zcodeScopedAuth(t, "Alice", "individual")})
	pp, _ := PluginOf("zcode")
	before := pluginLogins(pp)
	if len(before) != 1 || before[0].pluginIdentity == "" {
		t.Fatalf("initial selection: %+v", before)
	}
	oldID := LoginID("plugin:zcode", before[0].User)
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	f.Providers = append(f.Providers, Provider{ID: "zcode", AccountProxies: map[string]string{"alice": "direct"}, AccountCaps: map[string]int{"alice": 50}})
	if err := store(f); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{TrayUsages: []string{"zcode|Alice"}}); err != nil {
		t.Fatal(err)
	}
	if err := plugin.EditAuth(func(auth map[string]json.RawMessage) (bool, error) {
		auth["zcode"], _ = json.Marshal(zcodeScopedAuth(t, "Alice", "team"))
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	pp, _ = PluginOf("zcode")
	after := pluginLogins(pp)
	if len(after) != 1 || after[0].User == "Alice" || after[0].pluginIdentity == before[0].pluginIdentity {
		t.Fatalf("changed selection reused old identity: %+v", after)
	}
	if ProxyOfLogin("zcode", after[0].User) == "direct" || LoginID("plugin:zcode", after[0].User) == oldID {
		t.Fatalf("changed selection inherited old policy or account reference: %+v", after[0])
	}
	if got := settings.Load(); len(got.TrayUsages) != 1 || got.TrayUsages[0] != "zcode|Alice" {
		t.Fatalf("old tray selection was rebound: %+v", got.TrayUsages)
	}
}

func TestZCodePendingRenameResumesAfterStagedConflict(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{"zcode": zcodeScopedAuth(t, "Alice", "individual")})
	pp, _ := PluginOf("zcode")
	rows := pluginLogins(pp)
	if len(rows) != 1 {
		t.Fatalf("initial account: %+v", rows)
	}
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	f.Providers = append(f.Providers, Provider{ID: "zcode", AccountCaps: map[string]int{"alice": 70}})
	if err := store(f); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{TrayUsages: []string{"zcode|Alice"}}); err != nil {
		t.Fatal(err)
	}
	if err := plugin.EditAuth(func(auth map[string]json.RawMessage) (bool, error) {
		var value map[string]any
		if err := json.Unmarshal(auth["zcode"], &value); err != nil {
			return false, err
		}
		value["accountId"] = "Alicia"
		auth["zcode"], _ = json.Marshal(value)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	intent := &pluginRenameIntent{Slot: "zcode", From: "Alice", To: "Alicia",
		PreviousScope: rows[0].saved.PluginScope, Scope: rows[0].saved.PluginScope,
		Generation: rows[0].pluginGeneration, MovePolicy: true}
	if err := preparePluginIntent("zcode", intent); err != nil {
		t.Fatal(err)
	}
	intent.Prepared = true
	loginsMu.Lock()
	ls := readLogins()
	for i := range ls {
		if ls[i].Agent == "plugin:zcode" && ls[i].Home == "zcode" {
			ls[i].PluginRename = intent
		}
	}
	err = writeLogins(ls)
	loginsMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// These copies represent a crash after reference staging but before
	// publication. A conflicting later edit must be preserved.
	if err := stagePluginPolicies("zcode", intent, false); err != nil {
		t.Fatal(err)
	}
	if err := stagePluginTray("zcode", intent, false); err != nil {
		t.Fatal(err)
	}
	f, err = read()
	if err != nil {
		t.Fatal(err)
	}
	for i := range f.Providers {
		if f.Providers[i].ID == "zcode" {
			f.Providers[i].AccountCaps["alicia"] = 90
		}
	}
	if err := store(f); err != nil {
		t.Fatal(err)
	}
	pp, _ = PluginOf("zcode")
	if got := pluginLogins(pp); len(got) != 0 {
		t.Fatalf("partially moved account remained routable: %+v", got)
	}
	if _, err := ViaPluginAccount(context.Background(), "zcode", "zcode"); err == nil {
		t.Fatal("pending account accepted a native control")
	}
	if err := plugin.EditAuth(func(auth map[string]json.RawMessage) (bool, error) {
		var value map[string]any
		if err := json.Unmarshal(auth["zcode"], &value); err != nil {
			return false, err
		}
		value["accountEpoch"], value["access"] = "epoch-2", "rotated-token"
		auth["zcode"], _ = json.Marshal(value)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	f, _ = read()
	for i := range f.Providers {
		if f.Providers[i].ID == "zcode" {
			if f.Providers[i].AccountCaps["alice"] != 70 || f.Providers[i].AccountCaps["alicia"] != 90 {
				t.Fatalf("conflict overwrote policy: %+v", f.Providers[i].AccountCaps)
			}
			f.Providers[i].AccountCaps["alicia"] = 70
		}
	}
	if err := store(f); err != nil {
		t.Fatal(err)
	}
	pp, _ = PluginOf("zcode")
	if got := pluginLogins(pp); len(got) != 1 || got[0].User != "Alicia" {
		t.Fatalf("pending rename did not resume: %+v", got)
	}
	f, _ = read()
	for _, p := range f.Providers {
		if p.ID == "zcode" && (p.AccountCaps["alice"] != 0 || p.AccountCaps["alicia"] != 70) {
			t.Fatalf("resumed policy: %+v", p.AccountCaps)
		}
	}
	if got := settings.Load(); len(got.TrayUsages) != 1 || got.TrayUsages[0] != "zcode|Alicia" {
		t.Fatalf("resumed tray: %+v", got.TrayUsages)
	}
}

func TestProviderSnapshotRejectsConcurrentSave(t *testing.T) {
	providerFileHome(t)
	if err := replaceProviderFixture(file{
		Providers: []Provider{{ID: "original", Name: "Before", Chat: "https://original.invalid", Key: "synthetic"}},
		Groups:    []Group{{ID: "kept", Members: []string{"original/model"}}},
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := pluginPolicyFile()
	if err != nil {
		t.Fatal(err)
	}
	normal, err := read()
	if err != nil {
		t.Fatal(err)
	}
	stale.Providers[0].Name = "Stale edit"
	normal.Providers[0].Name = "Another stale edit"
	if err := Save(Provider{ID: "unrelated", Chat: "https://new.invalid", Key: "synthetic-new"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(Path() + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []file{stale, normal} {
		if err := store(snapshot); !errors.Is(err, errProvidersChanged) {
			t.Fatalf("stale provider snapshot: %v", err)
		}
	}
	after, err := os.ReadFile(Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("stale snapshot overwrote the unrelated Save: %v", err)
	}
	afterBackup, err := os.ReadFile(Path() + ".bak")
	if err != nil || !bytes.Equal(backup, afterBackup) {
		t.Fatalf("stale snapshot reached last-good publication: %v", err)
	}
	current, err := read()
	if err != nil || len(current.Providers) != 2 || len(current.Groups) != 1 {
		t.Fatalf("saved providers/groups not retained: %+v, %v", current, err)
	}
	if err := store(file{}); err == nil {
		t.Fatal("a source-less snapshot was treated as an explicit replacement")
	}
}

func TestProviderMissingSnapshotRejectsConcurrentCreation(t *testing.T) {
	providerFileHome(t)
	missing, err := read()
	if err != nil {
		t.Fatal(err)
	}
	strictMissing, err := pluginPolicyFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(Provider{ID: "created", Chat: "https://created.invalid", Key: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []file{missing, strictMissing} {
		if err := store(snapshot); !errors.Is(err, errProvidersChanged) {
			t.Fatalf("missing-source snapshot after creation: %v", err)
		}
	}
	after, err := os.ReadFile(Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("concurrently created provider was overwritten: %v", err)
	}
}

func TestProviderBaselineTracksRawAndFallbackSource(t *testing.T) {
	providerFileHome(t)
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	valid := []byte(`{"providers":[{"id":"kept"}]}`)
	withBOM := append(append([]byte(nil), utf8BOM...), valid...)
	if err := os.WriteFile(Path(), withBOM, 0o600); err != nil {
		t.Fatal(err)
	}
	bomSnapshot, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), valid, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store(bomSnapshot); !errors.Is(err, errProvidersChanged) {
		t.Fatalf("BOM-only source change not detected: %v", err)
	}
	existing, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(Path()); err != nil {
		t.Fatal(err)
	}
	if err := store(existing); !errors.Is(err, errProvidersChanged) {
		t.Fatalf("removed source was recreated from a stale snapshot: %v", err)
	}
	if _, err := os.Stat(Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed source reappeared: %v", err)
	}
	if err := os.WriteFile(Path()+".bak", valid, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(`{"providers":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pluginPolicyFile(); err == nil {
		t.Fatal("strict native policy read accepted a backup for invalid source")
	}
	fallback, err := read()
	if err != nil || len(fallback.Providers) != 1 {
		t.Fatalf("last-good fixture was not read: %+v, %v", fallback, err)
	}
	newBad := []byte(`{"providers":[`)
	if err := os.WriteFile(Path(), newBad, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store(fallback); !errors.Is(err, errProvidersChanged) {
		t.Fatalf("fallback snapshot ignored the actual source change: %v", err)
	}
	if after, err := os.ReadFile(Path()); err != nil || !bytes.Equal(after, newBad) {
		t.Fatalf("newer invalid source was overwritten: %q, %v", after, err)
	}
}

func TestProviderRenameKeepsCatalogOnRejectedPublication(t *testing.T) {
	providerFileHome(t)
	if err := replaceProviderFixture(file{Providers: []Provider{{
		ID: "original", Name: "Original", Chat: "https://original.invalid", Key: "synthetic",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("original", "https://original.invalid", []catalog.Model{{ID: "model"}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(Path() + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(Path()+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Rename("original", "renamed"); err == nil {
		t.Fatal("rename succeeded without its provider lock")
	}
	if after, err := os.ReadFile(Path()); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed rename changed providers: %v", err)
	}
	if _, err := os.Stat(catalog.LivePath("original")); err != nil {
		t.Fatalf("failed rename moved the original catalog: %v", err)
	}
	if _, err := os.Stat(catalog.LivePath("renamed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed rename created a destination catalog: %v", err)
	}
	if err := os.Remove(Path() + ".lock"); err != nil {
		t.Fatal(err)
	}
	wasChanged := catalog.Changed
	t.Cleanup(func() { catalog.Changed = wasChanged })
	notifications, sawMovedList := 0, false
	catalog.Changed = func() {
		notifications++
		_, err := os.Stat(catalog.LivePath("renamed"))
		sawMovedList = err == nil
	}
	if err := Rename("original", "renamed"); err != nil {
		t.Fatal(err)
	}
	if notifications != 1 || !sawMovedList {
		t.Fatalf("catalog notification preceded its renamed list: count=%d, list=%v", notifications, sawMovedList)
	}
}

func TestZCodeRenameResumesAfterStalePolicySnapshot(t *testing.T) {
	movedPlugin(t, "zcode", map[string]map[string]any{"zcode": zcodeScopedAuth(t, "Alice", "individual")})
	pp, _ := PluginOf("zcode")
	rows := pluginLogins(pp)
	if len(rows) != 1 {
		t.Fatalf("initial native account: %+v", rows)
	}
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	f.Providers = append(f.Providers, Provider{ID: "zcode", AccountCaps: map[string]int{"alice": 70}})
	if err := store(f); err != nil {
		t.Fatal(err)
	}
	if err := plugin.EditAuth(func(auth map[string]json.RawMessage) (bool, error) {
		var value map[string]any
		if err := json.Unmarshal(auth["zcode"], &value); err != nil {
			return false, err
		}
		value["accountId"] = "Alicia"
		auth["zcode"], _ = json.Marshal(value)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	intent := &pluginRenameIntent{Slot: "zcode", From: "Alice", To: "Alicia",
		PreviousScope: rows[0].saved.PluginScope, Scope: rows[0].saved.PluginScope,
		Generation: rows[0].pluginGeneration, MovePolicy: true}
	if err := preparePluginIntent("zcode", intent); err != nil {
		t.Fatal(err)
	}
	intent.Prepared = true
	loginsMu.Lock()
	ls := readLogins()
	for i := range ls {
		if ls[i].Agent == "plugin:zcode" && ls[i].Home == "zcode" {
			ls[i].PluginRename = intent
		}
	}
	err = writeLogins(ls)
	loginsMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	stale, err := pluginPolicyFile()
	if err != nil {
		t.Fatal(err)
	}
	moved, err := stagePluginPolicyMap(stale.Providers[0].AccountCaps, "alice", "alicia", intent.Policy["cap"], false)
	if err != nil || !moved {
		t.Fatalf("staging from the native snapshot: moved=%v, %v", moved, err)
	}
	current, err := read()
	if err != nil {
		t.Fatal(err)
	}
	current.Providers = append(current.Providers, Provider{ID: "unrelated", Chat: "https://new.invalid", Key: "synthetic-new"})
	if err := store(current); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := store(stale); !errors.Is(err, errProvidersChanged) {
		t.Fatalf("stale policy publication: %v", err)
	}
	if after, err := os.ReadFile(Path()); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("stale policy publication changed unrelated edit: %v", err)
	}
	if pendingPluginAccountError("plugin:zcode", "zcode") == nil {
		t.Fatal("stale publication cleared the durable native intent")
	}
	pp, _ = PluginOf("zcode")
	if rows := pluginLogins(pp); len(rows) != 1 || rows[0].User != "Alicia" {
		t.Fatalf("fresh checked read did not resume native rename: %+v", rows)
	}
	f, err = read()
	if err != nil || len(f.Providers) != 2 || f.Providers[0].AccountCaps["alice"] != 0 ||
		f.Providers[0].AccountCaps["alicia"] != 70 || f.Providers[1].ID != "unrelated" {
		t.Fatalf("resumed policy lost the unrelated provider: %+v, %v", f.Providers, err)
	}
}

func TestZCodeRenameKeepsIntentOnProviderFailures(t *testing.T) {
	for _, failure := range []string{"invalid-source", "read-error", "lock-error", "lastgood-error", "publication-error"} {
		t.Run(failure, func(t *testing.T) {
			movedPlugin(t, "zcode", map[string]map[string]any{"zcode": zcodeScopedAuth(t, "Alice", "individual")})
			pp, _ := PluginOf("zcode")
			if rows := pluginLogins(pp); len(rows) != 1 {
				t.Fatalf("initial native login: %+v", rows)
			}
			f, err := read()
			if err != nil {
				t.Fatal(err)
			}
			f.Providers = append(f.Providers, Provider{ID: "zcode", AccountCaps: map[string]int{"alice": 70}})
			if err := store(f); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(Path())
			if err != nil {
				t.Fatal(err)
			}
			originalSync := steady.Sync
			syncFailed := false
			if err := plugin.EditAuth(func(auth map[string]json.RawMessage) (bool, error) {
				var value map[string]any
				if err := json.Unmarshal(auth["zcode"], &value); err != nil {
					return false, err
				}
				value["accountId"] = "Alicia"
				auth["zcode"], _ = json.Marshal(value)
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "invalid-source":
				if err := os.WriteFile(Path(), []byte(`{"providers":`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "read-error":
				if err := os.Rename(Path(), Path()+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(Path(), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := store(f); !errors.Is(err, ErrUnreadable) {
					t.Fatalf("source read failure was not reported: %v", err)
				}
			case "lock-error":
				if err := os.Remove(Path() + ".lock"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(Path()+".lock", 0o700); err != nil {
					t.Fatal(err)
				}
			case "lastgood-error":
				if err := os.Remove(Path() + ".bak"); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if err := os.Mkdir(Path()+".bak", 0o700); err != nil {
					t.Fatal(err)
				}
			case "publication-error":
				if err := os.WriteFile(Path()+".bak", before, 0o600); err != nil {
					t.Fatal(err)
				}
				steady.Sync = func(f *os.File) error {
					name := filepath.Base(f.Name())
					if strings.HasPrefix(name, ".providers.json.") && !strings.HasPrefix(name, ".providers.json.bak.") {
						syncFailed = true
						return errors.New("injected provider publication failure")
					}
					return originalSync(f)
				}
				t.Cleanup(func() { steady.Sync = originalSync })
			}
			pp, _ = PluginOf("zcode")
			if rows := pluginLogins(pp); len(rows) != 0 {
				t.Fatalf("pending native account remained visible: %+v", rows)
			}
			var pending *pluginRenameIntent
			for _, login := range sharedLogins() {
				if login.Agent == "plugin:zcode" && login.Home == "zcode" {
					pending = login.PluginRename
				}
			}
			if pending == nil || pending.Published {
				t.Fatalf("native intent was lost or published on %s: %+v", failure, pending)
			}
			if (failure == "lock-error" || failure == "lastgood-error" || failure == "publication-error") && !pending.Prepared {
				t.Fatalf("native intent did not reach provider publication on %s: %+v", failure, pending)
			}
			if failure == "publication-error" && !syncFailed {
				t.Fatal("provider publication failure was not exercised")
			}
			switch failure {
			case "invalid-source":
				if got, err := os.ReadFile(Path()); err != nil || string(got) != `{"providers":` {
					t.Fatalf("invalid source was changed: %q, %v", got, err)
				}
				if err := os.WriteFile(Path(), before, 0o600); err != nil {
					t.Fatal(err)
				}
			case "read-error":
				if got, err := os.ReadFile(Path() + ".saved"); err != nil || !bytes.Equal(got, before) {
					t.Fatalf("unreadable source data was changed: %v", err)
				}
				if err := os.Remove(Path()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(Path()+".saved", Path()); err != nil {
					t.Fatal(err)
				}
			case "lock-error", "lastgood-error":
				if got, err := os.ReadFile(Path()); err != nil || !bytes.Equal(got, before) {
					t.Fatalf("failed publication changed source: %v", err)
				}
				suffix := ".lock"
				if failure == "lastgood-error" {
					suffix = ".bak"
				}
				if err := os.Remove(Path() + suffix); err != nil {
					t.Fatal(err)
				}
			case "publication-error":
				steady.Sync = originalSync
				if got, err := os.ReadFile(Path()); err != nil || !bytes.Equal(got, before) {
					t.Fatalf("failed provider flush changed source: %v", err)
				}
			}
			pp, _ = PluginOf("zcode")
			if rows := pluginLogins(pp); len(rows) != 1 || rows[0].User != "Alicia" {
				t.Fatalf("native rename did not resume after repair: %+v", rows)
			}
			f, err = read()
			if err != nil || len(f.Providers) != 1 ||
				f.Providers[0].AccountCaps["alice"] != 0 || f.Providers[0].AccountCaps["alicia"] != 70 {
				t.Fatalf("resumed native policy: %+v, %v", f.Providers, err)
			}
		})
	}
}

func TestNonNativeRenameAndTokenRotationRemainRoutable(t *testing.T) {
	movedPlugin(t, "devin", map[string]map[string]any{
		"devin": {"type": "oauth", "accountId": "Alice", "access": "old-token", "refresh": "old-refresh"},
	})
	pp, _ := PluginOf("devin")
	before := pluginLogins(pp)
	if len(before) != 1 || before[0].User != "Alice" {
		t.Fatalf("initial non-native account: %+v", before)
	}
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"providers":`)
	if err := os.WriteFile(Path(), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := plugin.EditAuth(func(auth map[string]json.RawMessage) (bool, error) {
		var value map[string]any
		if err := json.Unmarshal(auth["devin"], &value); err != nil {
			return false, err
		}
		value["accountId"], value["access"], value["refresh"] = "Alicia", "rotated-token", "rotated-refresh"
		auth["devin"], _ = json.Marshal(value)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	pp, _ = PluginOf("devin")
	after := pluginLogins(pp)
	if len(after) != 1 || after[0].User != "Alicia" || after[0].acct.Key != "devin" ||
		after[0].acct.Generation == before[0].acct.Generation || after[0].saved.PluginRename != nil {
		t.Fatalf("non-native rename/rotation was hidden or journaled: %+v", after)
	}
	if _, err := ViaPluginAccount(context.Background(), "devin", "devin"); err != nil {
		t.Fatalf("renamed non-native account cannot route: %v", err)
	}
	served := false
	for _, p := range pluginAccounts() {
		if p.PluginProvider() == "devin" && p.Account.User == "Alicia" && p.PluginAccountKey() == "devin" {
			served = true
		}
	}
	if !served || PluginAccountUser("devin", "devin") != "Alicia" {
		t.Fatal("old plugin serving path did not retain the renamed account and slot")
	}
	if got, err := os.ReadFile(Path()); err != nil || !bytes.Equal(got, bad) {
		t.Fatalf("non-native rename changed unrelated invalid provider data: %q, %v", got, err)
	}
}
