package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func guiOptionsFixture(test *testing.T) (*extension.Manager, extension.Installed) {
	test.Helper()
	manager := extension.NewManager(test.TempDir())
	test.Cleanup(func() { _ = manager.Close(context.Background()) })
	catalog := extension.NewCatalog(manager.Namespace())
	source := test.TempDir()
	body := `{"schema":1,"id":"options.fixture","version":"1.0.0","runtime":"exec","entry":"worker",
		"contract":{"version":{"major":1,"minor":9},"capabilities":["outbound-executor"]},
		"providers":[{"id":"vendor","name":"Vendor","protocols":["chat"],
		"options":[{"key":"paid","label":"Paid","confirm":"Uses account credits"}]}]}`
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), []byte(body), 0o600); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "worker"), []byte("must never execute"), 0o700); err != nil {
		test.Fatal(err)
	}
	entry, err := catalog.Install(test.Context(), source, func(context.Context, extension.Installed) error { return nil })
	if err != nil {
		test.Fatal(err)
	}
	return manager, entry
}

func TestExtensionProviderOptionsOrdinaryAPI(test *testing.T) {
	manager, entry := guiOptionsFixture(test)
	previous := extensionProviderOptions
	extensionProviderOptions = func() extensionOptionHost { return manager }
	test.Cleanup(func() { extensionProviderOptions = previous })
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	post := func(value any) *httptest.ResponseRecorder {
		test.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			test.Fatal(err)
		}
		reply := httptest.NewRecorder()
		mux.ServeHTTP(reply, httptest.NewRequest(http.MethodPost, "/api/provider/extension-options", bytes.NewReader(body)))
		return reply
	}
	update := map[string]any{"id": provider.ExtensionProviderID(entry.Manifest.ID, "vendor"),
		"module": entry.Manifest.ID, "provider": "vendor", "packageRevision": entry.Revision,
		"expected": "0", "values": map[string]bool{"paid": true}}
	if reply := post(update); reply.Code == http.StatusOK {
		test.Fatal("ordinary API enabled a confirmed switch without acknowledgement")
	}
	update["confirmed"] = []string{"paid"}
	if reply := post(update); reply.Code != http.StatusOK {
		test.Fatalf("ordinary API refused checked switch save: %d %s", reply.Code, reply.Body)
	}
	if reply := post(update); reply.Code == http.StatusOK {
		test.Fatal("ordinary API allowed a stale configuration write")
	}
	update["expected"] = "1"
	for _, invalid := range []any{nil, "true", 1} {
		update["values"] = map[string]any{"paid": invalid}
		if reply := post(update); reply.Code == http.StatusOK {
			test.Fatalf("ordinary API accepted a nonboolean switch: %v", invalid)
		}
	}
	update["values"], update["id"] = map[string]bool{"paid": false}, "unrelated"
	if reply := post(update); reply.Code == http.StatusOK {
		test.Fatal("ordinary API changed another provider's option owner")
	}
	snapshot, err := manager.ProviderOptions(test.Context(), entry.Manifest.ID, "vendor")
	if err != nil || snapshot.ConfigVersion != "1" || !snapshot.Options[0].On {
		test.Fatalf("rejected edits changed configuration: %+v %v", snapshot, err)
	}
}

func TestExtensionProviderOptionsResponseKeepsUnrelatedConfigPrivate(test *testing.T) {
	manager, entry := guiOptionsFixture(test)
	const sentinel = "private-root-config-must-stay-in-store"
	if _, err := manager.CompareAndSwapModuleConfig(test.Context(), entry.Manifest.ID, "0",
		json.RawMessage(`{"privateToken":"`+sentinel+`"}`)); err != nil {
		test.Fatal(err)
	}
	previous := extensionProviderOptions
	extensionProviderOptions = func() extensionOptionHost { return manager }
	test.Cleanup(func() { extensionProviderOptions = previous })
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	body, err := json.Marshal(map[string]any{
		"id":     provider.ExtensionProviderID(entry.Manifest.ID, "vendor"),
		"module": entry.Manifest.ID, "provider": "vendor", "packageRevision": entry.Revision,
		"expected": "1", "values": map[string]bool{"paid": true}, "confirmed": []string{"paid"},
	})
	if err != nil {
		test.Fatal(err)
	}
	reply := httptest.NewRecorder()
	mux.ServeHTTP(reply, httptest.NewRequest(http.MethodPost, "/api/provider/extension-options", bytes.NewReader(body)))
	if reply.Code != http.StatusOK {
		test.Fatalf("ordinary options save failed: %d %s", reply.Code, reply.Body)
	}
	var receipt map[string]json.RawMessage
	if err := json.Unmarshal(reply.Body.Bytes(), &receipt); err != nil {
		test.Fatal(err)
	}
	if len(receipt) != 1 || string(receipt["version"]) != `"2"` {
		test.Fatalf("provider options response must contain only its CAS version: %s", reply.Body)
	}
	if bytes.Contains(reply.Body.Bytes(), []byte(sentinel)) {
		test.Fatal("provider options response exposed unrelated private root configuration")
	}
	config, err := manager.ModuleConfig(test.Context(), entry.Manifest.ID)
	if err != nil || config.Version != "2" || !bytes.Contains(config.Values, []byte(sentinel)) ||
		!bytes.Contains(config.Values, []byte(`"paid":true`)) {
		test.Fatalf("saving provider options lost unrelated private configuration: %s %v", config.Values, err)
	}
}
