//go:build !nogui

package gui

import "testing"

// TestPageToldHiddenOnTheMacOnly: off the Mac the panel is shown again by
// Wails' ToggleWindow, which never tells the page it is back, so a page told
// hidden there would stop its polling for good (#4293).
func TestPageToldHiddenOnTheMacOnly(t *testing.T) {
	if pageHiddenJSOn("darwin") != pageHiddenJS {
		t.Fatal("a Mac window's page isn't told it is out of sight")
	}
	for _, goos := range []string{"windows", "linux"} {
		if js := pageHiddenJSOn(goos); js != "" {
			t.Fatalf("%s: a page is told %q as its window hides", goos, js)
		}
	}
}

func TestWindowVisibilityTransitions(t *testing.T) {
	var state windowVisibility
	if got := state.explicitShow(true); got != pageShownJS {
		t.Fatalf("explicitShow = %q, want %q", got, pageShownJS)
	}
	if got := state.occlusionShow(true, true); got != "" {
		t.Fatalf("duplicate occlusionShow = %q, want empty", got)
	}

	// Out-of-order app unhide before occlusion catches up must ignore a stale WindowHide
	// and avoid emitting a second shown signal when WindowShow arrives later.
	if got := state.hideEvent(false); got != pageHiddenJS {
		t.Fatalf("app hide = %q, want %q", got, pageHiddenJS)
	}
	if got := state.openEvent(true, false); got != pageShownJS {
		t.Fatalf("app unhide = %q, want %q", got, pageShownJS)
	}
	if got := state.occlusionHide(true, false); got != "" {
		t.Fatalf("stale WindowHide after unhide = %q, want empty", got)
	}
	if got := state.occlusionShow(true, true); got != "" {
		t.Fatalf("WindowShow after unhide = %q, want empty", got)
	}

	// Once WindowShow confirms visibility, real occlusion hides and uncovers cleanly.
	if got := state.occlusionHide(true, false); got != pageHiddenJS {
		t.Fatalf("real occlusion hide = %q, want %q", got, pageHiddenJS)
	}
	if got := state.occlusionHide(true, false); got != "" {
		t.Fatalf("duplicate occlusion hide = %q, want empty", got)
	}
	if got := state.occlusionShow(true, true); got != pageShownJS {
		t.Fatalf("uncover show = %q, want %q", got, pageShownJS)
	}

	// Closed window stays hidden across app hide/unhide and duplicate occlusion hides.
	if got := state.explicitHide(); got != pageHiddenJS {
		t.Fatalf("explicitHide = %q, want %q", got, pageHiddenJS)
	}
	if got := state.occlusionHide(false, false); got != "" {
		t.Fatalf("WindowHide after close = %q, want empty", got)
	}
	if got := state.hideEvent(false); got != "" {
		t.Fatalf("app hide while closed = %q, want empty", got)
	}
	if got := state.openEvent(false, false); got != "" {
		t.Fatalf("app unhide while closed = %q, want empty", got)
	}
	if got := state.navigationFinished(false, false); got != pageHiddenJS {
		t.Fatalf("navigation while closed = %q, want %q", got, pageHiddenJS)
	}

	// Late navigationFinished on an open window after explicitHide must not flip visible back.
	var panel windowVisibility
	if got := panel.explicitHide(); got != pageHiddenJS {
		t.Fatalf("panel explicitHide = %q, want %q", got, pageHiddenJS)
	}
	if got := panel.navigationFinished(true, true); got != "" {
		t.Fatalf("late navigationFinished = %q, want empty", got)
	}
	if got := panel.openEvent(true, true); got != pageShownJS {
		t.Fatalf("panel reopen after explicitHide = %q, want %q", got, pageShownJS)
	}
}
