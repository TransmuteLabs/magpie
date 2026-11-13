//go:build !cgo || (!darwin && !linux && !freebsd && !windows)

package extension

import (
	"context"
	"errors"
)

func openNativeTransport(ctx context.Context, filename string) (*moduleTransport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("native extensions require a supported cgo-enabled build")
}
