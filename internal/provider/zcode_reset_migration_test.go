package provider

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

func legacyResetFixtureContext() plugin.AccountContext {
	return plugin.AccountContext{Scope: "team-scope", Generation: "login-a", Origin: "https://zcode.z.ai", Family: "bigmodel", UserID: "zcode-user", AccountIdentity: "linked-customer", Kind: "team", Organization: "org", Project: "project", Product: "product"}
}

func legacyResetFixtureAuth(c plugin.AccountContext, verified bool) map[string]any {
	s := map[string]any{"site": c.Family, "kind": c.Kind, "uid": "family-user", "accountIdentity": c.AccountIdentity, "org": c.Organization, "project": c.Project, "product": c.Product}
	if verified {
		s["zcodeUserId"] = c.UserID
	}
	b, _ := json.Marshal(s)
	return map[string]any{"type": "oauth", "accountId": "fixture", "accountScope": c.Scope, "accountEpoch": c.Generation, "refresh": string(b)}
}

func legacyResetFixtureHash(c plugin.AccountContext) string {
	// This is the retired broker's JSON.stringify input, with three distinct
	// identities: its family UID, the verified session user and PAT customer.
	b, _ := json.Marshal([]string{c.Origin, c.Family, "family-user", c.AccountIdentity, c.Organization, c.Project})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeLegacyResetFixture(t *testing.T, c plugin.AccountContext) {
	t.Helper()
	started := time.Now().UTC().Add(-time.Minute)
	old := legacyZCodeReset{Seen: true, FiveHourUsed: 100, WeeklyUsed: 12,
		OpportunityKey: "opportunity-original", OpportunityClaim: "opportunity-owner", OpportunityStarted: started,
		Pending: &legacyZCodePending{Window: "fiveHour", Key: "lost-debit", Baseline: 100, Started: started, Claim: "old-owner", ClaimedAt: started}}
	b, err := json.Marshal(map[string]any{"version": 1, "scopes": map[string]any{legacyResetFixtureHash(c): old}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyZCodeResetPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func installLegacyResetRPC(t *testing.T, c plugin.AccountContext, uses, grants *int) {
	t.Helper()
	zcodeTestRPC(t, func(req plugin.AccountRequest) (plugin.AccountResult, error) {
		switch req.Operation {
		case "context":
			return zcodeTestData(t, c, nil), nil
		case "reset.status":
			return zcodeTestData(t, c, resetFixture(time.Now().Add(time.Hour).UnixMilli(), 101, false)), nil
		case "reset.use":
			*uses++
		case "reset.opportunity":
			*grants++
		}
		return plugin.AccountResult{}, os.ErrInvalid
	})
}

func TestZCodeLegacyResetRestartChild(t *testing.T) {
	if os.Getenv("MAGPIE_ZCODE_LEGACY_CHILD") != "1" {
		return
	}
	c := legacyResetFixtureContext()
	c.Generation = "login-b"
	uses, grants := 0, 0
	installLegacyResetRPC(t, c, &uses, &grants)
	ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
	state, err := ZCodeResetStatus(ctx, "zcode", "zcode", true)
	if err != nil || state.Pending != "observed-completion" || state.ObservedAt != 101 {
		t.Fatalf("restarted status = %+v, %v", state, err)
	}
	if _, err := UseZCodeReset(ctx, "zcode", "zcode", "five-hour"); err == nil || uses != 0 || grants != 0 {
		t.Fatalf("restart repeated a possibly spent reset: uses=%d grants=%d err=%v", uses, grants, err)
	}
}

func TestZCodeLegacyLostResponseCutoverSurvivesRestart(t *testing.T) {
	c := legacyResetFixtureContext()
	movedPlugin(t, "zcode", map[string]map[string]any{"zcode": legacyResetFixtureAuth(c, true)})
	writeLegacyResetFixture(t, c)
	uses, grants := 0, 0
	installLegacyResetRPC(t, c, &uses, &grants)
	ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
	state, err := ZCodeResetStatus(ctx, "zcode", "zcode", true)
	if err != nil || state.Pending != "observed-completion" || state.PendingWindow != "five-hour" || state.ObservedAt != 101 {
		t.Fatalf("imported lost response = %+v, %v", state, err)
	}
	if _, err := UseZCodeReset(ctx, "zcode", "zcode", "five-hour"); err == nil || uses != 0 || grants != 0 {
		t.Fatalf("imported debt authorized a new operation: uses=%d grants=%d err=%v", uses, grants, err)
	}
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		p := j.Reset.Pending
		if p == nil || p.Key != "lost-debit" || p.Baseline != 100 || j.Reset.OpportunityKey != "opportunity-original" || j.Reset.OpportunityAt == 0 {
			t.Errorf("imported journal lost old keys or baseline: %+v", j.Reset)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(legacyZCodeResetPath())
	var tombstone struct {
		Version int                        `json:"version"`
		Scopes  map[string]json.RawMessage `json:"scopes"`
	}
	if err != nil || json.Unmarshal(b, &tombstone) != nil || tombstone.Version != 2 || len(tombstone.Scopes) != 0 {
		t.Fatalf("old reader tombstone: %s, %v", b, err)
	}
	c.Generation = "login-b"
	auth, _ := json.Marshal(map[string]any{"zcode": legacyResetFixtureAuth(c, true)})
	if err := os.WriteFile(plugin.AuthPath(), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestZCodeLegacyResetRestartChild$", "-test.count=1")
	child.Env = append(os.Environ(), "MAGPIE_ZCODE_LEGACY_CHILD=1")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("restarted process: %v\n%s", err, out)
	}
}

func TestZCodeUnmappableLegacyDebtBlocksGrantAndDebit(t *testing.T) {
	c := legacyResetFixtureContext()
	movedPlugin(t, "zcode", map[string]map[string]any{"zcode": legacyResetFixtureAuth(c, false)})
	writeLegacyResetFixture(t, c)
	uses, grants := 0, 0
	installLegacyResetRPC(t, c, &uses, &grants)
	ctx := WithZCodeAccountExpectation(context.Background(), c.Scope, c.Generation)
	state, err := ZCodeResetStatus(ctx, "zcode", "zcode", true)
	if err != nil || state.Pending != "legacy-scope-unresolved" {
		t.Fatalf("unverified legacy identity was silently mapped: %+v, %v", state, err)
	}
	if _, err := UseZCodeReset(ctx, "zcode", "zcode", "five-hour"); err == nil || uses != 0 || grants != 0 {
		t.Fatalf("unmapped debt authorized use/grant: uses=%d grants=%d err=%v", uses, grants, err)
	}
}

func TestZCodeNativeV1JournalReaderFencedAtCutover(t *testing.T) {
	c := zcodeTestContext(t)
	path := zcodeJournalPath(c)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{zcodeJournalSchema, "PRAGMA application_id=1296518721", "PRAGMA user_version=1"} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	old := zcodeJournal{Version: 1, Scope: c.Scope, Origin: c.Origin, Reset: zcodeResetJournal{
		WeekHigh: 12, Pending: &zcodeResetClaim{Key: "v1-pending", Window: "week", Generation: "old-login", Baseline: 12, HighWater: 12},
	}}
	payload, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO native_journal(scope_key,origin,scope,version,payload) VALUES(?,?,?,?,?)",
		zcodeScopeName(c), c.Origin, c.Scope, 1, payload); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		if j.Reset.Pending == nil || j.Reset.Pending.Key != "v1-pending" || j.Reset.Pending.Baseline != 12 {
			t.Errorf("native v1 pending debt was lost: %+v", j.Reset)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("old native reader's version==1 guard would not fence: version=%d err=%v", version, err)
	}
	oldJSON, err := os.ReadFile(legacyZCodeResetPath())
	if err != nil || !strings.Contains(string(oldJSON), `"version":2`) {
		t.Fatalf("old JSON reader was not fenced: %s %v", oldJSON, err)
	}
}
