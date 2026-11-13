package extension

import (
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (s *Store) RefreshedAccount(scope sdk.Scope) (Account, error) {
	return s.refreshedAccount(scope, storeSync)
}

func (s *Store) refreshedAccount(scope sdk.Scope, confirm func(*os.Root) error) (Account, error) {
	if !storeValidScope(scope) || scope.Account == "" {
		return Account{}, storeInvalid()
	}
	var current Account
	err := s.withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		index := refreshIndex(records, scope)
		if index < 0 || records.Accounts[index].Deleted {
			return storeError("not_found", "Extension account is unavailable.")
		}
		account := records.Accounts[index]
		if account.Generation == scope.Generation {
			if account.Refresh != nil && account.Refresh.State == "completed" && account.Refresh.ToGeneration == account.Generation {
				if err := confirm(root); err != nil {
					return err
				}
			}
			current = account.Account
			return nil
		}
		receipt, err := refreshReceipt(account, scope)
		if err != nil {
			return err
		}
		if receipt == nil || account.Generation != scope.Generation+1 || receipt.ExpiresAt != account.ExpiresAt {
			return storeError("stale_generation", "Extension account generation changed.")
		}
		if err := confirm(root); err != nil {
			return err
		}
		current = account.Account
		return nil
	})
	return current, err
}
