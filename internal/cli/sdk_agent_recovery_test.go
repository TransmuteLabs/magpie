package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gui"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const recoveryAgent = "fixture-recovery"

func interruptedSDKAgent(t *testing.T) (path string, original, published []byte) {
	t.Helper()
	home := helpHome(t)
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}
	plugin := "fixture.recovery"
	descriptor := sdk.AgentDescriptor{
		ID: recoveryAgent, Name: "Recovery fixture",
		ConfigFiles: []string{".fixture-recovery.json"},
		Fields:      []sdk.AgentField{{Key: "model", Label: "model"}},
	}
	original = []byte("{\"model\":\"own-model\",\"user\":\"preserved\"}\n")
	published = []byte("{\"model\":\"magpie/relay/new-model\",\"user\":\"preserved\"}\n")
	path = filepath.Join(home, descriptor.ConfigFiles[0])
	if err := os.WriteFile(path, published, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := func(body []byte) []sdk.ActionSettingFile {
		return []sdk.ActionSettingFile{{
			Path: descriptor.ConfigFiles[0], Body: body, BeforeHash: fmt.Sprintf("%x", sha256.Sum256(body)),
			Mode: uint32(info.Mode().Perm()), Exists: true,
		}}
	}
	identity, err := json.Marshal([]string{filepath.Clean(canonical), plugin, recoveryAgent})
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{
		"plugin": plugin, "agent": recoveryAgent, "home": home, "revision": 1,
		"descriptor": descriptor, "phase": "pending", "values": map[string]string{},
		"before": snapshots(original), "applied": snapshots(published), "recoverySafe": true,
	}
	raw, err := json.Marshal(map[string]any{
		"schema":  1,
		"records": map[string]any{fmt.Sprintf("%x", sha256.Sum256(identity)): record},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(provider.Path())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sdk-agent-bindings.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, original, published
}

func checkRecoveryBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("agent configuration = %q, want %q: %v", got, want, err)
	}
}

func TestSDKAgentCLIDefaultRecoversPendingBindingWithFields(t *testing.T) {
	path, original, _ := interruptedSDKAgent(t)
	resolved, err := agent.Find(recoveryAgent)
	if err != nil || resolved == nil || resolved.Native == nil || resolved.Field("model") == nil || resolved.Wired() {
		t.Fatalf("interrupted SDK agent with a model field must remain discoverable and unwired: %v", err)
	}
	if err := runFieldlessCommand(recoveryAgent, "DEFAULT"); err != nil {
		t.Fatalf("default did not recover the interrupted binding: %v", err)
	}
	checkRecoveryBytes(t, path, original)
	if _, err := agent.Find(recoveryAgent); err == nil {
		t.Fatal("finished recovery left a row for the uninstalled module")
	}
}

func TestSDKAgentGUIRecoversPendingBindingWithFields(t *testing.T) {
	path, original, published := interruptedSDKAgent(t)
	handler := gui.Handler(nil, nil)
	call := func(method, path, body string, status int) []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		if rec.Code != status {
			t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, rec.Code, status, rec.Body.Bytes())
		}
		return rec.Body.Bytes()
	}
	var state struct {
		Agents []struct {
			ID     string
			Wired  bool
			Native *struct {
				Provider      string
				NeedsRecovery bool
			}
		}
	}
	if err := json.Unmarshal(call(http.MethodGet, "/api/state", "", 200), &state); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range state.Agents {
		if row.ID != recoveryAgent {
			continue
		}
		if row.Wired || row.Native == nil || row.Native.Provider != "invalid" || !row.Native.NeedsRecovery {
			t.Fatalf("Agents did not distinguish pending recovery from a connection: %+v, native %+v", row, row.Native)
		}
		found = true
	}
	if !found {
		t.Fatal("Agents omitted the interrupted binding")
	}

	var preview struct {
		Revision string
		Error    string
	}
	if err := json.Unmarshal(call(http.MethodGet, "/api/agents/preview/"+recoveryAgent, "", 200), &preview); err != nil ||
		preview.Error != "" || preview.Revision == "" {
		t.Fatalf("Agents did not prepare checked recovery: %+v %v", preview, err)
	}
	checkRecoveryBytes(t, path, published)
	edited := []byte("{\"model\":\"another-model\",\"user\":\"edited after preview\"}\n")
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if refused := call(http.MethodPost, "/api/agents/disconnect/"+recoveryAgent, "{}", 400); !bytes.Contains(refused, []byte(extension.ErrActionChanged.Error())) {
		t.Fatalf("disconnect was not refused by the checked recovery owner: %s", refused)
	}
	checkRecoveryBytes(t, path, edited)

	if err := os.WriteFile(path, published, 0o600); err != nil {
		t.Fatal(err)
	}
	state.Agents = nil
	if err := json.Unmarshal(call(http.MethodPost, "/api/agents/disconnect/"+recoveryAgent, "{}", 200), &state); err != nil {
		t.Fatal(err)
	}
	checkRecoveryBytes(t, path, original)
	for _, row := range state.Agents {
		if row.ID == recoveryAgent {
			t.Fatal("Agents still displayed the uninstalled module after recovery")
		}
	}
}
