package provider

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"slices"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
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
	return append(pluginCheckinProviders(), extensionCheckinProviders(ctx)...)
}

func extensionCheckinProviders(ctx context.Context) []pluginCheckinProvider {
	if ctx.Err() != nil || settings.AccountDiscoveryOff() {
		return nil
	}
	host := providerExtensionHost()
	owner, ok := host.(interface {
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
	revisions := map[string]string{}
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
		discovered, err := discoverExtensionModuleContext(ctx, host, entry)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Printf("extension check-ins unavailable for %s: %v", manifest.ID, err)
			continue
		}
		providers = append(providers, discovered...)
		revisions[manifest.ID] = entry.Revision
	}
	if len(providers) == 0 {
		return nil
	}
	providers, err = extensionPolicies(providers)
	if err != nil {
		log.Printf("extension check-ins unavailable: %v", err)
		return nil
	}
	out := []pluginCheckinProvider{}
	positions := map[string]int{}
	for _, selected := range providers {
		binding := selected.Account.extension
		descriptor := binding.descriptor.Checkin
		if descriptor == nil || binding.scope.Account == "" {
			continue
		}
		revision := revisions[binding.scope.Plugin]
		config, err := host.ModuleConfig(ctx, binding.scope.Plugin)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil || revision == "" {
			continue
		}
		position, found := positions[selected.ID]
		if !found {
			position = len(out)
			positions[selected.ID] = position
			out = append(out, pluginCheckinProvider{ID: selected.ID, Name: selected.Name, recheck: descriptor.Recheck})
		}
		out[position].accounts = append(out[position].accounts, pluginCheckinAcct{
			User: selected.Account.User, On: extensionAccountOn(selected.ID, selected.Account.User),
			key: pluginCheckinKey(selected.ID, binding.scope.Account), card: selected.ID,
			do: func(ctx context.Context) WorkBuddyCheckin {
				admit := func(current context.Context) error {
					return extensionCheckinAccountCurrent(current, selected.ID, binding.scope.Account)
				}
				if err := admit(ctx); err != nil {
					return wbCheckinFailed(err)
				}
				prepared, err := binding.prepare(ctx)
				if err != nil {
					return wbCheckinFailed(err)
				}
				scope := prepared.currentScope()
				ctx, err = extensionNetworkContext(ctx, scope)
				if err != nil {
					return wbCheckinFailed(err)
				}
				day, _ := ctx.Value(checkinDayKey{}).(string)
				if day == "" {
					return wbCheckinFailed(errors.New("check-in requires the ordinary owner's day"))
				}
				execution, err := owner.CheckinAccountWithAdmission(ctx, scope, revision, config.Version,
					sdk.AccountCheckinRequest{Day: day, Proxy: netproxy.Choice(ctx)}, admit)
				result := WorkBuddyCheckin{}
				if err != nil {
					result = wbCheckinFailed(err)
				} else if execution.Result == nil {
					result = wbCheckinFailed(errors.New("check-in returned no validated result"))
				} else {
					value := execution.Result
					result.Outcome, result.Msg = value.Outcome, value.Message
					if value.Credit != nil {
						result.Credit = *value.Credit
					}
					if value.Streak != nil {
						result.Streak = *value.Streak
					}
					result.ValidUntil, _ = time.Parse(time.RFC3339, value.Until)
				}
				if execution.Receipt.Scope.Operation != "" {
					result.Operation = &CheckinOperationReceipt{Scope: execution.Receipt.Scope, State: execution.Receipt.State}
				}
				return result
			},
		})
	}
	return out
}

func extensionCheckinAccountCurrent(ctx context.Context, provider, account string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	set, err := settings.LoadChecked()
	if err != nil {
		return err
	}
	if settings.AccountDiscoveryOffIn(set) {
		return errAccountDiscoveryOff
	}
	logins, err := filememo.ReadNow("logins", loginsPath(), parseLogins)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, login := range logins {
		if login.Agent == provider && login.User == account {
			if login.On && login.Hidden == "" {
				return nil
			}
			break
		}
	}
	return errors.New("extension check-in account is off, hidden or unavailable")
}
