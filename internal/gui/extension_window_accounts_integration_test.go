//go:build cgo && (darwin || linux || freebsd || windows)

package gui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	hybrid "github.com/yetone/magpie/examples/extensions/hybrid-go"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionWindowSelectedAccountNativeHybrid(test *testing.T) {
	distribution := windowHybridBuildMode(test, true)
	pagePlugins(test)
	ctx, cancel := context.WithTimeout(test.Context(), 90*time.Second)
	defer cancel()
	manager := extension.NewManager(test.TempDir())
	manager.Bun = filepath.Join(test.TempDir(), "absent-bun")
	test.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := manager.Close(closeCtx); err != nil {
			test.Error(err)
		}
	})
	previous := extensionWindows
	extensionWindows = func() extensionWindowHost { return manager }
	test.Cleanup(func() { extensionWindows = previous })
	entry, err := manager.Install(ctx, distribution)
	if err != nil {
		test.Fatal(err)
	}
	credentials := map[string]json.RawMessage{}
	for _, id := range []string{"account-a", "account-b"} {
		credentials[id] = json.RawMessage(`{"subject":"` + id + `","secret":"DO-NOT-SEND-TO-BROWSER"}`)
		if _, err := manager.Store().SaveAccount(extension.Account{Plugin: entry.Manifest.ID, Provider: "hybrid-fixture", ID: id,
			Auth: credentials[id]}, 0); err != nil {
			test.Fatal(err)
		}
	}
	handler := pageMux()
	post := func(path string, value any) *httptest.ResponseRecorder {
		test.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			test.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)).WithContext(ctx)
		result := do(handler, request)
		if bytes.Contains(result.Body.Bytes(), []byte("DO-NOT-SEND-TO-BROWSER")) {
			test.Fatal("selected account credentials reached the page response")
		}
		return result
	}
	readPages := do(handler, httptest.NewRequest(http.MethodGet, "/api/plugin-pages", nil).WithContext(ctx))
	var pages []pluginPage
	windowHybridDecode(test, readPages, &pages)
	var page pluginPage
	for _, candidate := range pages {
		if candidate.Name == entry.Manifest.ID {
			page = candidate
		}
	}
	if page.Ref == "" {
		test.Fatal("selected-account hybrid page was not discovered")
	}
	var summaries []sdk.AccountSummary
	windowHybridDecode(test, post("/api/plugin-pages/accounts", map[string]string{"ref": page.Ref, "binding": "page.pid"}), &summaries)
	if len(summaries) != 2 || manager.Running(entry.Manifest.ID) {
		test.Fatal("safe account discovery started the native module or lost accounts")
	}
	call := func(binding, request, account string, input any) *httptest.ResponseRecorder {
		return post("/api/plugin-pages/call", map[string]any{
			"ref": page.Ref, "binding": binding, "request": request, "input": input,
			"account": sdk.AccountSelection{ID: account, ExpectedGeneration: "1"},
		})
	}
	for _, account := range []string{"account-a", "account-b"} {
		var primary, backend hybrid.Identity
		input := map[string]string{"text": "selected", "provider": "foreign", "account": "not-the-selected-account"}
		windowHybridDecode(test, call("page.pid", "", account, input), &primary)
		windowHybridDecode(test, call("page.backend", "", account, input), &backend)
		digest := sha256.Sum256(credentials[account])
		if primary.PID != os.Getpid() || backend.PID <= 0 || backend.PID == primary.PID ||
			primary.Account != account || backend.Account != account || primary.Generation != 1 || backend.Generation != 1 ||
			primary.ConfigVersion != backend.ConfigVersion || primary.AuthFingerprint != hex.EncodeToString(digest[:]) ||
			backend.AuthFingerprint != primary.AuthFingerprint {
			test.Fatalf("native/service selected scope changed: %+v %+v", primary, backend)
		}
	}
	var forwarded hybrid.ForwardResult
	windowHybridDecode(test, call("page.forward", "frame:write-one", "account-a", map[string]string{"text": "once"}), &forwarded)
	if forwarded.Primary.Account != "account-a" || forwarded.Backend.Account != "account-a" ||
		forwarded.Primary.Generation != forwarded.Backend.Generation ||
		forwarded.Primary.AuthFingerprint != forwarded.Backend.AuthFingerprint {
		test.Fatal("private helper did not inherit the exact selected account")
	}
	for _, duplicate := range []struct{ account, text string }{{"account-a", "once"}, {"account-a", "changed"}, {"account-b", "once"}} {
		if result := call("page.forward", "frame:write-one", duplicate.account, map[string]string{"text": duplicate.text}); result.Code == 200 {
			test.Fatal("selected write intent was replayed with changed authority/input")
		}
	}
	state, err := manager.Store().StateGet(sdk.Scope{Plugin: entry.Manifest.ID, Provider: "hybrid-fixture", Account: "account-a", Generation: 1}, "hybrid.last")
	if err != nil || state.Version != 1 || !bytes.Contains(state.Value, []byte("once")) {
		test.Fatal("accepted native/helper effect was repeated or lost:", state, err)
	}
	for _, value := range []map[string]any{
		{"ref": page.Ref, "binding": "page.pid", "input": map[string]any{"account": "account-a", "auth": "forged"}},
		{"ref": page.Ref, "binding": "page.pid", "input": map[string]any{}, "account": sdk.AccountSelection{ID: "foreign", ExpectedGeneration: "1"}},
		{"ref": page.Ref, "binding": "page.pid", "input": map[string]any{}, "account": sdk.AccountSelection{ID: "account-a", ExpectedGeneration: "2"}},
	} {
		if result := post("/api/plugin-pages/call", value); result.Code == 200 {
			test.Fatal("an ungranted selection reached a native action")
		}
	}
	if _, err := manager.Store().SaveAccount(extension.Account{Plugin: entry.Manifest.ID, Provider: "hybrid-fixture", ID: "account-b",
		Auth: json.RawMessage(`{"subject":"replacement","secret":"DO-NOT-SEND-TO-BROWSER"}`)}, 1); err != nil {
		test.Fatal(err)
	}
	if result := call("page.pid", "", "account-b", map[string]any{}); result.Code == 200 {
		test.Fatal("UI silently adopted a replacement account generation")
	}
	if err := manager.SetEnabled(ctx, entry.Manifest.ID, false); err != nil {
		test.Fatal(err)
	}
	if result := post("/api/plugin-pages/accounts", map[string]string{"ref": page.Ref, "binding": "page.pid"}); result.Code == 200 {
		test.Fatal("disabled window retained account discovery authority")
	}
}
