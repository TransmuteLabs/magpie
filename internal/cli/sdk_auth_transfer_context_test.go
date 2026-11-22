package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKCLISavedActivationRepairsPolicyAfterCancellation(t *testing.T) {
	manager, manifest, _ := savedSignInFixture(t)
	trace := filepath.Join(t.TempDir(), "auth.jsonl")
	config, err := json.Marshal(authfixture.Config{Trace: trace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
		t.Fatal(err)
	}
	args := savedSubscriptionCommand(t, "activate", manifest.ID)
	dispatched, err := os.ReadFile(trace)
	if err != nil || bytes.Count(dispatched, []byte(`"method":"start"`)) != 1 || bytes.Count(dispatched, []byte(`"method":"finish"`)) != 1 {
		t.Fatalf("expected one preparation through the SDK auth worker: %v", err)
	}
	logins := filepath.Join(filepath.Dir(provider.Path()), "logins.json")
	publicID := provider.ExtensionProviderID(manifest.ID, "vendor")
	policy := []byte(`[{"agent":"` + publicID + `","user":"sibling","home":"sibling","on":false,"paused":true,"order":4}]`)
	if err := os.WriteFile(logins, policy, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, release := startSavedAuthCommandWaiting(t, ctx, logins+".lock",
		"internal/provider.registerExtensionLogin", "internal/filelock.lockPath", args)
	accounts, err := manager.Store().AccountGenerationsContext(t.Context(), manifest.ID, "vendor")
	if err != nil || !slices.Equal(accounts, []extensionhost.AccountGeneration{{ID: "chosen", Generation: 1}}) {
		t.Fatalf("the account must already have committed at the policy lock: %v", err)
	}
	cancel()
	var canceled savedAuthCommandResult
	select {
	case canceled = <-result:
	case <-time.After(2 * time.Second):
		t.Error("activate kept waiting for logins.json.lock after its account committed and the caller canceled")
		release()
		canceled = <-result
	}
	var receipt struct {
		sdk.AccountSummary
		Warning string
	}
	if canceled.err != nil || json.Unmarshal([]byte(canceled.out), &receipt) != nil || receipt.ID != "chosen" ||
		receipt.Generation != "1" || receipt.Warning == "" || strings.Contains(canceled.out, "PRIVATE") {
		t.Error("canceled activation omitted its credential-free committed receipt and recovery warning")
	}
	if current, err := os.ReadFile(logins); err != nil || !bytes.Equal(current, policy) {
		t.Fatalf("the canceled registration modified login policy: %v", err)
	}
	release()
	checkSavedAuthRetry(t, args)
	after, err := manager.Store().AccountGenerationsContext(t.Context(), manifest.ID, "vendor")
	if err != nil || !slices.Equal(accounts, after) {
		t.Fatalf("retry changed the committed credential generation: %v", err)
	}
	if calls, err := os.ReadFile(trace); err != nil || !bytes.Equal(dispatched, calls) {
		t.Fatalf("local policy recovery dispatched authentication again: %v", err)
	}
	data, err := os.ReadFile(logins)
	type loginPolicy struct {
		Agent, User string
		On, Paused  bool
		Order       int
	}
	var policies []loginPolicy
	if err != nil || json.Unmarshal(data, &policies) != nil ||
		len(policies) != 2 || !slices.Contains(policies, loginPolicy{Agent: publicID, User: "chosen", On: true}) ||
		!slices.Contains(policies, loginPolicy{Agent: publicID, User: "sibling", Paused: true, Order: 4}) {
		t.Fatalf("retry failed to add the account policy while keeping the sibling's pause and order: %v", err)
	}
	if _, err := said(t, func() error { return sdkPluginCmd(t.Context(), args) }); err == nil {
		t.Fatal("completed policy registration left its activation ticket usable")
	}
}

func TestSDKCLIFinishRepairsPolicyAfterCancellation(t *testing.T) {
	manager, manifest, _ := savedSignInFixture(t)
	trace := filepath.Join(t.TempDir(), "auth.jsonl")
	config, err := json.Marshal(authfixture.Config{Trace: trace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
		t.Fatal(err)
	}
	before, err := manager.Store().SaveAccount(extensionhost.Account{
		Plugin: manifest.ID, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{"token":"PRIVATE-BEFORE"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	accountScope := `{"account":"chosen"}`
	begin, err := said(t, func() error {
		return sdkPluginCmd(t.Context(), []string{"auth", "start", manifest.ID, "vendor", `{"method":"device"}`, "--scope", accountScope})
	})
	var start sdk.AuthStartResult
	if err != nil || json.Unmarshal([]byte(begin), &start) != nil || len(start.Continuation) == 0 {
		t.Fatalf("start did not return an authentication continuation: %v", err)
	}
	finish, err := json.Marshal(sdk.AuthFinishRequest{Continuation: start.Continuation})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"auth", "finish", manifest.ID, "vendor", string(finish), "--scope", accountScope}
	logins := filepath.Join(filepath.Dir(provider.Path()), "logins.json")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, release := startSavedAuthCommandWaiting(t, ctx, logins+".lock",
		"internal/provider.registerExtensionLogin", "internal/filelock.lockPath", args)
	accounts, err := manager.Store().AccountGenerationsContext(t.Context(), manifest.ID, "vendor")
	if err != nil || !slices.Equal(accounts, []extensionhost.AccountGeneration{{ID: before.ID, Generation: before.Generation + 1}}) {
		t.Errorf("SDK auth finish had not committed the new generation at the policy lock: %v", err)
	}
	cancel()
	var canceled savedAuthCommandResult
	select {
	case canceled = <-result:
	case <-time.After(2 * time.Second):
		t.Error("auth finish kept waiting for policy storage after cancellation")
		release()
		canceled = <-result
	}
	release()
	var receipt struct {
		ID, Generation, OwnerRevision, Warning string
	}
	if canceled.err != nil || json.Unmarshal([]byte(canceled.out), &receipt) != nil || receipt.ID != before.ID ||
		receipt.Generation != "2" || receipt.Warning == "" || strings.Contains(canceled.out, "PRIVATE") {
		t.Fatal("auth finish lost its committed credential-free recovery receipt")
	}
	dispatches, err := os.ReadFile(trace)
	if err != nil || bytes.Count(dispatches, []byte(`"method":"finish"`)) != 1 {
		t.Fatalf("sign-in did not authenticate exactly once: %v", err)
	}
	if _, err := os.Stat(logins); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled auth finish wrote login policy: %v", err)
	}
	register := func(generation, owner string) (string, error) {
		t.Helper()
		return said(t, func() error {
			return sdkPluginCmd(t.Context(), []string{"auth", "register", manifest.ID, "vendor", receipt.ID, generation, owner})
		})
	}
	for _, stale := range [][2]string{{"1", receipt.OwnerRevision}, {receipt.Generation, "other-owner"}} {
		if out, err := register(stale[0], stale[1]); err == nil || out != "" {
			t.Fatal("local registration accepted an obsolete account or owner")
		}
	}
	if _, err := os.Stat(logins); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete registration wrote login policy: %v", err)
	}
	if _, err := register(receipt.Generation, receipt.OwnerRevision); err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile(logins)
	if err != nil || !bytes.Contains(policy, []byte(`"user": "chosen"`)) || strings.Contains(string(policy), "PRIVATE") {
		t.Fatalf("could not recover local policy for the committed account: %v", err)
	}
	after, err := manager.Store().AccountGenerationsContext(t.Context(), manifest.ID, "vendor")
	if err != nil || !slices.Equal(accounts, after) {
		t.Fatalf("local registration changed credential generation: %v", err)
	}
	if calls, err := os.ReadFile(trace); err != nil || !bytes.Equal(calls, dispatches) {
		t.Fatalf("recovering local policy called the SDK auth worker: %v", err)
	}
}

func TestSDKCLISavedActivationRejectsUnknownInstalledPlugins(t *testing.T) {
	for _, invalid := range []struct {
		name, data string
	}{
		{"not-an-object", `null`},
		{"wrong-entry-list-type", `{"plugins":"damaged"}`},
		{"wrong-entry-type", `{"plugins":[null]}`},
		{"unreadable", ""},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			manager, manifest, source := savedSignInFixture(t)
			args := savedSubscriptionCommand(t, "activate", manifest.ID)
			before := savedAuthTransactionFiles(t, manager, manifest.ID)
			if invalid.data == "" {
				if err := os.Mkdir(plugin.ListPath(), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(plugin.ListPath(), []byte(invalid.data), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := said(t, func() error { return sdkPluginCmd(t.Context(), args) })
			if err == nil || out != "" || !strings.Contains(err.Error(), "installed plugins list") {
				t.Errorf("unknown installed plugins did not produce a checked-inventory refusal: %v, receipt present=%t", err, out != "")
			}
			checkSavedAuthTransactionFiles(t, manager, manifest.ID, source, before)
			if err := os.Remove(plugin.ListPath()); err != nil {
				t.Fatal(err)
			}
			checkSavedAuthRetry(t, args)
		})
	}
}

func TestSDKCLIImportRejectsRecoveredProviderProxy(t *testing.T) {
	for _, wait := range []bool{false, true} {
		name := "unreadable-at-start"
		if wait {
			name = "unreadable-after-source-wait"
		}
		t.Run(name, func(t *testing.T) {
			manager, manifest, source := savedSignInFixture(t)
			trace := filepath.Join(t.TempDir(), "auth.jsonl")
			const proxy = "http://127.0.0.1:15493"
			config, err := json.Marshal(authfixture.Config{Trace: trace, ImportProxy: proxy})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
				t.Fatal(err)
			}
			valid := []byte(`{"providers":[{"id":"vendor","proxy":"` + proxy + `"}]}`)
			write := func(path string, value []byte) {
				t.Helper()
				if err := os.WriteFile(path, value, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(lastgood.Bak(provider.Path()), valid)
			damaged := []byte(`{"providers":false}`)
			args := []string{"auth", "import", manifest.ID, "vendor", "vendor#selected"}
			before := savedAuthTransactionFiles(t, manager, manifest.ID)
			var out string
			if wait {
				write(provider.Path(), valid)
				result, release := startSavedAuthCommandWaiting(t, t.Context(), plugin.AuthPath()+".write.lock",
					"internal/plugin.withSavedAuths", "internal/plugin.lockAuth", args)
				write(provider.Path(), damaged)
				release()
				select {
				case finished := <-result:
					out, err = finished.out, finished.err
				case <-time.After(5 * time.Second):
					t.Fatal("import did not return after the source wait")
				}
			} else {
				write(provider.Path(), damaged)
				out, err = said(t, func() error { return sdkPluginCmd(t.Context(), args) })
			}
			if err == nil || !wait && !errors.Is(err, provider.ErrUnreadable) || out != "" {
				t.Errorf("import used unavailable provider egress: %v, receipt present=%t", err, out != "")
			}
			if _, err := os.Stat(trace); !errors.Is(err, os.ErrNotExist) {
				t.Error("import dispatched authentication after losing authoritative egress")
			}
			checkSavedAuthTransactionFiles(t, manager, manifest.ID, source, before)
			write(provider.Path(), valid)
			checkSavedAuthRetry(t, args)
		})
	}
}

func TestSDKCLISubscriptionRechecksCurrentProviders(t *testing.T) {
	const empty = `{"providers":[]}`
	const damaged = `{"providers":false}`
	const custom = `{"providers":[{"id":"vendor","chat":"http://127.0.0.1:1"}]}`
	const collision = `{"providers":[{"id":"vendor","chat":"http://127.0.0.1:1"},{"id":"vendor-plugin","chat":"http://127.0.0.1:1"}]}`
	for _, operation := range []string{"transfer", "rollback"} {
		for _, test := range []struct {
			name, initial, updated, message string
			wait                            bool
		}{
			{name: "damaged-at-start", initial: empty, updated: damaged},
			{name: "damaged-after-source-wait", initial: empty, updated: damaged, wait: true},
			{name: "public-id-after-source-wait", initial: empty, updated: custom, message: "subscription owner changed", wait: true},
			{name: "collision-after-source-wait", initial: custom, updated: collision, message: "a custom provider uses this subscription ID", wait: true},
		} {
			t.Run(operation+"/"+test.name, func(t *testing.T) {
				manager, manifest, source := savedSignInFixture(t)
				installSavedAuthSource(t)
				path := provider.Path()
				write := func(path, raw string) {
					t.Helper()
					if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				write(path, test.initial)
				write(lastgood.Bak(path), test.initial)
				args := savedSubscriptionCommand(t, operation, manifest.ID)
				before := savedAuthTransactionFiles(t, manager, manifest.ID)

				var out string
				var err error
				if test.wait {
					result, release := startSavedAuthCommandWaiting(t, t.Context(), plugin.AuthPath()+".write.lock",
						"internal/plugin.withSavedAuths", "internal/plugin.lockAuth", args)
					write(path, test.updated)
					release()
					select {
					case finished := <-result:
						out, err = finished.out, finished.err
					case <-time.After(8 * time.Second):
						t.Fatal("subscription command did not return after the source wait")
					}
				} else {
					write(path, test.updated)
					out, err = said(t, func() error { return sdkPluginCmd(t.Context(), args) })
				}
				switch {
				case err == nil || out != "":
					t.Errorf("subscription changed using stale provider IDs: %v, receipt present=%t", err, out != "")
				case test.message == "":
					if !errors.Is(err, provider.ErrUnreadable) {
						t.Errorf("expected unreadable current providers: %v", err)
					}
				case !strings.Contains(err.Error(), test.message):
					t.Errorf("expected %q: %v", test.message, err)
				}
				checkSavedAuthTransactionFiles(t, manager, manifest.ID, source, before)
				if test.initial == empty {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else {
					write(path, test.initial)
				}
				checkSavedAuthRetry(t, args)
			})
		}
	}
}

func TestSDKCLISubscriptionWaitUsesCallerContext(t *testing.T) {
	for _, operation := range []string{"transfer", "rollback"} {
		for _, file := range []string{"logins", "installed"} {
			t.Run(operation+"/"+file, func(t *testing.T) {
				manager, manifest, source := savedSignInFixture(t)
				installSavedAuthSource(t)
				args := savedSubscriptionCommand(t, operation, manifest.ID)
				var lock, lockFrame string
				if file == "logins" {
					lock = filepath.Join(filepath.Dir(provider.Path()), "logins.json.lock")
					lockFrame = "internal/provider." + map[string]string{
						"transfer": "TransferSavedPluginSubscription",
						"rollback": "RollbackSavedPluginSubscription",
					}[operation]
				} else {
					lock, lockFrame = plugin.ListPath()+".lock", "internal/plugin.WithInstalledSavedAuths"
				}
				before := savedAuthTransactionFiles(t, manager, manifest.ID)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result, release := startSavedAuthCommandWaiting(t, ctx, lock, lockFrame, "internal/filelock.lockPath", args)
				cancel()
				select {
				case done := <-result:
					if done.out != "" || !errors.Is(done.err, context.Canceled) {
						t.Errorf("canceled command returned %v, receipt present=%t", done.err, done.out != "")
					}
				case <-time.After(time.Second):
					t.Error("command still waited for a file lock after caller cancellation")
					release()
					<-result
				}
				release()
				checkSavedAuthTransactionFiles(t, manager, manifest.ID, source, before)
				checkSavedAuthRetry(t, args)
			})
		}
	}
}

func TestSDKCLISavedSignInRechecksDiscoveryAfterSourceWait(t *testing.T) {
	for _, operation := range []string{"import", "activate", "transfer", "rollback"} {
		t.Run(operation, func(t *testing.T) {
			manager, manifest, source := savedSignInFixture(t)
			if operation == "transfer" || operation == "rollback" {
				installSavedAuthSource(t)
			}
			args := savedSubscriptionCommand(t, operation, manifest.ID)
			if err := settings.Save(settings.Settings{}); err != nil {
				t.Fatal(err)
			}
			before := savedAuthTransactionFiles(t, manager, manifest.ID)
			releaseMemo := filememo.Hold()
			defer releaseMemo()
			if settings.AccountDiscoveryOff() {
				t.Fatal("discovery must be on before the command")
			}
			result, release := startSavedAuthCommandWaiting(t, t.Context(), plugin.AuthPath()+".write.lock",
				"internal/plugin.withSavedAuths", "internal/plugin.lockAuth", args)
			if err := os.WriteFile(settings.Path(), []byte(`{"noAccountDiscovery":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case done := <-result:
				if done.err == nil || done.out != "" || !strings.Contains(done.err.Error(), "subscription accounts are off") {
					t.Errorf("%s after discovery was disabled returned %v, receipt present=%t", operation, done.err, done.out != "")
				}
			case <-time.After(8 * time.Second):
				t.Fatal("saved sign-in command did not return")
			}
			releaseMemo()
			checkSavedAuthTransactionFiles(t, manager, manifest.ID, source, before)
			if err := settings.Save(settings.Settings{}); err != nil {
				t.Fatal(err)
			}
			checkSavedAuthRetry(t, args)
		})
	}
}

func TestSDKCLISavedTransferRechecksDiscoveryBeforeReconcilingLogins(t *testing.T) {
	manager, manifest, source := savedSignInFixture(t)
	installSavedAuthSource(t)
	args := savedSubscriptionCommand(t, "transfer", manifest.ID)
	logins := filepath.Join(filepath.Dir(provider.Path()), "logins.json")
	if err := os.WriteFile(logins, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{}); err != nil {
		t.Fatal(err)
	}
	writeDiscovery := func(value string) {
		t.Helper()
		if err := os.WriteFile(settings.Path(), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Now().Add(-time.Minute).Truncate(time.Second)
	keepStamp := func() {
		t.Helper()
		if err := os.Chtimes(settings.Path(), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	writeDiscovery(`{"noAccountDiscovery":false}`)
	keepStamp()
	before := savedAuthTransactionFiles(t, manager, manifest.ID)
	releaseMemo := filememo.Hold()
	defer releaseMemo()
	if settings.AccountDiscoveryOff() {
		t.Fatal("discovery must be on before the command")
	}
	result, release := startSavedAuthCommandWaiting(t, t.Context(), logins+".lock",
		"internal/provider.withPluginLogins", "internal/filelock.lockPath", args)
	writeDiscovery(`{"noAccountDiscovery":true }`)
	keepStamp()
	release()
	select {
	case done := <-result:
		if done.err == nil || done.out != "" || !strings.Contains(done.err.Error(), "subscription accounts are off") {
			t.Errorf("transfer after discovery was disabled returned %v, receipt present=%t", done.err, done.out != "")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("transfer did not return")
	}
	releaseMemo()
	checkSavedAuthTransactionFiles(t, manager, manifest.ID, source, before)
	if err := settings.Save(settings.Settings{}); err != nil {
		t.Fatal(err)
	}
	checkSavedAuthRetry(t, args)
}

func installSavedAuthSource(t *testing.T) {
	t.Helper()
	const spec = "@example/opencode-vendor-auth"
	installed, err := json.Marshal(plugin.List{Plugins: []plugin.Entry{{Spec: spec}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plugin.ListPath(), installed, 0o600); err != nil {
		t.Fatal(err)
	}
	plugin.UseCached([]plugin.Provider{{ID: "vendor", Spec: spec, Name: "Community vendor"}})
	t.Cleanup(func() { plugin.UseCached(nil) })
	if logins := provider.Logins("vendor"); len(logins) != 2 {
		t.Fatalf("community accounts: %d", len(logins))
	}
}

func savedSubscriptionCommand(t *testing.T, operation, module string) []string {
	t.Helper()
	selected := []string{"auth", "import", module, "vendor", "vendor#selected"}
	if operation == "import" {
		return selected
	}
	ticket := func(args []string) string {
		t.Helper()
		raw, err := said(t, func() error { return sdkPluginCmd(t.Context(), args) })
		var prepared extensionhost.PreparedSignIn
		if err != nil || json.Unmarshal([]byte(raw), &prepared) != nil || prepared.Ticket == "" {
			t.Fatalf("saved sign-in preparation failed: %v", err)
		}
		return prepared.Ticket
	}
	first := ticket(selected)
	if operation == "activate" {
		return []string{"auth", "activate", module, "vendor", first}
	}
	second := ticket([]string{"auth", "import", module, "vendor", "vendor"})
	transfer := []string{"auth", "transfer", module, "vendor", first, second}
	if operation == "transfer" {
		return transfer
	}
	raw, err := said(t, func() error { return sdkPluginCmd(t.Context(), transfer) })
	var owner extensionhost.SubscriptionOwner
	if err != nil || json.Unmarshal([]byte(raw), &owner) != nil || !owner.Active {
		t.Fatalf("prepare rollback owner: %v", err)
	}
	return []string{"auth", "rollback", owner.PublicID, owner.Revision}
}

type savedAuthCommandResult struct {
	out string
	err error
}

func TestSDKCLISavedAuthAndUnrelatedWriterFinish(t *testing.T) {
	for _, operation := range []string{"import", "activate", "transfer", "rollback"} {
		t.Run(operation, func(t *testing.T) {
			manager, manifest, _ := savedSignInFixture(t)
			if operation == "transfer" || operation == "rollback" {
				installSavedAuthSource(t)
			}
			args := savedSubscriptionCommand(t, operation, manifest.ID)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan savedAuthCommandResult, 1)
			finished := make(chan struct{})
			run := func() {
				go func() {
					defer close(finished)
					out, err := said(t, func() error { return sdkPluginCmd(ctx, args) })
					result <- savedAuthCommandResult{out, err}
				}()
				t.Cleanup(func() {
					cancel()
					<-finished
				})
			}
			var finishRelease string
			if operation == "import" {
				root := t.TempDir()
				started := filepath.Join(root, "started")
				finishRelease = filepath.Join(root, "release")
				config, err := json.Marshal(authfixture.Config{Started: started, Release: finishRelease})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
					t.Fatal(err)
				}
				run()
				bound := time.NewTimer(4 * time.Second)
				defer bound.Stop()
				tick := time.NewTicker(5 * time.Millisecond)
				defer tick.Stop()
				for {
					if _, err := os.Stat(started); err == nil {
						break
					}
					select {
					case done := <-result:
						t.Fatalf("import returned before verification: %v", done.err)
					case <-bound.C:
						t.Fatal("import did not reach verification")
					case <-tick.C:
					}
				}
			}
			entered := make(chan struct{})
			publish := make(chan struct{})
			allowWrite := sync.OnceFunc(func() { close(publish) })
			writer := make(chan error, 1)
			writerDone := make(chan struct{})
			go func() {
				defer close(writerDone)
				writer <- plugin.EditAuth(func(records map[string]json.RawMessage) (bool, error) {
					if _, exists := records["foreign"]; !exists {
						return false, errors.New("unrelated fixture account is missing")
					}
					close(entered)
					<-publish
					delete(records, "foreign")
					return true, nil
				})
			}()
			t.Cleanup(func() {
				cancel()
				allowWrite()
				<-writerDone
			})
			select {
			case <-entered:
			case err := <-writer:
				t.Fatalf("unrelated writer returned before taking its lock: %v", err)
			case <-time.After(4 * time.Second):
				t.Fatal("unrelated writer did not acquire its lock")
			}
			if finishRelease != "" {
				if err := os.WriteFile(finishRelease, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				run()
			}
			waitForSavedAuthCommand(t, result, filepath.Base(plugin.AuthPath())+".write.lock", "internal/plugin.withSavedAuths", "internal/plugin.lockAuth")
			allowWrite()
			bound := time.NewTimer(2 * time.Second)
			defer bound.Stop()
			select {
			case err := <-writer:
				if err != nil {
					t.Fatal(err)
				}
			case <-bound.C:
				t.Fatal("unrelated auth writer could not publish while the SDK command waited for its source")
			}
			select {
			case done := <-result:
				checkSavedAuthReceipt(t, operation, done.out, done.err)
			case <-bound.C:
				t.Fatal("saved auth command remained blocked after the unrelated write")
			}
			records, err := plugin.AuthEntries()
			if err != nil || len(records) != 2 || len(records["vendor"]) == 0 || len(records["vendor#selected"]) == 0 {
				t.Fatal("unrelated sign-out and saved auth operation did not both preserve their effects")
			}
		})
	}
}

func startSavedAuthCommandWaiting(t *testing.T, ctx context.Context, lock, origin, waiting string, args []string) (<-chan savedAuthCommandResult, func()) {
	t.Helper()
	unlock, held := filelock.Lock(lock, time.Second)
	if !held {
		t.Fatal("could not hold the command's file lock")
	}
	release := sync.OnceFunc(unlock)
	result := make(chan savedAuthCommandResult, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		release()
		<-done
	})
	go func() {
		defer close(done)
		out, err := said(t, func() error { return sdkPluginCmd(ctx, args) })
		result <- savedAuthCommandResult{out, err}
	}()
	waitForSavedAuthCommand(t, result, filepath.Base(lock), origin, waiting)
	return result, release
}

func waitForSavedAuthCommand(t *testing.T, result <-chan savedAuthCommandResult, label, origin, waiting string) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	bound := time.NewTimer(4 * time.Second)
	defer bound.Stop()
	stacks := make([]byte, 256<<10)
	for {
		current := stacks[:runtime.Stack(stacks, true)]
		for _, stack := range bytes.Split(current, []byte("\n\n")) {
			if bytes.Contains(stack, []byte(origin)) && bytes.Contains(stack, []byte(waiting+"(")) {
				return
			}
		}
		select {
		case terminated := <-result:
			t.Fatalf("command returned before waiting on %s: %v", label, terminated.err)
		case <-bound.C:
			t.Fatalf("command did not wait on %s", label)
		case <-ticker.C:
		}
	}
}

func savedAuthTransactionFiles(t *testing.T, manager *extensionhost.Manager, module string) map[string][]byte {
	t.Helper()
	private := filepath.Join(manager.Namespace(), "private")
	paths := []string{
		filepath.Join(private, ".saved-signins", "records.json"),
		filepath.Join(private, ".saved-signins", "subscription-owners.json"),
		filepath.Join(private, module, "accounts.json"),
		filepath.Join(filepath.Dir(provider.Path()), "logins.json"),
	}
	snapshot := make(map[string][]byte, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		snapshot[path] = raw
	}
	return snapshot
}

func checkSavedAuthTransactionFiles(t *testing.T, manager *extensionhost.Manager, module string, source []byte, before map[string][]byte) {
	t.Helper()
	for path, current := range savedAuthTransactionFiles(t, manager, module) {
		if !bytes.Equal(current, before[path]) {
			t.Errorf("refused command changed %s", filepath.Base(path))
		}
	}
	if current, err := os.ReadFile(plugin.AuthPath()); err != nil || !bytes.Equal(current, source) {
		t.Error("refused command changed source credentials")
	}
}

func checkSavedAuthRetry(t *testing.T, args []string) {
	t.Helper()
	out, err := said(t, func() error { return sdkPluginCmd(t.Context(), args) })
	checkSavedAuthReceipt(t, args[1], out, err)
}

func checkSavedAuthReceipt(t *testing.T, operation, out string, err error) {
	t.Helper()
	var receipt struct {
		Ticket, ID, Revision string
		Active, Pending      bool
	}
	if err != nil || json.Unmarshal([]byte(out), &receipt) != nil || strings.Contains(out, "PRIVATE-") {
		t.Fatalf("saved auth %s failed: %v", operation, err)
	}
	switch operation {
	case "import":
		if receipt.Ticket == "" {
			t.Fatal("import did not return a preparation ticket")
		}
	case "activate":
		if receipt.ID != "chosen" {
			t.Fatal("activation did not return the selected account")
		}
	case "transfer", "rollback":
		if receipt.Revision == "" || receipt.Pending || receipt.Active != (operation == "transfer") {
			t.Fatal("transition did not return a completed owner revision")
		}
	}
}
