package extension

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	Generation uint64          `json:"generation"`
	Auth       json.RawMessage `json:"auth"`
}

type JournalEntry struct {
	Scope   sdk.Scope `json:"scope"`
	State   string    `json:"state"`
	Updated time.Time `json:"updated"`
}

type Store struct {
	root string
	mu   sync.Mutex
}

type storeAccount struct {
	Account
	Deleted bool `json:"deleted,omitempty"`
}

type storeAccounts struct {
	Schema   int            `json:"schema"`
	Accounts []storeAccount `json:"accounts"`
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
	var result VersionedValue
	if !storeValidScope(scope) || !storeToken(key) {
		return result, storeInvalid()
	}
	err := s.withStore(scope.Plugin, func(root *os.Root) error {
		if err := storeCheckGeneration(root, scope); err != nil {
			return err
		}
		record, err := storeReadState(root, scope, key)
		result = record.VersionedValue
		return err
	})
	return result, err
}

func (s *Store) StateCAS(scope sdk.Scope, key string, expected uint64, value json.RawMessage) (VersionedValue, error) {
	var result VersionedValue
	if !storeValidScope(scope) || !storeToken(key) {
		return result, storeInvalid()
	}
	if len(value) > storeStateLimit || !json.Valid(value) {
		return result, storeError("invalid_state", "Extension state must be bounded JSON.")
	}
	err := s.withStore(scope.Plugin, func(root *os.Root) error {
		if err := storeCheckGeneration(root, scope); err != nil {
			return err
		}
		record, err := storeReadState(root, scope, key)
		if err != nil {
			return err
		}
		if record.Version != expected || expected == ^uint64(0) {
			return storeError("conflict", "Extension state version changed.")
		}
		record = storeState{
			Scope: storeStateScope(scope), Key: key,
			VersionedValue: VersionedValue{Version: expected + 1, Value: bytes.Clone(value)},
		}
		dir, err := storeDirectory(root, "state")
		if err != nil {
			return err
		}
		defer dir.Close()
		if err := storeWrite(dir, storeStateName(scope, key), record, storeFileLimit); err != nil {
			return err
		}
		result = record.VersionedValue
		return nil
	})
	return result, err
}

func (s *Store) Accounts(plugin, provider string) ([]Account, error) {
	var result []Account
	if !storePluginID(plugin) || !storeToken(provider) {
		return nil, storeInvalid()
	}
	err := s.withStore(plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, plugin)
		if err != nil {
			return err
		}
		result = []Account{}
		for _, record := range records.Accounts {
			if !record.Deleted && record.Provider == provider {
				result = append(result, record.Account)
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) GetAccount(plugin, provider, id string) (Account, error) {
	var result Account
	if !storeAccountID(plugin, provider, id) {
		return result, storeInvalid()
	}
	err := s.withStore(plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, plugin)
		if err != nil {
			return err
		}
		for _, record := range records.Accounts {
			if record.Provider == provider && record.ID == id && !record.Deleted {
				result = record.Account
				return nil
			}
		}
		return storeError("not_found", "Extension account is unavailable.")
	})
	return result, err
}

func (s *Store) SaveAccount(account Account, expectedGeneration uint64) (Account, error) {
	var result Account
	if !storeAccountID(account.Plugin, account.Provider, account.ID) {
		return result, storeInvalid()
	}
	if len(account.Auth) > storeFileLimit || !json.Valid(account.Auth) {
		return result, storeError("invalid_state", "Extension credentials must be bounded JSON.")
	}
	err := s.withStore(account.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, account.Plugin)
		if err != nil {
			return err
		}
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
					return storeError("conflict", "Extension account generation changed.")
				}
				break
			}
		}
		if index < 0 && expectedGeneration != 0 || generation == ^uint64(0) {
			return storeError("conflict", "Extension account generation changed.")
		}
		account.Generation = generation + 1
		account.Auth = bytes.Clone(account.Auth)
		if index < 0 {
			records.Accounts = append(records.Accounts, storeAccount{Account: account})
		} else {
			records.Accounts[index] = storeAccount{Account: account}
		}
		if err := storeWrite(root, "accounts.json", records, storeFileLimit); err != nil {
			return err
		}
		result = account
		return nil
	})
	return result, err
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
			record.Deleted = true
			records.Accounts[index] = record
			return storeWrite(root, "accounts.json", records, storeFileLimit)
		}
		return storeError("not_found", "Extension account is unavailable.")
	})
}

func (s *Store) Journal(scope sdk.Scope, state string) (JournalEntry, error) {
	var result JournalEntry
	if !storeValidScope(scope) || !storeToken(scope.Operation) || !storeJournalState(state) && state != "canceled" {
		return result, storeInvalid()
	}
	err := s.withStore(scope.Plugin, func(root *os.Root) error {
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
		if state == "dispatched" {
			if err := storeCheckGeneration(root, scope); err != nil {
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
		if err := storeWrite(dir, storeHash(scope.Operation)+".json", entry, storeFileLimit); err != nil {
			return &sdk.Error{Code: "store_write", Message: "Extension operation could not be persisted.", Outcome: "uncertain"}
		}
		result = entry
		return nil
	})
	return result, err
}

func (s *Store) Operation(plugin, operation string) (JournalEntry, error) {
	var result JournalEntry
	if !storePluginID(plugin) || !storeToken(operation) {
		return result, storeInvalid()
	}
	err := s.withStore(plugin, func(root *os.Root) error {
		entry, found, err := storeReadOperation(root, plugin, operation)
		if err != nil {
			return err
		}
		if !found {
			return storeError("not_found", "Extension operation is unavailable.")
		}
		result = entry
		return nil
	})
	return result, err
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
	dir, err := storeDirectory(root, "state")
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
	var records storeAccounts
	found, err := storeRead(root, "accounts.json", storeFileLimit, &records)
	if err != nil {
		return storeAccounts{}, err
	}
	if !found {
		return storeAccounts{Schema: 1, Accounts: []storeAccount{}}, nil
	}
	if records.Schema != 1 || records.Accounts == nil {
		return storeAccounts{}, storeUnreadable()
	}
	seen := make(map[string]bool, len(records.Accounts))
	for _, record := range records.Accounts {
		if record.Plugin != plugin || !storeAccountID(record.Plugin, record.Provider, record.ID) || record.Generation == 0 ||
			record.Deleted && len(record.Auth) != 0 && !bytes.Equal(record.Auth, []byte("null")) ||
			!record.Deleted && !json.Valid(record.Auth) {
			return storeAccounts{}, storeUnreadable()
		}
		identity := storeHash(record.Provider, record.ID)
		if seen[identity] {
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
	if s == nil || s.root == "" {
		return storeInvalid()
	}
	s.mu.Lock()
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
	started := time.Now()
	for {
		held, err := filelock.TryLock(lock)
		if err != nil {
			return storeError("store_locked", "Extension store lock is unavailable.")
		}
		if held {
			break
		}
		if time.Since(started) >= filelock.Wait {
			return storeError("store_locked", "Extension store lock is unavailable.")
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer filelock.Unlock(lock)
	namespace, err := storeDirectory(private, plugin)
	if err != nil {
		return err
	}
	defer namespace.Close()
	return action(namespace)
}

func storeOpenRoot(path string) (*os.Root, error) {
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
		next, err := storeOpenDirectory(root, component, false)
		root.Close()
		if err != nil {
			return nil, err
		}
		root = next
	}
	return root, nil
}

func storeDirectory(root *os.Root, name string) (*os.Root, error) {
	return storeOpenDirectory(root, name, true)
}

func storeOpenDirectory(root *os.Root, name string, private bool) (*os.Root, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
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
	if private {
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
	}
	return dir, nil
}

func storeOpenLock(root *os.Root) (*os.File, error) {
	for range 8 {
		info, err := root.Lstat(".lock")
		if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
			return nil, storeError("store_unreadable", "Extension store lock is not a regular file.")
		}
		flags := os.O_RDWR
		if errors.Is(err, os.ErrNotExist) {
			flags |= os.O_CREATE | os.O_EXCL
		}
		file, err := root.OpenFile(".lock", flags, 0o600)
		if errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, storeError("store_unreadable", "Extension store lock could not be opened.")
		}
		opened, err := file.Stat()
		current, statErr := root.Lstat(".lock")
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
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return false, storeUnreadable()
	}
	file, err := root.Open(name)
	if err != nil {
		return false, storeUnreadable()
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return false, storeUnreadable()
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return false, storeUnreadable()
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return false, storeUnreadable()
	}
	return true, nil
}

func storeWrite(root *os.Root, name string, value any, limit int) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil || buffer.Len() > limit {
		return storeError("invalid_state", "Extension store value exceeds its limit.")
	}
	data := buffer.Bytes()
	info, err := root.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return storeUnreadable()
	}
	temp := ".store-" + rand.Text()
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return storeError("store_write", "Extension store could not be persisted.")
	}
	defer root.Remove(temp)
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || root.Rename(temp, name) != nil {
		return storeError("store_write", "Extension store could not be persisted.")
	}
	return storeSync(root)
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
