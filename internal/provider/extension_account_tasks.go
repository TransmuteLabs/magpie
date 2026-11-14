package provider

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func PrepareExtensionAccountTask(ctx context.Context, manager *extensionhost.Manager, selection extensionhost.AccountTaskSelection) (context.Context, Provider, sdk.Scope, func(context.Context) error, error) {
	fail := func(err error) (context.Context, Provider, sdk.Scope, func(context.Context) error, error) {
		return ctx, Provider{}, sdk.Scope{}, nil, err
	}
	if manager == nil || manager.Namespace() != providerExtensionHost().Namespace() {
		return fail(errors.New("paid task belongs to another provider host"))
	}
	policy, err := extensionKeepAlivePolicyNow()
	id := ExtensionProviderID(selection.Scope.Plugin, selection.Scope.Provider)
	if err != nil || !policy.allows(id, selection.Scope.Account) {
		return fail(errors.New("paid task account is off, unlisted or unreadable"))
	}
	if _, disabled := ModelDisabled(id, selection.Model); disabled || DisabledUnknown() {
		return fail(errors.New("paid task model is disabled or its policy is unreadable"))
	}
	entries, err := manager.List()
	position := slices.IndexFunc(entries, func(entry extensionhost.Installed) bool {
		return entry.Manifest.ID == selection.Scope.Plugin && !entry.Disabled && entry.Revision == selection.Revision
	})
	if err != nil || position < 0 {
		return fail(errors.New("paid task package changed"))
	}
	entry := entries[position]
	config, err := manager.ModuleConfig(ctx, entry.Manifest.ID)
	if err != nil || config.Version != selection.Config {
		return fail(errors.New("paid task configuration changed"))
	}
	account, err := manager.Store().RefreshedAccount(selection.Scope)
	if err != nil {
		return fail(err)
	}
	var descriptor sdk.ProviderDescriptor
	found := false
	for _, candidate := range entry.Manifest.Providers {
		if candidate.ID == selection.Scope.Provider && len(candidate.AccountTasks) > 0 {
			descriptor, found = candidate, true
		}
	}
	if !found {
		return fail(errors.New("paid account tasks are no longer declared"))
	}
	revision, err := extensionModelRevision(ctx, manager, entry)
	if err != nil {
		return fail(err)
	}
	scope := selection.Scope
	scope.Generation = account.Generation
	bindingScope := scope
	bindingScope.Operation, bindingScope.Attempt, bindingScope.Session = "", 0, ""
	binding := &extensionAccount{host: manager, manifest: entry.Manifest, descriptor: descriptor,
		scope: bindingScope, modelKey: revision}
	prepared, err := binding.prepare(ctx)
	if err != nil {
		return fail(err)
	}
	binding = prepared
	scope.Generation = binding.scope.Generation
	selected := extensionProvider(binding)
	for _, stored := range policy.providers.Providers {
		if stored.ID == id {
			selected = accountSettings(selected, stored)
		}
	}
	if !selected.On() || selected.OwnPaused() || !selected.AccountServes(scope.Account, selection.Model) {
		return fail(errors.New("paid task selected account no longer serves its model"))
	}
	ctx, err = extensionNetworkContext(ctx, scope)
	if err != nil {
		return fail(err)
	}
	if err := netproxy.Held(ctx); err != nil {
		return fail(err)
	}
	models, err := binding.fetchModels(ctx)
	if err != nil || !slices.ContainsFunc(models, func(model catalog.Model) bool { return model.ID == selection.Model }) {
		return fail(errors.New("paid task model is absent from its current account catalog"))
	}
	quota := extensionQuota(ctx, Login{Agent: id, User: scope.Account, On: true, extension: binding})
	if quota.Error != "" || quota.ReadDeferred || quota.extensionUsage == nil ||
		!quota.extensionUsage.fresh || !quota.extensionUsage.current() {
		return fail(errors.New("paid task requires a fresh successful account quota reading"))
	}
	for _, window := range quota.Windows {
		if !window.CountsIn(quota.Windows, selection.Model) || window.Aside {
			continue
		}
		if window.Unknown || window.Error != "" || !window.Unlimited && !window.Text && window.Used >= 100 {
			return fail(errors.New("paid task account quota is unknown or exhausted"))
		}
	}
	if allowanceOf(quota.Windows, time.Now()).CapHeld(selection.Model, HoldCaps(selected, id, scope.Account), time.Now()) != nil {
		return fail(errors.New("paid task account is held at its usage cap"))
	}
	choice := netproxy.Choice(ctx)
	check := func(current context.Context) error {
		if err := current.Err(); err != nil {
			return err
		}
		now, err := extensionKeepAlivePolicyNow()
		if err != nil || !reflect.DeepEqual(now, policy) {
			return errors.New("paid task account policy changed during admission")
		}
		if _, disabled := ModelDisabled(id, selection.Model); disabled || DisabledUnknown() {
			return errors.New("paid task model was disabled during admission")
		}
		account, err := manager.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
		if err != nil || account.Generation != scope.Generation {
			return errors.New("paid task account generation changed during admission")
		}
		network, err := extensionNetworkContext(current, scope)
		if err != nil || netproxy.Choice(network) != choice {
			return errors.New("paid task selected proxy changed during admission")
		}
		if err := netproxy.Held(network); err != nil {
			return err
		}
		return binding.checkModelRevision(current)
	}
	return ctx, selected, scope, check, nil
}
