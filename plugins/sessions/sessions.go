// Package sessions is the Sessions page: the agents' past conversations,
// to read, search, resume and export. Off, the page leaves the window;
// the gateway still records conversations while Settings asks it to, and
// that keeps the plugin on.
package sessions

import (
	"github.com/yetone/magpie/internal/pluginrt"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	pluginrt.RegisterFeature(sdk.FeatureContribution{
		ID: "sessions", API: "^1", Requires: []string{"window"},
		Name: "Sessions", Sub: "Your agents' past conversations: read, search, resume and export them",
		One: "Recording gateway conversations", Many: "Recording gateway conversations",
		InUse: func() (int, error) {
			s, err := settings.LoadChecked()
			if err != nil {
				return 0, err
			}
			if s.GatewayConversations {
				return 1, nil
			}
			return 0, nil
		},
		Client: []string{"i18n.js", "sessions.css", "client.js"},
		Parts:  []sdk.FeaturePart{{Slot: sdk.FeatureNavigationPage, Key: "sessions"}},
	})
}
