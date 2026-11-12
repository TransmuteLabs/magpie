package extension

import (
	"path/filepath"
	"sync"

	"github.com/yetone/magpie/internal/appdir"
)

var changes struct {
	sync.Mutex
	listeners []func()
}

// CatalogPath is intentionally independent of Default and never starts or
// stops a process. File-stamp readers may call it under their own locks.
func CatalogPath() string { return filepath.Join(appdir.Config(), "extensions", "catalog.json") }

func OnChange(listener func()) {
	if listener == nil {
		return
	}
	changes.Lock()
	changes.listeners = append(changes.listeners, listener)
	changes.Unlock()
}

func notifyChanged() {
	changes.Lock()
	listeners := append([]func(){}, changes.listeners...)
	changes.Unlock()
	for _, listener := range listeners {
		listener()
	}
}
