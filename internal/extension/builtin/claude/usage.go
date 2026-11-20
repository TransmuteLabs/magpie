package claude

import sdk "github.com/yetone/magpie/pkg/extension"

func UsageWindow(currentSession bool, scope string, used float64) sdk.UsageWindow {
	if currentSession {
		return sdk.UsageWindow{Name: "5 hours", Span: 5 * 3600, Used: used}
	}
	name := "7 days"
	if scope != "" {
		name += " · " + scope
	}
	return sdk.UsageWindow{Name: name, Span: 7 * 86400, Used: used}
}
