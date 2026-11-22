package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type refreshRecord struct {
	Schema       int            `json:"schema"`
	Scope        sdk.Scope      `json:"scope"`
	Revision     string         `json:"revision"`
	Config       string         `json:"config"`
	State        string         `json:"state"`
	Updated      time.Time      `json:"updated"`
	ToGeneration uint64         `json:"toGeneration,omitempty"`
	Receipt      sdk.AuthResult `json:"receipt,omitempty"`
}

func validRefreshRecord(account storeAccount) bool {
	record := account.Refresh
	if record.Schema != 1 || !storeValidScope(record.Scope) || record.Scope.Plugin != account.Plugin ||
		record.Scope.Provider != account.Provider || record.Scope.Account != account.ID ||
		!storeToken(record.Scope.Operation) || record.Scope.Attempt == 0 || record.Updated.IsZero() ||
		!storeToken(record.Revision) || !storeToken(record.Config) || record.Scope.Generation > account.Generation ||
		len(record.Receipt.Credentials) != 0 || record.Receipt.Pending {
		return false
	}
	switch record.State {
	case "dispatched", "uncertain", "not-started":
		return record.ToGeneration == 0 && record.Receipt.ID == "" && record.Receipt.Name == "" && record.Receipt.ExpiresAt == 0
	case "completed":
		return record.Scope.Generation != ^uint64(0) && record.ToGeneration == record.Scope.Generation+1 &&
			record.ToGeneration <= account.Generation && record.Receipt.ID == account.ID &&
			(record.Receipt.Name == "" || storeToken(record.Receipt.Name)) && record.Receipt.ExpiresAt >= 0
	}
	return false
}

func refreshOperation(records storeAccounts, operation string) (JournalEntry, bool) {
	for _, account := range records.Accounts {
		if record := account.Refresh; record != nil && record.Scope.Operation == operation {
			return JournalEntry{Scope: record.Scope, State: record.State, Updated: record.Updated}, true
		}
	}
	return JournalEntry{}, false
}

func refreshConflict() error {
	return &sdk.Error{Code: "refresh_uncertain", Message: "The previous credential refresh is unresolved; reauthorize the account.", Outcome: "uncertain"}
}

func refreshIndex(records storeAccounts, scope sdk.Scope) int {
	for index, record := range records.Accounts {
		if record.Provider == scope.Provider && record.ID == scope.Account {
			return index
		}
	}
	return -1
}

func refreshReceipt(account storeAccount, scope sdk.Scope) (*sdk.AuthResult, error) {
	if record := account.Refresh; record != nil && record.Scope.Generation == scope.Generation {
		if record.State == "completed" && !account.Deleted && account.Generation == record.ToGeneration {
			receipt := record.Receipt
			return &receipt, nil
		}
		if record.State == "dispatched" || record.State == "uncertain" {
			return nil, refreshConflict()
		}
	}
	if account.Deleted || account.Generation != scope.Generation || account.Generation == ^uint64(0) {
		return nil, storeError("stale_generation", "Extension account generation changed.")
	}
	return nil, nil
}

func (s *Store) refreshStatus(scope sdk.Scope) (*sdk.AuthResult, error) {
	var receipt *sdk.AuthResult
	err := s.withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		index := refreshIndex(records, scope)
		if index < 0 {
			return storeError("not_found", "Extension account is unavailable.")
		}
		receipt, err = refreshReceipt(records.Accounts[index], scope)
		if record := records.Accounts[index].Refresh; record != nil && record.Scope.Generation == scope.Generation && record.State == "dispatched" {
			record.State, record.Updated = "uncertain", time.Now().UTC()
			if persistErr := storeWrite(root, "accounts.json", records, storeFileLimit); persistErr != nil {
				return persistErr
			}
		}
		if err == nil && receipt != nil {
			err = storeSync(root)
		}
		return err
	})
	return receipt, err
}

func (s *Store) claimRefresh(scope sdk.Scope, revision, config string) (Account, error) {
	var selected Account
	err := s.withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		index := refreshIndex(records, scope)
		if index < 0 {
			return storeError("not_found", "Extension account is unavailable.")
		}
		account := &records.Accounts[index]
		receipt, err := refreshReceipt(*account, scope)
		if err != nil {
			return err
		}
		if receipt != nil {
			return storeError("stale_generation", "Extension account refresh already completed.")
		}
		if previous := account.Refresh; previous != nil {
			if (previous.State == "dispatched" || previous.State == "uncertain") && previous.Scope.Generation == account.Generation {
				return refreshConflict()
			}
			entry := JournalEntry{Scope: previous.Scope, State: previous.State, Updated: previous.Updated}
			if entry.State == "dispatched" {
				entry.State = "uncertain"
			}
			dir, err := storeDirectory(root, "operations")
			if err != nil {
				return err
			}
			err = storeWrite(dir, storeHash(entry.Scope.Operation)+".json", entry, storeFileLimit)
			dir.Close()
			if err != nil {
				return err
			}
		}
		selected = account.namedAccount(root)
		account.Refresh = &refreshRecord{
			Schema: 1, Scope: scope, Revision: revision, Config: config,
			State: "dispatched", Updated: time.Now().UTC(),
		}
		records.Schema = max(records.Schema, 2)
		if !validRefreshRecord(*account) {
			return storeInvalid()
		}
		if err := storeWrite(root, "accounts.json", records, storeFileLimit-4096); err != nil {
			return err
		}
		selected.writeLabel(root)
		return nil
	})
	return selected, err
}

func (s *Store) finishRefresh(scope sdk.Scope, result *sdk.AuthResult, state string) (sdk.AuthResult, error) {
	var receipt sdk.AuthResult
	err := s.withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		index := refreshIndex(records, scope)
		if index < 0 || records.Accounts[index].Refresh == nil {
			return refreshConflict()
		}
		account := &records.Accounts[index]
		record := account.Refresh
		if record.Scope != scope || record.State != "dispatched" {
			return refreshConflict()
		}
		record.State, record.Updated = state, time.Now().UTC()
		if result != nil {
			if account.Deleted || account.Generation != scope.Generation {
				record.State = "uncertain"
				if err := storeWrite(root, "accounts.json", records, storeFileLimit); err != nil {
					return err
				}
				return storeError("stale_generation", "Extension account changed during credential refresh.")
			}
			receipt = *result
			receipt.ID, receipt.Credentials = scope.Account, nil
			account.Name = result.Name
			if account.Name == "" {
				account.Name = account.namedAccount(root).Name
			}
			account.Auth = bytes.Clone(result.Credentials)
			account.ExpiresAt = result.ExpiresAt
			account.Generation++
			record.State, record.ToGeneration, record.Receipt = "completed", account.Generation, receipt
		}
		if !validRefreshRecord(*account) {
			return storeInvalid()
		}
		if err := storeWrite(root, "accounts.json", records, storeFileLimit); err != nil {
			return err
		}
		if result != nil {
			account.writeLabel(root)
		}
		return nil
	})
	return receipt, err
}

func (s *Store) lockRefresh(ctx context.Context, scope sdk.Scope) (func(), error) {
	if !storeValidScope(scope) || scope.Account == "" {
		return nil, storeInvalid()
	}
	root, err := storeOpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	private, err := storeDirectory(root, "private")
	if err != nil {
		return nil, err
	}
	defer private.Close()
	namespace, err := storeDirectory(private, scope.Plugin)
	if err != nil {
		return nil, err
	}
	defer namespace.Close()
	locks, err := storeDirectory(namespace, "refresh-locks")
	if err != nil {
		return nil, err
	}
	defer locks.Close()
	account, err := storeDirectory(locks, storeHash(scope.Provider, scope.Account))
	if err != nil {
		return nil, err
	}
	defer account.Close()
	file, err := storeOpenLock(account)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		held, err := filelock.TryLock(file)
		if err != nil {
			file.Close()
			return nil, storeError("store_locked", "Extension account refresh lock is unavailable.")
		}
		if held {
			return func() { filelock.Unlock(file); file.Close() }, nil
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func validateRefreshResult(scope sdk.Scope, result sdk.AuthResult) error {
	if result.Pending || result.ID != "" && result.ID != scope.Account ||
		result.Name != "" && !storeToken(result.Name) || result.ExpiresAt < 0 ||
		len(result.Credentials) == 0 || len(result.Credentials) > storeFileLimit-8192 ||
		!json.Valid(result.Credentials) || bytes.Equal(bytes.TrimSpace(result.Credentials), []byte("null")) {
		return errors.New("extension returned an invalid credential refresh result")
	}
	return nil
}

func decodeRefreshResult(scope sdk.Scope, raw json.RawMessage) (sdk.AuthResult, error) {
	var result sdk.AuthResult
	if len(raw) > storeFileLimit || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return result, errors.New("extension credential refresh result exceeds its contract")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return sdk.AuthResult{}, errors.New("extension credential refresh result is invalid")
	}
	return result, validateRefreshResult(scope, result)
}
