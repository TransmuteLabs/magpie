package extension

import (
	"context"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestMain(m *testing.M) {
	if slices.Contains(os.Args, "--sdk-routing-disable") {
		routingCatalogChild()
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--sdk-usage-child") {
		usageChild()
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--sdk-refresh-child") {
		refreshChild()
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--sdk-activation-helper") {
		if err := serveActivationHelper(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--sdk-hook-worker") {
		if index := slices.Index(os.Args, "--sdk-activation-describe-gate"); index >= 0 {
			gate := os.Args[index+1]
			if _, err := os.Stat(gate); err == nil {
				if err := os.WriteFile(gate+".entered", nil, 0o600); err != nil {
					os.Exit(1)
				}
				for {
					if _, err := os.Stat(gate); os.IsNotExist(err) {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		}
		if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, gatewayFixtureModule()); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--sdk-wedged-input") {
		rawWorkerHelper(sdk.MethodToolCall)
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--sdk-raw-observer") {
		rawWorkerHelper(sdk.MethodLedgerObserve, sdk.MethodRequestObserve)
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--sdk-raw-usage") {
		rawWorkerHelper(sdk.MethodProviderUsage)
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--sdk-stall-handshake") {
		_ = os.WriteFile(os.Args[len(os.Args)-1], []byte("started"), 0o600)
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--sdk-test-worker") {
		if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, helperModule()); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	testenv.Main(m)
}
