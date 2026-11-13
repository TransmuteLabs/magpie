package extension

import (
	"context"
	"io"
	"os"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestMain(m *testing.M) {
	if slices.Contains(os.Args, "--sdk-hook-worker") {
		if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, gatewayFixtureModule()); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--sdk-wedged-input") {
		wedgedInputHelper()
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
