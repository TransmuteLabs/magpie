package extension

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	storeStateLimit = 1 << 20
	storeFileLimit  = 4 << 20
	storeTokenLimit = 256
)

type VersionedValue struct {
	Version uint64          `json:"version"`
	Value   json.RawMessage `json:"value"`
}

type Account struct {
	Plugin     string          `json:"plugin"`
	Provider   string          `json:"provider"`
	ID         string          `json:"id"`
	Name       string          `json:"-"`
	Generation uint64          `json:"generation"`
	Auth       json.RawMessage `json:"auth"`
	ExpiresAt  int64           `json:"expiresAt,omitempty"`
}

type AccountMetadata struct {
	ID         string
	Name       string
	Generation uint64
	ExpiresAt  int64
}

type AccountGeneration struct {
	ID         string
	Generation uint64
}

type JournalEntry struct {
	Scope   sdk.Scope `json:"scope"`
	State   string    `json:"state"`
	Updated time.Time `json:"updated"`
}

type Store struct {
	root                 string
	mu                   sync.Mutex
	accountSnapshots     [8]atomic.Pointer[storeReadSnapshot[storeAccounts]]
	largeAccountSnapshot atomic.Pointer[storeReadSnapshot[storeAccounts]]
	accountSyncMu        sync.Mutex
	accountSyncEnabled   bool
	accountSync          [8]*storeSyncConfirmation
	ownerSnapshot        atomic.Pointer[storeReadSnapshot[[]subscriptionRecord]]
}

type storeAccount struct {
	Account
	Deleted bool           `json:"deleted,omitempty"`
	Refresh *refreshRecord `json:"refresh,omitempty"`
	Usage   *usageState    `json:"usage,omitempty"`
}

type storeAccounts struct {
	Schema    int                `json:"schema"`
	Accounts  []storeAccount     `json:"accounts"`
	AuthFlows []windowAuthRecord `json:"authFlows,omitempty"`
}

func (records *storeAccounts) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return storeUnreadable()
	}
	var schema int
	if len(fields["schema"]) == 0 || bytes.Equal(bytes.TrimSpace(fields["schema"]), []byte("null")) ||
		json.Unmarshal(fields["schema"], &schema) != nil {
		return storeUnreadable()
	}
	if _, found := fields["authFlows"]; schema < 4 && found {
		return storeUnreadable()
	}
	if schema >= 4 {
		if len(fields["authFlows"]) == 0 || bytes.Equal(bytes.TrimSpace(fields["authFlows"]), []byte("null")) {
			return storeUnreadable()
		}
		probe := json.NewDecoder(bytes.NewReader(raw))
		probe.UseNumber()
		if catalogJSONValue(probe, 0) != nil || probe.Decode(new(any)) != io.EOF {
			return storeUnreadable()
		}
	}
	type plain storeAccounts
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return storeUnreadable()
	}
	*records = storeAccounts(value)
	return nil
}

func (records storeAccounts) MarshalJSON() ([]byte, error) {
	var flows *[]windowAuthRecord
	if records.Schema >= 4 {
		flows = &records.AuthFlows
	}
	return json.Marshal(struct {
		Schema    int                 `json:"schema"`
		Accounts  []storeAccount      `json:"accounts"`
		AuthFlows *[]windowAuthRecord `json:"authFlows,omitempty"`
	}{records.Schema, records.Accounts, flows})
}

type storeState struct {
	Scope sdk.Scope `json:"scope"`
	Key   string    `json:"key"`
	VersionedValue
}

func NewStore(root string) *Store {
	return &Store{root: root}
}

func (s *Store) StateGet(scope sdk.Scope, key string) (VersionedValue, error) {
	return s.StateGetContext(context.Background(), scope, key)
}

func (s *Store) StateGetContext(ctx context.Context, scope sdk.Scope, key string) (VersionedValue, error) {
	var result VersionedValue
	if !storeValidScope(scope) || !storeToken(key) {
		return result, storeInvalid()
	}
	err := s.withReadStoreContext(ctx, scope.Plugin, func(root *os.Root) error {
		if scope.Account != "" {
			records, err := s.readAccounts(root, scope.Plugin)
			if err != nil {
				return err
			}
			if err := checkAccountGeneration(records, scope); err != nil {
				return err
			}
		}
		record, err := storeReadState(root, scope, key)
		result = record.VersionedValue
		return err
	})
	if err != nil {
		return VersionedValue{}, err
	}
	return result, nil
}

func (s *Store) StateCAS(scope sdk.Scope, key string, expected uint64, value json.RawMessage) (VersionedValue, error) {
	return s.StateCASContext(context.Background(), scope, key, expected, value)
}

func (s *Store) StateCASContext(ctx context.Context, scope sdk.Scope, key string, expected uint64, value json.RawMessage) (VersionedValue, error) {
	var result VersionedValue
	if !storeValidScope(scope) || !storeToken(key) {
		return result, storeInvalid()
	}
	if len(value) > storeStateLimit || !json.Valid(value) {
		return result, storeError("invalid_state", "Extension state must be bounded JSON.")
	}
	err := s.withStoreContext(ctx, scope.Plugin, func(root *os.Root) error {
		if err := storeCheckGeneration(root, scope); err != nil {
			return err
		}
		var err error
		result, err = compareStoreState(ctx, root, scope, key, expected, value, false)
		return err
	})
	return result, err
}

func compareStoreState(ctx context.Context, root *os.Root, scope sdk.Scope, key string, expected uint64, value json.RawMessage, keepEqual bool) (VersionedValue, error) {
	record, err := storeReadState(root, scope, key)
	if err != nil {
		return VersionedValue{}, err
	}
	if record.Version != expected || expected == ^uint64(0) {
		return VersionedValue{}, storeError("conflict", "Extension state version changed.")
	}
	if keepEqual && bytes.Equal(record.Value, value) {
		return record.VersionedValue, ctx.Err()
	}
	record = storeState{
		Scope: storeStateScope(scope), Key: key,
		VersionedValue: VersionedValue{Version: expected + 1, Value: bytes.Clone(value)},
	}
	dir, err := storeDirectory(root, "state")
	if err != nil {
		return VersionedValue{}, err
	}
	defer dir.Close()
	if err := ctx.Err(); err != nil {
		return VersionedValue{}, err
	}
	if err := storeWrite(dir, storeStateName(scope, key), record, storeFileLimit); err != nil {
		return VersionedValue{}, err
	}
	return record.VersionedValue, nil
}

func (s *Store) Accounts(plugin, provider string) ([]Account, error) {
	return s.AccountsContext(context.Background(), plugin, provider)
}

func (s *Store) AccountsContext(ctx context.Context, plugin, provider string) ([]Account, error) {
	var result []Account
	if !storePluginID(plugin) || !storeToken(provider) {
		return nil, storeInvalid()
	}
	err := s.withReadStoreContext(ctx, plugin, func(root *os.Root) error {
		records, err := s.readAccounts(root, plugin)
		if err != nil {
			return err
		}
		result = []Account{}
		for _, record := range records.Accounts {
			if !record.Deleted && record.Provider == provider {
				account := record.namedAccount(root)
				account.Auth = bytes.Clone(account.Auth)
				result = append(result, account)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) AccountsMetadataContext(ctx context.Context, plugin string, providers []string, includeNames bool) (map[string][]AccountMetadata, error) {
	if !storePluginID(plugin) {
		return nil, storeInvalid()
	}
	for _, provider := range providers {
		if !storeToken(provider) {
			return nil, storeInvalid()
		}
	}
	var result map[string][]AccountMetadata
	err := s.withReadStoreContext(ctx, plugin, func(root *os.Root) error {
		records, err := s.readAccounts(root, plugin)
		if err != nil {
			return err
		}
		result = make(map[string][]AccountMetadata, len(providers))
		for _, record := range records.Accounts {
			if record.Deleted || providers != nil && !slices.Contains(providers, record.Provider) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			account := AccountMetadata{ID: record.ID, Generation: record.Generation, ExpiresAt: record.ExpiresAt}
			if includeNames {
				account.Name = record.namedAccount(root).Name
			}
			result[record.Provider] = append(result[record.Provider], account)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) AccountGenerationsContext(ctx context.Context, plugin, provider string) ([]AccountGeneration, error) {
	return projectProviderAccounts(ctx, s, plugin, provider, 0, func(_ *os.Root, record storeAccount) AccountGeneration {
		return AccountGeneration{ID: record.ID, Generation: record.Generation}
	})
}

func (s *Store) accountMetadataForProvider(ctx context.Context, plugin, provider string, limit int) ([]AccountMetadata, error) {
	return projectProviderAccounts(ctx, s, plugin, provider, limit, func(root *os.Root, record storeAccount) AccountMetadata {
		account := record.namedAccount(root)
		return AccountMetadata{ID: account.ID, Name: account.Name, Generation: account.Generation, ExpiresAt: account.ExpiresAt}
	})
}

func (s *Store) GetAccount(plugin, provider, id string) (Account, error) {
	return s.GetAccountContext(context.Background(), plugin, provider, id)
}

func (s *Store) GetAccountContext(ctx context.Context, plugin, provider, id string) (Account, error) {
	return s.readAccountContext(ctx, plugin, provider, id, nil, true)
}

func (s *Store) readAccountContext(ctx context.Context, plugin, provider, id string, generation *uint64, copyAuth bool) (Account, error) {
	var result Account
	if !storeAccountID(plugin, provider, id) {
		return result, storeInvalid()
	}
	err := s.withReadStoreContext(ctx, plugin, func(root *os.Root) error {
		records, err := s.readAccounts(root, plugin)
		if err != nil {
			return err
		}
		for _, record := range records.Accounts {
			if record.Provider == provider && record.ID == id && !record.Deleted {
				if generation != nil && record.Generation != *generation {
					return storeError("stale_generation", "Extension account generation is unavailable.")
				}
				result = record.Account
				if copyAuth {
					result.Auth = bytes.Clone(result.Auth)
				} else {
					result.Auth = nil
				}
				return nil
			}
		}
		return storeError("not_found", "Extension account is unavailable.")
	})
	if err != nil {
		return Account{}, err
	}
	return result, nil
}

func (s *Store) SaveAccount(account Account, expectedGeneration uint64) (Account, error) {
	return s.saveAccountContext(context.Background(), account, expectedGeneration, nil)
}

func (s *Store) saveAccountContext(ctx context.Context, account Account, expectedGeneration uint64, check func() error) (Account, error) {
	var result Account
	if !storeAccountID(account.Plugin, account.Provider, account.ID) {
		return result, storeInvalid()
	}
	if len(account.Auth) > storeFileLimit || !json.Valid(account.Auth) || account.ExpiresAt < 0 {
		return result, storeError("invalid_state", "Extension credentials must be bounded JSON.")
	}
	if !storeToken(account.Name) {
		account.Name = ""
	}
	err := s.withStoreContext(ctx, account.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, account.Plugin)
		if err != nil {
			return err
		}
		result, err = storeSaveAccount(ctx, root, records, account, expectedGeneration, check)
		return err
	})
	if err != nil {
		return Account{}, err
	}
	return result, err
}

func (s *Store) CheckAccountGenerationContext(ctx context.Context, scope sdk.Scope) error {
	if scope.Account == "" || scope.Generation == 0 || !storeValidScope(scope) {
		return storeInvalid()
	}
	return s.withReadStoreContext(ctx, scope.Plugin, func(root *os.Root) error {
		records, err := s.readAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		return checkAccountGeneration(records, scope)
	})
}

func storeSaveAccount(ctx context.Context, root *os.Root, records storeAccounts, account Account, expectedGeneration uint64, check func() error) (Account, error) {
	index := -1
	var generation uint64
	for position, record := range records.Accounts {
		if record.Provider == account.Provider && record.ID == account.ID {
			index = position
			generation = record.Generation
			if record.Deleted && expectedGeneration == 0 {
				break
			}
			if generation != expectedGeneration || record.Deleted {
				return Account{}, storeError("conflict", "Extension account generation changed.")
			}
			if account.Name == "" {
				account.Name = record.namedAccount(root).Name
			}
			break
		}
	}
	if index < 0 && expectedGeneration != 0 || generation == ^uint64(0) {
		return Account{}, storeError("conflict", "Extension account generation changed.")
	}
	account.Generation = generation + 1
	account.Auth = bytes.Clone(account.Auth)
	if index < 0 {
		record := storeAccount{Account: account}
		if records.Schema >= 3 {
			record.Usage = &usageState{}
		}
		records.Accounts = append(records.Accounts, record)
	} else {
		records.Accounts[index].Account = account
		records.Accounts[index].Deleted = false
	}
	published, err := storePublishChecked(ctx, root, "accounts.json", records, storeFileLimit, check)
	if !published {
		return Account{}, err
	}
	account.writeLabel(root)
	return account, err
}

func (s *Store) DeleteAccount(plugin, provider, id string, expectedGeneration uint64) error {
	if !storeAccountID(plugin, provider, id) {
		return storeInvalid()
	}
	return s.withStore(plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, plugin)
		if err != nil {
			return err
		}
		for index, record := range records.Accounts {
			if record.Provider != provider || record.ID != id {
				continue
			}
			if record.Deleted || record.Generation != expectedGeneration || expectedGeneration == ^uint64(0) {
				return storeError("conflict", "Extension account generation changed.")
			}
			record.Generation++
			record.Auth = nil
			record.ExpiresAt = 0
			record.Deleted = true
			records.Accounts[index] = record
			for flowIndex := range records.AuthFlows {
				flow := &records.AuthFlows[flowIndex]
				if flow.Scope.Provider != provider || flow.Scope.Account != "" {
					continue
				}
				switch flow.Phase {
				case "reserved", "waiting":
					flow.Phase, flow.Continuation = "canceled", nil
				case "starting", "polling":
				default:
					continue
				}
				flow.Canceled = true
			}
			if err := storeWrite(root, "accounts.json", records, storeFileLimit); err != nil {
				return err
			}
			_ = root.Remove(record.labelFile())
			return nil
		}
		return storeError("not_found", "Extension account is unavailable.")
	})
}

func (s *Store) Journal(scope sdk.Scope, state string) (JournalEntry, error) {
	return s.journal(context.Background(), scope, state, false)
}

func (s *Store) journal(ctx context.Context, scope sdk.Scope, state string, inference bool) (JournalEntry, error) {
	var result JournalEntry
	if !storeValidScope(scope) || !storeToken(scope.Operation) || !storeJournalState(state) && state != "canceled" {
		return result, storeInvalid()
	}
	err := s.withStoreContext(ctx, scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		if inference && state == "dispatched" && scope.Account != "" {
			account, err := usageAccount(&records, scope)
			if err != nil {
				return err
			}
			if usageStamp(*account).Dirty {
				return &sdk.Error{Code: "usage_refresh_required", Message: "A fresh account usage acknowledgment is required.", Outcome: "not-started"}
			}
		}
		if _, found := refreshOperation(records, scope.Operation); found {
			return storeError("operation_conflict", "Refresh receipts are owned by the account transaction.")
		}
		previous, found, err := storeReadOperation(root, scope.Plugin, scope.Operation)
		if err != nil {
			return err
		}
		if found && previous.Scope != scope {
			return storeJournalConflict(previous)
		}
		next, valid := storeTransition(previous.State, state)
		if !valid || !found && state != "dispatched" {
			return storeJournalConflict(previous)
		}
		if state == "dispatched" && scope.Account != "" {
			if err := checkAccountGeneration(records, scope); err != nil {
				return err
			}
		}
		if found && previous.State == next {
			result = previous
			return nil
		}
		dir, err := storeDirectory(root, "operations")
		if err != nil {
			return err
		}
		defer dir.Close()
		entry := JournalEntry{Scope: scope, State: next, Updated: time.Now().UTC()}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := storeWrite(dir, storeHash(scope.Operation)+".json", entry, storeFileLimit); err != nil {
			return &sdk.Error{Code: "store_write", Message: "Extension operation could not be persisted.", Outcome: "uncertain"}
		}
		result = entry
		return nil
	})
	return result, err
}

func (s *Store) Operation(plugin, operation string) (JournalEntry, error) {
	return s.OperationContext(context.Background(), plugin, operation)
}

func (s *Store) OperationContext(ctx context.Context, plugin, operation string) (JournalEntry, error) {
	var result JournalEntry
	if !storePluginID(plugin) || !storeToken(operation) {
		return result, storeInvalid()
	}
	err := s.withStoreContext(ctx, plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, plugin)
		if err != nil {
			return err
		}
		result, err = storeOperation(root, records, plugin, operation)
		return err
	})
	return result, err
}

func storeOperation(root *os.Root, records storeAccounts, plugin, operation string) (JournalEntry, error) {
	if entry, found := refreshOperation(records, operation); found {
		return entry, nil
	}
	entry, found, err := storeReadOperation(root, plugin, operation)
	if err != nil || found {
		return entry, err
	}
	if strings.HasPrefix(operation, "account-task-") {
		intent, exists, err := readAccountTaskIntent(root, operation)
		if err != nil {
			return JournalEntry{}, err
		}
		if exists {
			return intent.Receipt, nil
		}
	}
	return JournalEntry{}, storeError("not_found", "Extension operation is unavailable.")
}

func storeTransition(previous, next string) (string, bool) {
	if next == "canceled" {
		next = "uncertain"
	}
	switch previous {
	case "":
		return next, next == "dispatched"
	case "dispatched":
		return next, next != "dispatched"
	case "accepted":
		return next, next == "accepted" || next == "uncertain" || next == "completed"
	case "uncertain":
		return next, next == "uncertain" || next == "completed"
	case "completed", "not-started":
		return next, next == previous
	}
	return next, false
}

func storeJournalConflict(previous JournalEntry) error {
	outcome := previous.State
	if outcome == "dispatched" {
		outcome = "uncertain"
	}
	return &sdk.Error{Code: "operation_conflict", Message: "Extension operation transition refused.", Outcome: outcome}
}

func storeJournalState(state string) bool {
	switch state {
	case "dispatched", "accepted", "uncertain", "completed", "not-started":
		return true
	}
	return false
}

func storeStateScope(scope sdk.Scope) sdk.Scope {
	return sdk.Scope{Plugin: scope.Plugin, Provider: scope.Provider, Account: scope.Account, Generation: scope.Generation}
}

func storeStateName(scope sdk.Scope, key string) string {
	encoded, _ := json.Marshal(struct {
		Scope sdk.Scope
		Key   string
	}{storeStateScope(scope), key})
	return storeHash(string(encoded)) + ".json"
}

func storeReadState(root *os.Root, scope sdk.Scope, key string) (storeState, error) {
	var record storeState
	if root == nil {
		return record, nil
	}
	dir, err := storeOpenDirectory(root, "state", false)
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil {
		return record, err
	}
	defer dir.Close()
	found, err := storeRead(dir, storeStateName(scope, key), storeFileLimit, &record)
	if err != nil || !found {
		return storeState{}, err
	}
	if record.Scope != storeStateScope(scope) || record.Key != key || record.Version == 0 ||
		len(record.Value) > storeStateLimit || !json.Valid(record.Value) {
		return storeState{}, storeUnreadable()
	}
	return record, nil
}

func storeReadAccounts(root *os.Root, plugin string) (storeAccounts, error) {
	return storeLoadAccounts(root, plugin, nil)
}

func (s *Store) readAccounts(root *os.Root, plugin string) (storeAccounts, error) {
	return storeLoadAccounts(root, plugin, s)
}

func (s *Store) accountSnapshotIndex(plugin string) int {
	return int(sha256.Sum256([]byte(plugin))[0]) % len(s.accountSnapshots)
}

func storeLoadAccounts(root *os.Root, plugin string, store *Store) (storeAccounts, error) {
	view, err := storeLoadAccountSnapshot(root, plugin, store)
	if err != nil {
		return storeAccounts{}, err
	}
	if view == nil {
		return storeAccounts{Schema: 1, Accounts: []storeAccount{}}, nil
	}
	return view.value, nil
}

func storeLoadAccountSnapshot(root *os.Root, plugin string, store *Store) (*storeReadSnapshot[storeAccounts], error) {
	var small, large *atomic.Pointer[storeReadSnapshot[storeAccounts]]
	if store != nil {
		small, large = &store.accountSnapshots[store.accountSnapshotIndex(plugin)], &store.largeAccountSnapshot
	}
	return storeLoadSnapshotView(root, "accounts.json", plugin, small, large, func(data []byte) (storeAccounts, error) {
		return storeDecodeAccounts(data, plugin)
	})
}

func storeDecodeAccounts(data []byte, plugin string) (storeAccounts, error) {
	var records storeAccounts
	if err := storeDecode(data, &records); err != nil {
		return storeAccounts{}, err
	}
	if records.Schema < 1 || records.Schema > 4 || records.Accounts == nil || !validWindowAuthRecords(records, plugin) {
		return storeAccounts{}, storeUnreadable()
	}
	seen := make(map[string]bool, len(records.Accounts))
	for _, record := range records.Accounts {
		if record.Plugin != plugin || !storeAccountID(record.Plugin, record.Provider, record.ID) || record.Generation == 0 || record.ExpiresAt < 0 ||
			record.Deleted && len(record.Auth) != 0 && !bytes.Equal(record.Auth, []byte("null")) ||
			!record.Deleted && !json.Valid(record.Auth) {
			return storeAccounts{}, storeUnreadable()
		}
		identity := storeHash(record.Provider, record.ID)
		if seen[identity] {
			return storeAccounts{}, storeUnreadable()
		}
		if record.Refresh != nil && (records.Schema < 2 || !validRefreshRecord(record)) {
			return storeAccounts{}, storeUnreadable()
		}
		if records.Schema < 3 && record.Usage != nil || records.Schema >= 3 &&
			(record.Usage == nil || record.Usage.Acknowledged > record.Usage.Revision) {
			return storeAccounts{}, storeUnreadable()
		}
		seen[identity] = true
	}
	return records, nil
}

func storeCheckGeneration(root *os.Root, scope sdk.Scope) error {
	if scope.Account == "" {
		return nil
	}
	records, err := storeReadAccounts(root, scope.Plugin)
	if err != nil {
		return err
	}
	return checkAccountGeneration(records, scope)
}

func checkAccountGeneration(records storeAccounts, scope sdk.Scope) error {
	for _, record := range records.Accounts {
		if record.Provider == scope.Provider && record.ID == scope.Account &&
			!record.Deleted && record.Generation == scope.Generation {
			return nil
		}
	}
	return storeError("stale_generation", "Extension account generation is unavailable.")
}

func storeReadOperation(root *os.Root, plugin, operation string) (JournalEntry, bool, error) {
	var entry JournalEntry
	dir, err := storeDirectory(root, "operations")
	if err != nil {
		return entry, false, err
	}
	defer dir.Close()
	found, err := storeRead(dir, storeHash(operation)+".json", storeFileLimit, &entry)
	if err != nil || !found {
		return JournalEntry{}, found, err
	}
	if entry.Scope.Plugin != plugin || entry.Scope.Operation != operation ||
		!storeValidScope(entry.Scope) || !storeJournalState(entry.State) || entry.Updated.IsZero() {
		return JournalEntry{}, false, storeUnreadable()
	}
	return entry, true, nil
}

func (s *Store) withStore(plugin string, action func(*os.Root) error) error {
	return s.withStoreContext(context.Background(), plugin, action)
}

func (s *Store) withStoreContext(ctx context.Context, plugin string, action func(*os.Root) error) error {
	return s.withPrivateStore(ctx, func(private *os.Root) error {
		namespace, err := storeDirectory(private, plugin)
		if err != nil {
			return err
		}
		defer namespace.Close()
		if err := ctx.Err(); err != nil {
			return err
		}
		return action(namespace)
	})
}

func (s *Store) withPrivateStore(ctx context.Context, action func(*os.Root) error) error {
	if s == nil || s.root == "" {
		return storeInvalid()
	}
	if err := filelock.LockMutex(ctx, &s.mu); err != nil {
		return err
	}
	defer s.mu.Unlock()
	root, err := storeOpenRoot(s.root)
	if err != nil {
		return err
	}
	defer root.Close()
	private, err := storeDirectory(root, "private")
	if err != nil {
		return err
	}
	defer private.Close()
	lock, err := storeOpenLock(private)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := storeWaitLock(ctx, lock, filelock.TryLock); err != nil {
		return err
	}
	defer filelock.Unlock(lock)
	if err := ctx.Err(); err != nil {
		return err
	}
	return action(private)
}

func storeWaitLock(ctx context.Context, lock *os.File, try func(*os.File) (bool, error)) error {
	due := time.Now().Add(filelock.Wait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		held, err := try(lock)
		if err != nil {
			return storeError("store_locked", "Extension store lock is unavailable.")
		}
		if held {
			return nil
		}
		if time.Now().After(due) {
			return storeError("store_locked", "Extension store lock is unavailable.")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func storeOpenRoot(path string) (*os.Root, error) {
	return storeRoot(path, true)
}

func storeRoot(path string, create bool) (*os.Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, storeUnreadable()
	}
	temp := filepath.Clean(os.TempDir())
	if relative, err := filepath.Rel(temp, absolute); err == nil && filepath.IsLocal(relative) {
		if resolved, err := filepath.EvalSymlinks(temp); err == nil {
			absolute = filepath.Join(resolved, relative)
		}
	}
	volume := filepath.VolumeName(absolute) + string(filepath.Separator)
	root, err := os.OpenRoot(volume)
	if err != nil {
		return nil, storeUnreadable()
	}
	for _, component := range strings.Split(strings.TrimPrefix(absolute, volume), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		next, err := storeOpenDirectory(root, component, create)
		root.Close()
		if err != nil {
			return nil, err
		}
		root = next
	}
	return root, nil
}

func storeDirectory(root *os.Root, name string) (*os.Root, error) {
	dir, err := storeOpenDirectory(root, name, true)
	if err != nil {
		return nil, err
	}
	file, err := dir.Open(".")
	if err != nil {
		dir.Close()
		return nil, storeError("store_unreadable", "Extension store directory permissions are unavailable.")
	}
	err = file.Chmod(0o700)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		dir.Close()
		return nil, storeError("store_unreadable", "Extension store directory permissions could not be secured.")
	}
	return dir, nil
}

func storeOpenDirectory(root *os.Root, name string, create bool) (*os.Root, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, os.ErrNotExist
		}
		if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, storeError("store_unreadable", "Extension store directory could not be created.")
		}
		if err := storeSync(root); err != nil {
			return nil, err
		}
		info, err = root.Lstat(name)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, storeError("store_unreadable", "Extension store directory is unavailable.")
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return nil, storeError("store_unreadable", "Extension store directory could not be opened.")
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		dir.Close()
		return nil, storeError("store_unreadable", "Extension store directory changed while opening.")
	}
	return dir, nil
}

func storeOpenLock(root *os.Root) (*os.File, error) {
	return storeNamedLock(root, ".lock")
}

func storeNamedLock(root *os.Root, name string) (*os.File, error) {
	for range 8 {
		info, err := root.Lstat(name)
		if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
			return nil, storeError("store_unreadable", "Extension store lock is not a regular file.")
		}
		flags := os.O_RDWR
		if errors.Is(err, os.ErrNotExist) {
			flags |= os.O_CREATE | os.O_EXCL
		}
		file, err := root.OpenFile(name, flags, 0o600)
		if errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, storeError("store_unreadable", "Extension store lock could not be opened.")
		}
		opened, err := file.Stat()
		current, statErr := root.Lstat(name)
		if err != nil || statErr != nil || !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
			!os.SameFile(opened, current) || file.Chmod(0o600) != nil {
			file.Close()
			return nil, storeError("store_unreadable", "Extension store lock changed while opening.")
		}
		return file, nil
	}
	return nil, storeError("store_unreadable", "Extension store lock could not be opened.")
}

func storeRead(root *os.Root, name string, limit int64, target any) (bool, error) {
	data, found, err := storeReadBytes(root, name, limit)
	if err != nil || !found {
		return found, err
	}
	if err := storeDecode(data, target); err != nil {
		return false, err
	}
	return true, nil
}

func storeReadBytes(root *os.Root, name string, limit int64) ([]byte, bool, error) {
	data, info, err := storeReadFile(root, name, limit)
	return data, info != nil, err
}

func storeReadFile(root *os.Root, name string, limit int64) ([]byte, os.FileInfo, error) {
	if root == nil {
		return nil, nil, nil
	}
	file, info, err := storeOpenReadFile(root, name, limit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	data := make([]byte, info.Size()+1)
	n, err := io.ReadFull(file, data)
	if int64(n) != info.Size() || err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, nil, storeUnreadable()
	}
	return data[:n], info, nil
}

func storeDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return storeUnreadable()
	}
	return nil
}

func storeOpenReadFile(root *os.Root, name string, limit int64) (*os.File, os.FileInfo, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, os.ErrNotExist
	}
	if err != nil || !info.Mode().IsRegular() || limit >= 0 && info.Size() > limit {
		return nil, nil, storeUnreadable()
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, nil, storeUnreadable()
	}
	opened, err := file.Stat()
	current, namedErr := root.Lstat(name)
	if err != nil || namedErr != nil || !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(info, opened) || !os.SameFile(opened, current) || limit >= 0 && opened.Size() > limit {
		file.Close()
		return nil, nil, storeUnreadable()
	}
	return file, opened, nil
}

func storeWrite(root *os.Root, name string, value any, limit int) error {
	_, err := storePublish(root, name, value, limit)
	return err
}

func storePublish(root *os.Root, name string, value any, limit int) (bool, error) {
	return storePublishContext(context.Background(), root, name, value, limit)
}

func storePublishContext(ctx context.Context, root *os.Root, name string, value any, limit int) (bool, error) {
	return storePublishChecked(ctx, root, name, value, limit, nil)
}

func storePublishChecked(ctx context.Context, root *os.Root, name string, value any, limit int, check func() error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil || buffer.Len() > limit {
		return false, storeError("invalid_state", "Extension store value exceeds its limit.")
	}
	data := buffer.Bytes()
	info, err := root.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return false, storeUnreadable()
	}
	temp := ".store-" + rand.Text()
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, storeError("store_write", "Extension store could not be persisted.")
	}
	defer root.Remove(temp)
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return false, storeError("store_write", "Extension store could not be persisted.")
	}
	if check != nil {
		if err := check(); err != nil {
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if root.Rename(temp, name) != nil {
		return false, storeError("store_write", "Extension store could not be persisted.")
	}
	return true, storeSync(root)
}

func storeSync(root *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := root.Open(".")
	if err != nil {
		return storeError("store_write", "Extension store directory could not be persisted.")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return storeError("store_write", "Extension store directory could not be persisted.")
	}
	return nil
}

func storeHash(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func storePluginID(id string) bool {
	return sdk.ValidID(id)
}

func storeToken(token string) bool {
	if len(token) == 0 || len(token) > storeTokenLimit || !utf8.ValidString(token) {
		return false
	}
	for _, char := range token {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func storeAccountID(plugin, provider, id string) bool {
	return storePluginID(plugin) && storeToken(provider) && storeToken(id)
}

func storeValidScope(scope sdk.Scope) bool {
	if !storePluginID(scope.Plugin) ||
		scope.Provider != "" && !storeToken(scope.Provider) ||
		scope.Session != "" && !storeToken(scope.Session) ||
		scope.Operation != "" && !storeToken(scope.Operation) {
		return false
	}
	if scope.Account == "" {
		return scope.Generation == 0
	}
	return storeToken(scope.Provider) && storeToken(scope.Account) && scope.Generation != 0
}

func storeInvalid() error {
	return storeError("invalid_scope", "Extension store scope is invalid.")
}

func storeUnreadable() error {
	return storeError("store_unreadable", "Extension store could not be read; existing data was left unchanged.")
}

func storeError(code, message string) error {
	return &sdk.Error{Code: code, Message: message}
}
