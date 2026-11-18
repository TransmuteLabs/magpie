package extension

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strconv"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type usageState struct {
	Revision     uint64 `json:"revision"`
	Acknowledged uint64 `json:"acknowledged"`
}

func (state *usageState) UnmarshalJSON(raw []byte) error {
	var value struct {
		Revision     *uint64 `json:"revision"`
		Acknowledged *uint64 `json:"acknowledged"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF ||
		value.Revision == nil || value.Acknowledged == nil {
		return storeUnreadable()
	}
	state.Revision, state.Acknowledged = *value.Revision, *value.Acknowledged
	return nil
}

type UsageStamp struct {
	Scope    sdk.Scope
	Revision uint64
	Dirty    bool
}

func (stamp UsageStamp) Version() string { return strconv.FormatUint(stamp.Revision, 10) }

func usageStamp(account storeAccount) UsageStamp {
	stamp := UsageStamp{Scope: sdk.Scope{
		Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation,
	}}
	if account.Usage != nil {
		stamp.Revision = account.Usage.Revision
		stamp.Dirty = account.Usage.Acknowledged != account.Usage.Revision
	}
	return stamp
}

func usageAccount(records *storeAccounts, scope sdk.Scope) (*storeAccount, error) {
	if !storeValidScope(scope) || scope.Account == "" {
		return nil, storeInvalid()
	}
	index := refreshIndex(*records, scope)
	if index < 0 || records.Accounts[index].Deleted || records.Accounts[index].Generation != scope.Generation {
		return nil, storeError("stale_generation", "Extension usage account scope changed.")
	}
	return &records.Accounts[index], nil
}

func (s *Store) UsageStamp(scope sdk.Scope) (UsageStamp, error) {
	var stamp UsageStamp
	err := s.withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		account, err := usageAccount(&records, scope)
		if err != nil {
			return err
		}
		stamp = usageStamp(*account)
		if !stamp.Dirty {
			return storeSync(root)
		}
		return nil
	})
	return stamp, err
}

func (s *Store) InvalidateUsage(scope sdk.Scope) (UsageStamp, error) {
	var stamp UsageStamp
	err := s.withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		account, err := usageAccount(&records, scope)
		if err != nil {
			return err
		}
		if records.Schema < 3 {
			for index := range records.Accounts {
				records.Accounts[index].Usage = &usageState{}
			}
			records.Schema = 3
		}
		if account.Usage.Revision == ^uint64(0) {
			return storeError("conflict", "Extension usage revision is exhausted.")
		}
		account.Usage.Revision++
		if err := storeWrite(root, "accounts.json", records, storeFileLimit); err != nil {
			return err
		}
		stamp = usageStamp(*account)
		return nil
	})
	return stamp, err
}

func (s *Store) AcknowledgeUsage(scope sdk.Scope, revision uint64) (UsageStamp, error) {
	var stamp UsageStamp
	err := s.withStore(scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		account, err := usageAccount(&records, scope)
		if err != nil {
			return err
		}
		stamp = usageStamp(*account)
		if stamp.Revision != revision {
			return storeError("usage_stale", "Extension usage read preceded an invalidation.")
		}
		if stamp.Dirty {
			account.Usage.Acknowledged = revision
			if err := storeWrite(root, "accounts.json", records, storeFileLimit); err != nil {
				return err
			}
		} else if err := storeSync(root); err != nil {
			return err
		}
		stamp = usageStamp(*account)
		return nil
	})
	return stamp, err
}

func (s *Store) UsageReady(scope sdk.Scope) error {
	if scope.Account == "" {
		return nil
	}
	stamp, err := s.UsageStamp(scope)
	if err != nil {
		return err
	}
	if stamp.Dirty {
		return &sdk.Error{Code: "usage_refresh_required", Message: "A fresh account usage acknowledgment is required.", Outcome: "not-started"}
	}
	return nil
}
