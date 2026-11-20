// Package gateway is the Gateway page: the gateway's state, its keys, how
// to connect a client, the models it serves and its recent calls. The
// gateway itself is core and serves agents with the page off; while a
// gateway key is there, the page that lists and changes them stays on.
//
// It is the first part of the window a plugin draws itself (P1 of
// docs/subsystems/plugin-runtime.md): its scripts register the page with
// the window's kernel, and the host draws the form from its SDK contribution.
package gateway

import (
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/pluginrt"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	pluginrt.RegisterFeature(sdk.FeatureContribution{
		ID: "gateway", API: "^1", Requires: []string{"window", "gateway"}, Essential: true,
		Name: "Gateway page", Sub: "The gateway's state and keys, how to connect a client, its models and recent calls",
		One: "1 gateway key", Many: "{n} gateway keys",
		InUse: func() (int, error) {
			keys, err := access.List()
			return len(keys), err
		},
		Client:     []string{"keys.js", "client.js"},
		Parts:      []sdk.FeaturePart{{Slot: sdk.FeatureNavigationPage, Key: "gateway"}},
		SettingsAt: &sdk.FeatureSettingsLocation{Tab: "network", RowID: "portList"},
		Settings: []sdk.FeatureSetting{{
			Name: "callsPage", Kind: sdk.FeatureSettingInteger, Title: "Recent calls at a time",
			Description: "How many of the gateway's recent calls the page shows, and how many more each Show more adds",
			Default:     20, Enum: []any{10, 20, 50, 100},
		}},
	})
}
