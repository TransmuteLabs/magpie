package provider

import (
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func projectUsageWindow(window sdk.UsageWindow) QuotaWindow {
	entry := QuotaWindow{
		Name: window.Name, Span: time.Duration(window.Span) * time.Second,
		Limit: window.Limit, Unit: window.Unit,
	}
	entry.setUsageShare(window)
	return entry
}

func (entry *QuotaWindow) setUsageShare(window sdk.UsageWindow) {
	entry.Used, entry.Error, entry.Display = window.Used, window.Error, window.Display
	entry.Aside, entry.Unknown = window.Aside, window.Unknown
	entry.Text, entry.noShare = window.Unknown, window.Unknown
	if window.Unknown {
		entry.Used, entry.Aside = 0, true
		if entry.Error != "" {
			entry.Display = entry.Error
		}
		if entry.Display == "" {
			entry.Display = "unknown"
		}
	}
}
