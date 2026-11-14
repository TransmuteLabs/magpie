package provider

import (
	"context"
	"errors"
	"log"
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

func allPluginCheckinProviders() []pluginCheckinProvider {
	return append(pluginCheckinProviders(), extensionCheckinProviders()...)
}

func extensionCheckinProviders() []pluginCheckinProvider {
	if settings.AccountDiscoveryOff() {
		return nil
	}
	host := providerExtensionHost()
	owner, ok := host.(interface {
		CheckinAccount(context.Context, sdk.Scope, string, string, sdk.AccountCheckinRequest) (extensionhost.CheckinExecution, error)
	})
	if !ok {
		return nil
	}
	installed, err := host.List()
	if err != nil {
		log.Printf("extension check-ins unavailable: %v", err)
		return nil
	}
	providers, err := ExtensionProviders()
	if err != nil {
		log.Printf("extension check-ins unavailable: %v", err)
		return nil
	}
	revisions := map[string]string{}
	for _, entry := range installed {
		if !entry.Disabled {
			revisions[entry.Manifest.ID] = entry.Revision
		}
	}
	out := []pluginCheckinProvider{}
	positions := map[string]int{}
	for _, selected := range providers {
		binding := selected.Account.extension
		descriptor := binding.descriptor.Checkin
		if descriptor == nil || binding.manifest.Contract.Version.Minor < 8 || binding.scope.Account == "" {
			continue
		}
		revision := revisions[binding.scope.Plugin]
		config, err := host.ModuleConfig(context.Background(), binding.scope.Plugin)
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
				if settings.AccountDiscoveryOff() {
					return wbCheckinFailed(errAccountDiscoveryOff)
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
				execution, err := owner.CheckinAccount(ctx, scope, revision, config.Version,
					sdk.AccountCheckinRequest{Day: day, Proxy: netproxy.Choice(ctx)})
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
