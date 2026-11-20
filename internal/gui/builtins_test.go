package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/pluginrt"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	_ "github.com/yetone/magpie/plugins/all"
)

func builtinHome(t *testing.T) http.Handler {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	return Handler(nil, nil)
}

func postBuiltin(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/plugins/builtin", strings.NewReader(body)))
	return rec
}

// A switch is kept in plugins.yml as the user's own choice and the
// runtime follows it; one in use is refused off and stays on; an unknown
// one is refused.
func TestBuiltinSwitch(t *testing.T) {
	h := builtinHome(t)
	if _, err := pluginrt.Settle(); err != nil {
		t.Fatal(err)
	}
	// a new install has every one on, so a new user sees all magpie does
	for _, b := range builtinsState() {
		if !b.On || b.State != pluginrt.Active {
			t.Fatalf("a new install: %+v", b)
		}
	}
	if err := pluginrt.SetPreset(pluginrt.PresetEssentials); err != nil {
		t.Fatal(err)
	}
	if rec := postBuiltin(h, `{"id":"library","on":true}`); rec.Code != http.StatusOK {
		t.Fatalf("library on: %d %s", rec.Code, rec.Body)
	} else {
		var out []builtinJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out) != 14 {
			t.Fatalf("the built-ins: %+v", out)
		}
		for _, b := range out {
			// the Gateway, Routing and Usage pages, the routing strategies,
			// the gateway middleware, privacy masking, web search, key
			// limits and plugin tools are essential: on in every preset
			if b.On != (b.ID == "budget" || b.ID == "library" || b.ID == "gateway" || b.ID == "middleware" || b.ID == "redact" || b.ID == "rotation" || b.ID == "routing" || b.ID == "search" || b.ID == "tools" || b.ID == "usage") || b.On != (b.State == pluginrt.Active) {
				t.Fatalf("after library on: %+v", out)
			}
		}
	}
	if !pluginrt.Running().Active("library") || pluginrt.Running().Active("sessions") {
		t.Fatalf("the runtime: %+v", pluginrt.Running().Statuses())
	}
	if rec := postBuiltin(h, `{"id":"nope","on":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown plugin: %d", rec.Code)
	}
	// Observability in use: sending to a collector
	s := settings.Load()
	s.OTel.Enabled = true
	s.OTel.Endpoint = "http://127.0.0.1:4318"
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	if rec := postBuiltin(h, `{"id":"otel","on":false}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "in use") {
		t.Fatalf("otel off while in use: %d %s", rec.Code, rec.Body)
	}
	for _, b := range builtinsState() {
		if b.ID == "otel" && (!b.On || b.Chosen || b.InUse != 1 || b.Use != "Sending to a collector" || b.Source != pluginrt.FromInUse) {
			t.Fatalf("otel in use: %+v", b)
		}
	}
}

// A plugins.yml that doesn't parse leaves every plugin on, says so, and
// no switch writes over it.
func TestBuiltinBrokenFile(t *testing.T) {
	h := builtinHome(t)
	if err := os.MkdirAll(filepath.Dir(pluginrt.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginrt.Path(), []byte("plugins: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, b := range builtinsState() {
		if b.Broken == "" {
			t.Fatalf("not told broken: %+v", b)
		}
	}
	if rec := postBuiltin(h, `{"id":"library","on":false}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("switched over a broken file: %d", rec.Code)
	}
	if b, _ := os.ReadFile(pluginrt.Path()); string(b) != "plugins: [\n" {
		t.Fatalf("written over: %q", b)
	}
}

// boot.js says what is off before the page paints: nothing for an
// install as it starts, what Essentials leaves off once the user picks it.
func TestBootSaysBuiltinsOff(t *testing.T) {
	h := builtinHome(t)
	boot := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/boot.js", nil))
		return rec.Body.String()
	}
	if _, err := pluginrt.Settle(); err != nil {
		t.Fatal(err)
	}
	if b := boot(); !strings.Contains(b, `"featuresOff":[]`) {
		t.Fatalf("an install as it starts: %s", b)
	}
	if err := pluginrt.SetPreset(pluginrt.PresetEssentials); err != nil {
		t.Fatal(err)
	}
	if err := pluginrt.Set("library", true); err != nil {
		t.Fatal(err)
	}
	if b := boot(); !strings.Contains(b, `"featuresOff":["groups","otel","sessions","warmups"]`) {
		t.Fatalf("Essentials picked: %s", b)
	}
}

// boot.js says where the gateway listens when this process serves it, so
// the sidebar's line is drawn before the first paint (#3429), and nothing
// when it doesn't: one served by another magpie is the providers' read to
// find, never this process's guess.
func TestBootSaysWhereItsGatewayListens(t *testing.T) {
	h := builtinHome(t)
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:3481")
	boot := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/boot.js", nil))
		return rec.Body.String()
	}
	prev := served.Swap(nil)
	t.Cleanup(func() { served.Store(prev) })
	if b := boot(); strings.Contains(b, "sideGateway") {
		t.Fatalf("no gateway served here: %s", b)
	}
	served.Store(gateway.New())
	if b := boot(); !strings.Contains(b, `"sideGateway":{"boot":true,"running":true,"url":"http://127.0.0.1:3481"}`) {
		t.Fatalf("served here: %s", b)
	}
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3483")
	// on the network, it says so, with the addresses other machines use,
	// for the window's line to name them (coeo91 on Discord)
	if b := boot(); !strings.Contains(b, `"sideGateway":{"boot":true,"lan":true,`) || !strings.Contains(b, `"running":true,"url":"http://127.0.0.1:3483"}`) {
		t.Fatalf("every interface, reached on loopback: %s", b)
	}
	// MAGPIE_PUBLIC_URL goes with it while other machines are let in, for
	// magpie web's page to show (noahhrreion on Discord, in Docker), and
	// not while the gateway is this computer's alone
	t.Setenv("MAGPIE_PUBLIC_URL", "http://nas.local:3425")
	if b := boot(); !strings.Contains(b, `"sideGateway":{"boot":true,"lan":true,"lanURLs":["http://nas.local:3425"],"publicURL":"http://nas.local:3425","running":true,"url":"http://127.0.0.1:3483"}`) {
		t.Fatalf("on the network, with MAGPIE_PUBLIC_URL: %s", b)
	}
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:3483")
	if b := boot(); strings.Contains(b, "nas.local") || strings.Contains(b, `"lan"`) {
		t.Fatalf("on loopback, not shared: %s", b)
	}
	t.Setenv("MAGPIE_PUBLIC_URL", "")
	served.Store(nil)
	if b := boot(); strings.Contains(b, "sideGateway") {
		t.Fatalf("stopped: %s", b)
	}
}

// A plugin's settings are set only as their schema allows, kept in
// plugins.yml, and handed to the page in boot.js; a request says on or
// config, not both.
func TestBuiltinConfig(t *testing.T) {
	h := builtinHome(t)
	if _, err := pluginrt.Settle(); err != nil {
		t.Fatal(err)
	}
	gw := func() builtinJSON {
		for _, b := range builtinsState() {
			if b.ID == "gateway" {
				return b
			}
		}
		t.Fatal("no gateway plugin")
		return builtinJSON{}
	}
	if b := gw(); b.Schema == nil || b.Schema.Properties["callsPage"] == nil || b.Config["callsPage"] != int64(20) {
		t.Fatalf("before: %+v", b)
	}
	if rec := postBuiltin(h, `{"id":"gateway","config":{"callsPage":50}}`); rec.Code != http.StatusOK {
		t.Fatalf("50: %d %s", rec.Code, rec.Body)
	}
	if b := gw(); b.Config["callsPage"] != int64(50) || !b.On {
		t.Fatalf("after 50: %+v", b)
	}
	for _, bad := range []string{
		`{"id":"gateway","config":{"callsPage":30}}`,
		`{"id":"gateway","config":{"other":1}}`,
		`{"id":"gateway","on":true,"config":{"callsPage":10}}`,
		`{"id":"gateway"}`,
		`{"id":"library","config":{"callsPage":10}}`,
	} {
		if rec := postBuiltin(h, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if b := gw(); b.Config["callsPage"] != int64(50) {
		t.Fatalf("a refused one written: %+v", b)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/boot.js", nil))
	if !strings.Contains(rec.Body.String(), `"pluginConfig":{"gateway":{"callsPage":50}}`) {
		t.Fatalf("boot.js: %s", rec.Body)
	}
}

func TestBuiltinSectionAndInlinePartLifecycle(t *testing.T) {
	handler := builtinHome(t)
	if _, err := pluginrt.Settle(); err != nil {
		t.Fatal(err)
	}
	read := func(t *testing.T, id string) builtinJSON {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/plugins/builtin", nil))
		var features []builtinJSON
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &features) != nil {
			t.Fatalf("read built-ins: %d %s", response.Code, response.Body)
		}
		for _, feature := range features {
			if feature.ID == id {
				return feature
			}
		}
		t.Fatalf("missing built-in %s", id)
		return builtinJSON{}
	}
	for _, expected := range []pluginrt.Part{
		{Slot: "settings.section", Key: "otel"},
		{Slot: "part", Key: "groups"},
		{Slot: "part", Key: "rotation"},
		{Slot: "part", Key: "warmups"},
	} {
		t.Run(expected.Key, func(t *testing.T) {
			if initial := read(t, expected.Key); !initial.On || !slices.Equal(initial.Parts, []pluginrt.Part{expected}) {
				t.Fatalf("active: %+v, want %v", initial, expected)
			}
			off := postBuiltin(handler, `{"id":"`+expected.Key+`","on":false}`)
			if actual := read(t, expected.Key); off.Code != http.StatusOK || actual.On || len(actual.Parts) != 0 {
				t.Fatalf("off: %d %s, %+v", off.Code, off.Body, actual)
			}
			on := postBuiltin(handler, `{"id":"`+expected.Key+`","on":true}`)
			if actual := read(t, expected.Key); on.Code != http.StatusOK || !actual.On || !slices.Equal(actual.Parts, []pluginrt.Part{expected}) {
				t.Fatalf("restored: %d %s, %+v", on.Code, on.Body, actual)
			}
		})
	}
}

func TestGatewayBuiltinContributionLifecycle(t *testing.T) {
	handler := builtinHome(t)
	if _, err := pluginrt.Settle(); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) string {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, response.Code, response.Body)
		}
		return response.Body.String()
	}
	check := func(reply string, state pluginrt.State, callsPage float64) {
		t.Helper()
		var features []builtinJSON
		if err := json.Unmarshal([]byte(reply), &features); err != nil {
			t.Fatal(err)
		}
		pages := map[string]string{
			"gateway": "gateway", "library": "library", "redact": "masking",
			"routing": "routing", "sessions": "sessions", "usage": "usage",
		}
		for _, feature := range features {
			if expected, ok := pages[feature.ID]; ok {
				if feature.Page != expected {
					t.Fatalf("%s page: %q, want %q", feature.ID, feature.Page, expected)
				}
				delete(pages, feature.ID)
			}
			if feature.ID != "gateway" {
				continue
			}
			on := state == pluginrt.Active
			if feature.State != state || feature.On != on || feature.Config["callsPage"] != callsPage {
				t.Fatalf("Gateway contribution: %+v", feature)
			}
			var parts []pluginrt.Part
			if on {
				parts = []pluginrt.Part{{Slot: "nav.page", Key: "gateway"}}
			}
			if !slices.Equal(feature.Parts, parts) {
				t.Fatalf("Gateway active contributions: %v, want %v", feature.Parts, parts)
			}
			if feature.Schema == nil || !slices.Equal(feature.Schema.Order, []string{"callsPage"}) {
				t.Fatalf("Gateway settings order: %+v", feature.Schema)
			}
			property := feature.Schema.Properties["callsPage"]
			if property == nil || property.Type != "integer" || property.Default != float64(20) ||
				!slices.Equal(property.Enum, []any{float64(10), float64(20), float64(50), float64(100)}) {
				t.Fatalf("Gateway callsPage schema: %+v", property)
			}
		}
		if len(pages) > 0 {
			t.Fatalf("missing Built-in pages: %v", pages)
		}
	}
	check(request("GET", "/api/plugins/builtin", ""), pluginrt.Active, 20)
	check(request("POST", "/api/plugins/builtin", `{"id":"gateway","config":{"callsPage":100}}`), pluginrt.Active, 100)
	check(request("POST", "/api/plugins/builtin", `{"id":"gateway","on":false}`), pluginrt.Disposed, 100)
	if boot := request("GET", "/boot.js", ""); !strings.Contains(boot, `"featuresOff":["gateway"]`) ||
		strings.Contains(boot, `"callsPage":100`) {
		t.Fatalf("boot.js after retirement: %s", boot)
	}
	check(request("POST", "/api/plugins/builtin", `{"id":"gateway","on":true}`), pluginrt.Active, 100)
	if boot := request("GET", "/boot.js", ""); !strings.Contains(boot, `"pluginConfig":{"gateway":{"callsPage":100}}`) {
		t.Fatalf("boot.js after re-enable: %s", boot)
	}
}

func TestGatewayBuiltinDuplicateSettingsRows(t *testing.T) {
	handler := builtinHome(t)
	writeBuiltinYAML(t, `plugins:
  - id: gateway
    enabled: false
    config: {callsPage: 10}
  - id: library
    enabled: false
  - id: gateway
    enabled: true
    config: {callsPage: 50}
`)
	check := func(body string, on bool, callsPage float64) {
		t.Helper()
		feature := readGatewayBuiltin(t, handler, body)
		if feature.On != on || feature.Chosen != on || feature.Config["callsPage"] != callsPage {
			t.Errorf("after %q: on=%v chosen=%v config=%v, want on=%v callsPage=%v", body, feature.On, feature.Chosen, feature.Config, on, callsPage)
		}
	}
	check("", true, 50)
	check(`{"id":"gateway","on":false}`, false, 50)
	check(`{"id":"gateway","config":{"callsPage":100}}`, false, 100)
	check(`{"id":"gateway","on":true}`, true, 100)
	file, _, err := pluginrt.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Plugins) != 3 || file.Plugins[0].Config["callsPage"] != 10 ||
		file.Plugins[0].Enabled == nil || *file.Plugins[0].Enabled ||
		file.Plugins[1].ID != "library" || file.Plugins[1].Enabled == nil || *file.Plugins[1].Enabled {
		t.Fatalf("edit changed a shadowed or unrelated row: %+v", file.Plugins)
	}
}

func TestGatewayBuiltinMalformedSettingsKeepAppliedConfig(t *testing.T) {
	handler := builtinHome(t)
	writeBuiltinYAML(t, "plugins: [{id: gateway, config: {callsPage: 100}}]\n")
	config := readGatewayBuiltin(t, handler, "").Config
	if config["callsPage"] != float64(100) {
		t.Fatalf("initial callsPage: %v", config)
	}
	const malformed = "plugins: [\n"
	writeBuiltinYAML(t, malformed)
	feature := readGatewayBuiltin(t, handler, "")
	if !feature.On || !feature.Chosen || feature.Broken == "" || feature.Config["callsPage"] != float64(100) {
		t.Errorf("malformed file lost applied gateway settings: %+v", feature)
	}
	boot := httptest.NewRecorder()
	handler.ServeHTTP(boot, httptest.NewRequest("GET", "/boot.js", nil))
	if boot.Code != http.StatusOK || !strings.Contains(boot.Body.String(), `"pluginConfig":{"gateway":{"callsPage":100}}`) {
		t.Errorf("boot.js lost the applied Gateway callsPage: HTTP %d", boot.Code)
	}
	for _, update := range []string{`{"id":"gateway","on":false}`, `{"id":"gateway","config":{"callsPage":50}}`} {
		if response := postBuiltin(handler, update); response.Code != http.StatusBadRequest {
			t.Errorf("accepted %s over malformed file: %d", update, response.Code)
		}
	}
	if body, err := os.ReadFile(pluginrt.Path()); err != nil || string(body) != malformed {
		t.Fatalf("malformed file was changed: %q, %v", body, err)
	}
	writeBuiltinYAML(t, "plugins: [{id: gateway, config: {callsPage: 50}}]\n")
	feature = readGatewayBuiltin(t, handler, "")
	if !feature.On || feature.Broken != "" || feature.Config["callsPage"] != float64(50) {
		t.Fatalf("repaired file was not applied: %+v", feature)
	}
	writeBuiltinYAML(t, "plugins: []\n")
	if feature := readGatewayBuiltin(t, handler, ""); feature.Broken != "" || feature.Config["callsPage"] != float64(20) {
		t.Fatalf("removed setting did not return to its default: %+v", feature)
	}
}

func writeBuiltinYAML(t *testing.T, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(pluginrt.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginrt.Path(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readGatewayBuiltin(t *testing.T, handler http.Handler, body string) builtinJSON {
	t.Helper()
	response := httptest.NewRecorder()
	if body == "" {
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/plugins/builtin", nil))
	} else {
		response = postBuiltin(handler, body)
	}
	var features []builtinJSON
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &features) != nil {
		t.Fatalf("read built-ins: %d %s", response.Code, response.Body)
	}
	for _, feature := range features {
		if feature.ID == "gateway" {
			return feature
		}
	}
	t.Fatal("missing Gateway built-in")
	return builtinJSON{}
}

// Every file a built-in plugin says it has is on disk, loaded by the page
// and named with its hash so a new one isn't read from the cache: a script
// after the kernel and before app.js, which looks for its pages, a
// stylesheet in the head with the others.
func TestBuiltinClientsLoaded(t *testing.T) {
	page, err := os.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(page)
	at := func(name string) int {
		tag := `<script src="` + name + `"></script>`
		if strings.HasSuffix(name, ".css") {
			tag = `<link rel="stylesheet" href="` + name + `">`
		}
		i := strings.Index(src, tag)
		if i < 0 {
			t.Fatalf("index.html doesn't load %s", name)
		}
		return i
	}
	kernel, app, start := at("kernel.js"), at("app.js"), at("start.js")
	if kernel > app || app > start {
		t.Fatal("kernel.js, app.js and start.js out of order")
	}
	n := 0
	for _, m := range pluginrt.Registered() {
		for _, c := range m.Client {
			name := "plugins/" + m.ID + "/" + c
			if _, err := os.Stat(filepath.Join("assets", name)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			attr := "src"
			if strings.HasSuffix(name, ".css") {
				attr = "href"
				if i := at(name); i > strings.Index(src, "</head>") {
					t.Errorf("%s isn't in the head", name)
				}
			} else if c == "page.js" {
				// a page's code that runs on app.js's helpers: after app.js,
				// before start.js shows the first page
				if i := at(name); i < app || i > start {
					t.Errorf("%s isn't between app.js and start.js", name)
				}
			} else if i := at(name); i < kernel || i > app {
				t.Errorf("%s isn't between kernel.js and app.js", name)
			}
			if !strings.Contains(string(versionedPage(page)), attr+`="`+name+`?v=`) {
				t.Errorf("%s isn't named with its hash", name)
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("no plugin has a script")
	}
}

// Every string of every built-in plugin is said in every language.
func TestBuiltinsTranslated(t *testing.T) {
	src, err := os.ReadFile("assets/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range []string{"zh: {", `"zh-TW": {`, "ja: {", "de: {"} {
		_, block, ok := strings.Cut(string(src), "\n  "+lang+"\n")
		if !ok {
			t.Fatalf("no %s block", lang)
		}
		block, _, _ = strings.Cut(block, "\n  },")
		for _, m := range pluginrt.Registered() {
			says := []string{m.Name, m.Sub, m.One, m.Many}
			if sc := m.Schema(); sc != nil {
				for _, p := range sc.Properties {
					says = append(says, p.Title, p.Description)
				}
			}
			for _, s := range says {
				if s == "" {
					continue
				}
				k, _ := json.Marshal(s)
				if !strings.Contains(block, "\n    "+string(k)+": ") {
					t.Errorf("%s doesn't say %s", lang, k)
				}
			}
		}
	}
}

// Web search's row says what a model that can't search is searched with
// now (netube99 on X): nothing yet in a new home, a search API once one
// is set, and none while it is off.
func TestBuiltinSearchSaysWhatSearches(t *testing.T) {
	builtinHome(t)
	if _, err := pluginrt.Settle(); err != nil {
		t.Fatal(err)
	}
	search := func() builtinJSON {
		for _, b := range builtinsState() {
			if b.ID == "search" {
				return b
			}
		}
		t.Fatal("no search row")
		return builtinJSON{}
	}
	if b := search(); !b.On || len(b.Searches) != 0 {
		t.Fatalf("a new home: %+v", b)
	}
	if err := provider.SetSearchAPI(provider.SearchAPI{Vendor: "tavily", Key: "tvly-test"}); err != nil {
		t.Fatal(err)
	}
	if b := search(); !b.On || !slices.Equal(b.Searches, []string{"Tavily"}) {
		t.Fatalf("with Tavily: %+v", b)
	}
	if err := provider.RemoveSearchAPI("tavily"); err != nil {
		t.Fatal(err)
	}
	if err := pluginrt.Set("search", false); err != nil {
		t.Fatal(err)
	}
	if b := search(); b.On || b.Searches != nil {
		t.Fatalf("off: %+v", b)
	}
}

// A built-in with settings says where they are, and its page on or off,
// for its row to go to both (netube99 on X: masking's switch, settings and
// page didn't name each other)
func TestBuiltinSaysWhereItsSettingsAre(t *testing.T) {
	handler := builtinHome(t)
	if _, err := pluginrt.Settle(); err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{
		"budget":     {"", "", "gateway"},
		"gateway":    {"network #portList", "gateway", ""},
		"groups":     {"", "", "groups"},
		"library":    {"", "library", ""},
		"middleware": {"", "", "plugins"},
		"otel":       {"otel #otelList", "", ""},
		"redact":     {"privacy #redactList", "masking", ""},
		"rotation":   {"", "", "providers"},
		"routing":    {"", "routing", ""},
		"search":     {"models #searchList", "", ""},
		"sessions":   {"", "sessions", ""},
		"tools":      {"", "", "plugins"},
		"usage":      {"", "usage", ""},
		"warmups":    {"", "", "providers"},
	}
	read := func(path string) string {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body)
		}
		return response.Body.String()
	}
	check := func(disabled ...string) {
		t.Helper()
		var features []builtinJSON
		if err := json.Unmarshal([]byte(read("/api/plugins/builtin")), &features); err != nil {
			t.Fatal(err)
		}
		destinations := make(map[string][3]string, len(features))
		for _, feature := range features {
			destinations[feature.ID] = [3]string{feature.SetAt, feature.Page, feature.Opens}
			state := pluginrt.Active
			if slices.Contains(disabled, feature.ID) {
				state = pluginrt.Disposed
			}
			if feature.State != state || feature.On != (state == pluginrt.Active) {
				t.Errorf("%s: state %q, on %t, want %q", feature.ID, feature.State, feature.On, state)
			}
		}
		for id, expected := range want {
			actual, exists := destinations[id]
			if !exists || actual != expected {
				t.Errorf("%s destinations: %q, want %q", id, actual, expected)
			}
			delete(destinations, id)
		}
		if len(destinations) != 0 {
			t.Errorf("unexpected built-ins: %v", destinations)
		}
		if len(disabled) == 0 {
			disabled = []string{}
		}
		off, err := json.Marshal(disabled)
		if err != nil {
			t.Fatal(err)
		}
		if boot := read("/boot.js"); !strings.Contains(boot, `"featuresOff":`+string(off)) {
			t.Fatalf("boot.js off list, want %s: %s", off, boot)
		}
	}
	check()
	disabled := []string{"otel", "redact", "rotation", "search"}
	for _, id := range disabled {
		if response := postBuiltin(handler, `{"id":"`+id+`","on":false}`); response.Code != http.StatusOK {
			t.Fatalf("%s off: %d %s", id, response.Code, response.Body)
		}
	}
	check(disabled...)
	for _, id := range disabled {
		if response := postBuiltin(handler, `{"id":"`+id+`","on":true}`); response.Code != http.StatusOK {
			t.Fatalf("%s on: %d %s", id, response.Code, response.Body)
		}
	}
	check()
}
