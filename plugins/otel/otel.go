// Package otel is Settings' Observability: each request's traces and
// metrics sent to an OpenTelemetry collector, such as Langfuse. While it
// sends, it stays on.
//
// It is the first consumer of the gateway's observer point (P3 of
// docs/subsystems/plugin-runtime.md): its SDK declaration supplies internal/otlp's
// exporter as a usage.Observer, and off, the gateway exports nothing and
// keeps no reply whole for it.
package otel

import (
	"github.com/yetone/magpie/internal/otlp"
	"github.com/yetone/magpie/internal/pluginrt"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	pluginrt.RegisterGatewayFeature(pluginrt.GatewayFeature{
		FeatureContribution: sdk.FeatureContribution{
			ID: "otel", API: "^1", Requires: []string{"window", "settings", "gateway"},
			Name: "Observability", Sub: "Send each request's traces and metrics to an OpenTelemetry collector, such as Langfuse",
			One: "Sending to a collector", Many: "Sending to a collector",
			InUse:      inUse,
			Client:     []string{"i18n.js", "page.js"},
			Parts:      []sdk.FeaturePart{{Slot: sdk.FeatureSettingsSection, Key: "otel"}},
			SettingsAt: &sdk.FeatureSettingsLocation{Tab: "otel", RowID: "otelList"},
		},
		Observer: func() usage.Observer { return otlp.New() },
	})
}

func inUse() (int, error) {
	if _, err := settings.LoadChecked(); err != nil {
		return 0, err
	}
	config, err := settings.OTelExport()
	if err != nil {
		return 0, err
	}
	if config.Enabled {
		return 1, nil
	}
	return 0, nil
}
