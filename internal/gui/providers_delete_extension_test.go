package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestProvidersDeleteExtensionKeepsAccountsAndPackage(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	directory := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-mod=readonly", "-o", filepath.Join(directory, entry), "../provider/testdata/extensionkeepaliveworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build SDK lifecycle fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "bulk-delete.fixture", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth}},
	}
	for _, id := range []string{"selected", "sibling"} {
		manifest.Providers = append(manifest.Providers, sdk.ProviderDescriptor{
			ID: id, Name: "Bulk " + id, Protocols: []protocolext.Protocol{protocolext.Chat},
			Models: []sdk.Model{{ID: "fixture-chat", Name: "Fixture chat"}},
		})
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(plugin.Settle)
	installed, err := manager.Install(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	var accounts []extension.Account
	for _, account := range []extension.Account{
		{Provider: "selected", ID: "first", Auth: json.RawMessage(`{"token":"synthetic-first"}`)},
		{Provider: "selected", ID: "second", Auth: json.RawMessage(`{"token":"synthetic-second"}`)},
		{Provider: "sibling", ID: "other", Auth: json.RawMessage(`{"token":"synthetic-other"}`)},
	} {
		account.Plugin = manifest.ID
		account.ExpiresAt = time.Now().Add(24 * time.Hour).UnixMilli()
		saved, err := manager.Store().SaveAccount(account, 0)
		if err != nil {
			t.Fatal(err)
		}
		if saved.ID == "first" {
			saved.Auth = json.RawMessage(`{"token":"synthetic-replaced"}`)
			saved, err = manager.Store().SaveAccount(saved, saved.Generation)
			if err != nil {
				t.Fatal(err)
			}
		}
		accounts = append(accounts, saved)
	}
	for _, id := range []string{"raw-delete", "raw-keep"} {
		if err := provider.Save(provider.Provider{ID: id, Name: id, Key: "synthetic-key",
			Chat: "http://127.0.0.1:1/v1", Models: []string{"fixture-chat"}}); err != nil {
			t.Fatal(err)
		}
	}
	selected := provider.ExtensionProviderID(manifest.ID, "selected")
	sibling := provider.ExtensionProviderID(manifest.ID, "sibling")
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	request := func(method, path, body string, status int) providersJSON {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(body)))
		if response.Code != status {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.Code, status, response.Body.String())
		}
		var state providersJSON
		if status == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
				t.Fatal(err)
			}
		}
		return state
	}
	listed := func(state providersJSON, id string) bool {
		return slices.ContainsFunc(state.Providers, func(item providerJSON) bool { return item.ID == id })
	}
	before := request(http.MethodGet, "/api/providers", "", http.StatusOK)
	for _, id := range []string{selected, sibling, "raw-delete", "raw-keep"} {
		if !listed(before, id) {
			t.Fatalf("ordinary route did not list fixture provider %q before deletion", id)
		}
	}
	body, err := json.Marshal(map[string][]string{"ids": {selected, "raw-delete", "missing", selected}})
	if err != nil {
		t.Fatal(err)
	}
	after := request(http.MethodPost, "/api/providers/delete", string(body), http.StatusOK)
	if len(after.Unremoved) != 1 || after.Unremoved[0].ID != "missing" || after.Unremoved[0].Error == "" {
		t.Fatalf("partial deletion lost the missing-provider result: %+v", after.Unremoved)
	}
	for _, state := range []providersJSON{after, request(http.MethodGet, "/api/providers", "", http.StatusOK)} {
		if listed(state, selected) || listed(state, "raw-delete") || !listed(state, sibling) || !listed(state, "raw-keep") {
			t.Fatal("bulk route did not hide only the selected SDK provider and remove only the selected raw provider")
		}
	}
	if slices.ContainsFunc(provider.All(), func(item provider.Provider) bool { return item.ID == selected }) {
		t.Fatal("deleted SDK provider is still available to ordinary provider selection")
	}
	if !slices.ContainsFunc(provider.Hidden(), func(item provider.Provider) bool { return item.ID == selected }) {
		t.Fatal("SDK provider was removed instead of being retained as hidden")
	}
	stored, err := os.ReadFile(provider.Path())
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Deleted []struct{ ID string }
	}
	if err := json.Unmarshal(stored, &saved); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(saved.Deleted, func(item struct{ ID string }) bool { return item.ID == "raw-delete" }) ||
		slices.ContainsFunc(saved.Deleted, func(item struct{ ID string }) bool { return item.ID == selected }) {
		t.Fatal("raw deletion archive and hidden SDK provider lifecycle were conflated")
	}
	refused := httptest.NewRecorder()
	mux.ServeHTTP(refused, httptest.NewRequest(http.MethodPost, "/api/providers/delete", strings.NewReader(`{"ids":["missing"]}`)))
	if refused.Code == http.StatusOK {
		t.Fatal("bulk deletion reported success when no provider could be removed")
	}
	remaining := request(http.MethodGet, "/api/providers", "", http.StatusOK)
	if listed(remaining, selected) || !listed(remaining, sibling) || !listed(remaining, "raw-keep") {
		t.Fatal("failed bulk deletion changed the remaining provider selection")
	}
	for _, before := range accounts {
		after, err := manager.Store().GetAccount(before.Plugin, before.Provider, before.ID)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("bulk deletion changed SDK account credentials, expiry or generation for %s/%s: %v", before.Provider, before.ID, err)
		}
	}
	for _, id := range []string{"selected", "sibling"} {
		after, err := manager.Store().Accounts(manifest.ID, id)
		want := 1
		if id == "selected" {
			want = 2
		}
		if err != nil || len(after) != want {
			t.Fatalf("bulk deletion changed saved SDK accounts for %s: count=%d, want=%d, err=%v", id, len(after), want, err)
		}
	}
	packages, err := manager.List()
	if err != nil || len(packages) != 1 || packages[0].Disabled || packages[0].Revision != installed.Revision {
		t.Fatalf("bulk provider deletion disabled, removed or replaced its installed SDK package: %v", err)
	}
	if _, err := manager.Manifest(manifest.ID); err != nil {
		t.Fatal("bulk provider deletion made its package unavailable:", err)
	}
}
