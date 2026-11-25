//go:build darwin && cgo && !nogui

package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// A closed NSWindow keeps its WKWebView's document visible unless magpie
// says otherwise. The page here records what it saw as it was hidden, and
// reports that record once it is shown again; without hideWebview's signal
// it remains a visible page with running animations.
//
// The page reads document.hidden as app.js does, from magpie's signal.
// WebKit does turn an ordered-out window's document.hidden on, but in its
// own time and without a visibilitychange of its own, so a page reading
// WebKit's value saw itself visible whenever magpie's signal arrived first,
// recorded nothing, and the window was shown again after a fixed pause with
// nothing to report (#4924). The window is now shown again only once the
// page has said it saw itself hidden.
//
// On mote (macOS 27.2) WebKit's own document.hidden stayed true from load
// to the end, the window shown or not, with no visibilitychange of its own.
// The shown signal's own visibilitychange then read hidden and unmarked,
// and was taken for the next hiding: its record and its /hidden reached
// the host before the panel's Escape was told, and the panel's second
// round failed with marked=false play="running" (#5322). Only a
// visibilitychange outside the shown signal counts as a hiding now.
//
// On mica (macOS 26) WebKit fired a visibilitychange of its own as the
// panel ordered out, hidden by its own value and before magpie's signal
// was in. The page took it for the hiding, and the host showed the panel
// again before the page was ever told: 4 of 6 runs failed with
// marked=false play="running" (#6203). Only a visibilitychange once
// magpie's signal says hidden counts now; without that signal the host
// still waits in vain and the test fails.
func TestClosingWindowMarksPageOutOfSight(t *testing.T) {
	out, err := runAppKit(t, 2*time.Minute, []string{"MAGPIE_TEST_WINDOW_VISIBILITY=1"})
	if err != nil || !strings.Contains(string(out), "window visibility: ok") {
		t.Fatalf("window visibility: %v\n%s", err, out)
	}
}

// The tray panel is hidden by Wails itself more often than by magpie: on a
// click elsewhere (HideOnFocusLost) and on Escape, a plain Hide that
// hideWebview never sees. Its page is told all the same, or it is never
// told it is back and keeps what it was drawn with: Settings' currency and
// number units never reached it (blrbrzkj on Discord).
func TestPanelHiddenByWailsMarksPageOutOfSight(t *testing.T) {
	out, err := runAppKit(t, 2*time.Minute, []string{"MAGPIE_TEST_WINDOW_VISIBILITY=panel"})
	if err != nil || !strings.Contains(string(out), "window visibility: ok") {
		t.Fatalf("panel visibility: %v\n%s", err, out)
	}
}

// A panel created hidden (Hidden: true) never receives a hide event before its
// page finishes loading. Its page must still be told it is out of sight once
// navigation completes, or its timers and animations stay live while hidden.
func TestHiddenPanelLoadMarksPageOutOfSight(t *testing.T) {
	out, err := runAppKit(t, 2*time.Minute, []string{"MAGPIE_TEST_WINDOW_VISIBILITY=panel-hidden-load"})
	if err != nil || !strings.Contains(string(out), "window visibility: ok") {
		t.Fatalf("hidden panel load visibility: %v\n%s", err, out)
	}
}

// System hides that bypass hideWebview on macOS — minimising, hiding the app
// (Command-H), and occluding the window behind another — must still mark the
// main window's page out of sight and restore it when visible again, while a
// closed window stays hidden across an app hide/unhide cycle.
func TestMacSystemHidesMarkMainWindowOutOfSight(t *testing.T) {
	out, err := runAppKit(t, 2*time.Minute, []string{"MAGPIE_TEST_WINDOW_VISIBILITY=system-hide"})
	if err != nil || !strings.Contains(string(out), "window visibility: ok") {
		t.Fatalf("system hide visibility: %v\n%s", err, out)
	}
}

func init() {
	which := os.Getenv("MAGPIE_TEST_WINDOW_VISIBILITY")
	if which != "1" && which != "panel" && which != "panel-hidden-load" && which != "system-hide" {
		return
	}
	ready := make(chan struct{}, 1)
	hidden := make(chan url.Values, 1)
	reports := make(chan url.Values, 1)
	policy := application.ActivationPolicyProhibited
	if which == "system-hide" {
		policy = application.ActivationPolicyAccessory
	}
	app := application.New(application.Options{
		Name: "magpie window visibility regression",
		Mac:  application.MacOptions{ActivationPolicy: policy},
		Assets: application.AssetOptions{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/":
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<!doctype html><script type="module">import "/wails/runtime.js";</script><style>
#probe{animation:spin 1s linear infinite}@keyframes spin{to{transform:rotate(1turn)}}
html.magpie-out-of-sight #probe{animation-play-state:paused!important}
</style><i id="probe"></i><script>
const probe = document.querySelector("#probe");
// as app.js: hidden while magpie says so
let outOfSight = false;
for (const [k, off] of [["hidden", true], ["visibilityState", "hidden"]]) {
  const own = Object.getOwnPropertyDescriptor(Document.prototype, k)?.get;
  own && Object.defineProperty(Document.prototype, k, { configurable: true, get() { return outOfSight ? off : own.call(this); } });
}
let hiddenTold = false, showing = false;
window.magpieWindowVisibility = (visible) => {
  if (visible) hiddenTold = false;
  // shown again: what it last saw while hidden, once
  const seen = visible && localStorage.getItem("hiddenProbe");
  if (seen) {
    localStorage.removeItem("hiddenProbe");
    fetch("/report?probe=" + encodeURIComponent(seen));
  }
  outOfSight = !visible;
  document.documentElement.classList.toggle("magpie-out-of-sight", !visible);
  showing = visible;
  document.dispatchEvent(new Event("visibilitychange"));
  showing = false;
};
// a hiding is what follows magpie's hide signal: the shown signal's own
// event is not one, and neither is WebKit's own, which it may fire as the
// window orders out before magpie's signal is in
document.addEventListener("visibilitychange", () => {
  if (outOfSight && !showing) {
    localStorage.setItem("hiddenProbe", JSON.stringify({
      hidden: document.hidden,
      state: document.visibilityState,
      marked: document.documentElement.classList.contains("magpie-out-of-sight"),
      play: getComputedStyle(probe).animationPlayState,
    }));
    // once a hiding: the host shows the window again only after this
    if (!hiddenTold) {
      hiddenTold = true;
      fetch("/hidden");
    }
  }
});
fetch("/ready");
</script>`)
			case "/ready":
				select {
				case ready <- struct{}{}:
				default:
				}
				w.WriteHeader(http.StatusNoContent)
			case "/hidden":
				select {
				case hidden <- r.URL.Query():
				default:
				}
				w.WriteHeader(http.StatusNoContent)
			case "/report":
				select {
				case reports <- r.URL.Query():
				default:
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		})},
	})
	h := &host{app: app}
	fail := func(format string, a ...any) {
		fmt.Printf("window visibility: "+format+"\n", a...)
		os.Exit(1)
	}
	wait := func(what string, ch <-chan url.Values) url.Values {
		select {
		case v := <-ch:
			return v
		case <-time.After(15 * time.Second):
			fail("timed out waiting for %s", what)
			return nil
		}
	}
	// each way the window is closed: the main window by magpie; the panel
	// by Wails, on a click elsewhere and on Escape
	closes := map[string][]func(*host, *application.WebviewWindow){
		"1": {func(h *host, _ *application.WebviewWindow) { application.InvokeSync(h.hideMain) }},
		"panel": {
			func(_ *host, w *application.WebviewWindow) { w.HandleWindowEvent(uint(events.Common.WindowLostFocus)) },
			func(_ *host, w *application.WebviewWindow) { w.HandleKeyEvent("escape") },
		},
	}
	check := func(report url.Values) {
		probe := struct {
			Hidden bool   `json:"hidden"`
			State  string `json:"state"`
			Marked bool   `json:"marked"`
			Play   string `json:"play"`
		}{}
		if err := json.Unmarshal([]byte(report.Get("probe")), &probe); err != nil {
			fail("the page's hidden event is %q: %v", report.Get("probe"), err)
		}
		if !probe.Hidden || probe.State != "hidden" || !probe.Marked || probe.Play != "paused" {
			fail("while hidden the page saw hidden=%v state=%q marked=%v play=%q", probe.Hidden, probe.State, probe.Marked, probe.Play)
		}
	}
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go func() {
			if which == "panel-hidden-load" {
				var win *application.WebviewWindow
				application.InvokeSync(func() {
					win = h.makePanel()
					h.panel = win
				})
				select {
				case <-ready:
				case <-time.After(15 * time.Second):
					fail("timed out waiting for the hidden panel page")
				}
				wait("the hidden panel page to see itself hidden on load", hidden)
				application.InvokeSync(func() { showWebview(win) })
				check(wait("the hidden panel's report after show", reports))
				fmt.Println("window visibility: ok")
				app.Quit()
				return
			}
			if which == "system-hide" {
				onMain := func(fn func() bool) (ok bool) {
					application.InvokeSync(func() { ok = fn() })
					return
				}
				waitFor := func(what string, cond func() bool) {
					for deadline := time.Now().Add(10 * time.Second); !onMain(cond); time.Sleep(20 * time.Millisecond) {
						if time.Now().After(deadline) {
							fail("timed out waiting until %s", what)
						}
					}
				}
				var win *application.WebviewWindow
				application.InvokeSync(func() {
					win = h.makeMain("/")
					h.main = win
					showWebview(win)
				})
				select {
				case <-ready:
				case <-time.After(15 * time.Second):
					fail("timed out waiting for the main window page")
				}
				waitFor("the main window is open", func() bool { return windowOpen(win) })

				// 1. Minimise / unminimise without calling hideWebview or showWebview.
				application.InvokeSync(func() { win.Minimise() })
				waitFor("the main window is minimised", func() bool { return win.IsMinimised() })
				wait("the page to see itself hidden on minimise", hidden)
				application.InvokeSync(func() { win.UnMinimise() })
				waitFor("the main window is unminimised", func() bool { return windowOpen(win) })
				check(wait("the page's report after unminimise", reports))

				// 2. App hide (Command-H) / unhide without calling hideWebview or showWebview.
				waitFor("magpie is hidden", func() bool {
					if windowOpen(win) {
						app.Hide()
					}
					return !windowOpen(win)
				})
				wait("the page to see itself hidden on app hide", hidden)
				application.InvokeSync(app.Show)
				waitFor("magpie is shown again", func() bool { return windowOpen(win) })
				check(wait("the page's report after app unhide", reports))

				// 3. Occluded behind another window on an unlocked screen.
				if onMain(win.IsVisible) {
					var cover *application.WebviewWindow
					application.InvokeSync(func() {
						x, y := win.Position()
						wd, ht := win.Size()
						cover = app.Window.NewWithOptions(application.WebviewWindowOptions{
							Name: "cover", Width: wd + 200, Height: ht + 200,
							Frameless: true, AlwaysOnTop: true,
							BackgroundColour: application.NewRGB(0, 128, 128),
						})
						cover.SetPosition(x-100, y-100)
						cover.Show()
					})
					waitFor("the cover hides the main window", func() bool { return !win.IsVisible() })
					wait("the page to see itself hidden on occlusion", hidden)
					application.InvokeSync(func() { cover.Close() })
					waitFor("the main window is uncovered", win.IsVisible)
					check(wait("the page's report after uncover", reports))
				}

				// 4. Closed main window stays hidden across an app hide/unhide cycle.
				h.hideMain()
				wait("the page to see itself hidden on close", hidden)
				application.InvokeSync(app.Hide)
				time.Sleep(200 * time.Millisecond)
				application.InvokeSync(app.Show)
				select {
				case report := <-reports:
					fail("closed window reported visible after app unhide: %v", report)
				case <-time.After(400 * time.Millisecond):
				}
				application.InvokeSync(func() { showWebview(win) })
				check(wait("the closed window's report after reopen", reports))

				fmt.Println("window visibility: ok")
				app.Quit()
				return
			}
			var win *application.WebviewWindow
			application.InvokeSync(func() {
				if which == "panel" {
					win = h.makePanel()
					h.panel = win
					win.Show()
					return
				}
				win = app.Window.NewWithOptions(application.WebviewWindowOptions{
					Name:   "visibility",
					Title:  "visibility",
					URL:    "/",
					Width:  480,
					Height: 320,
				})
				h.main = win
			})
			select {
			case <-ready:
			case <-time.After(15 * time.Second):
				fail("timed out waiting for the page")
			}

			// the panel by a click elsewhere, then by Escape
			for _, hide := range closes[which] {
				hide(h, win)
				wait("the page to see itself hidden", hidden)
				application.InvokeSync(func() { showWebview(win).Focus() })
				check(wait("the page's hidden report", reports))
			}
			fmt.Println("window visibility: ok")
			app.Quit()
		}()
	})
	if err := app.Run(); err != nil {
		panic(err)
	}
	os.Exit(0)
}
