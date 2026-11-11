package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

type zcodeFallbackDenial struct {
	status int
	reason string
}

func (e *zcodeFallbackDenial) Error() string { return e.reason }

func denyZCodeFallback(status int, format string, args ...any) error {
	return &zcodeFallbackDenial{status: status, reason: fmt.Sprintf(format, args...)}
}

// authorizeZCodeFallback applies ordinary routing policy to the exact account
// already in use. It never selects a different account or treats the native
// lane's unspent allowance as evidence about the ordinary model.
func authorizeZCodeFallback(ctx context.Context, c candidate, from provider.Protocol, request provider.ZCodeFallbackRequest) error {
	if heldKey(access.Caller(ctx)) {
		return denyZCodeFallback(http.StatusForbidden, "this gateway key cannot implicitly change models; request the ordinary model explicitly")
	}
	if !c.p.IsPlugin() || c.p.PluginProvider() != "zcode" || request.Provider != "zcode" ||
		request.Account == "" || request.Account != c.p.PluginAccountKey() {
		return denyZCodeFallback(http.StatusForbidden, "ordinary ZCode fallback cannot change account")
	}
	if request.Model != "GLM-5.3" && request.Model != "GLM-5.3-Flash" {
		return denyZCodeFallback(http.StatusBadRequest, "the ordinary ZCode fallback model is invalid")
	}
	listed := c.p.ListedModelID(request.Model)
	if _, off := provider.ModelDisabled(c.p.ID, listed); off || provider.DisabledUnknown() {
		return denyZCodeFallback(http.StatusForbidden, "ordinary model %s is disabled or its enablement is unknown", listed)
	}
	resolvedProvider, resolvedModel, ok := provider.ResolveRoute(c.p.ID + "/" + listed)
	if !ok || resolvedProvider.ID != c.p.ID || !strings.EqualFold(c.p.OwnModelID(resolvedModel), request.Model) {
		return denyZCodeFallback(http.StatusForbidden, "ordinary model %s is not enabled for routing", listed)
	}
	on, _, left, barred := perKeyEvery(c.p, listed, from)
	same := func(other candidate) bool {
		return other.who() == c.who() && other.p.IsPlugin() && other.p.PluginAccountKey() == request.Account
	}
	var target candidate
	found := false
	for _, other := range on {
		if same(other) {
			target, found = other, true
			break
		}
	}
	if !found {
		for _, other := range barred {
			if same(other) && other.capped != nil {
				if other.capped.unread != "" {
					return denyZCodeFallback(http.StatusTooManyRequests, "ordinary model %s is held because its usage is unavailable: %s", listed, other.capped.unread)
				}
				return denyZCodeFallback(http.StatusTooManyRequests, "ordinary model %s is held at its usage cap", listed)
			}
		}
		for _, other := range left {
			if same(other) {
				return denyZCodeFallback(http.StatusForbidden, "the selected account does not list ordinary model %s", listed)
			}
		}
		return denyZCodeFallback(http.StatusForbidden, "the selected account is not enabled for ordinary model %s", listed)
	}
	now := time.Now()
	if _, resting := restingOf(target); resting {
		return denyZCodeFallback(http.StatusTooManyRequests, "ordinary model %s or its account is resting", listed)
	}
	if target.model != request.Model {
		target.model = request.Model
		if _, resting := restingOf(target); resting {
			return denyZCodeFallback(http.StatusTooManyRequests, "ordinary model %s or its account is resting", listed)
		}
	}
	if allowance, known := target.allowance(); known {
		used, _ := allowance.For(request.Model, now)
		if listed != request.Model {
			if v, _ := allowance.For(listed, now); v > used {
				used = v
			}
		}
		if used >= 100 {
			return denyZCodeFallback(http.StatusTooManyRequests, "ordinary model %s has exhausted its allowance (%.0f%% used)", listed, used)
		}
	}
	return nil
}
