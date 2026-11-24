package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/sessions"
	"github.com/yetone/magpie/internal/settings"
)

// The Sessions page's consent and Settings' rows (RustCats on X): a fresh
// install's /api/sessions/reads lists the agents found, none on, and
// sessions/manage lists no session until one is allowed; allow selected
// reads that agent only, and Settings turns it off again.
func TestSessionReadsRoutes(t *testing.T) {
	h := sandboxHome(t)
	claude := filepath.Join(h, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	f := filepath.Join(claude, "projects", "-work-app", "sess1.jsonl")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","timestamp":"2026-09-20T10:00:00Z","message":{"role":"user","content":"my secret plan"},"cwd":"/work/app","sessionId":"sess1"}`
	if err := os.WriteFile(f, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a fresh install as SettleReads leaves it: decided, not asked
	if err := settings.SetSessionReads(func(*settings.SessionReads) *settings.SessionReads {
		return &settings.SessionReads{}
	}); err != nil {
		t.Fatal(err)
	}
	sessions.Reset()
	t.Cleanup(sessions.Reset)

	mux := http.NewServeMux()
	sessionManageRoutes(mux, folderOnly{})
	call := func(method, path, body string) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		return w.Body.Bytes()
	}
	var r readsJSON
	json.Unmarshal(call("GET", "/api/sessions/reads", ""), &r)
	if r.Asked || r.All || len(r.Found) != 1 || r.Found[0].Agent != "claude" || r.Found[0].On {
		t.Fatalf("fresh reads = %+v, want claude found and off", r)
	}
	type manage struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
		Reads readsJSON `json:"reads"`
	}
	var m manage
	json.Unmarshal(call("GET", "/api/sessions/manage", ""), &m)
	if len(m.Sessions) != 0 || m.Reads.Asked || len(m.Reads.Found) != 1 {
		t.Fatalf("not asked: manage = %+v", m)
	}
	if strings.Contains(string(call("GET", "/api/sessions/manage", "")), "secret") {
		t.Fatal("manage carries the session's text before it was allowed")
	}

	// allow selected, Claude Code on
	json.Unmarshal(call("POST", "/api/sessions/reads", `{"all":false,"agents":{"claude":true}}`), &r)
	if !r.Asked || r.All || !r.Found[0].On {
		t.Fatalf("after allow selected: %+v", r)
	}
	m = manage{}
	json.Unmarshal(call("GET", "/api/sessions/manage", ""), &m)
	if len(m.Sessions) != 1 || m.Sessions[0].ID != "sess1" {
		t.Fatalf("claude allowed: manage = %+v", m)
	}

	// Settings turns it off
	json.Unmarshal(call("POST", "/api/sessions/reads", `{"agents":{"claude":false}}`), &r)
	m = manage{}
	json.Unmarshal(call("GET", "/api/sessions/manage", ""), &m)
	if len(m.Sessions) != 0 || !m.Reads.Asked || m.Reads.Found[0].On {
		t.Fatalf("claude turned off: manage = %+v", m)
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/sessions/reads", strings.NewReader("{")))
	if w.Code == 200 {
		t.Fatal("a body that isn't JSON was taken")
	}
}

func TestSessionReadsFindsOMPStores(t *testing.T) {
	const id = "019a0000-0000-7000-8000-00000000000a"
	const filename = "2026-09-28T08-00-00-000Z_" + id + ".jsonl"
	fixture, err := os.ReadFile("../sessions/testdata/omp/agent/sessions/--work-omp--/" + filename)
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range []struct {
		name, root, config, override string
		xdg                          bool
	}{
		{name: "default", root: ".omp/agent/sessions"},
		{name: "profile", root: ".omp/profiles/work/agent/sessions"},
		{name: "renamed_config", root: ".custom-omp/agent/sessions", config: ".custom-omp"},
		{name: "agent_override", root: "shared-agent/sessions", override: "shared-agent"},
		{name: "xdg_profile", root: "data/omp/profiles/work/sessions", xdg: true},
	} {
		t.Run(store.name, func(t *testing.T) {
			if store.xdg && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
				t.Skip("OMP uses XDG data on Linux and macOS")
			}
			home := sandboxHome(t)
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			if store.config != "" {
				t.Setenv("PI_CONFIG_DIR", store.config)
			}
			if store.override != "" {
				t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, store.override))
			}
			if err := settings.SetSessionReads(func(*settings.SessionReads) *settings.SessionReads {
				return &settings.SessionReads{}
			}); err != nil {
				t.Fatal(err)
			}
			sessions.Reset()
			t.Cleanup(sessions.Reset)
			mux := http.NewServeMux()
			sessionReadsRoutes(mux)
			call := func(method, body string) (readsJSON, []readAgentJSON) {
				t.Helper()
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, httptest.NewRequest(method, "/api/sessions/reads", strings.NewReader(body)))
				if response.Code != http.StatusOK {
					t.Fatalf("%s reads: %d %s", method, response.Code, response.Body)
				}
				var result readsJSON
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				var omp []readAgentJSON
				for _, agent := range result.Found {
					if agent.Agent == "omp" {
						omp = append(omp, agent)
					}
				}
				return result, omp
			}

			if _, omp := call(http.MethodGet, ""); len(omp) != 0 {
				t.Fatalf("OMP offered with no store: %+v", omp)
			}
			journal := filepath.Join(home, filepath.FromSlash(store.root), "--work-omp--", filename)
			if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(journal, fixture, 0o600); err != nil {
				t.Fatal(err)
			}
			if reads, omp := call(http.MethodGet, ""); reads.Asked || reads.All || len(omp) != 1 || omp[0].On {
				t.Fatalf("OMP consent row missing or already allowed: %+v", reads)
			}
			if _, ok := sessions.Find("omp", id); ok {
				t.Fatal("OMP journal listed before consent")
			}
			if reads, omp := call(http.MethodPost, `{"all":false,"agents":{"omp":true}}`); !reads.Asked || reads.All || len(omp) != 1 || !omp[0].On {
				t.Fatalf("OMP consent did not take effect: %+v", reads)
			}
			if row, ok := sessions.Find("omp", id); !ok || row.Path != journal || row.Title != "Port the parser to omp" {
				t.Fatalf("allowed OMP journal not found at %q: %+v", journal, row)
			}
			if reads, omp := call(http.MethodPost, `{"agents":{"omp":false}}`); !reads.Asked || len(omp) != 1 || omp[0].On || omp[0].New {
				t.Fatalf("OMP consent not revoked: %+v", reads)
			}
			if _, ok := sessions.Find("omp", id); ok {
				t.Fatal("OMP journal still listed after consent was revoked")
			}
		})
	}
}

// An agent installed after "allow selected" (baysonfox on X: Antigravity
// 2.0's sessions showed only while it was connected to magpie, agy CLI's
// always). The choice read agy only, and nothing asked about the app found
// later. /api/sessions/reads marks it new; the page's Show their sessions names it on and
// its sessions are listed, and Keep off names it off and it isn't new again.
// The app's folder is a real Antigravity 2.30 one (testdata/antigravity-2.30,
// written by its language server on Windows), agy's a real agy CLI's.
func TestSessionReadsNewAgent(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(map[bool]string{true: "allow", false: "keep off"}[allow], func(t *testing.T) {
			h := sandboxHome(t)
			copyTree(t, "../sessions/testdata/agy-sessions", filepath.Join(h, ".gemini", "antigravity-cli"))
			copyTree(t, "../sessions/testdata/antigravity-2.30", filepath.Join(h, ".gemini", "antigravity"))
			// the choice made before the app was installed: agy only
			if err := settings.SetSessionReads(func(*settings.SessionReads) *settings.SessionReads {
				return &settings.SessionReads{Asked: true, Agents: map[string]bool{"agy": true}}
			}); err != nil {
				t.Fatal(err)
			}
			sessions.Reset()
			t.Cleanup(sessions.Reset)
			mux := http.NewServeMux()
			sessionManageRoutes(mux, folderOnly{})
			call := func(method, path, body string) []byte {
				t.Helper()
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
				if w.Code != 200 {
					t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
				}
				return w.Body.Bytes()
			}
			of := func(r readsJSON, agent string) readAgentJSON {
				t.Helper()
				for _, f := range r.Found {
					if f.Agent == agent {
						return f
					}
				}
				t.Fatalf("%s not found: %+v", agent, r.Found)
				return readAgentJSON{}
			}
			listed := func() []string {
				t.Helper()
				var m struct {
					Sessions []struct {
						ID    string `json:"id"`
						Agent string `json:"agent"`
					} `json:"sessions"`
				}
				json.Unmarshal(call("GET", "/api/sessions/manage?agent=antigravity", ""), &m)
				// with none of its own the page is on another agent's (agy's)
				var ids []string
				for _, s := range m.Sessions {
					if s.Agent == "antigravity" {
						ids = append(ids, s.ID)
					}
				}
				return ids
			}
			var r readsJSON
			json.Unmarshal(call("GET", "/api/sessions/reads", ""), &r)
			if a := of(r, "antigravity"); a.On || !a.New {
				t.Fatalf("antigravity found after allow selected = %+v, want off and new", a)
			}
			if a := of(r, "agy"); !a.On || a.New {
				t.Fatalf("agy, named on = %+v", a)
			}
			if ids := listed(); len(ids) != 0 {
				t.Fatalf("antigravity listed before it was allowed: %v", ids)
			}
			body := `{"agents":{"antigravity":false}}`
			if allow {
				body = `{"agents":{"antigravity":true}}`
			}
			// a fresh value: new is left out when false
			r = readsJSON{}
			json.Unmarshal(call("POST", "/api/sessions/reads", body), &r)
			a := of(r, "antigravity")
			if a.New || a.On != allow {
				t.Fatalf("after the answer: %+v", a)
			}
			if s := settings.Shared().SessionReads; s == nil || s.All || !s.Agents["agy"] {
				t.Fatalf("the answer changed the rest of the choice: %+v", s)
			}
			ids := listed()
			if allow && (len(ids) != 1 || ids[0] != "d96c88af-9e92-463f-a81c-9bb0f7007818") {
				t.Fatalf("antigravity allowed: listed %v", ids)
			}
			if !allow && len(ids) != 0 {
				t.Fatalf("antigravity kept off: listed %v", ids)
			}
		})
	}
}

// Only a choice that reads agents by name finds one new: allow all reads
// those found later, and before the choice the consent asks about all.
func TestSessionReadsNewOnlyWhenNamed(t *testing.T) {
	for name, r := range map[string]*settings.SessionReads{
		"not asked":    {},
		"allow all":    {Asked: true, All: true},
		"all, agy off": {Asked: true, All: true, Agents: map[string]bool{"agy": false}},
	} {
		t.Run(name, func(t *testing.T) {
			h := sandboxHome(t)
			copyTree(t, "../sessions/testdata/antigravity-2.30", filepath.Join(h, ".gemini", "antigravity"))
			if err := settings.SetSessionReads(func(*settings.SessionReads) *settings.SessionReads { return r }); err != nil {
				t.Fatal(err)
			}
			for _, f := range readsNow().Found {
				if f.New {
					t.Fatalf("%s new under %+v", f.Agent, r)
				}
			}
		})
	}
}
