// Package usage is the Usage page: the allowances, what was spent and on
// what, the sessions, each request, and what filled the context window.
// Off, the page leaves the window; the gateway still counts what it
// passes on, and the tray panel's Usage tab still shows the allowances.
package usage

import (
	"github.com/yetone/magpie/internal/pluginrt"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	pluginrt.RegisterFeature(sdk.FeatureContribution{
		ID: "usage", API: "^1", Requires: []string{"window"}, Essential: true,
		Name: "Usage", Sub: "Your allowances, what was spent and on what, each request, and what filled the context window",
		Client: []string{"i18n.js", "client.js", "page.js"},
		Parts:  []sdk.FeaturePart{{Slot: sdk.FeatureNavigationPage, Key: "usage"}},
	})
}
