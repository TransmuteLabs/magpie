package provider

import (
	"context"
	"errors"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionSearchHost interface {
	Namespace() string
	SearchSources() []extensionhost.SearchBinding
	SearchSource(string) (extensionhost.SearchBinding, bool)
	RunSearch(context.Context, extensionhost.SearchBinding, sdk.Scope, sdk.SearchRequest) (sdk.SearchResult, error)
}

func extensionSearchVendors() []SearchVendor {
	host, ok := providerExtensionHost().(extensionSearchHost)
	if !ok {
		return nil
	}
	out := []SearchVendor{}
	for _, binding := range host.SearchSources() {
		out = append(out, SearchVendor{ID: "sdk:" + binding.Module + ":" + binding.Descriptor.ID,
			Name: binding.Descriptor.Name, Extension: binding.Module, ExtensionID: binding.Descriptor.ID,
			KeyOptional: true, Auth: binding.Descriptor.Provider})
	}
	return out
}

func extensionSearchSignIn(v SearchVendor) (string, *Provider) {
	name := v.Auth
	for _, current := range All() {
		scope, ok := current.ExtensionScope()
		if ok && scope.Plugin == v.Extension && scope.Provider == v.Auth {
			return current.Name, &current
		}
	}
	return name, nil
}

func RunExtensionSearch(ctx context.Context, vendor SearchVendor, selected *Provider, query string, count int) (sdk.SearchResult, error) {
	host, ok := providerExtensionHost().(extensionSearchHost)
	if !ok || vendor.Extension == "" || vendor.ID != "sdk:"+vendor.Extension+":"+vendor.ExtensionID {
		return sdk.SearchResult{}, errors.New("extension search host is unavailable")
	}
	binding, found := host.SearchSource(vendor.Extension + "/" + vendor.ExtensionID)
	if !found || binding.Descriptor.Provider != vendor.Auth {
		return sdk.SearchResult{}, errors.New("extension search source changed")
	}
	if err := netproxy.Held(ctx); err != nil {
		return sdk.SearchResult{}, err
	}
	scope := sdk.Scope{Plugin: binding.Module}
	if binding.Descriptor.Provider != "" {
		if selected == nil || !selected.IsExtension() {
			return sdk.SearchResult{}, errors.New("extension search requires its selected account")
		}
		account := selected.Account.extension
		scope = account.currentScope()
		if account.host.Namespace() != host.Namespace() || scope.Plugin != binding.Module ||
			scope.Provider != binding.Descriptor.Provider || scope.Account == "" {
			return sdk.SearchResult{}, errors.New("extension search account belongs to another source")
		}
		prepared, err := account.prepare(ctx)
		if err != nil {
			return sdk.SearchResult{}, errors.New("extension search account preflight failed")
		}
		scope = prepared.currentScope()
	} else if selected != nil {
		return sdk.SearchResult{}, errors.New("extension search source does not grant account selection")
	}
	ctx, err := extensionNetworkContext(ctx, scope)
	if err != nil {
		return sdk.SearchResult{}, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return sdk.SearchResult{}, err
	}
	return host.RunSearch(ctx, binding, scope, sdk.SearchRequest{
		Source: binding.Descriptor.ID, Query: query, Count: count, Proxy: netproxy.Choice(ctx),
	})
}
