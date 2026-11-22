package provider

import (
	"context"
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type checkinDayKey struct{}

type CheckinOperationReceipt struct {
	Scope sdk.Scope `json:"scope"`
	State string    `json:"state"`
}

func withCheckinDay(ctx context.Context, day string) context.Context {
	return context.WithValue(ctx, checkinDayKey{}, day)
}

func allPluginCheckinProviders(ctx context.Context) []pluginCheckinProvider {
	if ctx.Err() != nil {
		return nil
	}
	return append(pluginCheckinProviders(ctx), extensionCheckinProviders(ctx)...)
}

func extensionCheckinProviders(ctx context.Context) []pluginCheckinProvider {
	if ctx.Err() != nil || settings.AccountDiscoveryOff() {
		return nil
	}
	host := providerExtensionHost()
	owner, ok := host.(interface {
		CheckinModuleConfig(context.Context, extensionhost.Installed) (string, error)
		CheckinAccountWithAdmission(context.Context, sdk.Scope, string, string, sdk.AccountCheckinRequest, func(context.Context) error) (extensionhost.CheckinExecution, error)
	})
	if !ok {
		return nil
	}
	installed, err := activeProviderModules(ctx, host)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("extension check-ins unavailable: %v", err)
		}
		return nil
	}
	var providers []Provider
	modules := map[string]extensionhost.Installed{}
	owners := sync.OnceValue(func() extensionSubscriptionOwners { return currentSubscriptionOwners(ctx) })
	for _, entry := range installed {
		manifest := entry.Manifest
		if manifest.Contract.Version.Minor < 8 || !manifest.Contract.Has(sdk.AccountAuth) ||
			!manifest.Contract.Has(sdk.ModuleRPC) {
			continue
		}
		if !slices.ContainsFunc(manifest.Providers, func(descriptor sdk.ProviderDescriptor) bool {
			return descriptor.Checkin != nil
		}) {
			continue
		}
		if settings.AccountDiscoveryOff() {
			return nil
		}
		config, err := owner.CheckinModuleConfig(ctx, entry)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Printf("extension check-ins unavailable for %s: read module configuration: %v", manifest.ID, err)
			continue
		}
		discovered, err := projectExtensionProviders(ctx, host, entry, entry.Revision+"\x00"+config, owners)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Printf("extension check-ins unavailable for %s: %v", manifest.ID, err)
			continue
		}
		providers = append(providers, discovered...)
		modules[manifest.ID] = entry
	}
	if len(providers) == 0 {
		return nil
	}
	saved, err := extensionPolicies(providers)
	if err != nil {
		log.Printf("extension check-ins unavailable: %v", err)
		return nil
	}
	out := []pluginCheckinProvider{}
	positions := map[string]int{}
	accountOn := make(map[extensionLoginKey]bool, len(saved))
	for _, login := range saved {
		key := extensionLoginKey{login.Agent, login.User}
		if _, exists := accountOn[key]; !exists {
			accountOn[key] = extensionPolicyOn(login)
		}
	}
	configs := make(map[string]string, len(modules))
	for _, selected := range providers {
		binding := selected.Account.extension
		descriptor := binding.descriptor.Checkin
		if descriptor == nil || binding.scope.Account == "" {
			continue
		}
		module := modules[binding.scope.Plugin]
		config, checked := configs[binding.scope.Plugin]
		if !checked {
			config, err = owner.CheckinModuleConfig(ctx, module)
			if err != nil {
				config = ""
			}
			configs[binding.scope.Plugin] = config
		}
		if ctx.Err() != nil {
			return nil
		}
		if config == "" {
			continue
		}
		binding.modelKey = module.Revision + "\x00" + config
		position, found := positions[selected.ID]
		if !found {
			position = len(out)
			positions[selected.ID] = position
			out = append(out, pluginCheckinProvider{ID: selected.ID, Name: selected.Name, recheck: descriptor.Recheck})
		}
		on, known := accountOn[extensionKey(binding.identity())]
		out[position].accounts = append(out[position].accounts, pluginCheckinAcct{
			User: selected.Account.User, On: !known || on,
			key: pluginCheckinKey(selected.ID, selected.Account.User), card: selected.ID,
			do: func(ctx context.Context) WorkBuddyCheckin {
				admit := func(current context.Context) error {
					return extensionCheckinAccountCurrent(current, binding, known)
				}
				if err := admit(ctx); err != nil {
					return extensionCheckinPreflightFailure(err)
				}
				prepared, err := binding.prepareWithRefreshPolicy(ctx, func(current context.Context, public extensionPublicIdentity) error {
					return extensionCheckinLoginCurrent(current, public, known)
				})
				if err != nil {
					return extensionCheckinPreflightFailure(err)
				}
				scope := prepared.currentScope()
				ctx, err = extensionNetworkContext(ctx, scope)
				if err != nil {
					return extensionCheckinPreflightFailure(err)
				}
				day, _ := ctx.Value(checkinDayKey{}).(string)
				if day == "" {
					return wbCheckinFailed(errors.New("check-in requires the ordinary owner's day"))
				}
				execution, err := owner.CheckinAccountWithAdmission(ctx, scope, module.Revision, config,
					sdk.AccountCheckinRequest{Day: day, Proxy: netproxy.Choice(ctx)}, admit)
				receipt := execution.Receipt
				result := WorkBuddyCheckin{}
				if err != nil && execution.Result == nil && (receipt.Scope.Operation == "" || receipt.State == "not-started") {
					result = extensionCheckinPreflightFailure(err)
				} else if err != nil {
					result = wbCheckinFailed(err)
				} else if execution.Result == nil {
					result = wbCheckinFailed(errors.New("check-in returned no validated result"))
				} else {
					value := execution.Result
					result.Outcome, result.Msg = value.Outcome, value.Message
					if value.Outcome == sdk.AccountCheckinRateLimited {
						result.Outcome = CheckinFailed
					}
					if value.Credit != nil {
						result.Credit = *value.Credit
					}
					if value.Streak != nil {
						result.Streak = *value.Streak
					}
					result.ValidUntil, _ = time.Parse(time.RFC3339, value.Until)
				}
				if result.Outcome != "" && receipt.Scope.Operation != "" {
					result.Operation = &CheckinOperationReceipt{Scope: receipt.Scope, State: receipt.State}
				}
				return result
			},
		})
	}
	return out
}

func extensionCheckinPreflightFailure(err error) WorkBuddyCheckin {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return WorkBuddyCheckin{}
	}
	var rpc *sdk.Error
	if errors.As(err, &rpc) && rpc != nil && (rpc.Code == "canceled" || rpc.Code == "deadline") {
		return WorkBuddyCheckin{}
	}
	return wbCheckinFailed(err)
}

func extensionCheckinAccountCurrent(ctx context.Context, binding *extensionAccount, requirePolicy bool) error {
	if err := requireExtensionAccountDiscovery(ctx); err != nil {
		return err
	}
	public := binding.identity()
	if err := CheckExtensionSubscription(ctx, binding.scope.Plugin, binding.scope.Provider, public.revision); err != nil {
		return err
	}
	return extensionCheckinLoginCurrent(ctx, public, requirePolicy)
}

func extensionCheckinLoginCurrent(ctx context.Context, public extensionPublicIdentity, requirePolicy bool) error {
	logins, err := readCurrentLogins()
	if err != nil {
		return err
	}
	for _, login := range logins {
		if public.hasPolicy(login) {
			if login.On && login.Hidden == "" {
				return ctx.Err()
			}
			return errors.New("extension check-in account is off or hidden")
		}
	}
	if requirePolicy {
		return errors.New("extension check-in account policy changed")
	}
	return ctx.Err()
}
