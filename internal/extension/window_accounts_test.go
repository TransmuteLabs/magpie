package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

type windowAccountFixture struct {
	id, trace, started, release string
}

func (fixture *windowAccountFixture) Load() int32 {
	raw, err := os.ReadFile(fixture.trace)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		return -1
	}
	return int32(bytes.Count(raw, []byte("\n")))
}

func accountWindowFixture(test *testing.T) (*Manager, *windowAccountFixture, string, *windowAccountFixture) {
	test.Helper()
	manager := NewManager(test.TempDir())
	manifest := sdk.Manifest{
		Schema: 1, ID: "account-window", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract:  sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.ModuleRPC, sdk.AccountAuth, sdk.ModelDiscovery}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Vendor", Protocols: []protocolext.Protocol{protocolext.Chat}}},
		Actions: []sdk.ActionDescriptor{
			{ID: "helper.read", Component: "helper", Effect: "read", InputSchema: json.RawMessage(`{}`)},
			{ID: "helper.write", Component: "helper", Effect: "write", InputSchema: json.RawMessage(`{}`)},
		},
		Services: []sdk.ServiceDescriptor{{ID: "helper", Runtime: "exec", Entry: "worker", Args: []string{"--service"},
			Contract: sdk.ServiceContract{ID: "helper", Version: 1}, Methods: []string{"helper.read", "helper.write"}, Start: "lazy"}},
		Window: &sdk.WindowDescriptor{Metadata: "package.json"},
		Bindings: []sdk.BindingDescriptor{
			{ID: "page.read", From: "window:page", Action: "helper.read", Provider: "vendor"},
			{ID: "page.write", From: "window:page", Action: "helper.write", Provider: "vendor"},
			{ID: "page.root", From: "window:page", Action: "helper.read"},
		},
	}
	directory := test.TempDir()
	if runtime.GOOS == "windows" {
		manifest.Entry += ".exe"
		manifest.Services[0].Entry = manifest.Entry
	}
	build := proc.CommandContext(testenv.DeadlineContext(test, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-ldflags=-s -w",
		"-o", filepath.Join(directory, manifest.Entry), "./testdata/windowworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		test.Fatalf("build account window fixture: %v %s", err, output)
	}
	raw, _ := json.Marshal(manifest)
	for name, content := range map[string][]byte{
		sdk.ManifestFile: raw,
		"package.json":   []byte(`{"name":"account-window","magpie":{"page":{"title":"Accounts","file":"page.html"}}}`),
		"page.html":      []byte(`<!doctype html><html><body>Accounts</body></html>`),
	} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0600); err != nil {
			test.Fatal(err)
		}
	}
	entry, err := manager.Install(test.Context(), directory)
	if err != nil {
		test.Fatal(err)
	}
	fixture := &windowAccountFixture{id: manifest.ID, trace: filepath.Join(test.TempDir(), "trace"),
		started: filepath.Join(test.TempDir(), "started"), release: filepath.Join(test.TempDir(), "release")}
	configRaw, _ := json.Marshal(map[string]string{"trace": fixture.trace})
	config, err := manager.CompareAndSwapModuleConfig(test.Context(), manifest.ID, "0", configRaw)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			test.Error(err)
		}
	})
	for _, id := range []string{"account-a", "account-b"} {
		if _, err := manager.Store().SaveAccount(Account{Plugin: manifest.ID, Provider: "vendor", ID: id,
			Auth: json.RawMessage(`{"credential":"SECRET-` + id + `"}`)}, 0); err != nil {
			test.Fatal(err)
		}
	}
	return manager, fixture, windowRevision(entry.Revision, config.Version), fixture
}

func TestWindowSelectedAccountsSafeProjectionAndAuthority(test *testing.T) {
	manager, fixture, revision, calls := accountWindowFixture(test)
	id := fixture.id
	accounts, err := manager.WindowAccounts(test.Context(), id, revision, "window:page", "page.read")
	raw, _ := json.Marshal(accounts)
	if err != nil || len(accounts) != 2 || bytes.Contains(raw, []byte("SECRET")) || bytes.Contains(raw, []byte("auth")) {
		test.Fatalf("unsafe account projection: %s %v", raw, err)
	}
	selection := &sdk.AccountSelection{ID: "account-b", ExpectedGeneration: "1"}
	result, err := manager.CallWindowBindingWithOptions(test.Context(), id, revision, "window:page", "page.read",
		json.RawMessage(`{"account":"account-a","provider":"foreign","auth":"FORGED"}`), sdk.WindowCallOptions{Account: selection})
	if err != nil || !bytes.Contains(result, []byte(`"account":"account-b"`)) || bytes.Contains(result, []byte("SECRET")) {
		test.Fatalf("selected authority changed: %s %v", result, err)
	}
	for _, scenario := range []struct {
		name, ref, part, binding string
		account                  *sdk.AccountSelection
	}{
		{"missing", revision, "window:page", "page.read", nil},
		{"foreign-id", revision, "window:page", "page.read", &sdk.AccountSelection{ID: "not-installed", ExpectedGeneration: "1"}},
		{"stale", revision, "window:page", "page.read", &sdk.AccountSelection{ID: "account-b", ExpectedGeneration: "2"}},
		{"wrong-ref", "old", "window:page", "page.read", selection},
		{"wrong-part", revision, "window:panel:0", "page.read", selection},
		{"root-binding", revision, "window:page", "page.root", selection},
	} {
		if _, err := manager.CallWindowBindingWithOptions(test.Context(), id, scenario.ref, scenario.part, scenario.binding,
			json.RawMessage(`{}`), sdk.WindowCallOptions{Account: scenario.account}); err == nil {
			test.Fatal("invalid account invocation accepted:", scenario.name)
		}
	}
	if calls.Load() != 1 {
		test.Fatal("a refused call reached the helper:", calls.Load())
	}
	if _, err := manager.WindowAccounts(test.Context(), id, revision, "window:page", "page.root"); err == nil {
		test.Fatal("root binding listed credential owners")
	}
	if _, err := manager.CallWindowBinding(test.Context(), id, revision, "window:page", "page.root", json.RawMessage(`{}`)); err != nil {
		test.Fatal("legacy root call changed:", err)
	}
}

func TestWindowSelectedWriteIntentPinsAccountGenerationAndInput(test *testing.T) {
	manager, fixture, revision, calls := accountWindowFixture(test)
	id := fixture.id
	selection := &sdk.AccountSelection{ID: "account-a", ExpectedGeneration: "1"}
	options := sdk.WindowCallOptions{Account: selection}
	ctx := WithModuleRequestID(test.Context(), "parent-frame:message-one")
	if _, err := manager.CallWindowBindingWithOptions(ctx, id, revision, "window:page", "page.write", json.RawMessage(`{"reset":"week"}`), options); err != nil {
		test.Fatal(err)
	}
	for _, change := range []struct {
		selection sdk.AccountSelection
		input     string
	}{
		{*selection, `{"reset":"week"}`},
		{*selection, `{"reset":"day"}`},
		{sdk.AccountSelection{ID: "account-b", ExpectedGeneration: "1"}, `{"reset":"week"}`},
	} {
		if _, err := manager.CallWindowBindingWithOptions(ctx, id, revision, "window:page", "page.write",
			json.RawMessage(change.input), sdk.WindowCallOptions{Account: &change.selection}); err == nil {
			test.Fatal("write intent replay was accepted")
		}
	}
	if calls.Load() != 1 {
		test.Fatal("a replay reached the helper:", calls.Load())
	}
	scope := sdk.Scope{Plugin: id, Provider: "vendor", Account: "account-a", Generation: 1, Operation: "reserved-before-dispatch", Attempt: 1}
	if err := manager.store.bindWindowIntent(scope, "page.write", json.RawMessage(`{"one":1}`)); err != nil {
		test.Fatal(err)
	}
	if err := manager.store.bindWindowIntent(scope, "page.write", json.RawMessage(`{"one":2}`)); err == nil {
		test.Fatal("an undispatched intent accepted changed business input")
	}
	if err := manager.store.bindWindowIntent(scope, "another.binding", json.RawMessage(`{"one":1}`)); err == nil {
		test.Fatal("an undispatched intent accepted another binding")
	}
	replacement, err := manager.Store().SaveAccount(Account{Plugin: id, Provider: "vendor", ID: "account-a",
		Auth: json.RawMessage(`{"credential":"SECRET-account-a"}`)}, 1)
	if err != nil {
		test.Fatal(err)
	}
	selection.ExpectedGeneration = strconv.FormatUint(replacement.Generation, 10)
	if _, err := manager.CallWindowBindingWithOptions(ctx, id, revision, "window:page", "page.write", json.RawMessage(`{"reset":"week"}`), options); err == nil {
		test.Fatal("an old intent acquired a successor generation")
	}
}

func TestWindowSelectedSummaryAndCallKeepLargeGeneration(test *testing.T) {
	manager, fixture, revision, _ := accountWindowFixture(test)
	const generation = uint64(9007199254740993)
	if err := manager.store.withStore(fixture.id, func(root *os.Root) error {
		records, err := storeReadAccounts(root, fixture.id)
		if err != nil {
			return err
		}
		for index := range records.Accounts {
			if records.Accounts[index].ID == "account-b" {
				records.Accounts[index].Generation = generation
			}
		}
		return storeWrite(root, "accounts.json", records, storeFileLimit)
	}); err != nil {
		test.Fatal(err)
	}
	summaries, err := manager.WindowAccounts(test.Context(), fixture.id, revision, "window:page", "page.read")
	if err != nil {
		test.Fatal(err)
	}
	var selected *sdk.AccountSelection
	for _, summary := range summaries {
		if summary.ID == "account-b" {
			selected = &sdk.AccountSelection{ID: summary.ID, ExpectedGeneration: summary.Generation}
		}
	}
	if selected == nil || selected.ExpectedGeneration != "9007199254740993" {
		test.Fatal("account discovery rounded uint64 generation")
	}
	raw, err := manager.CallWindowBindingWithOptions(test.Context(), fixture.id, revision, "window:page", "page.read",
		json.RawMessage(`{}`), sdk.WindowCallOptions{Account: selected})
	var result struct {
		Generation uint64 `json:"generation"`
	}
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Generation != generation {
		test.Fatalf("selected backend lost uint64 generation: %s %v", raw, err)
	}
}

func TestWindowSelectedConcurrentIntentDispatchesOnce(test *testing.T) {
	manager, fixture, revision, calls := accountWindowFixture(test)
	ctx, cancel := context.WithTimeout(WithModuleRequestID(test.Context(), "same-frame:one-write"), 15*time.Second)
	defer cancel()
	results := make(chan error, 8)
	for index := 0; index < cap(results); index++ {
		go func() {
			_, err := manager.CallWindowBindingWithOptions(ctx, fixture.id, revision, "window:page", "page.write",
				json.RawMessage(`{"effect":"once"}`), sdk.WindowCallOptions{
					Account: &sdk.AccountSelection{ID: "account-a", ExpectedGeneration: "1"},
				})
			results <- err
		}()
	}
	success := 0
	for index := 0; index < cap(results); index++ {
		select {
		case err := <-results:
			if err == nil {
				success++
			}
		case <-ctx.Done():
			test.Fatal("concurrent window writes did not terminate")
		}
	}
	if success != 1 || calls.Load() != 1 {
		test.Fatalf("same intent dispatched more than once: success=%d effects=%d", success, calls.Load())
	}
}

func TestWindowSelectedIntentSurvivesReopenAndUnreadableRecord(test *testing.T) {
	manager, fixture, _, _ := accountWindowFixture(test)
	scope := sdk.Scope{Plugin: fixture.id, Provider: "vendor", Account: "account-a", Generation: 1, Operation: "intent", Attempt: 1}
	input := json.RawMessage(`{"effect":"one"}`)
	if err := manager.store.bindWindowIntent(scope, "page.write", input); err != nil {
		test.Fatal(err)
	}
	reopened := NewStore(manager.Namespace())
	if err := reopened.bindWindowIntent(scope, "page.write", input); err != nil {
		test.Fatal("unchanged intent lost on restart:", err)
	}
	if err := reopened.bindWindowIntent(scope, "page.write", json.RawMessage(`{"effect":"two"}`)); err == nil {
		test.Fatal("reopened store forgot the bound input")
	}
	if err := manager.store.withStore(fixture.id, func(root *os.Root) error {
		return root.WriteFile(filepath.Join("window-intents", storeHash(scope.Operation)+".json"), []byte("{broken"), 0600)
	}); err != nil {
		test.Fatal(err)
	}
	if err := reopened.bindWindowIntent(scope, "page.write", input); err == nil {
		test.Fatal("unreadable intent was treated as a new operation")
	}
}

func TestWindowSelectedAccountChecksAfterLazyStartup(test *testing.T) {
	for _, mutation := range []string{"account", "off", "config", "cancel"} {
		test.Run(mutation, func(test *testing.T) {
			manager, fixture, revision, calls := accountWindowFixture(test)
			id := fixture.id
			values, _ := json.Marshal(map[string]string{"trace": fixture.trace, "started": fixture.started, "release": fixture.release})
			config, err := manager.CompareAndSwapModuleConfig(test.Context(), id, "1", values)
			if err != nil {
				test.Fatal(err)
			}
			modules, err := manager.Windows(test.Context())
			if err != nil || len(modules) != 1 {
				test.Fatal("window fixture catalog changed", err)
			}
			revision = modules[0].Revision
			if modules[0].ConfigVersion != config.Version {
				test.Fatal("window fixture configuration changed")
			}
			ctx, cancel := context.WithCancel(test.Context())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := manager.CallWindowBindingWithOptions(ctx, id, revision, "window:page", "page.read", json.RawMessage(`{}`),
					sdk.WindowCallOptions{Account: &sdk.AccountSelection{ID: "account-a", ExpectedGeneration: "1"}})
				finished <- err
			}()
			wait := time.NewTimer(5 * time.Second)
			defer wait.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
		waiting:
			for {
				select {
				case <-tick.C:
					if _, err := os.Stat(fixture.started); err == nil {
						break waiting
					}
				case err := <-finished:
					test.Fatal("call did not reach lazy startup:", err)
				case <-wait.C:
					cancel()
					test.Fatal("helper did not start")
				}
			}
			switch mutation {
			case "account":
				_, err := manager.Store().SaveAccount(Account{Plugin: id, Provider: "vendor", ID: "account-a",
					Auth: json.RawMessage(`{"credential":"replacement"}`)}, 1)
				if err != nil {
					test.Fatal(err)
				}
			case "off":
				other := NewManager(manager.Namespace())
				if err := other.SetEnabled(test.Context(), id, false); err != nil {
					test.Fatal(err)
				}
				defer other.Close(context.Background())
			case "config":
				other := NewManager(manager.Namespace())
				if _, err := other.CompareAndSwapModuleConfig(test.Context(), id, "2", json.RawMessage(`{"new":true}`)); err != nil {
					test.Fatal(err)
				}
				defer other.Close(context.Background())
			case "cancel":
				cancel()
			}
			if err := os.WriteFile(fixture.release, []byte("release"), 0600); err != nil {
				test.Fatal(err)
			}
			select {
			case err := <-finished:
				if err == nil || calls.Load() != 0 {
					test.Fatalf("%s reached effects: %v calls=%d", mutation, err, calls.Load())
				}
				if strings.Contains(err.Error(), "credentials") {
					test.Fatal("private credential diagnostic escaped")
				}
			case <-time.After(5 * time.Second):
				test.Fatal("revoked call did not finish")
			}
		})
	}
}
