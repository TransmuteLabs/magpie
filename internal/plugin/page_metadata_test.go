package plugin

import (
	"testing"

	"github.com/yetone/magpie/internal/windowmeta"
)

// Public aliases keep existing callers assignable to the shared types, with
// ServicePort inherited rather than implemented a second time in plugin.
var (
	_ *WindowSpec  = (*windowmeta.WindowSpec)(nil)
	_ *PageSpec    = (*windowmeta.PageSpec)(nil)
	_ *PanelSpec   = (*windowmeta.PanelSpec)(nil)
	_ *ActionSpec  = (*windowmeta.ActionSpec)(nil)
	_ *ServiceSpec = (*windowmeta.ServiceSpec)(nil)
	_ interface {
		ServicePort(map[string]any) int
	} = (*ServiceSpec)(nil)
)

func TestWindowMetadataListsAreShared(t *testing.T) {
	if &ActionMenus[0] != &windowmeta.ActionMenus[0] ||
		&PageReads[0] != &windowmeta.PageReads[0] ||
		&PanelSlots[0] != &windowmeta.PanelSlots[0] ||
		&Replaceable[0] != &windowmeta.Replaceable[0] {
		t.Fatal("legacy metadata lists were copied instead of shared")
	}
}
