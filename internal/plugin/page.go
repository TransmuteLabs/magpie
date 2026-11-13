package plugin

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/yetone/magpie/internal/windowmeta"
)

// Window metadata has one parser for legacy plugins and SDK modules.
type WindowSpec = windowmeta.WindowSpec
type PageSpec = windowmeta.PageSpec
type PanelSpec = windowmeta.PanelSpec
type ServiceSpec = windowmeta.ServiceSpec
type ActionSpec = windowmeta.ActionSpec

var (
	ActionMenus = windowmeta.ActionMenus
	PageReads   = windowmeta.PageReads
	PanelSlots  = windowmeta.PanelSlots
	Replaceable = windowmeta.Replaceable
)

const (
	maxActions = windowmeta.MaxActions
	maxPanels  = windowmeta.MaxPanels
)

// Window is what a plugin at target adds to magpie's window. Invalid parts
// are filtered out; only is true when it names neither main nor exports.
func Window(target string) (w *WindowSpec, only bool) {
	st, err := os.Stat(target)
	if err != nil || !st.IsDir() {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(target, "package.json"))
	if err != nil {
		return nil, false
	}
	return WindowFromData(target, b)
}

// WindowFromData reads metadata already obtained by the caller. It never
// loads a plugin host.
func WindowFromData(target string, b []byte) (*WindowSpec, bool) {
	return windowmeta.WindowFromData(target, b)
}

// FileIn is a file at rel inside dir, never a directory or a link outside it.
func FileIn(dir, rel string) (string, bool) {
	return windowmeta.FileIn(dir, rel)
}

// portOf keeps legacy host option handling on the shared port rules.
func portOf(v any) (int, bool) {
	return windowmeta.PortOf(v)
}

// SetPageOptions sets the options of a plugin from its page or a panel.
// One whose page and panels are all it has is never loaded by the host, so the host goes on as
// it was; any other (a middleware, an agent, a gateway hook, a provider) is SetOptions, which
// starts the host again for it.
func SetPageOptions(spec string, opts map[string]any) error {
	t := Target(spec)
	if mw, _ := Middleware(t); mw != "" {
		return SetOptions(spec, opts)
	}
	if ag, _ := Agent(t); ag != "" {
		return SetOptions(spec, opts)
	}
	// a gateway hook is handed the options the host loaded it with
	if Gateway(t) != "" {
		return SetOptions(spec, opts)
	}
	if w, only := Window(t); w == nil || !only {
		return SetOptions(spec, opts)
	}
	listMu.Lock()
	defer listMu.Unlock()
	l := Load()
	i := indexOf(l, spec)
	if i < 0 {
		return fmt.Errorf("no plugin %q", spec)
	}
	if len(opts) == 0 {
		opts = nil
	}
	l.Plugins[i].Options = opts
	return save(l)
}
