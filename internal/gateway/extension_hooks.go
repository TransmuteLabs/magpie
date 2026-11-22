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

func (s *Server) selectGateway(r *http.Request) extension.GatewaySelection {
	origin := gatewayOrigin(r)
	if hubFromPage(r) {
		origin = "browser"
	}
	return s.extensions.SelectGateway(r.Context(), origin)
}

func (s *Server) gatewayHooks(w http.ResponseWriter, r *http.Request, from provider.Protocol, body []byte) (http.ResponseWriter, []byte, func()) {
	return applyGatewayHooks(w, r, from, body, s.selectGateway(r))
}

func applyGatewayHooks(w http.ResponseWriter, r *http.Request, from provider.Protocol, body []byte, selection extension.GatewaySelection) (http.ResponseWriter, []byte, func()) {
	legacy := plugin.HasGateway()
	if !legacy && !selection.HasHooks() {
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
	var finish []func()
	accept := func(accepted json.RawMessage, run gatewayHookRun, observe bool) {
		body = accepted
		if !observe {
			finish = append(finish, func() { run.Finish(r.Context(), nil, false) })
			return
		}
		var done func()
		w, done = gatewayResponse(w, r, from, accepted, run)
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
			Cwd: info.Cwd, Query: info.Query, Caller: pluginCaller(r.Context()),
		})
		waited()
		if run != nil {
			accept(run.Body, run, true)
		}
	}
	if selection.HasHooks() && r.Context().Err() == nil {
		waited := noteWait(r.Context(), "plugin hooks")
		run := selection.BeginGateway(r.Context(), body, info)
		waited()
		if run != nil {
			accept(run.Body, run, run.ObservesResponse())
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

func (s *Server) signInHooksAvailable(ctx context.Context) bool {
	if plugin.HasGateway() {
		return true
	}
	manager := s.extensions
	return manager != nil && manager.HasGatewaySignIn(ctx)
}

func (s *Server) gatewaySignIn(ctx context.Context, event plugin.SignInEvent) {
	if plugin.HasGateway() {
		plugin.GatewaySignIn(ctx, event)
	}
	if manager := s.extensions; manager != nil {
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
