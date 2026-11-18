package extension

import (
	"context"
	"errors"
	"sync"

	"github.com/yetone/magpie/internal/proc"
)

var bunRuntime struct {
	sync.RWMutex
	resolve func(context.Context) (string, error)
}

// SetBunResolver connects the application's existing managed Bun owner without
// making the extension supervisor depend on its legacy plugin host.
func SetBunResolver(resolve func(context.Context) (string, error)) {
	bunRuntime.Lock()
	bunRuntime.resolve = resolve
	bunRuntime.Unlock()
}

func resolveBun(ctx context.Context) (string, error) {
	bunRuntime.RLock()
	resolve := bunRuntime.resolve
	bunRuntime.RUnlock()
	if resolve != nil {
		return resolve(ctx)
	}
	if path := proc.FindTool("bun"); path != "" {
		return path, nil
	}
	return "", errors.New("Bun is required for this extension")
}
