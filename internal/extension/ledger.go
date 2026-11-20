package extension

import (
	"context"
	"errors"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type LedgerTarget struct {
	Module           string
	Revision         string
	Directory        string
	ConfigVersion    string
	EnableGeneration string
	Timeout          time.Duration
}

func (manager *Manager) LedgerTargets(ctx context.Context) ([]LedgerTarget, error) {
	return manager.observationTargets(ctx, sdk.LedgerObserver)
}

func (manager *Manager) RequestObservationTargets(ctx context.Context) ([]LedgerTarget, error) {
	return manager.observationTargets(ctx, sdk.RequestObserver)
}

func observerTimeout(manifest sdk.Manifest, capability sdk.Capability) (int, bool) {
	if !manifest.Contract.Has(capability) {
		return 0, false
	}
	switch capability {
	case sdk.LedgerObserver:
		if manifest.Ledger != nil {
			return manifest.Ledger.TimeoutMs, true
		}
	case sdk.RequestObserver:
		if manifest.RequestObserver != nil {
			return manifest.RequestObserver.TimeoutMs, true
		}
	}
	return 0, false
}

func (manager *Manager) observationTargets(ctx context.Context, capability sdk.Capability) ([]LedgerTarget, error) {
	entries, err := manager.catalog.list(ctx)
	if err != nil {
		return nil, err
	}
	var targets []LedgerTarget
	for _, entry := range entries {
		timeoutMs, declared := observerTimeout(entry.Manifest, capability)
		if entry.Disabled || !declared || entry.observerEnablementErr != nil {
			continue
		}
		config, err := manager.runtimeModuleConfigContext(ctx, entry.Manifest)
		if err != nil {
			// An unreadable configuration revokes only that module.
			continue
		}
		timeout := time.Duration(timeoutMs) * time.Millisecond
		if timeout == 0 {
			timeout = 250 * time.Millisecond
		}
		targets = append(targets, LedgerTarget{Module: entry.Manifest.ID, Revision: entry.Revision,
			Directory: entry.Directory, ConfigVersion: config.Version, EnableGeneration: entry.EnableGeneration, Timeout: timeout})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return targets, nil
}

func (manager *Manager) ObserveLedger(ctx context.Context, target LedgerTarget, row sdk.LedgerObservation) error {
	if err := sdk.ValidateLedgerObservation(row); err != nil {
		return err
	}
	return manager.callObserver(ctx, target, sdk.LedgerObserver, sdk.MethodLedgerObserve, row)
}

func (manager *Manager) ObserveRequest(ctx context.Context, target LedgerTarget, event sdk.RequestObservation) error {
	if err := sdk.ValidateRequestObservation(event); err != nil {
		return err
	}
	return manager.callObserver(ctx, target, sdk.RequestObserver, sdk.MethodRequestObserve, event)
}

func (manager *Manager) callObserver(ctx context.Context, target LedgerTarget, capability sdk.Capability, method string, event any) error {
	worker, client, err := manager.acquire(ctx, target.Module, &expectedGeneration{
		packageRevision: target.Revision, configVersion: target.ConfigVersion, enableGeneration: &target.EnableGeneration,
	})
	if err != nil {
		return err
	}
	owned, cancel := context.WithCancel(ctx)
	lease := &Lease{manager: manager, worker: worker, client: client, group: client.group, ctx: owned, cancel: cancel}
	defer lease.Close()
	if _, declared := observerTimeout(worker.entry.Manifest, capability); worker.entry.Directory != target.Directory || !declared {
		return errors.New("observer generation changed")
	}
	_, err = lease.Call(ctx, method, sdk.Scope{}, event, nil)
	return err
}
