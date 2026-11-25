//go:build !nogui

package gui

import (
	"runtime"

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

func hideWebview(w *application.WebviewWindow) {
	if w == nil {
		return
	}
	if js := pageHiddenJSOn(runtime.GOOS); js != "" {
		w.ExecJS(js)
	}
	w.Hide()
}

func showWebview(w *application.WebviewWindow) *application.WebviewWindow {
	if w == nil {
		return nil
	}
	w.Show()
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
	if js := pageHiddenJSOn(runtime.GOOS); js != "" {
		w.OnWindowEvent(events.Common.WindowLostFocus, func(*application.WindowEvent) { w.ExecJS(js) })
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
	syncHidden := func() {
		application.InvokeAsync(func() {
			if !windowOpen(w) {
				w.ExecJS(pageHiddenJS)
			}
		})
	}
	syncShown := func() {
		application.InvokeAsync(func() {
			if windowOpen(w) {
				w.ExecJS(pageShownJS)
			}
		})
	}
	w.OnWindowEvent(events.Mac.WebViewDidFinishNavigation, func(*application.WindowEvent) { syncHidden() })
	w.OnWindowEvent(events.Mac.WindowDidMiniaturize, func(*application.WindowEvent) { syncHidden() })
	w.OnWindowEvent(events.Mac.WindowDidDeminiaturize, func(*application.WindowEvent) { syncShown() })
	w.OnWindowEvent(events.Mac.WindowHide, func(*application.WindowEvent) {
		application.InvokeAsync(func() {
			if !w.IsVisible() {
				w.ExecJS(pageHiddenJS)
			}
		})
	})
	w.OnWindowEvent(events.Mac.WindowShow, func(*application.WindowEvent) {
		application.InvokeAsync(func() {
			if windowOpen(w) && w.IsVisible() {
				w.ExecJS(pageShownJS)
			}
		})
	})
	if h == nil || h.app == nil {
		return
	}
	h.appVisOnce.Do(func() {
		h.app.Event.OnApplicationEvent(events.Mac.ApplicationDidHide, func(*application.ApplicationEvent) {
			application.InvokeAsync(func() {
				for _, win := range []*application.WebviewWindow{h.main, h.panel, h.remote} {
					if win != nil && !windowOpen(win) {
						win.ExecJS(pageHiddenJS)
					}
				}
			})
		})
		h.app.Event.OnApplicationEvent(events.Mac.ApplicationDidUnhide, func(*application.ApplicationEvent) {
			application.InvokeAsync(func() {
				for _, win := range []*application.WebviewWindow{h.main, h.panel, h.remote} {
					if win != nil && windowOpen(win) {
						win.ExecJS(pageShownJS)
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
