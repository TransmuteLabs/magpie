// Package groups is routing groups: several models agents pick as one,
// each request sent to the right one and on to the next when one fails.
// While the user has a group, it stays on.
//
// It serves the user's own groups on the gateway's router strategy point
// (P3 of docs/subsystems/plugin-runtime.md): its SDK declaration holds
// provider.ServeGroups, and off, the gateway neither lists nor routes them.
// The groups magpie finds (one model several providers serve) are the
// core's, on or off.
package groups

import (
	"github.com/yetone/magpie/internal/pluginrt"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	pluginrt.RegisterFeature(sdk.FeatureContribution{
		ID: "groups", API: "^1", Requires: []string{"window", "providers"},
		Name: "Routing groups", Sub: "Several models agents pick as one: each request goes to the right one, and to the next when one fails",
		One: "1 group in use", Many: "{n} groups in use",
		// a providers.json that can't be read is not known, so it stays on
		InUse:         provider.NamedGroups,
		RoutingGroups: true,
		Parts:         []sdk.FeaturePart{{Slot: sdk.FeatureInlinePart, Key: "groups"}},
		OpenPage:      "groups",
	})
}
