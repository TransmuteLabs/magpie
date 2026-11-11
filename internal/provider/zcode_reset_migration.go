package provider

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/steady"
)

type legacyZCodePending struct {
	Window    string    `json:"window"`
	Key       string    `json:"key"`
	Baseline  float64   `json:"baseline"`
	Started   time.Time `json:"started"`
	Claim     string    `json:"claim,omitempty"`
	ClaimedAt time.Time `json:"claimedAt,omitempty"`
}

type legacyZCodeReset struct {
	Seen               bool                `json:"seen"`
	FiveHourUsed       float64             `json:"fiveHourUsed"`
	WeeklyUsed         float64             `json:"weeklyUsed"`
	FiveHourConfirmed  time.Time           `json:"fiveHourConfirmed,omitempty"`
	WeeklyConfirmed    time.Time           `json:"weeklyConfirmed,omitempty"`
	OpportunityKey     string              `json:"opportunityKey,omitempty"`
	OpportunityAt      time.Time           `json:"opportunityAt,omitempty"`
	OpportunityClaim   string              `json:"opportunityClaim,omitempty"`
	OpportunityStarted time.Time           `json:"opportunityStarted,omitempty"`
	Pending            *legacyZCodePending `json:"pending,omitempty"`
}

func legacyZCodeResetPath() string { return filepath.Join(settings.Dir(), "zcode-resets.json") }

func validLegacyZCodeReset(r legacyZCodeReset) bool {
	if r.FiveHourUsed < 0 || r.WeeklyUsed < 0 || math.IsNaN(r.FiveHourUsed) || math.IsNaN(r.WeeklyUsed) ||
		math.IsInf(r.FiveHourUsed, 0) || math.IsInf(r.WeeklyUsed, 0) || len(r.OpportunityKey) > 64 ||
		r.OpportunityClaim != "" && (r.OpportunityKey == "" || r.OpportunityStarted.IsZero()) {
		return false
	}
	if p := r.Pending; p != nil {
		return (p.Window == "fiveHour" || p.Window == "weekly") && p.Key != "" && len(p.Key) <= 64 &&
			p.Baseline >= 0 && !math.IsNaN(p.Baseline) && !math.IsInf(p.Baseline, 0) && !p.Started.IsZero() &&
			(p.Claim == "" || !p.ClaimedAt.IsZero())
	}
	return true
}

func validLegacyZCodeScope(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// Called with both the old JSON lock and the SQLite journal lock held. The
// SQLite commit precedes the durable tombstone; after a crash before that
// tombstone the old records are imported again before any native operation.
func fenceLegacyZCodeResets(db *sql.DB) error {
	path := legacyZCodeResetPath()
	data, err := os.ReadFile(path)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return err
	}
	if !missing {
		var state struct {
			Version int                        `json:"version"`
			Scopes  map[string]json.RawMessage `json:"scopes"`
		}
		if json.Unmarshal(data, &state) != nil || state.Scopes == nil {
			return errors.New("legacy ZCode reset state is unreadable")
		}
		if state.Version == 2 {
			if len(state.Scopes) != 0 {
				return errors.New("legacy ZCode reset tombstone is invalid")
			}
			return nil
		}
		if state.Version != 1 {
			return errors.New("legacy ZCode reset state has an unsupported version")
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for scope, raw := range state.Scopes {
			var record legacyZCodeReset
			if !validLegacyZCodeScope(scope) || json.Unmarshal(raw, &record) != nil || !validLegacyZCodeReset(record) {
				return errors.New("legacy ZCode reset state is invalid")
			}
			var mapped string
			err := tx.QueryRow("SELECT mapped_scope FROM legacy_zcode_reset WHERE scope_key=?", scope).Scan(&mapped)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if mapped != "" {
				return errors.New("legacy ZCode reset state reappeared after adoption")
			}
			if _, err := tx.Exec(`INSERT INTO legacy_zcode_reset(scope_key,payload,mapped_scope) VALUES(?,?, '')
				ON CONFLICT(scope_key) DO UPDATE SET payload=excluded.payload`, scope, []byte(raw)); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if err := steady.WriteFile(path, []byte("{\"version\":2,\"scopes\":{}}\n"), 0o600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = dir.Sync()
		closeErr := dir.Close()
		if err != nil {
			return fmt.Errorf("syncing ZCode reset tombstone: %w", err)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// The retired fetch broker hashed its saved family UID, which need not be
// the verified ZCode session user or the linked BigModel customer ID. Read
// that UID only from the captured slot, never infer it from either identity.
func legacyZCodeScope(c plugin.AccountContext) string {
	if c.UserID == "" || (c.Kind != "individual" && c.Kind != "team") {
		return ""
	}
	uid := ""
	for _, auth := range plugin.Auths("zcode") {
		if auth["accountScope"] != c.Scope || auth["accountEpoch"] != c.Generation || auth["type"] != "oauth" {
			continue
		}
		refresh, ok := auth["refresh"].(string)
		if !ok {
			continue
		}
		var saved struct {
			Site            string `json:"site"`
			UID             string `json:"uid"`
			ZCodeUserID     string `json:"zcodeUserId"`
			AccountIdentity string `json:"accountIdentity"`
			Organization    string `json:"org"`
			Project         string `json:"project"`
		}
		if json.Unmarshal([]byte(refresh), &saved) != nil {
			continue
		}
		if saved.UID == "" {
			saved.UID, _ = auth["uid"].(string)
		}
		account := saved.AccountIdentity
		if account == "" {
			account = saved.UID
		}
		if saved.UID == "" || saved.Site != c.Family || saved.ZCodeUserID != c.UserID ||
			account != c.AccountIdentity || saved.Organization != c.Organization || saved.Project != c.Project {
			continue
		}
		if uid != "" && uid != saved.UID {
			return ""
		}
		uid = saved.UID
	}
	if uid == "" {
		return ""
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode([]string{c.Origin, c.Family, uid, c.AccountIdentity, c.Organization, c.Project}); err != nil {
		return ""
	}
	h := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return hex.EncodeToString(h[:])
}

func adoptLegacyZCodeReset(tx *sql.Tx, c plugin.AccountContext, j *zcodeJournal) (bool, error) {
	if c.Kind != "individual" && c.Kind != "team" {
		return false, nil
	}
	var exists bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM legacy_zcode_reset)").Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	scope := legacyZCodeScope(c)
	if scope == "" {
		return false, nil
	}
	var raw []byte
	var mapped string
	err := tx.QueryRow("SELECT payload, mapped_scope FROM legacy_zcode_reset WHERE scope_key=?", scope).Scan(&raw, &mapped)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if mapped != "" {
		if mapped != zcodeScopeName(c) {
			return false, errors.New("legacy ZCode reset scope is already bound to another account")
		}
		return false, nil
	}
	var old legacyZCodeReset
	if json.Unmarshal(raw, &old) != nil || !validLegacyZCodeReset(old) {
		return false, errors.New("legacy ZCode reset debt is invalid")
	}
	r := &j.Reset
	r.FiveHigh = max(r.FiveHigh, old.FiveHourUsed)
	r.WeekHigh = max(r.WeekHigh, old.WeeklyUsed)
	if p := old.Pending; p != nil {
		window, high := "five-hour", r.FiveHigh
		if p.Window == "weekly" {
			window, high = "week", r.WeekHigh
		}
		if r.Pending != nil && (r.Pending.Key != p.Key || r.Pending.Window != window || r.Pending.Baseline != p.Baseline) {
			return false, errors.New("conflicting ZCode reset debts require manual reconciliation")
		}
		if r.Pending == nil {
			r.Pending = &zcodeResetClaim{Key: p.Key, Window: window, Generation: "legacy:" + scope, Baseline: p.Baseline, HighWater: high}
		}
	}
	if old.OpportunityKey != "" {
		if r.OpportunityKey != "" && r.OpportunityKey != old.OpportunityKey {
			return false, errors.New("conflicting ZCode opportunity claims require reconciliation")
		}
		r.OpportunityKey = old.OpportunityKey
	}
	if !old.OpportunityAt.IsZero() {
		r.OpportunityAt = max(r.OpportunityAt, old.OpportunityAt.UnixMilli())
	}
	if !old.OpportunityStarted.IsZero() && old.OpportunityClaim != "" {
		r.OpportunityAt = max(r.OpportunityAt, old.OpportunityStarted.Add(5*time.Minute).UnixMilli())
	}
	if _, err := tx.Exec("UPDATE legacy_zcode_reset SET mapped_scope=? WHERE scope_key=? AND mapped_scope=''", zcodeScopeName(c), scope); err != nil {
		return false, err
	}
	return true, nil
}

func unmappedLegacyZCodeResetDebt(tx *sql.Tx) (bool, error) {
	rows, err := tx.Query("SELECT payload FROM legacy_zcode_reset WHERE mapped_scope=''")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var r legacyZCodeReset
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		if json.Unmarshal(raw, &r) != nil || !validLegacyZCodeReset(r) {
			return false, errors.New("legacy ZCode reset debt is invalid")
		}
		if r.Pending != nil || r.OpportunityKey != "" || r.OpportunityClaim != "" {
			return true, nil
		}
	}
	return false, rows.Err()
}
