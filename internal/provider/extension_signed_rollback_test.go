package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type signedRollbackHost struct {
	*extensionhost.Manager
	afterJournal func(context.Context, extensionhost.ProviderExecutionBinding)
}

func (host signedRollbackHost) Call(ctx context.Context, id, method string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
	if method == sdk.MethodProviderExecute {
		selected := ctx
		ctx = extensionhost.WithProviderExecutionBinding(ctx, func(binding extensionhost.ProviderExecutionBinding) {
			host.afterJournal(selected, binding)
		})
	}
	return host.Manager.Call(ctx, id, method, scope, params, consume)
}

func TestExtensionSignedExecuteBlocksRollbackAtStoreWait(test *testing.T) {
	manager := discoveryCatalogManager(test)
	home := test.TempDir()
	test.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	test.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	test.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	test.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	test.Setenv("MAGPIE_NO_ACCOUNTS", "0")
	module, trace := installSignedRollbackWorker(test, manager)
	account, err := manager.Store().SaveAccount(extensionhost.Account{
		Plugin: module, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{"token":"PRIVATE-CREDENTIAL"}`),
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}, 0)
	if err != nil {
		test.Fatal(err)
	}
	scope := sdk.Scope{Plugin: module, Provider: account.Provider, Account: account.ID, Generation: account.Generation}
	const ownerRevision, user = "fixture-owner", "fixture-user"
	publicID, ownerPath := seedSignedRollbackCommunity(test, manager.Namespace(), scope, ownerRevision, user)
	ownerBefore, err := os.ReadFile(ownerPath)
	if err != nil {
		test.Fatal(err)
	}
	peer := extensionhost.NewManager(manager.Namespace())
	test.Cleanup(func() { closeDiscoveryCatalogManager(test, peer) })

	ctx, cancel := context.WithTimeout(testenv.DeadlineContext(test, 5*time.Second), 25*time.Second)
	var work sync.WaitGroup
	defer func() {
		cancel()
		work.Wait()
	}()
	entry, config, err := manager.ActiveModuleConfig(ctx, module)
	if err != nil {
		test.Fatal(err)
	}
	reached := make(chan extensionhost.ProviderExecutionBinding, 1)
	proceed := make(chan struct{})
	host := signedRollbackHost{Manager: manager, afterJournal: func(ctx context.Context, binding extensionhost.ProviderExecutionBinding) {
		select {
		case reached <- binding:
		case <-ctx.Done():
			return
		}
		select {
		case <-proceed:
		case <-ctx.Done():
		}
	}}
	providerExtensionHost = func() extensionProviderHost { return host }
	Changed()
	selected, err := FindContext(ctx, publicID)
	if err != nil {
		test.Fatal(err)
	}
	if actual, ok := selected.ExtensionScope(); !ok || actual != scope ||
		selected.Account.User != user || selected.Account.extension.identity().revision != ownerRevision {
		test.Fatalf("public provider did not select the transferred account: %+v", actual)
	}
	if _, found := PluginOf(publicID); found {
		test.Fatal("Community provider is visible while the SDK owns the subscription")
	}
	const payload = `{"model":"fixture-model","messages":[{"role":"user","content":"test signed rollback admission"}]}`
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, selected.Chat+"/chat/completions", strings.NewReader(payload))
	request.Header.Set(ConversationHeader, "signed-rollback")
	if err := selected.Sign(ctx, request, Chat, []byte(payload)); err != nil {
		test.Fatal(err)
	}
	var reply []byte
	var status int
	var callErr error
	callDone := make(chan struct{})
	work.Go(func() {
		defer close(callDone)
		response, err := selected.Do(http.DefaultClient, request)
		if err != nil {
			callErr = err
			return
		}
		status = response.StatusCode
		reply, callErr = io.ReadAll(response.Body)
		if err := response.Body.Close(); callErr == nil {
			callErr = err
		}
	})
	var observed extensionhost.ProviderExecutionBinding
	select {
	case observed = <-reached:
	case <-callDone:
		test.Fatalf("signed execute returned before post-journal admission: %v", callErr)
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	scope.Operation, scope.Attempt, scope.Session = observed.Scope.Operation, 1, "signed-rollback"
	expected := extensionhost.ProviderExecutionBinding{
		Namespace: manager.Namespace(), PackageRevision: entry.Revision, ConfigVersion: config.Version,
		EnableGeneration: entry.EnableGeneration, Scope: scope,
	}
	if scope.Operation == "" || observed != expected {
		test.Fatalf("unexpected signed execution binding: %+v, want %+v", observed, expected)
	}
	checkJournal := func(state string) {
		test.Helper()
		receipt, err := manager.Store().OperationContext(ctx, module, scope.Operation)
		if err != nil || receipt.Scope != scope || receipt.State != state {
			test.Fatalf("signed execution journal = %+v, %v, want %s for %+v", receipt, err, state, scope)
		}
	}
	checkJournal("dispatched")
	unlock, held := filelock.Lock(filepath.Join(manager.Namespace(), "private", ".lock"), 0)
	if !held {
		test.Fatal("could not hold the selected SDK private Store")
	}
	releaseStore := sync.OnceFunc(unlock)
	defer releaseStore()
	close(proceed)
	waitSignedRollbackStore(test, ctx, callDone, "(*Manager).admitSubscriptionCall(")
	var rollback extensionhost.SubscriptionOwner
	var rollbackErr error
	rollbackDone := make(chan struct{})
	work.Go(func() {
		defer close(rollbackDone)
		rollback, rollbackErr = RollbackSavedPluginSubscription(ctx, peer, publicID, ownerRevision)
	})
	waitSignedRollbackStore(test, ctx, rollbackDone, "(*Manager).RollbackSavedSubscription(")
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		test.Fatalf("worker reached before the private Store was released: %v", err)
	}
	releaseStore()
	select {
	case <-rollbackDone:
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	const busy = "the SDK subscription is busy; try again when its current operations finish"
	if rollbackErr == nil || rollbackErr.Error() != busy || rollback.Revision != "" {
		test.Fatalf("rollback did not report busy: %+v, %v", rollback, rollbackErr)
	}
	if current, err := os.ReadFile(ownerPath); err != nil || !bytes.Equal(current, ownerBefore) {
		test.Fatalf("busy rollback changed the owner snapshot: %v", err)
	}
	select {
	case <-callDone:
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	const wantReply = `{"id":"fixture","choices":[{"message":{"role":"assistant","content":"SDK executed"},"finish_reason":"stop"}]}`
	if callErr != nil || status != http.StatusOK || string(reply) != wantReply {
		test.Fatalf("signed execute did not complete: %d %s, %v", status, reply, callErr)
	}
	checkJournal("completed")
	raw, err := os.ReadFile(trace)
	var event struct {
		Method string
		Scope  sdk.Scope
	}
	if err != nil || json.Unmarshal(raw, &event) != nil || event.Method != "execute" || event.Scope != scope {
		test.Fatalf("expected exactly one worker execution and no refresh for %+v, got %s: %v", scope, raw, err)
	}
	owner, err := RollbackSavedPluginSubscription(ctx, peer, publicID, ownerRevision)
	if err != nil || owner.Active || owner.Pending || owner.Revision == "" || owner.Revision == ownerRevision {
		test.Fatalf("rollback after signed execution did not publish a new inactive owner: %+v, %v", owner, err)
	}
	current, revision, accountUser, visible := ExtensionPublicLogin(scope)
	community, restored := PluginOf(publicID)
	if visible || revision != owner.Revision || !restored || !community.SignedIn ||
		len(community.Accounts) != 1 || community.Accounts[0].Key != scope.Provider || community.Accounts[0].AccountID != user {
		test.Fatalf("rollback did not restore the same Community account: SDK %s/%s, revision %q, visible=%t, Community restored=%t",
			current, accountUser, revision, visible, restored)
	}
}

func installSignedRollbackWorker(test *testing.T, manager *extensionhost.Manager) (string, string) {
	test.Helper()
	manifest := authfixture.Manifest()
	manifest.Window = nil
	manifest.Contract.Version.Minor = 9
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities,
		sdk.ProviderSignIn, sdk.ProviderSavedSignIn, sdk.OutboundExecutor)
	manifest.Providers[0].Models = []sdk.Model{{ID: "fixture-model", Protocols: manifest.Providers[0].Protocols}}
	manifest.AuthBindings = []sdk.WindowAuthBindingDescriptor{{
		ID: "saved.signin", From: sdk.ProviderSignInPart, Provider: "vendor",
		Methods: []sdk.WindowAuthMethod{{ID: "saved-signin", Mode: "poll", Label: "Import saved sign-in",
			Fields: []sdk.AuthInputField{{Key: "record", Type: "secret", Label: "Saved account"}}}},
	}}
	if runtime.GOOS == "windows" {
		manifest.Entry += ".exe"
	}
	source := test.TempDir()
	build := proc.CommandContext(testenv.DeadlineContext(test, 30*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(source, manifest.Entry), "../extension/testdata/authworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		test.Fatalf("build auth fixture: %v\n%s", err, output)
	}
	writeFile(test, filepath.Join(source, sdk.ManifestFile), manifest)
	if _, err := manager.Install(test.Context(), source); err != nil {
		test.Fatal(err)
	}
	trace := filepath.Join(test.TempDir(), "worker.jsonl")
	config, err := json.Marshal(authfixture.Config{Trace: trace})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(test.Context(), manifest.ID, "0", config); err != nil {
		test.Fatal(err)
	}
	return manifest.ID, trace
}

func seedSignedRollbackCommunity(test *testing.T, namespace string, scope sdk.Scope, revision, user string) (string, string) {
	test.Helper()
	const spec = "@magpie-test/opencode-vendor-auth"
	auth := json.RawMessage(fmt.Sprintf(`{"type":"api","key":"synthetic-community","accountId":%q}`, user))
	was := plugin.ListInBackground(false)
	plugin.UseCached(nil)
	test.Cleanup(func() {
		plugin.UseCached(nil)
		plugin.ListInBackground(was)
	})
	writeFile(test, filepath.Join(settings.Dir(), "plugins.json"), map[string]any{
		"plugins": []plugin.Entry{{Spec: spec}},
	})
	writeFile(test, filepath.Join(settings.Dir(), "plugin-providers.json"), []plugin.Provider{{
		ID: scope.Provider, Spec: spec, Name: "Fixture Community",
	}})
	writeFile(test, filepath.Join(settings.Dir(), "plugin-auth.json"), map[string]json.RawMessage{scope.Provider: auth})
	writeFile(test, loginsPath(), []savedLogin{{Agent: "plugin:" + scope.Provider, Home: scope.Provider, User: user, On: true}})
	publicID := PluginID(scope.Provider)
	ownerPath := filepath.Join(namespace, "private", ".saved-signins", "subscription-owners.json")
	writeFile(test, ownerPath, []struct {
		extensionhost.SubscriptionOwner
		GrantRevision string            `json:"grantRevision"`
		Digests       map[string]string `json:"digests"`
		Tickets       []string          `json:"tickets"`
		Generations   []uint64          `json:"generations"`
	}{{
		SubscriptionOwner: extensionhost.SubscriptionOwner{
			Module: scope.Plugin, Provider: scope.Provider, PublicID: publicID, Revision: revision, Active: true,
			Accounts: []extensionhost.SubscriptionAccount{{
				Key: scope.Provider, ID: scope.Account, User: user, LoginID: LoginID(publicID, user),
			}},
		},
		GrantRevision: strings.Repeat("a", 64), Digests: map[string]string{scope.Provider: fmt.Sprintf("%x", sha256.Sum256(auth))},
		Tickets: []string{"fixture-ticket"}, Generations: []uint64{scope.Generation},
	}})
	return publicID, ownerPath
}

func waitSignedRollbackStore(test *testing.T, ctx context.Context, done <-chan struct{}, frame string) {
	test.Helper()
	stacks := make([]byte, 1<<20)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		for stack := range bytes.SplitSeq(stacks[:runtime.Stack(stacks, true)], []byte("\n\n")) {
			if bytes.Contains(stack, []byte(frame)) && bytes.Contains(stack, []byte("storeWaitLock(")) {
				return
			}
		}
		select {
		case <-done:
			test.Fatalf("%s returned without reaching the private Store wait", frame)
		case <-ctx.Done():
			test.Fatalf("%s did not reach the private Store wait: %v", frame, ctx.Err())
		case <-tick.C:
		}
	}
}
