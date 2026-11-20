package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

const sdkSearchVendor = "sdk:search.fixture:specstream"

func buildSDKSearch(test *testing.T) string {
	test.Helper()
	directory := test.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(test.Context(), 2*time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-o",
		filepath.Join(directory, entry), "../extension/testdata/searchworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		test.Fatalf("build search fixture: %v\n%s", err, output)
	}
	return directory
}

func installSDKSearch(test *testing.T, manager *extension.Manager, directory, version string, authenticated bool) {
	test.Helper()
	installSDKSearchDeclaration(test, manager, directory, version, authenticated, false)
}

func installSDKSearchDeclaration(test *testing.T, manager *extension.Manager, directory, version string, authenticated, registry bool) {
	test.Helper()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "search.fixture", Version: version, Runtime: "exec", Entry: entry,
		Contract:      sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
		Actions:       []sdk.ActionDescriptor{{ID: "search-read", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		SearchSources: []sdk.SearchSourceDescriptor{{ID: "specstream", Name: "SpecStream fixture", Action: "search-read"}}}
	if authenticated {
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.OutboundExecutor, sdk.AccountAuth)
		manifest.Providers = []sdk.ProviderDescriptor{{ID: "vendor", Name: "Search account", Protocols: []protocolext.Protocol{"chat"}}}
		manifest.SearchSources[0].Provider = "vendor"
	}
	if registry {
		manifest.Contract.Version.Minor = 16
		manifest.Contributions = []sdk.ContributionDescriptor{{
			Point: sdk.ContributionSearch, Version: 1, Search: &manifest.SearchSources[0],
		}}
		manifest.SearchSources = nil
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		test.Fatal(err)
	}
	if _, err := manager.Install(test.Context(), directory); err != nil {
		test.Fatal(err)
	}
}

func configureSDKSearch(test *testing.T, manager *extension.Manager, options map[string]any) {
	test.Helper()
	current, err := manager.ModuleConfig(test.Context(), "search.fixture")
	if err != nil {
		test.Fatal(err)
	}
	raw, _ := json.Marshal(options)
	if _, err := manager.CompareAndSwapModuleConfig(test.Context(), "search.fixture", current.Version, raw); err != nil {
		test.Fatal(err)
	}
}

func setupSDKSearch(test *testing.T, directory string, authenticated bool) *extension.Manager {
	test.Helper()
	return setupSDKSearchDeclaration(test, directory, authenticated, false)
}

func setupSDKSearchDeclaration(test *testing.T, directory string, authenticated, registry bool) *extension.Manager {
	test.Helper()
	fresh(test)
	forgetSearchKeys()
	test.Cleanup(forgetSearchKeys)
	test.Setenv("MAGPIE_BUN", filepath.Join(test.TempDir(), "absent"))
	test.Setenv("MAGPIE_NO_ACCOUNTS", "")
	test.Setenv("MAGPIE_ADDR", "")
	manager := extension.Default()
	test.Cleanup(func() { closeSDKGatewayHook(test, manager) })
	installSDKSearchDeclaration(test, manager, directory, "1.0.0", authenticated, registry)
	if authenticated {
		saveSDKSearchAccount(test, manager, "alice", "", 0, 0)
		saveSDKSearchAccount(test, manager, "bob", "", 0, 0)
		provider.Changed()
	}
	if err := provider.SetSearchAPI(provider.SearchAPI{Vendor: sdkSearchVendor}); err != nil {
		test.Fatal(err)
	}
	return manager
}

func saveSDKSearchAccount(test *testing.T, manager *extension.Manager, id, mode string, expires int64, expected uint64) {
	test.Helper()
	auth, _ := json.Marshal(map[string]string{"token": "PRIVATE-" + id, "mode": mode})
	if _, err := manager.Store().SaveAccount(extension.Account{Plugin: "search.fixture", Provider: "vendor",
		ID: id, Auth: auth, ExpiresAt: expires}, expected); err != nil {
		test.Fatal(err)
	}
	provider.Changed()
}

type sdkSearchTrace struct {
	Version string            `json:"version"`
	Request sdk.SearchRequest `json:"request"`
	Scope   sdk.Scope         `json:"scope"`
	Proxy   string            `json:"proxy"`
	HasAuth bool              `json:"hasAuth"`
}

func readSDKSearchTrace(test *testing.T, path string) []sdkSearchTrace {
	test.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE-") || strings.Contains(string(raw), `"credentials"`) || strings.Contains(string(raw), `"key"`) {
		test.Fatal("search DTO or trace exposed credentials")
	}
	var entries []sdkSearchTrace
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	for {
		var entry sdkSearchTrace
		if err := decoder.Decode(&entry); err != nil {
			if err != io.EOF {
				test.Fatal(err)
			}
			break
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestSDKSearchOrdinaryFrontAndLedger(test *testing.T) {
	directory := buildSDKSearch(test)
	test.Run("legacy", func(test *testing.T) { sdkSearchOrdinaryFrontAndLedger(test, directory, false) })
	test.Run("registry", func(test *testing.T) { sdkSearchOrdinaryFrontAndLedger(test, directory, true) })
}

func TestSDKSearchDiscoveryRespectsRequestDeadline(test *testing.T) {
	manager := setupSDKSearchDeclaration(test, buildSDKSearch(test), false, true)
	if err := settings.Save(settings.Settings{Searcher: "off"}); err != nil {
		test.Fatal(err)
	}
	server := New()
	lock, err := os.OpenFile(filepath.Join(manager.Namespace(), "catalog.lock"), os.O_RDWR, 0600)
	if err != nil {
		test.Fatal(err)
	}
	defer lock.Close()
	for name, search := range map[string]func(context.Context, string) (string, []Hit, error){
		"web": server.webSearch,
		"api": server.apiSearch,
	} {
		test.Run(name, func(test *testing.T) {
			if held, err := filelock.TryLock(lock); err != nil || !held {
				test.Fatalf("hold SDK catalog: %t, %v", held, err)
			}
			ctx, cancel := context.WithTimeout(test.Context(), 100*time.Millisecond)
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, _, err := search(ctx, "query")
				finished <- err
			}()
			select {
			case err := <-finished:
				filelock.Unlock(lock)
				if !errors.Is(err, context.DeadlineExceeded) {
					test.Fatalf("search returned %v under the locked catalog", err)
				}
			case <-time.After(2 * time.Second):
				filelock.Unlock(lock)
				<-finished
				test.Fatal("search discovery was still waiting for the SDK catalog after the deadline")
			}
		})
	}
}

func TestSDKSearchDispatchKeepsTheSelectedRevision(test *testing.T) {
	directory := buildSDKSearch(test)
	manager := setupSDKSearchDeclaration(test, directory, false, true)
	selected, found := provider.SearchVendorFor(test.Context(), sdkSearchVendor)
	if !found {
		test.Fatal("source not selected")
	}
	peer := extension.NewManager(manager.Namespace())
	test.Cleanup(func() { closeSDKGatewayHook(test, peer) })
	installSDKSearchDeclaration(test, peer, directory, "2.0.0", false, true)
	trace := filepath.Join(test.TempDir(), "trace")
	configureSDKSearch(test, peer, map[string]any{"trace": trace})
	if _, err := provider.RunExtensionSearch(test.Context(), selected, nil, "stale", 2); err == nil {
		test.Error("a stale selection dispatched to the replacement module")
	}
	if entries := readSDKSearchTrace(test, trace); len(entries) != 0 {
		test.Errorf("replacement received a stale query: %+v", entries)
	}
	_, hits, err := New().apiSearch(test.Context(), "current")
	if err != nil || len(hits) == 0 {
		test.Fatalf("fresh search did not use the replacement: %v, %v", hits, err)
	}
	if entries := readSDKSearchTrace(test, trace); len(entries) != 1 || entries[0].Version != "2.0.0" ||
		entries[0].Request.Query != "current" {
		test.Fatalf("unexpected dispatches to replacement: %+v", entries)
	}
}

func sdkSearchOrdinaryFrontAndLedger(test *testing.T, directory string, registry bool) {
	manager := setupSDKSearchDeclaration(test, directory, false, registry)
	trace := filepath.Join(test.TempDir(), "trace")
	configureSDKSearch(test, manager, map[string]any{"trace": trace, "mode": "state-write"})
	vendor, found := provider.SearchVendorOf(sdkSearchVendor)
	if !found || vendor.WantsURL() || !vendor.KeyOptional || vendor.Plugin != "" || manager.Running("search.fixture") {
		test.Fatal("search metadata started a runtime or borrowed legacy tool authority")
	}
	for _, api := range []provider.SearchAPI{
		{Vendor: sdkSearchVendor, Key: "must-not-be-sent"}, {Vendor: sdkSearchVendor, URL: "https://example.invalid"},
	} {
		if err := provider.SetSearchAPI(api); err == nil {
			test.Fatal("source accepted an undeclared key or address override")
		}
	}
	if err := settings.Save(settings.Settings{RedactPersonal: true}); err != nil {
		test.Fatal(err)
	}
	server := New()
	reply := readMCP(test, mcpPost(server.Front(), "127.0.0.1:50000",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"phone 13812345678","count":2}}}`))
	raw, _ := json.Marshal(reply)
	if reply.Error != nil || reply.Result.IsError || !strings.Contains(string(raw), "SpecStream fixture") ||
		!strings.Contains(string(raw), "ordinary search 1.0.0") {
		test.Fatalf("ordinary Front did not use SDK search source: %s", raw)
	}
	entries := readSDKSearchTrace(test, trace)
	if len(entries) != 1 || entries[0].HasAuth || entries[0].Scope.Provider != "" || entries[0].Request.Count != 2 ||
		!strings.Contains(entries[0].Request.Query, "{{PHONE_") || strings.Contains(entries[0].Request.Query, "13812345678") {
		test.Fatalf("ordinary source projection changed: %+v", entries)
	}
	state, err := manager.Store().StateGet(sdk.Scope{Plugin: "search.fixture"}, "search.must-not-write")
	if err != nil || state.Version != 0 {
		test.Fatal("search read action wrote host state:", err)
	}
	rows, _ := searchRows(test)
	if len(rows) != 1 || rows[0].Source != usage.SourceSearch || rows[0].Status != 200 || rows[0].Requested != "SpecStream fixture" {
		test.Fatalf("ordinary search ledger did not record exactly one ask: %+v", rows)
	}
	if err := provider.SwitchSearchAPI(sdkSearchVendor, false); err != nil {
		test.Fatal(err)
	}
	server.apiSearch(test.Context(), "off")
	if len(readSDKSearchTrace(test, trace)) != 1 {
		test.Fatal("disabled search selection still dispatched")
	}
}

func TestSDKSearchAccountOrderRestsPreflightAndProxy(test *testing.T) {
	directory := buildSDKSearch(test)
	manager := setupSDKSearch(test, directory, true)
	trace := filepath.Join(test.TempDir(), "trace")
	var lock sync.Mutex
	var upstream []string
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		lock.Lock()
		upstream = append(upstream, request.URL.String()+" "+request.Header.Get("Authorization"))
		lock.Unlock()
		io.WriteString(writer, "fixture response")
	}))
	defer proxy.Close()
	configureSDKSearch(test, manager, map[string]any{"trace": trace, "endpoint": "http://sdk-search.invalid/search"})
	accounts, err := provider.ExtensionProviders()
	if err != nil || len(accounts) != 2 {
		test.Fatalf("search accounts: %+v %v", accounts, err)
	}
	selected := accounts[0]
	selected.Proxy = "direct"
	selected.AccountProxies = map[string]string{"alice": proxy.URL, "bob": proxy.URL}
	if err := provider.Save(selected); err != nil {
		test.Fatal(err)
	}
	if err := provider.SetAccountOrder(selected.ID, []string{"alice", "bob"}); err != nil {
		test.Fatal(err)
	}
	saveSDKSearchAccount(test, manager, "alice", "refused", 0, 1)
	server := New()
	ctx := netproxy.With(test.Context(), "http://foreign-provider.invalid:1")
	if _, hits, err := server.apiSearch(ctx, "first query"); err != nil || len(hits) != 1 {
		test.Fatalf("ordinary account refusal did not try next: %+v %v", hits, err)
	}
	if _, _, err := server.apiSearch(ctx, "second query"); err != nil {
		test.Fatal(err)
	}
	entries := readSDKSearchTrace(test, trace)
	if len(entries) != 3 || entries[0].Scope.Account != "alice" || entries[1].Scope.Account != "bob" || entries[2].Scope.Account != "bob" {
		test.Fatalf("search ignored account order or refusal rests: %+v", entries)
	}
	for _, entry := range entries {
		if !entry.HasAuth || entry.Proxy != proxy.URL || entry.Request.Proxy != proxy.URL ||
			entry.Scope.Plugin != "search.fixture" || entry.Scope.Provider != "vendor" {
			test.Fatalf("search borrowed outer provider credentials/proxy: %+v", entry)
		}
	}
	lock.Lock()
	gotUpstream := append([]string(nil), upstream...)
	lock.Unlock()
	if len(gotUpstream) != 2 || gotUpstream[0] != "http://sdk-search.invalid/search Bearer PRIVATE-bob" {
		test.Fatalf("actual loopback proxy did not use selected account: %v", gotUpstream)
	}
	rows, _ := searchRows(test)
	if len(rows) != 3 || rows[0].Status != 429 || rows[1].Status != 200 || rows[2].Status != 200 {
		test.Fatalf("account attempts lost their ordinary ledger rows: %+v", rows)
	}
	for _, row := range rows {
		if strings.Contains(row.Error, "PRIVATE-") || strings.Contains(row.Error, "first query") {
			test.Fatal("search refusal exposed token or query")
		}
	}
	if why, _ := SearchKeyRest(sdkSearchVendor, "account alice"); why == "" {
		test.Fatal("refused account did not rest")
	}
	if why, _ := SearchKeyRest(sdkSearchVendor, "account bob"); why != "" {
		test.Fatal("successful account inherited another account's rest")
	}
	stale := accounts[0]
	vendor, _ := provider.SearchVendorOf(sdkSearchVendor)
	before := len(entries)
	if _, err := provider.RunExtensionSearch(ctx, vendor, &stale, "stale", 2); err == nil {
		test.Fatal("manual account replacement borrowed the new generation")
	}
	held := netproxy.Hold(ctx, errors.New("search is not admitted"))
	if _, _, err := server.apiSearch(held, "held"); err == nil {
		test.Fatal("held search dispatched")
	}
	if len(readSDKSearchTrace(test, trace)) != before {
		test.Fatal("stale or held account reached the module")
	}
	saveSDKSearchAccount(test, manager, "alice", "", time.Now().Add(-time.Minute).UnixMilli(), 2)
	forgetSearchKeys()
	if err := provider.SetLoginOn(selected.ID, "bob", false); err != nil {
		test.Fatal(err)
	}
	if _, _, err := server.apiSearch(ctx, "refresh"); err != nil {
		test.Fatal("transactional preflight failed:", err)
	}
	account, err := manager.Store().GetAccount("search.fixture", "vendor", "alice")
	if err != nil || account.Generation != 4 || !strings.Contains(string(account.Auth), "PRIVATE-REFRESHED") {
		test.Fatal("search skipped transactional credential refresh:", err)
	}
	saveSDKSearchAccount(test, manager, "alice", "refresh-failed", time.Now().Add(-time.Minute).UnixMilli(), 4)
	before = len(readSDKSearchTrace(test, trace))
	if _, _, err := server.apiSearch(ctx, "failed refresh"); err == nil {
		test.Fatal("failed preflight proceeded to search")
	}
	if len(readSDKSearchTrace(test, trace)) != before {
		test.Fatal("failed preflight dispatched source action")
	}
}

func TestSDKSearchFailureDoesNotRotateAccounts(test *testing.T) {
	directory := buildSDKSearch(test)
	for _, mode := range []string{"error", "invalid"} {
		test.Run(mode, func(test *testing.T) {
			manager := setupSDKSearch(test, directory, true)
			trace := filepath.Join(test.TempDir(), "trace")
			configureSDKSearch(test, manager, map[string]any{"trace": trace})
			saveSDKSearchAccount(test, manager, "alice", mode, 0, 1)
			id := provider.ExtensionProviderID("search.fixture", "vendor")
			if err := provider.SetAccountOrder(id, []string{"alice", "bob"}); err != nil {
				test.Fatal(err)
			}
			_, _, err := New().apiSearch(test.Context(), "private query")
			if err == nil || strings.Contains(err.Error(), "PRIVATE-") {
				test.Fatal("source failure was accepted or leaked credentials:", err)
			}
			if entries := readSDKSearchTrace(test, trace); len(entries) != 1 || entries[0].Scope.Account != "alice" {
				test.Fatalf("generic failure rotated into another account: %+v", entries)
			}
			if why, _ := SearchKeyRest(sdkSearchVendor, "account alice"); why != "" {
				test.Fatal("generic failure masqueraded as an account refusal")
			}
		})
	}
}

func waitSDKSearchMarker(test *testing.T, path string) {
	test.Helper()
	ctx, cancel := context.WithTimeout(test.Context(), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if raw, _ := os.ReadFile(path); len(raw) != 0 {
			return
		}
		select {
		case <-ctx.Done():
			test.Fatal("search did not reach fixture barrier:", path)
		case <-tick.C:
		}
	}
}

func TestSDKSearchLeaseRejectsRevokedPublication(test *testing.T) {
	directory := buildSDKSearch(test)
	for _, mutation := range []string{"disable", "config", "replacement", "account", "cancel"} {
		test.Run(mutation, func(test *testing.T) {
			manager := setupSDKSearch(test, directory, true)
			started := filepath.Join(test.TempDir(), "started")
			release := filepath.Join(test.TempDir(), "release")
			configureSDKSearch(test, manager, map[string]any{"mode": "hold", "started": started, "release": release})
			binding, found := manager.SearchSource(test.Context(), "search.fixture/specstream")
			if !found {
				test.Fatal("source not discovered")
			}
			scope := sdk.Scope{Plugin: "search.fixture", Provider: "vendor", Account: "alice", Generation: 1}
			request := sdk.SearchRequest{Source: "specstream", Query: "held", Count: 2}
			forged := binding
			forged.Descriptor.Action = "foreign-read"
			if _, err := manager.RunSearch(test.Context(), forged, scope, request); err == nil {
				test.Fatal("forged action acquired search authority")
			}
			foreign := scope
			foreign.Provider = "foreign"
			if _, err := manager.RunSearch(test.Context(), binding, foreign, request); err == nil {
				test.Fatal("foreign provider acquired search authority")
			}
			ctx, cancel := context.WithTimeout(test.Context(), 15*time.Second)
			finished := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := manager.RunSearch(ctx, binding, scope, request)
				finished <- err
			}()
			test.Cleanup(func() {
				cancel()
				_ = os.WriteFile(release, []byte("release"), 0600)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					test.Error("search goroutine outlived cleanup")
				}
			})
			waitSDKSearchMarker(test, started)
			switch mutation {
			case "disable":
				if err := manager.SetEnabled(test.Context(), "search.fixture", false); err != nil {
					test.Fatal(err)
				}
			case "config":
				configureSDKSearch(test, manager, map[string]any{})
			case "replacement":
				installSDKSearch(test, manager, directory, "2.0.0", true)
			case "account":
				saveSDKSearchAccount(test, manager, "alice", "", 0, 1)
			case "cancel":
				cancel()
				waitSDKSearchMarker(test, started+".canceled")
			}
			if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
				test.Fatal(err)
			}
			select {
			case err := <-finished:
				if err == nil {
					test.Fatalf("held search published after %s", mutation)
				}
			case <-time.After(10 * time.Second):
				test.Fatal("revoked search did not complete")
			}
			if mutation != "cancel" {
				if _, err := manager.RunSearch(test.Context(), binding, scope, request); err == nil {
					test.Fatal("stale source/account snapshot rebound to current authority")
				}
			}
		})
	}
}
