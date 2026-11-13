package windowmeta_test

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/windowmeta"
)

func windowFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "ui"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"ui/index.html": `<p>page</p>`,
		"ui/panel.html": `<p>panel</p>`,
		"ui/icon.svg":   `<svg/>`,
		"package.json":  `{"magpie":{"page":{"file":"missing.html","title":"Unchecked disk metadata"}}}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A checked metadata reader passes its bytes to the shared parser. The parser
// must not reopen package.json and substitute unverified, newer metadata.
func TestWindowFromDataUsesSuppliedMetadata(t *testing.T) {
	dir := windowFiles(t)
	data := []byte(`{"magpie":{
		"page":{"title":" Supplied ","file":"ui/index.html","icon":"ui/icon.svg","replaces":"usage","keepAlive":true,"titles":{" zh ":" 页面 ","ja":"","de":7}},
		"panels":[
			{"slot":"unknown","file":"ui/panel.html"},
			{"slot":"agents","file":"ui/panel.html","title":" First real panel "},
			{"slot":"usage","file":"ui/missing.html"}
		],
		"actions":[{"id":"open","menu":"provider"},{"id":"open","menu":"agent"},{"id":"Bad ID","menu":"provider"}],
		"permissions":["providers:list","secrets:all","providers:list","usage:day"]
	}}`)
	window, only := windowmeta.WindowFromData(dir, data)
	if window == nil || window.Page == nil || !only {
		t.Fatalf("checked metadata was not used: %+v, only=%v", window, only)
	}
	page := window.Page
	if page.Title != "Supplied" || page.Icon != "icon.svg" || page.Replaces != "usage" || !page.KeepAlive || page.Titles["zh"] != "页面" || len(page.Titles) != 1 {
		t.Fatalf("page metadata changed: %+v", page)
	}
	if page.File != filepath.Join(dir, "ui", "index.html") || page.Dir != filepath.Join(dir, "ui") {
		t.Fatalf("page paths changed: %+v", page)
	}
	if len(window.Panels) != 1 || window.Panels[0].Slot != "agents" || window.Panels[0].Title != "First real panel" {
		t.Fatalf("filtered panel index changed: %+v", window.Panels)
	}
	if len(window.Actions) != 1 || window.Actions[0].ID != "open" || window.Actions[0].Menu != "provider" {
		t.Fatalf("legacy action filtering changed: %+v", window.Actions)
	}
	if !slices.Equal(window.Reads, []string{"providers:list", "usage:day"}) {
		t.Fatalf("public read filtering changed: %v", window.Reads)
	}
}

func TestWindowFromDataPreservesLegacyHostSelection(t *testing.T) {
	dir := windowFiles(t)
	for _, tc := range []struct {
		name, host string
		only       bool
	}{
		{"page only", "", true},
		{"legacy main", `"main":"legacy.ts",`, false},
		{"legacy exports", `"exports":{".":"./legacy.ts"},`, false},
		{"legacy null exports", `"exports":null,`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`{` + tc.host + `"magpie":{"page":{"file":"ui/index.html","service":{"port":"3477","option":" port "}}}}`)
			window, only := windowmeta.WindowFromData(dir, data)
			if window == nil || window.Page == nil || only != tc.only {
				t.Fatalf("legacy selection changed: %+v, only=%v", window, only)
			}
			service := window.Page.Service
			if tc.only {
				if service != nil {
					t.Fatal("page-only metadata gained a legacy host service")
				}
				return
			}
			if service == nil || service.Port != 3477 || service.Option != "port" || service.ServicePort(map[string]any{"port": "4321"}) != 4321 {
				t.Fatalf("legacy service selection changed: %+v", service)
			}
		})
	}
}

func TestFileInAndUnreadableWindow(t *testing.T) {
	dir := windowFiles(t)
	ui := filepath.Join(dir, "ui")
	if file, ok := windowmeta.FileIn(ui, "index.html"); !ok || file != filepath.Join(ui, "index.html") {
		t.Fatalf("own file: %q %v", file, ok)
	}
	for _, name := range []string{"", "../package.json", "..", ".", `..\package.json`, filepath.Join(dir, "package.json"), "missing.html"} {
		if file, ok := windowmeta.FileIn(ui, name); ok {
			t.Fatalf("outside, missing or directory file accepted: %q -> %q", name, file)
		}
	}
	for _, data := range []string{
		`{`, `null`, `[]`, `{}`,
		`{"magpie":{"page":{"file":"ui/missing.html"}}}`,
		`{"magpie":{"page":{"file":"package.json"}}}`,
		`{"magpie":{"panels":[{"slot":"unknown","file":"ui/panel.html"}]}}`,
	} {
		if window, only := windowmeta.WindowFromData(dir, []byte(data)); window != nil || only {
			t.Fatalf("unreadable/absent window became a part: %s -> %+v, only=%v", data, window, only)
		}
	}
}

func TestServicePortKeepsLegacyBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		port  int
	}{
		{"minimum", float64(1), 1},
		{"maximum", float64(65535), 65535},
		{"trimmed string", " 3477 ", 3477},
		{"zero", float64(0), 0},
		{"above maximum", float64(65536), 0},
		{"fraction", 1.5, 0},
		{"NaN", math.NaN(), 0},
		{"infinity", math.Inf(1), 0},
		{"unexpected Go integer", 3477, 0},
		{"boolean", true, 0},
		{"invalid string", "port", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, ok := windowmeta.PortOf(tc.value)
			if port != tc.port || ok != (tc.port != 0) {
				t.Fatalf("PortOf(%v) = %d, %v; want %d", tc.value, port, ok, tc.port)
			}
			service := &windowmeta.ServiceSpec{Port: 3477, Option: "port"}
			want := tc.port
			if want == 0 {
				want = 3477
			}
			if got := service.ServicePort(map[string]any{"port": tc.value}); got != want {
				t.Fatalf("option port=%d; want %d", got, want)
			}
		})
	}
	var service *windowmeta.ServiceSpec
	if service.ServicePort(nil) != 0 {
		t.Fatal("nil legacy service acquired a port")
	}
}
