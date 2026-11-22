package gui

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionSubscriptionJSON struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Icon          string                 `json:"icon"`
	Page          string                 `json:"page,omitempty"`
	Revision      string                 `json:"revision,omitempty"`
	OwnerRevision string                 `json:"ownerRevision,omitempty"`
	Methods       []sdk.WindowAuthMethod `json:"methods,omitempty"`
	Reauthorize   bool                   `json:"reauthorize,omitempty"`
}

func sdkProviderIcon(module string) string {
	return provider.Held("sdk-provider-icon:"+module, func() string {
		ctx := context.Background()
		host := extension.Default()
		entry, err := host.ActiveModule(ctx, module)
		if err != nil {
			return "generic"
		}
		return cmp.Or(sdkPluginIcon(host.PluginPresentation(ctx, entry).Icon), "generic")
	})
}

func extensionSubscriptions() []extensionSubscriptionJSON {
	if settings.AccountDiscoveryOff() {
		return nil
	}
	var out []extensionSubscriptionJSON
	for _, item := range extension.Default().ProviderSignIns(context.Background()) {
		id, owner, visible := provider.ExtensionPublicProvider(item.Module, item.Provider)
		if !visible {
			continue
		}
		methods := slices.DeleteFunc(slices.Clone(item.Binding.Methods), func(method sdk.WindowAuthMethod) bool {
			return method.ID == sdk.SavedSignInMethod
		})
		if len(methods) == 0 && !item.Page {
			continue
		}
		row := extensionSubscriptionJSON{
			ID: id, Name: item.Name, Icon: sdkProviderIcon(item.Module), Revision: item.Revision, OwnerRevision: owner,
			Methods: methods, Reauthorize: item.Binding.Reauthorize,
		}
		if item.Page {
			row.Page = extensionPageKey(item.Module)
		}
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b extensionSubscriptionJSON) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return out
}

func extensionProviderSignInRoute(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/provider/sdk-signin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var in struct {
			ID            string `json:"id"`
			Revision      string `json:"revision"`
			OwnerRevision string `json:"ownerRevision,omitempty"`
			extensionAuthRequest
		}
		if !readExtensionAuthRequest(w, r, &in) {
			return
		}
		if !settings.AccountDiscoveryOff() && in.Revision != "" {
			host := extension.Default()
			for _, target := range host.ProviderSignIns(r.Context()) {
				id, owner, visible := provider.ExtensionPublicProvider(target.Module, target.Provider)
				if !visible || in.Revision != target.Revision || in.ID != id || in.OwnerRevision != owner {
					continue
				}
				ctx := extension.WithSubscriptionOwnerRevision(r.Context(), owner)
				if selection := in.Options.Account; in.Operation == "start" && selection != nil {
					generation, err := selection.Validate()
					scope, found := provider.ResolveExtensionLogin(id, selection.ID)
					if err != nil || !found || scope.Plugin != target.Module || scope.Provider != target.Provider || scope.Generation != generation {
						fail(w, errModuleAuthentication)
						return
					}
					in.Options.Account = &sdk.AccountSelection{ID: scope.Account, ExpectedGeneration: selection.ExpectedGeneration}
				}
				view, err := runExtensionAuth(ctx, host, target.Module, target.Revision, sdk.ProviderSignInPart, target.Binding.ID, in.extensionAuthRequest)
				if err != nil {
					fail(w, err)
					return
				}
				if view.Account != nil {
					publicID, ownerRevision, user, visible := provider.ExtensionPublicLogin(sdk.Scope{
						Plugin: target.Module, Provider: target.Provider, Account: view.Account.ID,
					})
					if !visible || publicID != id || ownerRevision != owner ||
						provider.CheckExtensionSubscription(ctx, target.Module, target.Provider, owner) != nil {
						fail(w, errModuleAuthentication)
						return
					}
					account := *view.Account
					account.ID = user
					view.Account = &account
				}
				writeJSON(w, struct {
					sdk.WindowAuthFlowView
					LinkOnly bool `json:"linkOnly,omitempty"`
				}{view, settings.Shared().SignInLinkOnly})
				return
			}
		}
		fail(w, errors.New("provider sign-in is unavailable or has changed"))
	})
}
