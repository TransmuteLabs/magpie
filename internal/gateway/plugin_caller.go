package gateway

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// tellCaller tells a plugin's fetch the agent its request came from, as
// the gateway records it (callerFrom): the agent's id and name, and the
// computers a request another magpie passed on came through (@febsincos on
// X: a plugin on the receiving magpie couldn't tell which agent asked).
// The same for a request from an agent here and one passed on: the agent,
// never the magpie that passed it on. Headers the caller sent of these
// names are dropped first. The plugin host takes them off whatever the
// plugin sends on (host.js), so no vendor is told.
func tellCaller(ctx context.Context, h http.Header) {
	h.Del(provider.CallerAgentHeader)
	h.Del(provider.CallerNameHeader)
	h.Del(provider.CallerViaHeader)
	info := pluginCaller(ctx)
	if info == nil {
		return
	}
	h.Set(provider.CallerAgentHeader, url.PathEscape(info.Agent))
	h.Set(provider.CallerNameHeader, url.PathEscape(info.Name))
	if len(info.Via) > 0 {
		via := make([]string, len(info.Via))
		for i, v := range info.Via {
			via[i] = url.PathEscape(v)
		}
		h.Set(provider.CallerViaHeader, strings.Join(via, ","))
	}
}

func pluginCaller(ctx context.Context) *sdk.ExecuteCaller {
	c, ok := ctx.Value(callerCtx{}).(caller)
	if !ok || c.agent == "" {
		return nil
	}
	via := slices.Clone(c.chain)
	if len(via) == 0 && c.via != "" {
		via = []string{c.via}
	}
	return &sdk.ExecuteCaller{Agent: c.agent, Name: usage.AgentName(c.agent), Via: via}
}
