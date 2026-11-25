//go:build !nogui

package gui

import (
	"runtime"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// A macOS window hidden with orderOut keeps its WKWebView's page visible, so
// tell the page before hiding it and tell it again when it is shown. Only
// the Mac is told: WebView2 and WebKitGTK mark a hidden window's page hidden
// themselves, and there the panel is also shown by Wails' ToggleWindow,
// which would never tell the page it is back (#4293).
const (
	pageHiddenJS = "window.magpieWindowVisibility?.(false)"
	pageShownJS  = "window.magpieWindowVisibility?.(true)"
)

type windowVisibility struct {
	known      bool
	visible    bool
	sawVisible bool
}

func (state *windowVisibility) explicitHide() string {
	state.known = true
	state.visible = false
	state.sawVisible = false
	return pageHiddenJS
}

func (state *windowVisibility) explicitShow(occludedVisible bool) string {
	state.known = true
	state.visible = true
	state.sawVisible = occludedVisible
	return pageShownJS
}

func (state *windowVisibility) navigationFinished(open, occludedVisible bool) string {
	if !open {
		state.known = true
		state.visible = false
		state.sawVisible = false
		return pageHiddenJS
	}
	if occludedVisible {
		state.sawVisible = true
	} else if state.sawVisible {
		state.known = true
		state.visible = false
		return pageHiddenJS
	}
	if !state.known {
		state.known = true
		state.visible = true
	}
	return ""
}

func (state *windowVisibility) hideEvent(open bool) string {
	if open {
		return ""
	}
	state.sawVisible = false
	if state.known && !state.visible {
		return ""
	}
	state.known = true
	state.visible = false
	return pageHiddenJS
}

func (state *windowVisibility) openEvent(open, occludedVisible bool) string {
	if !open {
		return ""
	}
	if !state.known || !state.visible {
		state.known = true
		state.visible = true
		state.sawVisible = occludedVisible
		return pageShownJS
	}
	if occludedVisible {
		state.sawVisible = true
	}
	return ""
}

func (state *windowVisibility) occlusionHide(open, occludedVisible bool) string {
	if occludedVisible {
		return ""
	}
	if !open {
		state.sawVisible = false
		if state.known && !state.visible {
			return ""
		}
		state.known = true
		state.visible = false
		return pageHiddenJS
	}
	if !state.sawVisible || (state.known && !state.visible) {
		return ""
	}
	state.known = true
	state.visible = false
	return pageHiddenJS
}

func (state *windowVisibility) occlusionShow(open, occludedVisible bool) string {
	if !open || !occludedVisible {
		return ""
	}
	state.sawVisible = true
	if state.known && state.visible {
		return ""
	}
	state.known = true
	state.visible = true
	return pageShownJS
}

var (
	visibilityMu     sync.Mutex
	visibilityStates = map[*application.WebviewWindow]*windowVisibility{}
)

func applyWindowVisibility(w *application.WebviewWindow, transition func(*windowVisibility) string) {
	visibilityMu.Lock()
	state := visibilityStates[w]
	if state == nil {
		state = &windowVisibility{}
		visibilityStates[w] = state
	}
	js := transition(state)
	visibilityMu.Unlock()
	if js != "" {
		w.ExecJS(js)
	}
}

func forgetWindowVisibility(w *application.WebviewWindow) {
	visibilityMu.Lock()
	delete(visibilityStates, w)
	visibilityMu.Unlock()
}

func hideWebview(w *application.WebviewWindow) {
	if w == nil {
		return
	}
	if pageHiddenJSOn(runtime.GOOS) != "" {
		applyWindowVisibility(w, (*windowVisibility).explicitHide)
	}
	w.Hide()
}

func showWebview(w *application.WebviewWindow) *application.WebviewWindow {
	if w == nil {
		return nil
	}
	w.Show()
	if pageHiddenJSOn(runtime.GOOS) != "" {
		occludedVisible := w.IsVisible()
		applyWindowVisibility(w, func(state *windowVisibility) string {
			return state.explicitShow(occludedVisible)
		})
		return w
	}
	w.ExecJS(pageShownJS)
	return w
}

// sayWailsHides tells the panel's page when Wails hides the panel itself:
// on a click elsewhere (HideOnFocusLost), the way it is mostly closed, and
// on Escape (HideOnEscape), both a plain Hide that hideWebview never sees.
// A page never told it was hidden isn't told it is back either, so it kept
// the settings it was drawn with: Settings' ¥ and 万 / 亿 never reached the
// panel's Usage tab (blrbrzkj on Discord).
func sayWailsHides(w *application.WebviewWindow) {
	if pageHiddenJSOn(runtime.GOOS) != "" {
		w.OnWindowEvent(events.Common.WindowLostFocus, func(*application.WindowEvent) {
			applyWindowVisibility(w, (*windowVisibility).explicitHide)
		})
	}
	w.RegisterKeyBinding("escape", func(application.Window) { hideWebview(w) })
}

// watchVisibility keeps a macOS window's page visibility aligned with AppKit
// transitions that bypass hideWebview/showWebview: a window created hidden
// finishing navigation, reload while hidden, miniaturise/deminiaturise,
// occlusion (WindowHide/WindowShow), and Command-H / Hide Others.
func (h *host) watchVisibility(w *application.WebviewWindow) {
	if w == nil || pageHiddenJSOn(runtime.GOOS) == "" {
		return
	}
	w.OnWindowEvent(events.Mac.WebViewDidFinishNavigation, func(*application.WindowEvent) {
		application.InvokeAsync(func() {
			open, occludedVisible := windowOpen(w), w.IsVisible()
			applyWindowVisibility(w, func(state *windowVisibility) string {
				return state.navigationFinished(open, occludedVisible)
			})
		})
	})
	w.OnWindowEvent(events.Mac.WindowDidMiniaturize, func(*application.WindowEvent) {
		application.InvokeAsync(func() {
			open := windowOpen(w)
			applyWindowVisibility(w, func(state *windowVisibility) string {
				return state.hideEvent(open)
			})
		})
	})
	w.OnWindowEvent(events.Mac.WindowDidDeminiaturize, func(*application.WindowEvent) {
		application.InvokeAsync(func() {
			open, occludedVisible := windowOpen(w), w.IsVisible()
			applyWindowVisibility(w, func(state *windowVisibility) string {
				return state.openEvent(open, occludedVisible)
			})
		})
	})
	w.OnWindowEvent(events.Mac.WindowHide, func(*application.WindowEvent) {
		application.InvokeAsync(func() {
			open, occludedVisible := windowOpen(w), w.IsVisible()
			applyWindowVisibility(w, func(state *windowVisibility) string {
				return state.occlusionHide(open, occludedVisible)
			})
		})
	})
	w.OnWindowEvent(events.Mac.WindowShow, func(*application.WindowEvent) {
		application.InvokeAsync(func() {
			open, occludedVisible := windowOpen(w), w.IsVisible()
			applyWindowVisibility(w, func(state *windowVisibility) string {
				return state.occlusionShow(open, occludedVisible)
			})
		})
	})
	w.OnWindowEvent(events.Mac.WindowWillClose, func(*application.WindowEvent) {
		forgetWindowVisibility(w)
	})
	if h == nil || h.app == nil {
		return
	}
	h.appVisOnce.Do(func() {
		h.app.Event.OnApplicationEvent(events.Mac.ApplicationDidHide, func(*application.ApplicationEvent) {
			application.InvokeAsync(func() {
				for _, win := range []*application.WebviewWindow{h.main, h.panel, h.remote} {
					if win != nil {
						open := windowOpen(win)
						applyWindowVisibility(win, func(state *windowVisibility) string {
							return state.hideEvent(open)
						})
					}
				}
			})
		})
		h.app.Event.OnApplicationEvent(events.Mac.ApplicationDidUnhide, func(*application.ApplicationEvent) {
			application.InvokeAsync(func() {
				for _, win := range []*application.WebviewWindow{h.main, h.panel, h.remote} {
					if win != nil {
						open, occludedVisible := windowOpen(win), win.IsVisible()
						applyWindowVisibility(win, func(state *windowVisibility) string {
							return state.openEvent(open, occludedVisible)
						})
					}
				}
			})
		})
	})
}

// pageHiddenJSOn is what a page on goos is told as its window hides.
func pageHiddenJSOn(goos string) string {
	if goos != "darwin" {
		return ""
	}
	return pageHiddenJS
}
