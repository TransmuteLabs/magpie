package provider

import (
	"context"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionAttemptUsageResetsAtExecutionBegin(test *testing.T) {
	ctx, attempt := WithExtensionAttempt(context.Background())
	scope := sdk.Scope{Plugin: "fixture.external", Provider: "vendor", Account: "alice", Generation: 1, Operation: "first"}
	extensionBegin(ctx, scope)
	extensionUsage(ctx, &sdk.Usage{InputTokens: 31, OutputTokens: 7})
	if used := attempt.Usage(); used == nil || used.InputTokens != 31 {
		test.Fatalf("first execution lost its measured usage: %+v", used)
	}
	scope.Operation = "second"
	extensionBegin(ctx, scope)
	if used := attempt.Usage(); used != nil {
		test.Fatalf("new execution inherited previous totals: %+v", used)
	}
	extensionUsage(ctx, nil)
	if used := attempt.Usage(); used != nil {
		test.Fatalf("absent usage borrowed a previous execution: %+v", used)
	}
}

func TestExtensionAttemptUsageMatchesSelectedScope(test *testing.T) {
	fixtureExtensionProvider(test, true)
	providers, err := ExtensionProviders()
	if err != nil || len(providers) != 2 {
		test.Fatalf("selected accounts: %+v %v", providers, err)
	}
	selected := providers[0]
	scope, ok := selected.ExtensionScope()
	if !ok {
		test.Fatal("fixture did not expose an SDK account")
	}
	ctx, attempt := WithExtensionAttempt(context.Background())
	view, ok := any(attempt).(interface{ UsageFor(Provider) *sdk.Usage })
	if !ok {
		test.Fatal("execution usage cannot be associated with the selected provider/account/generation")
	}
	scope.Operation = "measured"
	extensionBegin(ctx, scope)
	original := &sdk.Usage{InputTokens: 31, OutputTokens: 7}
	extensionUsage(ctx, original)
	original.InputTokens = 999
	if used := view.UsageFor(selected); used == nil || used.InputTokens != 31 {
		test.Fatalf("selected execution lost or aliased its snapshot: %+v", used)
	}
	used := view.UsageFor(selected)
	used.InputTokens = 888
	if used := view.UsageFor(selected); used == nil || used.InputTokens != 31 {
		test.Fatalf("snapshot reader mutated execution accounting: %+v", used)
	}
	input, deferred := int64(31), int64(12)
	sparse := &sdk.Usage{Facts: &sdk.ExecutionUsageFacts{
		ExecutionCounts: sdk.ExecutionCounts{InputTokens: &input},
		Deferred:        &sdk.ExecutionCounts{InputTokens: &deferred},
	}}
	extensionUsage(ctx, sparse)
	*sparse.Facts.InputTokens, *sparse.Facts.Deferred.InputTokens = 999, 999
	snapshot := view.UsageFor(selected)
	if snapshot == nil || snapshot.Facts == nil || *snapshot.Facts.InputTokens != 31 ||
		snapshot.Facts.Deferred == nil || *snapshot.Facts.Deferred.InputTokens != 12 {
		test.Fatalf("selected sparse snapshot aliased the writer: %+v", snapshot)
	}
	*snapshot.Facts.InputTokens, *snapshot.Facts.Deferred.InputTokens = 888, 888
	snapshot = attempt.Usage()
	if *snapshot.Facts.InputTokens != 31 || *snapshot.Facts.Deferred.InputTokens != 12 {
		test.Fatalf("selected sparse snapshot aliased its reader: %+v", snapshot)
	}
	*snapshot.Facts.InputTokens = 777
	if *view.UsageFor(selected).Facts.InputTokens != 31 {
		test.Fatal("compatibility usage reader aliased the selected snapshot")
	}
	if view.UsageFor(providers[1]) != nil || view.UsageFor(Provider{ID: "ordinary"}) != nil {
		test.Fatal("another account or ordinary provider inherited SDK totals")
	}
	for _, dimension := range []string{"module", "provider", "account", "generation"} {
		test.Run(dimension, func(test *testing.T) {
			other := scope
			switch dimension {
			case "module":
				other.Plugin = "fixture.other"
			case "provider":
				other.Provider = "other"
			case "account":
				other.Account = "bob"
			case "generation":
				other.Generation++
			}
			extensionBegin(ctx, other)
			extensionUsage(ctx, &sdk.Usage{InputTokens: 52})
			if used := view.UsageFor(selected); used != nil {
				test.Fatalf("selected provider borrowed another %s's usage: %+v", dimension, used)
			}
		})
	}
}
