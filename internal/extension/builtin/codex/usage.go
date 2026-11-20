package codex

import (
	"fmt"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func UsageWindow(seconds int64, used *float64) sdk.UsageWindow {
	window := sdk.UsageWindow{Name: "Allowance", Span: seconds, Unknown: used == nil}
	switch {
	case seconds > 0 && seconds%86400 == 0:
		window.Name = fmt.Sprintf("%d days", seconds/86400)
	case seconds > 0 && seconds%3600 == 0:
		window.Name = fmt.Sprintf("%d hours", seconds/3600)
	}
	if used != nil {
		window.Used = *used
	}
	return window
}
