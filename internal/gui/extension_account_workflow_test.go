package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionAccountWorkflowThroughParent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("account workflow proof requires actual Node:", err)
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "worker")
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", binary, "./testdata/accountworkflow")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build actual workflow module: %v\n%s", err, output)
	}
	manifest := authfixture.Manifest()
	manifest.Actions = []sdk.ActionDescriptor{
		{ID: "inspect", Effect: "read", InputSchema: json.RawMessage(`{}`)},
		{ID: "root", Effect: "read", InputSchema: json.RawMessage(`{}`)},
	}
	manifest.Bindings = []sdk.BindingDescriptor{
		{ID: "inspect", From: "window:page", Action: "inspect", Provider: "vendor"},
		{ID: "root", From: "window:page", Action: "root"},
	}
	writeManifest := func(value sdk.Manifest, destination string) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string][]byte{
			sdk.ManifestFile: raw,
			"package.json":   []byte(`{"name":"workflow","magpie":{"page":{"title":"Workflow","file":"page.html"}}}`),
			"page.html":      []byte(`<!doctype html><script src="_magpie/bridge.js"></script>`),
		} {
			if err := os.WriteFile(filepath.Join(destination, name), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeManifest(manifest, directory)
	legacyDirectory := t.TempDir()
	worker, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDirectory, "worker"), worker, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := manifest.Clone()
	legacy.ID, legacy.Contract.Version.Minor = "workflow-legacy", 4
	legacy.AuthBindings = nil
	legacy.Providers = nil
	legacy.Contract.Capabilities = []sdk.Capability{sdk.ModuleRPC}
	legacy.Bindings = legacy.Bindings[1:]
	writeManifest(legacy, legacyDirectory)
	source, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	pagePlugins(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager := extension.NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	for _, path := range []string{directory, legacyDirectory} {
		if _, err := manager.Install(t.Context(), path); err != nil {
			t.Fatal(err)
		}
	}
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	started, release := filepath.Join(t.TempDir(), "started"), filepath.Join(t.TempDir(), "release")
	if err := os.WriteFile(release, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(authfixture.Config{Trace: trace, Started: started, Release: release})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
		t.Fatal(err)
	}
	previous := extensionWindows
	extensionWindows = func() extensionWindowHost { return manager }
	t.Cleanup(func() { extensionWindows = previous })
	handler := pageMux()
	server := httptest.NewServer(handler)
	defer server.Close()
	listed := do(handler, httptest.NewRequest(http.MethodGet, "/api/plugin-pages", nil))
	var pages []pluginPage
	if err := json.Unmarshal(listed.Body.Bytes(), &pages); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"origin": server.URL, "source": source, "started": started, "release": release}
	for _, page := range pages {
		if page.Name == manifest.ID {
			payload["page"] = page
		}
		if page.Name == legacy.ID {
			payload["legacy"] = page
		}
	}
	if payload["page"] == nil || payload["legacy"] == nil || manager.Running(manifest.ID) {
		t.Fatal("workflow metadata missing or eagerly started module")
	}
	raw, _ := json.Marshal(payload)
	configuration := filepath.Join(t.TempDir(), "driver.json")
	if err := os.WriteFile(configuration, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	driver := exec.CommandContext(ctx, node, "--experimental-vm-modules",
		filepath.Join(source, "sdk", "js", "test", "account-workflow-http.mjs"), configuration)
	driver.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	if output, err := driver.CombinedOutput(); err != nil {
		t.Fatalf("real parent/native host workflow: %v\n%s", err, output)
	} else {
		t.Log(strings.TrimSpace(string(output)))
	}
	account, err := manager.Store().GetAccount(manifest.ID, "vendor", "chosen")
	if err != nil || account.Generation != 2 || string(account.Auth) != `{"token":"PRIVATE-CREDENTIAL"}` {
		t.Fatal("canceled exchange changed published credential authority:", account.Generation, err)
	}
	accounts, err := manager.Store().Accounts(manifest.ID, "vendor")
	if err != nil || len(accounts) != 1 {
		t.Fatal("workflow invented an account identity:", err)
	}
	body, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var event struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		counts[event.Method]++
	}
	if counts["start"] != 3 || counts["finish"] != 3 {
		t.Fatalf("duplicate start or cancel re-exchanged authorization: %v", counts)
	}
}
