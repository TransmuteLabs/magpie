package provider

import (
	"context"
	"errors"
	"strings"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionSearchBinding = extensionhost.SearchBinding

type extensionSearchHost interface {
	Namespace() string
	SearchSources() []extensionhost.SearchBinding
	SearchSource(context.Context, string) (extensionhost.SearchBinding, bool)
	RunSearch(context.Context, extensionhost.SearchBinding, sdk.Scope, sdk.SearchRequest) (sdk.SearchResult, error)
}

func extensionSearchVendors() []SearchVendor {
	host, ok := providerExtensionHost().(extensionSearchHost)
	if !ok {
		return nil
	}
	out := []SearchVendor{}
	for _, binding := range host.SearchSources() {
		out = append(out, extensionSearchVendor(binding))
	}
	return out
}

func extensionSearchVendor(binding extensionhost.SearchBinding) SearchVendor {
	return SearchVendor{ID: "sdk:" + binding.Module + ":" + binding.Descriptor.ID,
		Name: binding.Descriptor.Name, Extension: binding.Module, ExtensionID: binding.Descriptor.ID,
		KeyOptional: true, Auth: binding.Descriptor.Provider, extensionBinding: binding}
}

func extensionSearchVendorFor(ctx context.Context, id string) (SearchVendor, bool) {
	module, source, found := strings.Cut(strings.TrimPrefix(id, "sdk:"), ":")
	host, available := providerExtensionHost().(extensionSearchHost)
	if !found || !available {
		return SearchVendor{}, false
	}
	binding, found := host.SearchSource(ctx, module+"/"+source)
	if !found {
		return SearchVendor{}, false
	}
	return extensionSearchVendor(binding), true
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
	if err := ctx.Err(); err != nil {
		return sdk.SearchResult{}, err
	}
	host, ok := providerExtensionHost().(extensionSearchHost)
	if !ok || vendor.Extension == "" || vendor.ID != "sdk:"+vendor.Extension+":"+vendor.ExtensionID {
		return sdk.SearchResult{}, errors.New("extension search host is unavailable")
	}
	binding := vendor.extensionBinding
	if binding.Namespace != host.Namespace() || binding.Module != vendor.Extension ||
		binding.Descriptor.ID != vendor.ExtensionID || binding.Descriptor.Provider != vendor.Auth {
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
