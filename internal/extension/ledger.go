package extension

import (
	"context"
	"errors"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type LedgerTarget struct {
	Module        string
	Revision      string
	Directory     string
	ConfigVersion string
	Timeout       time.Duration
}

func (manager *Manager) LedgerTargets() ([]LedgerTarget, error) {
	entries, err := manager.List()
	if err != nil {
		return nil, err
	}
	var targets []LedgerTarget
	for _, entry := range entries {
		if entry.Disabled || !entry.Manifest.Contract.Has(sdk.LedgerObserver) || entry.Manifest.Ledger == nil {
			continue
		}
		config, err := manager.runtimeModuleConfig(entry.Manifest)
		if err != nil {
			return nil, err
		}
		timeout := time.Duration(entry.Manifest.Ledger.TimeoutMs) * time.Millisecond
		if timeout == 0 {
			timeout = 250 * time.Millisecond
		}
		targets = append(targets, LedgerTarget{Module: entry.Manifest.ID, Revision: entry.Revision,
			Directory: entry.Directory, ConfigVersion: config.Version, Timeout: timeout})
	}
	return targets, nil
}

func (manager *Manager) ObserveLedger(ctx context.Context, target LedgerTarget, row sdk.LedgerObservation) error {
	if err := sdk.ValidateLedgerObservation(row); err != nil {
		return err
	}
	worker, client, err := manager.acquire(ctx, target.Module, &expectedGeneration{
		packageRevision: target.Revision, configVersion: target.ConfigVersion,
	})
	if err != nil {
		return err
	}
	owned, cancel := context.WithCancel(ctx)
	lease := &Lease{manager: manager, worker: worker, client: client, group: client.group, ctx: owned, cancel: cancel}
	defer lease.Close()
	if worker.entry.Directory != target.Directory || !worker.entry.Manifest.Contract.Has(sdk.LedgerObserver) ||
		worker.entry.Manifest.Ledger == nil {
		return errors.New("ledger observer generation changed")
	}
	_, err = lease.Call(ctx, sdk.MethodLedgerObserve, sdk.Scope{}, row, nil)
	return err
}
