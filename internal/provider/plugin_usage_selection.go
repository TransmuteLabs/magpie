package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

type pluginUsageSelection struct {
	Agent     string `json:"agent"`
	User      string `json:"user"`
	Slot      string `json:"slot"`
	LoginID   string `json:"loginId"`
	Epoch     string `json:"epoch,omitempty"`
	AuthEpoch string `json:"authEpoch,omitempty"`
	Public    bool   `json:"public,omitempty"`
}

var errPluginUsageAccountChanged = errors.New("the plugin account has changed")
var errPluginUsageDiscovery = errors.New("the usage discovery policy cannot be read")

func pluginUsageOf(provider plugin.Provider, login pluginLogin) *pluginUsageSelection {
	saved := login.saved
	return &pluginUsageSelection{
		Agent: saved.Agent, User: saved.User, Slot: saved.Home,
		LoginID: loginID(saved), Epoch: saved.PluginObservations.Epoch,
		AuthEpoch: login.acct.AuthEpoch, Public: publicPlugin(provider),
	}
}

func (selected *pluginUsageSelection) matches(other *pluginUsageSelection) bool {
	if selected == nil {
		return true
	}
	if other == nil {
		return selected.Epoch == "" && selected.AuthEpoch == ""
	}
	return *selected == *other
}

func (selected *pluginUsageSelection) currentAuth(epochs map[string]string) bool {
	epoch, present := epochs[selected.Slot]
	return present && epoch == selected.AuthEpoch
}

func (selected *pluginUsageSelection) loginIndex(logins []savedLogin) int {
	return slices.IndexFunc(logins, func(login savedLogin) bool {
		return login.Agent == selected.Agent && login.Home == selected.Slot &&
			loginID(login) == selected.LoginID && login.PluginObservations.Epoch == selected.Epoch &&
			accountKey(login.User) == accountKey(selected.User) && login.Hidden == ""
	})
}

func (selected *pluginUsageSelection) deferred(ctx context.Context, quota SubscriptionQuota) SubscriptionQuota {
	reason := errPluginUsageAccountChanged
	if quota.ReadDeferred && quota.Error != "" {
		reason = errors.New(quota.Error)
	}
	if err := ctx.Err(); err != nil {
		reason = err
	}
	return deferPluginQuota(quota, selected.User, reason)
}

func currentUsageDiscovery(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	off, err := settings.AccountDiscoveryOffNow()
	if err != nil {
		return false, fmt.Errorf("%w: %w", errPluginUsageDiscovery, err)
	}
	return off, ctx.Err()
}

func withCurrentPluginUsage(ctx context.Context, quota SubscriptionQuota, provider string, observation pluginLoginObservation) SubscriptionQuota {
	selected := quota.pluginUsage
	observeLogin := observation.recordsLogin()
	wallet := countsWalletSpend(quota, selected.User)
	writes := observeLogin || wallet
	if writes {
		release, err := plugin.GuardCommunitySubscription(ctx, provider)
		if err != nil {
			return deferPluginQuota(quota, selected.User, err)
		}
		defer release()
	} else if err := plugin.CheckCommunitySubscription(ctx, provider); err != nil {
		return deferPluginQuota(quota, selected.User, err)
	}
	discovery := func(allowOff bool) (bool, error) {
		off, err := currentUsageDiscovery(ctx)
		if err == nil && off && !allowOff {
			err = errAccountDiscoveryOff
		}
		return off, err
	}
	off, err := discovery(selected.Public)
	if err != nil {
		return deferPluginQuota(quota, selected.User, err)
	}
	if writes && !off {
		if !loginsMu.LockCheckedContext(ctx) {
			err := ctx.Err()
			if err == nil {
				err = errors.New("the plugin account lock is unavailable")
			}
			return deferPluginQuota(quota, selected.User, err)
		}
		defer loginsMu.Unlock()
		off, err = discovery(selected.Public)
		if err != nil {
			return deferPluginQuota(quota, selected.User, err)
		}
	}
	var logins []savedLogin
	index := -1
	if !off {
		logins, err = readCurrentLogins()
		if err != nil {
			return deferPluginQuota(quota, selected.User, errors.New("the plugin account cannot be read"))
		}
		index = selected.loginIndex(logins)
		if index < 0 {
			return deferPluginQuota(quota, selected.User, errPluginUsageAccountChanged)
		}
	}
	publish := func() error {
		if observeLogin && index >= 0 {
			err := recordPluginLogin(logins, index, observation, func() error {
				_, err := discovery(false)
				return err
			})
			if err != nil && !(selected.Public && errors.Is(err, errAccountDiscoveryOff)) {
				quota = deferPluginQuota(quota, selected.User, err)
				return nil
			}
		}
		if wallet {
			quota = withWalletSpend(ctx, quota, selected.User, time.Now(), walletObservation{
				pluginLoginOrder: observation.order, LoginID: observation.loginID,
				Epoch: observation.epoch, AuthEpoch: selected.AuthEpoch,
			}, func() error {
				_, err := discovery(selected.Public)
				return err
			})
		}
		return nil
	}
	switch {
	case off:
		_ = publish()
	case writes:
		if err := plugin.WithAuthEpoch(ctx, selected.Slot, selected.AuthEpoch, publish); err != nil {
			reason := ctx.Err()
			if reason == nil {
				reason = errPluginUsageAccountChanged
			}
			return deferPluginQuota(quota, selected.User, reason)
		}
	default:
		epochs, err := plugin.CurrentAuthEpochs(ctx)
		if err != nil {
			return deferPluginQuota(quota, selected.User, err)
		}
		if !selected.currentAuth(epochs) {
			return deferPluginQuota(quota, selected.User, errPluginUsageAccountChanged)
		}
	}
	return quota
}

func deferPluginQuota(q SubscriptionQuota, user string, err error) SubscriptionQuota {
	return SubscriptionQuota{
		pluginUsage: q.pluginUsage, readSeq: q.readSeq,
		Provider: q.Provider, Name: q.Name, Icon: q.Icon, User: user,
		Kind: q.Kind, From: q.From, Via: q.Via, Alias: q.Alias, Named: q.Named, Seat: q.Seat,
		Error: err.Error(), ReadDeferred: true,
	}
}
