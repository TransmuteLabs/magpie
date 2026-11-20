// Package library is the Library page: instructions, MCP servers, skills
// and hooks, kept once and given to every agent. While it gives agents
// anything, it stays on.
package library

import (
	lib "github.com/yetone/magpie/internal/library"
	"github.com/yetone/magpie/internal/pluginrt"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	pluginrt.RegisterFeature(sdk.FeatureContribution{
		ID: "library", API: "^1", Requires: []string{"window"},
		Name: "Library", Sub: "Instructions, MCP servers, skills and hooks, kept once and given to every agent",
		One: "1 item given to agents", Many: "{n} items given to agents",
		InUse:  lib.InUse,
		Client: []string{"i18n.js", "library.css", "client.js"},
		Parts:  []sdk.FeaturePart{{Slot: sdk.FeatureNavigationPage, Key: "library"}},
	})
}
