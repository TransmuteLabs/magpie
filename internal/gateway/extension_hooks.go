package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// gatewayHookRun lets both runtimes share client-facing response capture.
// Each run owns its accepted request and releases its own runtime generation.
type gatewayHookRun interface {
	Finish(context.Context, json.RawMessage, bool, ...json.RawMessage)
	Close()
}

var gatewayExtensionManager = extension.Default

func gatewayHookChain(w http.ResponseWriter, r *http.Request, from provider.Protocol, body []byte) (http.ResponseWriter, []byte, func()) {
	legacy := plugin.HasGateway()
	manager := gatewayExtensionManager()
	sdkHooks := manager != nil && manager.HasGateway()
	if !legacy && !sdkHooks {
		return w, body, func() {}
	}
	// Derive provenance and identity once, before either runtime can enrich the
	// body. Neither receives HTTP headers, auth, key names or peer addresses.
	info := sdk.GatewayContext{
		Origin: gatewayOrigin(r), Protocol: string(from), Model: modelOf(body),
		Stream: streamOf(body), Path: r.URL.Path, Agent: agentOf(r),
		Session: sessionOf(r.Header), Kind: hookKind(r, agentOf(r)),
	}
	if req, err := parse(from, body); err == nil {
		info.Cwd, info.Query = workDir(req), gatewayQuery(req)
	}
	sdkInfo := info
	if hubFromPage(r) {
		sdkInfo.Origin = "browser"
	}
	var finish []func()
	wrap := func(accepted json.RawMessage, run gatewayHookRun) {
		var done func()
		w, done = gatewayResponse(w, r, from, accepted, run)
		body = accepted
		finish = append(finish, func() {
			defer run.Close()
			done()
		})
	}
	if legacy {
		waited := noteWait(r.Context(), "plugin hooks")
		run := plugin.GatewayBegin(r.Context(), body, plugin.GatewayContext{
			Origin: info.Origin, Protocol: info.Protocol, Model: info.Model, Stream: info.Stream,
			Path: info.Path, Agent: info.Agent, Session: info.Session, Kind: info.Kind,
			Cwd: info.Cwd, Query: info.Query,
		})
		waited()
		if run != nil {
			wrap(run.Body, run)
		}
	}
	if sdkHooks && r.Context().Err() == nil {
		waited := noteWait(r.Context(), "plugin hooks")
		run := manager.BeginGateway(r.Context(), body, sdkInfo)
		waited()
		if run != nil {
			wrap(run.Body, run)
		}
	}
	return w, body, func() {
		// SDK capture wraps legacy capture, which wraps the client. Finish in
		// the same reverse order as the response middleware; each sees the
		// delivered bytes and the request that its own hook chain accepted.
		for i := len(finish) - 1; i >= 0; i-- {
			finish[i]()
		}
	}
}

func gatewayHooksAvailable() bool {
	if plugin.HasGateway() {
		return true
	}
	manager := gatewayExtensionManager()
	return manager != nil && manager.HasGateway()
}

func gatewaySignIn(ctx context.Context, event plugin.SignInEvent) {
	if plugin.HasGateway() {
		plugin.GatewaySignIn(ctx, event)
	}
	if manager := gatewayExtensionManager(); manager != nil && manager.HasGateway() {
		signIn := sdk.GatewaySignInEvent{
			Provider: event.Provider, ProviderName: event.ProviderName, Account: event.Account,
			State: event.State,
		}
		if event.EndsAt != nil {
			signIn.EndsAt = event.EndsAt.Format(time.RFC3339)
		}
		manager.GatewaySignIn(ctx, signIn)
	}
}
