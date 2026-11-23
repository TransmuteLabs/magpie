package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/sessions"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestOMPInstalledAgentsAndSessionSource(t *testing.T) {
	distribution, hasDistribution := os.LookupEnv("MAGPIE_OMP_TEST_DIST")
	fixtures, hasFixtures := os.LookupEnv("MAGPIE_OMP_TEST_FIXTURES")
	binary, hasBinary := os.LookupEnv("MAGPIE_OMP_TEST_CLI")
	if !hasDistribution && !hasFixtures && !hasBinary {
		t.Skip("requires an explicitly built OMP native distribution, synthetic fixtures and same-source CLI")
	}
	if !filepath.IsAbs(distribution) || !filepath.IsAbs(fixtures) || !filepath.IsAbs(binary) {
		t.Fatal("requires absolute paths to the pinned native distribution, JSONL fixtures and same-source CLI")
	}
	home := sandboxHome(t)
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	t.Setenv("MAGPIE_BUN", "")
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
		sessions.Reset()
	})
	previousServer := served.Swap(nil)
	t.Cleanup(func() { served.Store(previousServer) })

	const module = "omp-sessions"
	const spec = "sdk:" + module
	const source = "omp-history"
	const sessionID = "019a0000-0000-7000-8000-00000000000a"
	const transcriptPath = "/api/sessions/transcript?agent=omp&id=" + sessionID
	journal, err := os.ReadFile(filepath.Join(fixtures, "omp-source-usage-v3.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, ".omp", "agent", "config.yml")
	journalPath := filepath.Join(filepath.Dir(configPath), "sessions", "--work-omp--", "time_"+sessionID+".jsonl")
	agentConfig := []byte("modelRoles:\n  default: own/model-a\n")
	for name, data := range map[string][]byte{configPath: agentConfig, journalPath: journal} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handler := Handler(nil, nil)
	call := func(method, path, body string, status int) []byte {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(t.Context())
		handler.ServeHTTP(recorder, request)
		if recorder.Code != status {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, recorder.Code, status, recorder.Body)
		}
		return recorder.Body.Bytes()
	}
	decode := func(body []byte, result any) {
		t.Helper()
		if err := json.Unmarshal(body, result); err != nil {
			t.Fatalf("decode response: %v: %s", err, body)
		}
	}
	cli := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		body, err := command.Output()
		if err != nil {
			t.Fatalf("magpie %v: %v: %s", args, err, stderr.String())
		}
		return body
	}
	setConsent := func(on bool) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"all": false, "agents": map[string]bool{"omp": on}})
		if err != nil {
			t.Fatal(err)
		}
		var reads readsJSON
		decode(call("POST", "/api/sessions/reads", string(body), http.StatusOK), &reads)
		if !reads.Asked || reads.All || !slices.ContainsFunc(reads.Found, func(row readAgentJSON) bool {
			return row.Agent == "omp" && row.On == on
		}) {
			t.Fatalf("OMP consent %v was not published: %+v", on, reads)
		}
	}
	checkAgent := func() {
		t.Helper()
		var state stateJSON
		decode(call("GET", "/api/state", "", http.StatusOK), &state)
		omp := slices.DeleteFunc(state.Agents, func(row agentJSON) bool {
			return row.ID != "omp" && !strings.HasPrefix(row.ID, "omp#") && row.ID != module
		})
		if len(omp) != 1 || omp[0].ID != "omp" || omp[0].Path != tilde(configPath) || omp[0].Wired {
			t.Fatalf("expected one original, unwired OMP Agent row: %+v", omp)
		}
	}
	checkInstalled := func(installed bool) {
		t.Helper()
		var listing pluginsJSON
		decode(call("GET", "/api/plugins", "", http.StatusOK), &listing)
		if listing.Bun || listing.SDKError != "" || listing.Error != "" {
			t.Fatalf("unexpected installed inventory state: Bun=%v SDK=%q plugins=%q", listing.Bun, listing.SDKError, listing.Error)
		}
		var matching []pluginEntryJSON
		for _, row := range listing.Plugins {
			if row.Spec == spec {
				matching = append(matching, row)
			}
		}
		if !installed {
			if len(matching) != 0 {
				t.Fatal("optional OMP module appeared before Add")
			}
			return
		}
		if len(matching) != 1 {
			t.Fatalf("expected one %q installed row, got %d", spec, len(matching))
		}
		row := matching[0]
		info := row.SDK
		capabilities := []sdk.Capability{"module-rpc", "session-migration", "session-related-inputs", "session-source"}
		if row.Version != "0.1.0" || row.Off || row.Error != "" || row.IsAgent || len(row.Providers) != 0 ||
			info == nil || info.Runtime != "native" || info.NativeABI != 1 || info.API != "1.10" || info.HostAPI != "^1.10" ||
			info.Window || len(info.Agents) != 0 || len(info.ProviderIDs) != 0 ||
			!slices.Equal(slices.Sorted(slices.Values(info.Capabilities)), capabilities) ||
			!slices.Equal(info.Sessions, []string{
				"OMP v3 to OMP conversation document", "OMP conversation document to OMP v3",
				"Carry OMP context into Claude Code", "Carry OMP context into Codex",
				"Carry Claude Code context into OMP", "Carry Codex context into OMP", source,
			}) {
			t.Fatalf("unexpected 0.1.0 Installed projection: %+v SDK=%+v", row, info)
		}
	}
	checkHistory := func(owner string, visible bool) {
		t.Helper()
		var listing sessionsJSON
		decode(call("GET", "/api/sessions", "", http.StatusOK), &listing)
		relevant := slices.DeleteFunc(listing.Sessions, func(row sessionJSON) bool {
			return row.Agent != "omp" && row.ID != sessionID
		})
		if !visible {
			if len(relevant) != 0 {
				t.Fatalf("consent revoked but OMP history still listed: %+v", relevant)
			}
			body := call("GET", transcriptPath, "", http.StatusBadRequest)
			if bytes.Contains(body, []byte("Recovered summary")) || !bytes.Contains(body, []byte("no such session")) {
				t.Fatalf("expected no transcript after revocation: %s", body)
			}
			return
		}
		if len(relevant) != 1 || relevant[0].Agent != "omp" || relevant[0].ID != sessionID ||
			relevant[0].SourceStatus != owner || relevant[0].Gateway || !relevant[0].Transcript {
			t.Fatalf("expected one OMP history row with source status %q: %+v", owner, relevant)
		}
		if owner == "ready" && relevant[0].Input != 130 {
			t.Fatalf("delegated OMP JSONL token count=%d, want 130", relevant[0].Input)
		}
		var transcript sessions.Transcript
		decode(call("GET", transcriptPath, "", http.StatusOK), &transcript)
		for _, text := range []string{"Recovered summary", "Working", "Done"} {
			if !slices.ContainsFunc(transcript.Parts, func(part sessions.Part) bool {
				return strings.Contains(part.Text, text)
			}) {
				t.Fatalf("missing OMP transcript text %q: %+v", text, transcript)
			}
		}
	}

	setConsent(true)
	checkInstalled(false)
	checkAgent()
	checkHistory("", true)

	add, err := json.Marshal(map[string]string{"spec": distribution})
	if err != nil {
		t.Fatal(err)
	}
	call("POST", "/api/plugins/add", string(add), http.StatusOK)
	checkInstalled(true)
	checkAgent()
	checkHistory("", true)
	var unbound sessions.SessionSourceStatus
	decode(call("GET", "/api/sessions/sources", "", http.StatusOK), &unbound)
	if len(unbound.Bindings) != 0 || len(unbound.Errors) != 0 || unbound.Pending || manager.Running(module) {
		t.Fatalf("installing the optional module claimed native history or started a worker: %+v", unbound)
	}

	var packageInfo extension.Installed
	decode(cli("plugin", "sdk", "inspect", module), &packageInfo)
	if packageInfo.Revision == "" || packageInfo.Manifest.ID != module {
		t.Fatalf("CLI did not inspect the same Installed module: %+v", packageInfo)
	}
	var config sdk.ConfigSnapshot
	decode(cli("plugin", "sdk", "config", module), &config)
	cli("plugin", "sdk", "config", module,
		`{"sessionSource":{"ownOff":false,"records":[{"sourceId":"omp-history","owned":true,"enabled":true}]}}`,
		"--expected", config.Version)
	cli("sessions", "sources", "bind", module, source, "--expected", packageInfo.Revision)

	checkHistory("ready", true)
	var bound sessions.SessionSourceStatus
	decode(call("GET", "/api/sessions/sources", "", http.StatusOK), &bound)
	if len(bound.Errors) != 0 || bound.Pending || len(bound.Bindings) != 1 ||
		bound.Bindings[0].Plugin != module || bound.Bindings[0].Source != source ||
		bound.Bindings[0].Agent != "omp" || bound.Bindings[0].Identity == "" {
		t.Fatalf("expected exact OMP source binding: %+v", bound)
	}
	checkAgent()
	setConsent(false)
	checkHistory("", false)
	checkInstalled(true)
	checkAgent()
	setConsent(true)
	checkHistory("ready", true)
	for name, before := range map[string][]byte{configPath: agentConfig, journalPath: journal} {
		after, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("acceptance changed %s: %v", name, err)
		}
	}
}
