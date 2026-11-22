package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	savedSignInsDir  = ".saved-signins"
	savedSignInsFile = "records.json"
	savedSignInLimit = 32
)

func admitSavedSignInStart(ctx context.Context, raw []byte) error {
	if _, ok := ctx.Value(savedSignInStateKey{}).(*savedSignInState); ok {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := catalogDecode(raw, &fields); err != nil {
		return errors.New("invalid authentication start request")
	}
	for field, value := range fields {
		var method string
		if strings.EqualFold(field, "method") && json.Unmarshal(value, &method) == nil && method == sdk.SavedSignInMethod {
			return errors.New("saved sign-ins require the local import command")
		}
	}
	return nil
}

type SavedSignInRecord struct {
	Raw         json.RawMessage
	Credentials json.RawMessage
}

type SavedSignInSource func(ctx context.Context, provider, key string, use func(SavedSignInRecord) error) error

func CheckSavedSignInDiscovery(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	off, err := settings.AccountDiscoveryOffNow()
	if err != nil {
		return fmt.Errorf("subscription account discovery settings could not be read: %w", err)
	}
	if off {
		return errors.New("subscription accounts are off in magpie (Settings › Privacy, or `magpie accounts discovery on`)")
	}
	return ctx.Err()
}

type PreparedSignIn struct {
	Ticket  string             `json:"ticket"`
	Account sdk.AccountSummary `json:"account"`
}

type savedSignIn struct {
	OwnerRevision string         `json:"ownerRevision,omitempty"`
	Ticket        string         `json:"ticket"`
	Module        string         `json:"module"`
	Provider      string         `json:"provider"`
	Key           string         `json:"key"`
	Digest        string         `json:"digest"`
	Revision      string         `json:"revision"`
	Deadline      int64          `json:"deadline"`
	Generation    uint64         `json:"generation"`
	Result        sdk.AuthResult `json:"result"`
}

func (m *Manager) savedSignInGrant(ctx context.Context, module, provider string) (ProviderSignIn, sdk.ConfigSnapshot, Installed, error) {
	for _, target := range m.ProviderSignIns(ctx) {
		if target.Module != module || target.Provider != provider || target.Page {
			continue
		}
		if slices.ContainsFunc(target.Binding.Methods, func(method sdk.WindowAuthMethod) bool {
			return method.ID == sdk.SavedSignInMethod
		}) {
			entry, config, err := m.authEntry(ctx, module, target.Revision, sdk.ProviderSignInPart)
			return target, config, entry, err
		}
	}
	return ProviderSignIn{}, sdk.ConfigSnapshot{}, Installed{}, errors.New("this provider does not offer saved sign-in import")
}

func savedSignInAuthority(target ProviderSignIn, entry Installed, config sdk.ConfigSnapshot) windowAuthRecord {
	part, revision := windowAuthJournalSelection(entry, config.Version, sdk.ProviderSignInPart)
	return windowAuthRecord{Binding: target.Binding.ID, Part: part, Revision: revision, Config: config.Version,
		Scope: sdk.Scope{Plugin: target.Module, Provider: target.Provider}}
}

func (m *Manager) PrepareSavedSignIn(ctx context.Context, module, provider, key string, source SavedSignInSource) (PreparedSignIn, error) {
	if !storeToken(key) {
		return PreparedSignIn{}, errors.New("an exact saved account key is required")
	}
	target, config, entry, err := m.savedSignInGrant(ctx, module, provider)
	if err != nil {
		return PreparedSignIn{}, err
	}
	var input json.RawMessage
	var digest string
	if err := source(ctx, provider, key, func(record SavedSignInRecord) error {
		if !utf8.Valid(record.Raw) || !utf8.Valid(record.Credentials) {
			return errors.New("the selected saved plugin sign-in contains invalid UTF-8")
		}
		digest = savedSignInDigest(record.Raw)
		if digest == "" {
			return errors.New("the selected saved plugin sign-in is invalid")
		}
		var err error
		input, err = json.Marshal(map[string]string{"record": string(record.Credentials)})
		return err
	}); err != nil {
		return PreparedSignIn{}, err
	}
	authority := savedSignInAuthority(target, entry, config)
	lease, err := m.acquireLease(ctx, module, &expectedGeneration{
		packageRevision: entry.Revision, configVersion: config.Version, enableGeneration: &entry.EnableGeneration,
	})
	if err != nil {
		return PreparedSignIn{}, errors.New("saved sign-in backend is unavailable")
	}
	defer lease.Close()
	ctx = context.WithValue(ctx, windowAdmissionKey{}, func(current context.Context) error {
		return m.withWindowAuthAuthority(current, authority, func() error { return nil })
	})
	result, err := verifySavedSignIn(ctx, lease, authority.Scope, input)
	if err != nil {
		return PreparedSignIn{}, err
	}
	ticket, err := randomID()
	if err != nil {
		return PreparedSignIn{}, err
	}
	candidate := savedSignIn{
		Ticket: ticket, Module: module, Provider: provider, Key: key, Digest: digest, Revision: target.Revision,
		Deadline: time.Now().Add(10 * time.Minute).UnixMilli(), Result: result,
	}
	err = m.withWindowAuthAuthority(ctx, authority, func() error {
		publish := func(private *os.Root) error {
			tickets, err := storeDirectory(private, savedSignInsDir)
			if err != nil {
				return err
			}
			defer tickets.Close()
			root, err := storeDirectory(private, module)
			if err != nil {
				return err
			}
			defer root.Close()
			records, err := storeReadAccounts(root, module)
			if err != nil {
				return err
			}
			rows, err := readSubscriptionRecords(tickets)
			if err != nil {
				return err
			}
			owner := slices.IndexFunc(rows, func(row subscriptionRecord) bool {
				return row.Module == module && row.Provider == provider
			})
			if owner >= 0 {
				candidate.OwnerRevision = rows[owner].Revision
			}
			if current, found := savedSignInAccount(records, candidate); found {
				candidate.Generation = current.Generation
				if current.Deleted && current.Generation == ^uint64(0) {
					return errors.New("saved sign-in cannot replace an existing account")
				}
				if !current.Deleted {
					if owner < 0 || rows[owner].hiddenAccount(candidate) < 0 {
						return errors.New("saved sign-in cannot replace an existing account")
					}
					if current.Generation > 0 && candidate.matches(current) {
						candidate.Generation--
					} else if !rows[owner].canReplaceSavedSignIn(candidate, current) {
						return errors.New("saved sign-in cannot replace an existing account")
					}
				}
			}
			saved, err := readSavedSignIns(tickets)
			if err != nil {
				return err
			}
			now := time.Now().UnixMilli()
			saved = slices.DeleteFunc(saved, func(other savedSignIn) bool {
				return other.expired(now) || other.Module == module && other.Provider == provider && other.Key == key
			})
			if len(saved) >= savedSignInLimit {
				return errors.New("saved sign-in preparation capacity is occupied")
			}
			if candidate.expired(time.Now().UnixMilli()) {
				return errors.New("the verified saved sign-in has expired")
			}
			_, err = storePublishChecked(ctx, tickets, savedSignInsFile, append(saved, candidate), storeFileLimit,
				func() error { return CheckSavedSignInDiscovery(ctx) })
			return err
		}
		return candidate.withSource(ctx, source, func() error {
			return m.store.withPrivateStore(ctx, publish)
		})
	})
	if err != nil {
		return PreparedSignIn{}, err
	}
	return PreparedSignIn{Ticket: ticket, Account: candidate.summary()}, nil
}

func verifySavedSignIn(ctx context.Context, lease *Lease, scope sdk.Scope, input json.RawMessage) (sdk.AuthResult, error) {
	ctx = context.WithValue(ctx, savedSignInStateKey{}, &savedSignInState{values: make(map[string]VersionedValue)})
	call := func(method string, params, out any) error {
		raw, err := lease.Call(ctx, method, scope, params, func(Event) error {
			return errors.New("saved sign-in verification requires a unary result")
		})
		var failure *sdk.Error
		if errors.As(err, &failure) && failure.Code == "reauthorization_required" {
			return &sdk.Error{Code: failure.Code, Message: "This saved sign-in requires interactive authorization", Outcome: "not-started"}
		}
		if err != nil || json.Unmarshal(raw, out) != nil {
			return errors.New("the SDK provider could not verify the saved sign-in")
		}
		return nil
	}
	var start sdk.AuthStartResult
	if err := call(sdk.MethodAuthStart, sdk.AuthStartRequest{Method: sdk.SavedSignInMethod, Input: input}, &start); err != nil {
		return sdk.AuthResult{}, err
	}
	if start.URL != "" || start.UserCode != "" || start.PollAfterMs != 0 || !savedSignInJSON(start.Continuation) {
		return sdk.AuthResult{}, errors.New("this saved sign-in requires interactive authorization")
	}
	var result sdk.AuthResult
	if err := call(sdk.MethodAuthFinish, sdk.AuthFinishRequest{Continuation: start.Continuation}, &result); err != nil {
		return sdk.AuthResult{}, err
	}
	if !validSavedSignInResult(result) || result.ExpiresAt != 0 && result.ExpiresAt <= time.Now().UnixMilli() {
		return sdk.AuthResult{}, errors.New("the SDK provider returned an invalid saved sign-in")
	}
	if !storeToken(result.Name) {
		result.Name = ""
	}
	return result, nil
}

func (m *Manager) ActivateSavedSignIn(ctx context.Context, module, provider, ticket string, source SavedSignInSource, register func(sdk.AccountSummary) error) (sdk.AccountSummary, error) {
	target, config, entry, err := m.savedSignInGrant(ctx, module, provider)
	if err != nil {
		return sdk.AccountSummary{}, err
	}
	var summary sdk.AccountSummary
	err = m.withWindowAuthAuthority(ctx, savedSignInAuthority(target, entry, config), func() error {
		var selected savedSignIn
		if err := m.store.withReadStoreContext(ctx, savedSignInsDir, func(tickets *os.Root) error {
			var err error
			selected, err = activatableSavedSignIn(tickets, target, ticket)
			return err
		}); err != nil {
			return err
		}
		return selected.withSource(ctx, source, func() error {
			return m.store.withPrivateStore(ctx, func(private *os.Root) error {
				tickets, err := storeDirectory(private, savedSignInsDir)
				if err != nil {
					return err
				}
				defer tickets.Close()
				candidate, err := activatableSavedSignIn(tickets, target, ticket)
				if err != nil {
					return err
				}
				if candidate.Key != selected.Key || candidate.Digest != selected.Digest {
					return errors.New("the selected saved plugin sign-in changed")
				}
				root, err := storeDirectory(private, module)
				if err != nil {
					return err
				}
				defer root.Close()
				if candidate.expired(time.Now().UnixMilli()) {
					return errors.New("prepared sign-in expired before activation")
				}
				account, err := activateSavedSignIn(ctx, root, candidate, false)
				if account.ID != "" {
					summary = candidate.summary()
				}
				return err
			})
		})
	})
	if summary.ID == "" {
		return summary, err
	}
	defer notifyChanged()
	if err != nil {
		return summary, err
	}
	if err := register(summary); err != nil {
		return summary, err
	}
	err = m.store.withPrivateStore(ctx, func(private *os.Root) error {
		tickets, err := storeDirectory(private, savedSignInsDir)
		if err != nil {
			return err
		}
		defer tickets.Close()
		return consumeSubscriptionTickets(tickets, []string{ticket})
	})
	return summary, err
}

func activatableSavedSignIn(tickets *os.Root, target ProviderSignIn, ticket string) (savedSignIn, error) {
	unavailable := errors.New("prepared sign-in is unavailable or expired")
	if tickets == nil {
		return savedSignIn{}, unavailable
	}
	owners, err := readSubscriptionRecords(tickets)
	if err != nil {
		return savedSignIn{}, err
	}
	if slices.ContainsFunc(owners, func(r subscriptionRecord) bool { return r.Module == target.Module && r.Provider == target.Provider }) {
		return savedSignIn{}, errors.New("this SDK provider is reserved for a whole-subscription transition")
	}
	saved, err := readSavedSignIns(tickets)
	if err != nil {
		return savedSignIn{}, err
	}
	now := time.Now().UnixMilli()
	index := slices.IndexFunc(saved, func(candidate savedSignIn) bool {
		return candidate.Module == target.Module && candidate.Provider == target.Provider && candidate.Ticket == ticket && !candidate.expired(now)
	})
	if index < 0 {
		return savedSignIn{}, unavailable
	}
	candidate := saved[index]
	if candidate.Revision != target.Revision {
		return savedSignIn{}, errors.New("prepared sign-in's package selection changed")
	}
	return candidate, nil
}

func (candidate savedSignIn) withSource(ctx context.Context, source SavedSignInSource, use func() error) error {
	return source(ctx, candidate.Provider, candidate.Key, func(record SavedSignInRecord) error {
		if savedSignInDigest(record.Raw) != candidate.Digest {
			return errors.New("the selected saved plugin sign-in changed")
		}
		return use()
	})
}

func savedSignInDigest(record json.RawMessage) string {
	record = bytes.TrimSpace(record)
	if len(record) == 0 || record[0] != '{' || !utf8.Valid(record) {
		return ""
	}
	stable, err := json.Marshal(record)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(stable)
	return hex.EncodeToString(digest[:])
}

func (candidate savedSignIn) summary() sdk.AccountSummary {
	return sdk.AccountSummary{ID: candidate.Result.ID, Name: candidate.Result.Name, ExpiresAt: candidate.Result.ExpiresAt,
		Generation: strconv.FormatUint(candidate.Generation+1, 10)}
}

func savedSignInAccount(records storeAccounts, candidate savedSignIn) (storeAccount, bool) {
	for _, account := range records.Accounts {
		if account.Provider == candidate.Provider && account.ID == candidate.Result.ID {
			return account, true
		}
	}
	return storeAccount{}, false
}

func (candidate savedSignIn) matches(current storeAccount) bool {
	return current.ExpiresAt == candidate.Result.ExpiresAt && bytes.Equal(current.Auth, candidate.Result.Credentials)
}

func activateSavedSignIn(ctx context.Context, root *os.Root, candidate savedSignIn, replace bool) (Account, error) {
	records, err := storeReadAccounts(root, candidate.Module)
	if err != nil {
		return Account{}, err
	}
	current, found := savedSignInAccount(records, candidate)
	account := Account{Plugin: candidate.Module, Provider: candidate.Provider, ID: candidate.Result.ID,
		Name: candidate.Result.Name, Auth: candidate.Result.Credentials, ExpiresAt: candidate.Result.ExpiresAt}
	if found && !current.Deleted && current.Generation == candidate.Generation+1 && candidate.matches(current) {
		account.Generation = current.Generation
		if current.namedAccount(root).Name == "" && CheckSavedSignInDiscovery(ctx) == nil {
			account.writeLabel(root)
		}
		return account, storeSync(root)
	}
	if current.Generation != candidate.Generation || found && !current.Deleted && !replace {
		return Account{}, errors.New("the selected SDK account generation changed")
	}
	check := func() error { return CheckSavedSignInDiscovery(ctx) }
	if found && !current.Deleted {
		return storeSaveAccount(ctx, root, records, account, candidate.Generation, check)
	}
	return storeSaveAccount(ctx, root, records, account, 0, check)
}

func readSavedSignIns(root *os.Root) ([]savedSignIn, error) {
	var saved []savedSignIn
	_, err := storeRead(root, savedSignInsFile, storeFileLimit, &saved)
	if err != nil {
		return nil, err
	}
	if len(saved) > savedSignInLimit {
		return nil, storeUnreadable()
	}
	for _, entry := range saved {
		if !storeToken(entry.Ticket) || !storePluginID(entry.Module) || !sdk.ValidID(entry.Provider) || !storeToken(entry.Key) ||
			len(entry.Digest) != 64 || len(entry.Revision) != 64 || entry.Deadline <= 0 ||
			entry.Generation == ^uint64(0) || !validSavedSignInResult(entry.Result) ||
			entry.Result.Name != "" && !storeToken(entry.Result.Name) {
			return nil, storeUnreadable()
		}
	}
	return saved, nil
}

func (candidate savedSignIn) expired(now int64) bool {
	return candidate.Deadline <= now || candidate.Result.ExpiresAt != 0 && candidate.Result.ExpiresAt <= now
}

func activeSavedSignIns(root *os.Root, now int64) ([]savedSignIn, error) {
	saved, err := readSavedSignIns(root)
	if err != nil {
		return nil, err
	}
	count := len(saved)
	saved = slices.DeleteFunc(saved, func(candidate savedSignIn) bool { return candidate.expired(now) })
	if len(saved) != count {
		err = writeSavedSignIns(root, saved)
	}
	return saved, err
}

func writeSavedSignIns(root *os.Root, saved []savedSignIn) error {
	if len(saved) == 0 {
		if err := root.Remove(savedSignInsFile); err != nil {
			return err
		}
		return storeSync(root)
	}
	return storeWrite(root, savedSignInsFile, saved, storeFileLimit)
}

func (s *Store) reapSavedSignIns(ctx context.Context) error {
	if _, err := os.Lstat(filepath.Join(s.root, "private", savedSignInsDir, savedSignInsFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return s.withPrivateStore(ctx, func(private *os.Root) error {
		tickets, err := storeDirectory(private, savedSignInsDir)
		if err != nil {
			return err
		}
		defer tickets.Close()
		_, err = activeSavedSignIns(tickets, time.Now().UnixMilli())
		return err
	})
}

type savedSignInStateKey struct{}

type savedSignInState struct {
	mu     sync.Mutex
	values map[string]VersionedValue
}

func (state *savedSignInState) get(ctx context.Context, scope sdk.Scope, key string) (VersionedValue, error) {
	if !storeValidScope(scope) || !storeToken(key) {
		return VersionedValue{}, storeInvalid()
	}
	if err := filelock.LockMutex(ctx, &state.mu); err != nil {
		return VersionedValue{}, err
	}
	defer state.mu.Unlock()
	return state.values[storeStateName(scope, key)], nil
}

func (state *savedSignInState) cas(ctx context.Context, scope sdk.Scope, key string, version uint64, value json.RawMessage) (VersionedValue, error) {
	if !storeValidScope(scope) || !storeToken(key) {
		return VersionedValue{}, storeInvalid()
	}
	if len(value) > 64<<10 || !json.Valid(value) {
		return VersionedValue{}, storeError("invalid_state", "Saved sign-in state must be bounded JSON.")
	}
	if err := filelock.LockMutex(ctx, &state.mu); err != nil {
		return VersionedValue{}, err
	}
	defer state.mu.Unlock()
	key = storeStateName(scope, key)
	current := state.values[key]
	if current.Version != version || version == ^uint64(0) {
		return VersionedValue{}, storeError("conflict", "Saved sign-in state version changed.")
	}
	if version == 0 && len(state.values) >= savedSignInLimit {
		return VersionedValue{}, storeError("invalid_state", "Saved sign-in state capacity is occupied.")
	}
	next := VersionedValue{Version: version + 1, Value: bytes.Clone(value)}
	if err := ctx.Err(); err != nil {
		return VersionedValue{}, err
	}
	state.values[key] = next
	return next, nil
}

func validSavedSignInResult(result sdk.AuthResult) bool {
	return !result.Pending && storeToken(result.ID) && result.ExpiresAt >= 0 && savedSignInJSON(result.Credentials)
}

func savedSignInJSON(raw json.RawMessage) bool {
	return len(raw) <= sdk.MaxHostBytes && json.Valid(raw) && string(bytes.TrimSpace(raw)) != "null"
}
