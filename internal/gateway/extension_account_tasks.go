package gateway

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

func (server *Server) activateAccountTasks(ctx context.Context) func() {
	if server.extensions == nil {
		return func() {}
	}
	return serveExtensionOwner(ctx, "account tasks", server.extensions.BeginAccountTasks)
}

func (server *Server) accountTaskRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		original := access.Caller(request.Context())
		localOwner := extensionhost.LocalOwner(extensionToolHTTPContext(request))
		if original.KeyID == "" && !localOwner {
			next.ServeHTTP(writer, request)
			return
		}
		caller := "key:" + original.KeyID
		if original.KeyID == "" {
			caller = "local-owner"
		}
		ctx := extensionhost.WithAccountTaskAdmission(request.Context(), caller, server.accountTaskAdmitter(original, localOwner))
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func (server *Server) accountTaskAdmitter(original access.Identity, localOwner bool) extensionhost.AccountTaskAdmit {
	return func(ctx context.Context, selection extensionhost.AccountTaskSelection) (extensionhost.AccountTaskAdmission, error) {
		who := original
		if who.KeyID != "" {
			var err error
			who, err = access.CurrentIdentity(original)
			if err != nil {
				return extensionhost.AccountTaskAdmission{}, err
			}
		} else if !localOwner {
			return extensionhost.AccountTaskAdmission{}, errors.New("paid account task has no current caller authority")
		}
		if who.ReadOnly || who.Browser {
			return extensionhost.AccountTaskAdmission{}, errors.New("paid account task caller cannot write")
		}
		ctx = access.WithIdentity(ctx, who)
		ctx = usage.WithOwnCallAppender(ctx, budget.Append)
		ctx, selected, scope, current, err := provider.PrepareExtensionAccountTask(ctx, server.extensions, selection)
		if err != nil {
			return extensionhost.AccountTaskAdmission{}, err
		}
		if !modelAllowed(who, selected, selection.Model) ||
			!accountAllowed(who, selected, provider.CurrentAccountIDs(ctx)) {
			return extensionhost.AccountTaskAdmission{}, errors.New("paid account task is outside the caller's current model/account authority")
		}
		candidate, err := accountTaskCandidate(ctx, selected, selection.Model)
		if err != nil {
			return extensionhost.AccountTaskAdmission{}, err
		}
		if _, resting := restedUntil(candidate); resting {
			return extensionhost.AccountTaskAdmission{}, errors.New("paid account task account/model is resting")
		}
		ctx = context.WithValue(ctx, budgetExtensionManagerKey{}, server.extensions)
		now := limitClock()
		if hasScopedBudget(who) {
			spender, err := withSpentBudget(ctx, who, now)
			if err != nil {
				return extensionhost.AccountTaskAdmission{}, err
			}
			if spent, ok := spentFor(spender, selected, selection.Model); ok {
				return extensionhost.AccountTaskAdmission{}, errors.New(spent.Why)
			}
		}
		releaseBudget, err := reserveFor(ctx, who, selection.Bytes, selected.ID+"/"+selection.Model, now)
		if err != nil {
			return extensionhost.AccountTaskAdmission{}, err
		}
		hold := &keyHold{s: server, who: who, turn: make(chan struct{}, 1)}
		var releases []func()
		releases = append(releases, releaseBudget, hold.release)
		release := sync.OnceFunc(func() {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
		})
		if _, err := hold.take(ctx); err != nil {
			release()
			return extensionhost.AccountTaskAdmission{}, err
		}
		lane, _, err := server.lanes.take(ctx, candidate.who(), selected.LaneLimit(),
			selected.QueueLimit, time.Duration(selected.QueueWait)*time.Second, nil)
		if err != nil {
			release()
			return extensionhost.AccountTaskAdmission{}, err
		}
		releases = append(releases, lane)
		if _, err := server.rpms.wait(ctx, candidate.who(), selected.RPMLimit(), rpmWait(selected)); err != nil {
			release()
			return extensionhost.AccountTaskAdmission{}, err
		}
		check := func(checkCtx context.Context) error {
			if original.KeyID != "" {
				latest, err := access.CurrentIdentity(original)
				if err != nil || !reflect.DeepEqual(latest, who) {
					return errors.New("paid account task caller policy changed during admission")
				}
			}
			if !accountAllowed(who, selected, provider.CurrentAccountIDs(checkCtx)) {
				return errors.New("paid account task is outside the caller's current model/account authority")
			}
			if _, resting := restedUntil(candidate); resting {
				return errors.New("paid account task account/model began resting")
			}
			if err := current(checkCtx); err != nil {
				return err
			}
			if selected.AtOnce() > 0 {
				_, err := accountTaskCandidate(checkCtx, selected, selection.Model)
				return err
			}
			return nil
		}
		if err := check(ctx); err != nil {
			release()
			return extensionhost.AccountTaskAdmission{}, err
		}
		return extensionhost.AccountTaskAdmission{Context: ctx, Scope: scope, Check: check, Release: release}, nil
	}
}

func accountTaskCandidate(ctx context.Context, selected provider.Provider, model string) (candidate, error) {
	primary, err := provider.FindContext(ctx, selected.ID)
	if err != nil {
		if ctx.Err() != nil {
			return candidate{}, ctx.Err()
		}
		return candidate{}, errors.New("paid task provider's current routing pool is unavailable")
	}
	if primary.Account == nil {
		return candidate{}, errors.New("paid task provider's current routing pool is unavailable")
	}
	current, beside, _, _ := perKeyBarredContext(ctx, *primary, model, "")
	if err := ctx.Err(); err != nil {
		return candidate{}, err
	}
	for _, entry := range append(current, beside...) {
		if entry.reserve == 0 && entry.p.Account != nil && entry.p.Account.User == selected.Account.User {
			return entry, nil
		}
	}
	return candidate{}, errors.New("paid task account is outside the current routing pool")
}
