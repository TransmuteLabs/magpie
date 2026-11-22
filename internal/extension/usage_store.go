package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	return s.UsageStampContext(context.Background(), scope)
}

func (s *Store) UsageStampContext(ctx context.Context, scope sdk.Scope) (UsageStamp, error) {
	return s.usageStampContext(ctx, scope, storeSync)
}

func (s *Store) usageStampContext(ctx context.Context, scope sdk.Scope, confirm func(*os.Root) error) (UsageStamp, error) {
	var stamp UsageStamp
	if !storeValidScope(scope) || scope.Account == "" {
		return stamp, storeInvalid()
	}
	err := s.withReadAccountsSnapshot(ctx, scope.Plugin, func(root *os.Root, snapshot *storeReadSnapshot[storeAccounts]) error {
		var err error
		stamp, err = s.usageStampFromSnapshot(ctx, root, snapshot, scope, confirm)
		return err
	})
	if err != nil {
		return UsageStamp{}, err
	}
	return stamp, nil
}

func (s *Store) usageStampFromSnapshot(ctx context.Context, root *os.Root, snapshot *storeReadSnapshot[storeAccounts], scope sdk.Scope, confirm func(*os.Root) error) (UsageStamp, error) {
	if snapshot == nil {
		return UsageStamp{}, storeError("stale_generation", "Extension usage account scope changed.")
	}
	records := snapshot.value
	account, err := usageAccount(&records, scope)
	if err != nil {
		return UsageStamp{}, err
	}
	stamp := usageStamp(*account)
	if !stamp.Dirty {
		return stamp, s.confirmAccountSnapshot(ctx, root, snapshot, confirm)
	}
	return stamp, nil
}

func (s *Store) InvalidateUsage(scope sdk.Scope) (UsageStamp, error) {
	return s.invalidateUsage(context.Background(), scope, false)
}

func (s *Store) invalidateUsage(ctx context.Context, scope sdk.Scope, requireReceipt bool) (UsageStamp, error) {
	var stamp UsageStamp
	if !storeValidScope(scope) || scope.Account == "" || requireReceipt && scope.Operation == "" {
		return stamp, storeInvalid()
	}
	err := s.withStoreContext(ctx, scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		if requireReceipt {
			operation, err := storeOperation(root, records, scope.Plugin, scope.Operation)
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			if err != nil || operation.Scope != scope || operation.State != "accepted" {
				return errors.New("usage invalidation requires durable write acceptance")
			}
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
		if err := ctx.Err(); err != nil {
			return err
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
	return s.AcknowledgeUsageContext(context.Background(), scope, revision)
}

func (s *Store) AcknowledgeUsageContext(ctx context.Context, scope sdk.Scope, revision uint64) (UsageStamp, error) {
	var stamp UsageStamp
	err := s.withStoreContext(ctx, scope.Plugin, func(root *os.Root) error {
		var err error
		stamp, err = acknowledgeUsageLocked(ctx, root, scope, revision)
		return err
	})
	return stamp, err
}

func acknowledgeUsageLocked(ctx context.Context, root *os.Root, scope sdk.Scope, revision uint64) (UsageStamp, error) {
	records, err := storeReadAccounts(root, scope.Plugin)
	if err != nil {
		return UsageStamp{}, err
	}
	account, err := usageAccount(&records, scope)
	if err != nil {
		return UsageStamp{}, err
	}
	stamp := usageStamp(*account)
	if stamp.Revision != revision {
		return stamp, storeError("usage_stale", "Extension usage read preceded an invalidation.")
	}
	if err := ctx.Err(); err != nil {
		return stamp, err
	}
	if !stamp.Dirty {
		return stamp, storeSync(root)
	}
	account.Usage.Acknowledged = revision
	if err := storeWrite(root, "accounts.json", records, storeFileLimit); err != nil {
		return stamp, err
	}
	return usageStamp(*account), nil
}

func (s *Store) UsageReady(scope sdk.Scope) error {
	return s.UsageReadyContext(context.Background(), scope)
}

func (s *Store) UsageReadyContext(ctx context.Context, scope sdk.Scope) error {
	if scope.Account == "" {
		return ctx.Err()
	}
	stamp, err := s.UsageStampContext(ctx, scope)
	if err != nil {
		return err
	}
	if stamp.Dirty {
		return &sdk.Error{Code: "usage_refresh_required", Message: "A fresh account usage acknowledgment is required.", Outcome: "not-started"}
	}
	return nil
}
