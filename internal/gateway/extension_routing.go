package gateway

import (
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/extensionrouting"
	"github.com/yetone/magpie/internal/routes"
)

func init() {
	routes.RegisterLookup(extensionrouting.Lookup(extension.Default))
}
