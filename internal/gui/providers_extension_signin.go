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
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Icon        string                 `json:"icon"`
	Page        string                 `json:"page,omitempty"`
	Revision    string                 `json:"revision,omitempty"`
	Methods     []sdk.WindowAuthMethod `json:"methods,omitempty"`
	Reauthorize bool                   `json:"reauthorize,omitempty"`
}

func extensionSubscriptions() []extensionSubscriptionJSON {
	if settings.AccountDiscoveryOff() {
		return nil
	}
	var out []extensionSubscriptionJSON
	for _, item := range extension.Default().ProviderSignIns(context.Background()) {
		row := extensionSubscriptionJSON{
			ID: provider.ExtensionProviderID(item.Module, item.Provider), Name: item.Name, Icon: "generic", Revision: item.Revision,
			Methods: item.Binding.Methods, Reauthorize: item.Binding.Reauthorize,
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
			ID       string `json:"id"`
			Revision string `json:"revision"`
			extensionAuthRequest
		}
		if !readExtensionAuthRequest(w, r, &in) {
			return
		}
		if !settings.AccountDiscoveryOff() && in.Revision != "" {
			host := extension.Default()
			for _, target := range host.ProviderSignIns(r.Context()) {
				if in.Revision == target.Revision && in.ID == provider.ExtensionProviderID(target.Module, target.Provider) {
					runExtensionAuth(w, r.Context(), host, target.Module, target.Revision, sdk.ProviderSignInPart, target.Binding.ID, in.extensionAuthRequest)
					return
				}
			}
		}
		fail(w, errors.New("provider sign-in is unavailable or has changed"))
	})
}
