package gateway

import (
	"context"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/extensionrouting"
	"github.com/yetone/magpie/internal/pluginrt"
	"github.com/yetone/magpie/internal/routes"
)

func routingWithExtensions(catalog *routes.Catalog, manager *extension.Manager) *routes.Catalog {
	if manager == nil {
		return catalog
	}
	return catalog.WithSource(extensionrouting.Source(manager))
}

func (s *Server) routingCatalog() *routes.Catalog {
	if s.strategyCatalog != nil {
		return s.strategyCatalog
	}
	return pluginrt.ProcessRuntime().RoutingCatalog()
}

func (s *Server) RoutingOptions() []routes.Option {
	return s.RoutingOptionsContext(context.Background())
}

func (s *Server) RoutingOptionsContext(ctx context.Context) []routes.Option {
	return s.routingCatalog().ListContext(ctx)
}

// RoutingOptions lists the application catalog for a window or terminal
// that has no local gateway.
func RoutingOptions() []routes.Option {
	return RoutingOptionsContext(context.Background())
}

func RoutingOptionsContext(ctx context.Context) []routes.Option {
	return routingWithExtensions(pluginrt.ProcessRuntime().RoutingCatalog(), extension.Default()).ListContext(ctx)
}
