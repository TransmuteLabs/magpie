package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	_ "github.com/yetone/magpie/internal/sqlitedb"
)

// ZCode account state contains no vendor credentials. The opaque scope is
// stable across logins; generation belongs to each claim, not the filename.
type zcodeJournal struct {
	Version         int                          `json:"version"`
	Scope           string                       `json:"scope"`
	Origin          string                       `json:"origin"`
	Reset           zcodeResetJournal            `json:"reset"`
	Claims          map[string]zcodeBillingClaim `json:"claims,omitempty"`
	Executions      map[string]zcodeExecution    `json:"executions,omitempty"`
	DrawAfter       int64                        `json:"drawAfter,omitempty"`
	LegacyResetDebt bool                         `json:"-"`
}

var zcodeAccountRPC = plugin.AccountOperation

type zcodeAccountExpectation struct {
	scope      string
	generation string
}

type zcodeAccountExpectationKey struct{}

// WithZCodeAccountExpectation binds a control operation to the account
// selection and login generation presented to its caller.
func WithZCodeAccountExpectation(ctx context.Context, scope, generation string) context.Context {
	return context.WithValue(ctx, zcodeAccountExpectationKey{}, zcodeAccountExpectation{scope: scope, generation: generation})
}

func expectedZCodeAccount(ctx context.Context) (zcodeAccountExpectation, bool) {
	expectation, ok := ctx.Value(zcodeAccountExpectationKey{}).(zcodeAccountExpectation)
	return expectation, ok
}

// ZCodeAccountChanged reports an identity fence failure, including one
// wrapped by a coordinator.
func ZCodeAccountChanged(err error) bool {
	var accountErr *plugin.AccountError
	if !errors.As(err, &accountErr) {
		return false
	}
	switch accountErr.Kind {
	case "account_changed", "account_scope_changed", "account_generation_changed", "scope_changed":
		return true
	}
	return false
}

func zcodeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("invalid native account origin")
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if scheme != "https" && !(scheme == "http" && (host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())) {
		return "", errors.New("native account origin must use HTTPS or explicit loopback HTTP")
	}
	port := u.Port()
	if port == "443" && scheme == "https" || port == "80" && scheme == "http" {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

func zcodeIdentity(c plugin.AccountContext) (plugin.AccountContext, error) {
	origin, err := zcodeOrigin(c.Origin)
	if err != nil {
		return c, err
	}
	if c.Scope == "" || c.Generation == "" || c.Family == "" || c.Kind == "" || c.Kind != "legacy-key" && c.UserID == "" {
		return c, errors.New("native account identity is unavailable; sign in again")
	}
	if c.Kind == "team" && (c.Organization == "" || c.Project == "" || c.Product == "") {
		return c, errors.New("native team selection is incomplete")
	}
	c.Origin = origin
	return c, nil
}

func zcodeContext(ctx context.Context, providerID, accountKey string) (plugin.AccountContext, error) {
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(accountKey) == "" {
		return plugin.AccountContext{}, errors.New("name both the provider and the account")
	}
	ctx, err := ViaPluginAccount(ctx, providerID, accountKey)
	if err != nil {
		return plugin.AccountContext{}, err
	}
	request := plugin.AccountRequest{Operation: "context"}
	expectation, expected := expectedZCodeAccount(ctx)
	if expected {
		if expectation.scope == "" || expectation.generation == "" {
			return plugin.AccountContext{}, &plugin.AccountError{Kind: "invalid_context", Message: "both account scope and generation are required", DispatchedKnown: true}
		}
		request.Scope, request.Generation = expectation.scope, expectation.generation
	}
	r, err := zcodeAccountRPC(ctx, providerID, accountKey, request)
	if err != nil {
		if expected && ZCodeAccountChanged(err) {
			var accountErr *plugin.AccountError
			if errors.As(err, &accountErr) {
				// Only the context preflight ran; the requested operation has
				// not been dispatched, regardless of the context hook's flag.
				refusal := *accountErr
				refusal.Dispatched, refusal.DispatchedKnown = false, true
				return plugin.AccountContext{}, &refusal
			}
		}
		return plugin.AccountContext{}, err
	}
	if expected && r.Context.Scope != expectation.scope {
		return plugin.AccountContext{}, &plugin.AccountError{Kind: "account_scope_changed", Message: "native account scope changed", DispatchedKnown: true}
	}
	if expected && r.Context.Generation != expectation.generation {
		return plugin.AccountContext{}, &plugin.AccountError{Kind: "account_generation_changed", Message: "native account login generation changed", DispatchedKnown: true}
	}
	return zcodeIdentity(r.Context)
}

func zcodeAccountCall(ctx context.Context, providerID, accountKey string, c plugin.AccountContext, operation string, params any) (json.RawMessage, error) {
	ctx, err := ViaPluginAccount(ctx, providerID, accountKey)
	if err != nil {
		return nil, err
	}
	r, err := zcodeAccountRPC(ctx, providerID, accountKey, plugin.AccountRequest{Operation: operation, Params: params, Scope: c.Scope, Generation: c.Generation})
	if err != nil {
		return nil, err
	}
	if operation != "selection.select" && r.Context.Scope != c.Scope {
		return nil, &plugin.AccountError{Kind: "account_scope_changed", Message: "native account scope changed during the operation"}
	}
	if operation != "selection.select" && r.Context.Generation != c.Generation {
		return nil, &plugin.AccountError{Kind: "account_generation_changed", Message: "native account login generation changed during the operation"}
	}
	return r.Data, nil
}

func zcodeJournalPath(_ plugin.AccountContext) string {
	return filepath.Join(settings.Dir(), "native-account-state.sqlite")
}

func zcodeScopeName(c plugin.AccountContext) string {
	h := sha256.Sum256([]byte(c.Origin + "\x00" + c.Scope))
	return hex.EncodeToString(h[:])
}

func zcodeJournalKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

const zcodeJournalSchema = `CREATE TABLE native_journal (
	scope_key TEXT PRIMARY KEY NOT NULL,
	origin TEXT NOT NULL,
	scope TEXT NOT NULL,
	version INTEGER NOT NULL,
	payload BLOB NOT NULL
)`

const zcodeLegacyResetSchema = `CREATE TABLE legacy_zcode_reset (
	scope_key TEXT PRIMARY KEY NOT NULL,
	payload BLOB NOT NULL,
	mapped_scope TEXT NOT NULL
)`

// openZCodeJournal is called under the cross-process store fence. A missing
// file can be initialized once; an existing file without our exact schema
// is never interpreted as an empty history.
func openZCodeJournal(path string) (*sql.DB, error) {
	info, err := os.Lstat(path)
	created := false
	if errors.Is(err, os.ErrNotExist) {
		// A tombstone means this journal already owned financial state. Its
		// loss must not be mistaken for a new account with no prior debt.
		if b, readErr := os.ReadFile(legacyZCodeResetPath()); readErr == nil {
			var header struct {
				Version int `json:"version"`
			}
			if json.Unmarshal(b, &header) != nil || header.Version == 2 {
				return nil, errors.New("ZCode account journal is missing after reset cutover")
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return nil, readErr
		}
		for _, suffix := range []string{"-journal", "-wal", "-shm"} {
			if _, err := os.Lstat(path + suffix); err == nil || !errors.Is(err, os.ErrNotExist) {
				return nil, errors.New("native account journal is missing but recovery files remain")
			}
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("creating native account journal: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		created = true
	} else if err != nil {
		return nil, fmt.Errorf("native account journal unavailable: %w", err)
	} else {
		// Windows reports permissions from the read-only attribute, not the
		// file's ACL, so Unix permission bits cannot establish privacy there.
		if !info.Mode().IsRegular() || runtime.GOOS != "windows" && (info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o600 != 0o600) {
			return nil, errors.New("native account journal is not a private regular file")
		}
	}
	u := url.URL{Scheme: "file", Path: sqlitePath(path), RawQuery: "mode=rw&_pragma=busy_timeout(3000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*sql.DB, error) { db.Close(); return nil, err }
	if !created {
		if err := verifyZCodeJournal(db, true); err != nil {
			return fail(err)
		}
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&mode); err != nil || !strings.EqualFold(mode, "delete") {
		return fail(errors.New("native account journal cannot use rollback journal mode"))
	}
	for _, pragma := range []string{"PRAGMA synchronous=EXTRA", "PRAGMA fullfsync=ON"} {
		if _, err := db.Exec(pragma); err != nil {
			return fail(fmt.Errorf("configuring native account journal: %w", err))
		}
	}
	var synchronous, fullsync int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 3 {
		return fail(errors.New("native account journal cannot use EXTRA synchronization"))
	}
	if err := db.QueryRow("PRAGMA fullfsync").Scan(&fullsync); err != nil && !errors.Is(err, sql.ErrNoRows) || err == nil && fullsync != 1 {
		return fail(errors.New("native account journal cannot enable fullfsync"))
	}
	if created {
		tx, err := db.Begin()
		if err != nil {
			return fail(err)
		}
		for _, q := range []string{zcodeJournalSchema, zcodeLegacyResetSchema, "PRAGMA application_id=1296518721", "PRAGMA user_version=2"} {
			if _, err := tx.Exec(q); err != nil {
				tx.Rollback()
				return fail(fmt.Errorf("initializing native account journal: %w", err))
			}
		}
		if err := tx.Commit(); err != nil {
			return fail(fmt.Errorf("committing native account journal schema: %w", err))
		}
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version == 1 {
		tx, err := db.Begin()
		if err != nil {
			return fail(err)
		}
		for _, q := range []string{zcodeLegacyResetSchema, "PRAGMA user_version=2"} {
			if _, err := tx.Exec(q); err != nil {
				tx.Rollback()
				return fail(fmt.Errorf("upgrading ZCode account journal: %w", err))
			}
		}
		if err := tx.Commit(); err != nil {
			return fail(err)
		}
	}
	if err := verifyZCodeJournal(db, false); err != nil {
		return fail(err)
	}
	return db, nil
}

func verifyZCodeJournal(db *sql.DB, allowOld bool) error {
	var appID, version int
	var schema, integrity string
	if err := db.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
		return fmt.Errorf("reading native account journal: %w", err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading native account journal: %w", err)
	}
	if err := db.QueryRow("SELECT sql FROM sqlite_schema WHERE type='table' AND name='native_journal'").Scan(&schema); err != nil {
		return fmt.Errorf("native account journal schema unavailable: %w", err)
	}
	if err := db.QueryRow("PRAGMA quick_check(1)").Scan(&integrity); err != nil {
		return fmt.Errorf("checking native account journal: %w", err)
	}
	if appID != 1296518721 || version != 2 && !(allowOld && version == 1) || schema != zcodeJournalSchema || integrity != "ok" {
		return errors.New("native account journal is corrupt or has an unexpected schema")
	}
	var triggers int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND tbl_name='native_journal'").Scan(&triggers); err != nil || triggers != 0 {
		return errors.New("native account journal has unexpected triggers")
	}
	if version == 2 {
		if err := db.QueryRow("SELECT sql FROM sqlite_schema WHERE type='table' AND name='legacy_zcode_reset'").Scan(&schema); err != nil || schema != zcodeLegacyResetSchema {
			return errors.New("ZCode legacy reset journal has an unexpected schema")
		}
		if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND tbl_name='legacy_zcode_reset'").Scan(&triggers); err != nil || triggers != 0 {
			return errors.New("ZCode legacy reset journal has unexpected triggers")
		}
	}
	return nil
}

// withZCodeJournal commits each scope's nonsecret state in SQLite before
// returning. No database transaction or store fence spans vendor I/O.
func withZCodeJournal(c plugin.AccountContext, change func(*zcodeJournal) (bool, error)) error {
	path := zcodeJournalPath(c)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	oldUnlock, oldHeld := filelock.Lock(legacyZCodeResetPath()+".lock", filelock.Wait)
	defer oldUnlock()
	if !oldHeld {
		return errors.New("legacy ZCode reset state is locked; no operation was started")
	}
	unlock, held := filelock.Lock(path+".lock", filelock.Wait)
	defer unlock()
	if !held {
		return errors.New("native account journal is locked or unavailable; no operation was started")
	}
	db, err := openZCodeJournal(path)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := fenceLegacyZCodeResets(db); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("starting native account journal transaction: %w", err)
	}
	defer tx.Rollback()
	j := zcodeJournal{Version: 1, Scope: c.Scope, Origin: c.Origin}
	var origin, scope string
	var version int
	var b []byte
	err = tx.QueryRow("SELECT origin, scope, version, payload FROM native_journal WHERE scope_key=?", zcodeScopeName(c)).Scan(&origin, &scope, &version, &b)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading native account journal: %w", err)
	}
	if err == nil {
		var header struct {
			Reset json.RawMessage `json:"reset"`
		}
		if json.Unmarshal(b, &header) != nil || len(header.Reset) == 0 || string(header.Reset) == "null" {
			return errors.New("native account journal is corrupt or incomplete")
		}
		var saved zcodeJournal
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&saved); err != nil {
			return fmt.Errorf("native account journal is corrupt: %w", err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF || version != 1 || origin != c.Origin || scope != c.Scope ||
			saved.Scope != c.Scope || saved.Origin != c.Origin || !saved.valid() {
			return errors.New("native account journal is corrupt or belongs to another account")
		}
		j = saved
	}
	imported, err := adoptLegacyZCodeReset(tx, c, &j)
	if err != nil {
		return err
	}
	j.LegacyResetDebt, err = unmappedLegacyZCodeResetDebt(tx)
	if err != nil {
		return err
	}
	dirty, err := change(&j)
	dirty = dirty || imported
	if err != nil || !dirty {
		return err
	}
	if !j.valid() {
		return errors.New("refusing to write invalid native account journal")
	}
	b, err = json.Marshal(j)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO native_journal(scope_key, origin, scope, version, payload) VALUES(?,?,?,?,?)
		ON CONFLICT(scope_key) DO UPDATE SET origin=excluded.origin, scope=excluded.scope, version=excluded.version, payload=excluded.payload`,
		zcodeScopeName(c), c.Origin, c.Scope, 1, b); err != nil {
		return fmt.Errorf("saving native account journal: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing native account journal: %w", err)
	}
	return nil
}

// zcodeFence prevents a live owner from being mistaken for an abandoned
// claim while the journal lock is released for network I/O.
func zcodeFence(c plugin.AccountContext, kind string) (func(), error) {
	path := filepath.Join(settings.Dir(), "native-account-"+zcodeScopeName(c)+"."+kind+".lock")
	unlock, held := filelock.Lock(path, 0)
	if !held {
		unlock()
		return nil, errors.New("another native account operation is still in flight")
	}
	return unlock, nil
}

func (j *zcodeJournal) valid() bool {
	if j.Version != 1 || j.Scope == "" || j.Origin == "" || !j.Reset.valid() || !validZCodeBillingClaims(j.Claims) || !validZCodeExecutions(j.Executions, j.DrawAfter) {
		return false
	}
	for _, execution := range j.Executions {
		if execution.Scope != j.Scope {
			return false
		}
	}
	return true
}
