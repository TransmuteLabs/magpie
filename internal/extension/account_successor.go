package extension

import (
	"bytes"
	"context"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (s *Store) RefreshedAccount(scope sdk.Scope) (Account, error) {
	return s.RefreshedAccountContext(context.Background(), scope)
}

func (s *Store) RefreshedAccountContext(ctx context.Context, scope sdk.Scope) (Account, error) {
	return s.refreshedAccount(ctx, scope, storeSync)
}

func (s *Store) RefreshedAccountMetadataContext(ctx context.Context, scope sdk.Scope) (AccountMetadata, error) {
	account, err := s.readRefreshedAccount(ctx, scope, storeSync, false)
	if err != nil {
		return AccountMetadata{}, err
	}
	return AccountMetadata{ID: account.ID, Generation: account.Generation, ExpiresAt: account.ExpiresAt}, nil
}

func (s *Store) refreshedAccount(ctx context.Context, scope sdk.Scope, confirm func(*os.Root) error) (Account, error) {
	return s.readRefreshedAccount(ctx, scope, confirm, true)
}

func (s *Store) readRefreshedAccount(ctx context.Context, scope sdk.Scope, confirm func(*os.Root) error, copyAuth bool) (Account, error) {
	if !storeValidScope(scope) || scope.Account == "" {
		return Account{}, storeInvalid()
	}
	var current Account
	err := s.withReadAccountsSnapshot(ctx, scope.Plugin, func(root *os.Root, snapshot *storeReadSnapshot[storeAccounts]) error {
		if snapshot == nil {
			return storeError("not_found", "Extension account is unavailable.")
		}
		records := snapshot.value
		index := refreshIndex(records, scope)
		if index < 0 || records.Accounts[index].Deleted {
			return storeError("not_found", "Extension account is unavailable.")
		}
		account := records.Accounts[index]
		if account.Generation != scope.Generation {
			receipt, err := refreshReceipt(account, scope)
			if err != nil {
				return err
			}
			if receipt == nil || account.Generation != scope.Generation+1 || receipt.ExpiresAt != account.ExpiresAt {
				return storeError("stale_generation", "Extension account generation changed.")
			}
		}
		if account.Refresh != nil && account.Refresh.State == "completed" && account.Refresh.ToGeneration == account.Generation {
			if err := s.confirmAccountSnapshot(ctx, root, snapshot, confirm); err != nil {
				return err
			}
		}
		current = account.Account
		if !copyAuth {
			current.Auth = nil
		}
		return nil
	})
	if err != nil {
		return Account{}, err
	}
	current.Auth = bytes.Clone(current.Auth)
	return current, nil
}
