//go:build !cgo || (!darwin && !linux && !freebsd && !windows)

package extension

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNativeUnsupportedBuildFailsClearly(t *testing.T) {
	transport, err := openNativeTransport(t.Context(), "fixture")
	if transport != nil || err == nil || !strings.Contains(err.Error(), "cgo") {
		t.Fatalf("unsupported native runtime = %+v, %v", transport, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := openNativeTransport(ctx, "fixture"); !errors.Is(err, context.Canceled) {
		t.Fatalf("unsupported native runtime lost cancellation: %v", err)
	}
}
