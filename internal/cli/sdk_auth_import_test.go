package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const subscriptionRollbackPeer = "MAGPIE_TEST_SUBSCRIPTION_ROLLBACK_PEER"

type subscriptionRollbackSelection struct {
	ID, Revision, Spec string
	Busy               bool
}

func TestSDKCLISubscriptionRollbackPeer(t *testing.T) {
	selection := os.Getenv(subscriptionRollbackPeer)
	if selection == "" {
		return
	}
	var args subscriptionRollbackSelection
	if os.Getenv(testenv.Marker) == "" || json.Unmarshal([]byte(selection), &args) != nil {
		t.Fatal("invalid peer rollback fixture")
	}
	plugin.UseCached([]plugin.Provider{{ID: args.ID, Spec: args.Spec, Name: "Community vendor"}})
	err := sdkPluginCmd(t.Context(), []string{"auth", "rollback", args.ID, args.Revision})
	if args.Busy {
		if err == nil || !strings.Contains(err.Error(), "SDK subscription is busy") {
			t.Fatalf("peer could roll back before the canceled handler finished: %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

func checkSubscriptionRollbackAdmission(t *testing.T, owner extensionhost.SubscriptionOwner, spec, trace, queueRelease, executeRelease string) {
	t.Helper()
	manager := extensionhost.Default()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	var work sync.WaitGroup
	defer func() {
		cancel()
		work.Wait()
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := manager.Close(stop); err != nil {
			t.Error(err)
		}
	}()
	lane := sdk.MaxConcurrent/2 - 2
	holds := make(chan error, lane)
	handler := gateway.New().Front()
	pass := func(requestContext context.Context) (*httptest.ResponseRecorder, <-chan struct{}) {
		body := `{"model":"` + owner.PublicID + `/fixture-model","messages":[{"role":"user","content":"test rollback admission"}]}`
		req := httptest.NewRequestWithContext(requestContext, http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:54321"
		reply := httptest.NewRecorder()
		result := make(chan struct{})
		work.Go(func() {
			defer close(result)
			handler.ServeHTTP(reply, req)
		})
		return reply, result
	}
	peer := func(busy bool) {
		t.Helper()
		selection, err := json.Marshal(subscriptionRollbackSelection{ID: owner.PublicID, Revision: owner.Revision, Spec: spec, Busy: busy})
		if err != nil {
			t.Fatal(err)
		}
		command := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestSDKCLISubscriptionRollbackPeer$")
		command.Env = append(os.Environ(), subscriptionRollbackPeer+"="+string(selection), "GORACE=atexit_sleep_ms=0")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("peer rollback did not match expected busy=%t: %v\n%s", busy, err, output)
		}
	}
	wait := func(label string, ready func() bool) {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for !ready() {
			select {
			case err := <-holds:
				traceBytes, _ := os.ReadFile(trace)
				t.Fatalf("a worker slot was released while waiting for %s (%d/%d occupied): %v (%v)",
					label, bytes.Count(traceBytes, []byte(`"method":"hold-queue"`)), lane, err, errors.Unwrap(err))
			case <-ctx.Done():
				t.Fatalf("timed out waiting for %s", label)
			case <-ticker.C:
			}
		}
	}
	requestContext, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	defer os.WriteFile(executeRelease, nil, 0o600)
	_, canceled := pass(requestContext)
	wait("dispatched gateway execution", func() bool {
		data, err := os.ReadFile(trace)
		return err == nil && bytes.Contains(data, []byte(`"method":"execute"`))
	})
	cancelRequest()
	wait("canceled handler with a returned gateway call", func() bool {
		select {
		case <-canceled:
		default:
			return false
		}
		data, _ := os.ReadFile(trace)
		for line := range bytes.SplitSeq(data, []byte{'\n'}) {
			var event struct {
				Method string
				Scope  sdk.Scope
			}
			if json.Unmarshal(line, &event) == nil && event.Method == "execute-canceled" {
				receipt, err := manager.Store().OperationContext(ctx, owner.Module, event.Scope.Operation)
				return err == nil && receipt.State == "uncertain"
			}
		}
		return false
	})
	peer(true)
	if err := os.WriteFile(executeRelease, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range lane {
		work.Go(func() {
			_, err := manager.Call(ctx, owner.Module, sdk.MethodProviderModels,
				sdk.Scope{Plugin: owner.Module, Provider: owner.Provider, Session: "hold-queue"}, sdk.ModelsRequest{}, nil)
			holds <- err
		})
	}
	wait("occupied worker slots", func() bool {
		data, err := os.ReadFile(trace)
		return err == nil && bytes.Count(data, []byte(`"method":"hold-queue"`)) == lane
	})
	reply, result := pass(ctx)
	wait("queued gateway request", func() bool {
		select {
		case <-result:
			t.Fatalf("gateway request finished before rollback: %d %s", reply.Code, reply.Body.String())
		default:
		}
		dir := filepath.Join(manager.Namespace(), "private", owner.Module, "operations")
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			var operation extensionhost.JournalEntry
			if err == nil && json.Unmarshal(data, &operation) == nil && operation.State == "dispatched" {
				return true
			}
		}
		return false
	})
	peer(false)
	if err := os.WriteFile(queueRelease, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range lane {
		select {
		case err := <-holds:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	select {
	case <-result:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte(`"method":"execute"`)) != 1 || strings.Contains(reply.Body.String(), "SDK executed") {
		t.Fatal("the gateway sent provider.execute to the former owner after peer rollback")
	}
}

func savedSignInFixture(t *testing.T) (*extensionhost.Manager, sdk.Manifest, []byte) {
	t.Helper()
	helpHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	manager := extensionhost.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	manifest := authfixture.Manifest()
	manifest.Window = nil
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.ProviderSignIn, sdk.ProviderSavedSignIn,
		sdk.OutboundExecutor, sdk.UsageReporter)
	manifest.Providers[0].Models = []sdk.Model{{ID: "fixture-model", Protocols: manifest.Providers[0].Protocols}}
	manifest.AuthBindings = []sdk.WindowAuthBindingDescriptor{{
		ID: "saved.signin", From: sdk.ProviderSignInPart, Provider: "vendor",
		Methods: []sdk.WindowAuthMethod{{
			ID: "saved-signin", Mode: "poll", Label: "Import saved sign-in",
			Fields: []sdk.AuthInputField{{Key: "record", Type: "secret", Label: "Saved account"}},
		}},
	}}
	if runtime.GOOS == "windows" {
		manifest.Entry += ".exe"
	}
	dir := t.TempDir()
	command := proc.CommandContext(testenv.DeadlineContext(t, 10*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(dir, manifest.Entry), "../extension/testdata/authworker")
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build auth fixture: %v\n%s", err, output)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	source := []byte(`{"vendor":{"type":"api","key":"PRIVATE-OTHER"},"vendor#selected":{"type":"api","key":"PRIVATE-SAVED"},"foreign":{"type":"api","key":"PRIVATE-FOREIGN"}}`)
	if err := os.MkdirAll(filepath.Dir(plugin.AuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plugin.AuthPath(), source, 0o600); err != nil {
		t.Fatal(err)
	}
	return manager, manifest, source
}

func TestSDKCLIImportsOneCurrentSavedSignIn(t *testing.T) {
	manager, manifest, source := savedSignInFixture(t)
	run := func(operation, value string) (string, error) {
		t.Helper()
		out, err := said(t, func() error {
			return sdkPluginCmd(t.Context(), []string{"auth", operation, manifest.ID, "vendor", value})
		})
		if strings.Contains(out, "PRIVATE") || err != nil && strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("import exposed private data")
		}
		return out, err
	}
	if _, err := run("import", "foreign"); err == nil {
		t.Fatal("accepted a saved sign-in from a different provider")
	}
	if err := os.WriteFile(lastgood.Bak(plugin.AuthPath()), source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plugin.AuthPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("import", "vendor#selected"); err == nil {
		t.Fatal("import used a backup instead of the unreadable current source")
	}
	if err := os.WriteFile(plugin.AuthPath(), source, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := said(t, func() error {
		return sdkPluginCmd(t.Context(), []string{"call", manifest.ID, sdk.MethodAuthStart,
			`{"method":"saved-signin","input":{"record":"PRIVATE-SAVED"}}`, "--scope", `{"provider":"vendor"}`})
	})
	if err == nil || !strings.Contains(err.Error(), "require the local import command") {
		t.Fatal("raw SDK dispatch accepted the reserved import method")
	}
	out, err := run("import", "vendor#selected")
	var prepared struct {
		Ticket  string
		Account sdk.AccountSummary
	}
	if err != nil || json.Unmarshal([]byte(out), &prepared) != nil || prepared.Ticket == "" ||
		prepared.Account.ID != "chosen" || prepared.Account.Name != "reader@example.test" {
		t.Fatalf("saved sign-in preparation returned %q (%v)", out, err)
	}
	accounts, err := manager.Store().Accounts(manifest.ID, "vendor")
	if err != nil || len(accounts) != 0 {
		t.Fatal("preparation published usable credentials")
	}
	if state, err := manager.Store().StateGet(sdk.Scope{Plugin: manifest.ID, Provider: "vendor"}, "fixture.saved-sign-in"); err != nil ||
		state.Version != 0 || len(state.Value) != 0 {
		t.Fatal("verification persisted the source credential outside the expiring ticket")
	}
	out, err = run("activate", prepared.Ticket)
	var summary sdk.AccountSummary
	if err != nil || json.Unmarshal([]byte(out), &summary) != nil || summary.ID != "chosen" || summary.Generation != "1" {
		t.Fatalf("saved sign-in activation returned %q (%v)", out, err)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(provider.Path()), "logins.json"))
	var policies []struct {
		Agent, User string
		On          bool
	}
	if err != nil || json.Unmarshal(data, &policies) != nil {
		t.Fatalf("completed SDK sign-in has no readable account policy: %v", err)
	}
	id := provider.ExtensionProviderID(manifest.ID, "vendor")
	count := 0
	for _, row := range policies {
		if row.Agent == id && row.User == "chosen" && row.On {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("completed SDK sign-in registered %d enabled policies, want 1", count)
	}
	if _, err := run("activate", prepared.Ticket); err == nil {
		t.Fatal("the preparation ticket remained usable after activation")
	}
	if _, err := run("import", "vendor#selected"); err == nil {
		t.Fatal("another import overwrote the selected account")
	}
	accounts, err = manager.Store().AccountsContext(t.Context(), manifest.ID, "vendor")
	if err != nil || len(accounts) != 1 || accounts[0].Generation != 1 || accounts[0].Name != "reader@example.test" {
		t.Fatal("import did not preserve the generation or verified account name")
	}
	if after, err := os.ReadFile(plugin.AuthPath()); err != nil || !bytes.Equal(after, source) {
		t.Fatal("import changed the source account store")
	}
}

func TestAccountRowsRecheckSubscriptionAfterOtherReads(t *testing.T) {
	host, manifest, _ := savedSignInFixture(t)
	prepared, err := provider.ImportSavedPluginAuth(t.Context(), host, manifest.ID, "vendor", "vendor#selected")
	if err != nil {
		t.Fatal(err)
	}
	account, err := provider.ActivateSavedPluginAuth(t.Context(), host, manifest.ID, "vendor", prepared.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	id := provider.ExtensionProviderID(manifest.ID, "vendor")
	scope, ok := provider.ResolveExtensionLogin(id, account.ID)
	if !ok {
		t.Fatal("fixture account is absent from the ordinary login inventory")
	}
	logins := provider.Logins(id)
	alice := provider.LoginUsage(t.Context(), id)[account.ID]
	if len(logins) != 1 || len(alice.Windows) != 1 || alice.Windows[0].Used != 17 {
		t.Fatalf("initial SDK account usage: %+v", alice)
	}

	alreadyRead := make(chan struct{})
	publication := make(chan error, 1)
	provider.LoginUsageVia(func(ctx context.Context, agent string) map[string]provider.SubscriptionQuota {
		if agent == id {
			close(alreadyRead)
			return map[string]provider.SubscriptionQuota{account.ID: alice}
		}
		select {
		case <-alreadyRead:
		case <-ctx.Done():
			publication <- ctx.Err()
			return nil
		}
		_, err := host.Store().InvalidateUsage(scope)
		publication <- err
		return map[string]provider.SubscriptionQuota{"bob": {
			Provider: "codex", User: "bob", Windows: []provider.QuotaWindow{{Name: "quota", Used: 30}},
		}}
	})
	t.Cleanup(func() { provider.LoginUsageVia(nil) })
	rows := accountRows(append(logins, provider.Login{Agent: "codex", User: "bob"}), time.Now())
	select {
	case err := <-publication:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("the second account did not finish")
	}
	if len(rows) != 2 || rows[0].User != account.ID || rows[0].Error == "" || len(rows[0].Windows) != 0 ||
		rows[1].User != "bob" || len(rows[1].Windows) != 1 || rows[1].Windows[0].Used != 30 {
		t.Fatalf("account output did not defer the revoked reading and retain its sibling: %+v", rows)
	}
}

func TestSDKCLITransfersAndRestoresTheWholeSubscription(t *testing.T) {
	manager, manifest, source := savedSignInFixture(t)
	const (
		id          = "vendor"
		selectedKey = "vendor#selected"
		proxy       = "http://127.0.0.1:15493"
	)
	spec := filepath.Join(t.TempDir(), "index.js")
	if err := os.WriteFile(spec, []byte("export default async () => ({})"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(plugin.Settle)
	installCommunity := func() {
		t.Helper()
		if _, err := provider.AddPlugin(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		plugin.UseCached([]plugin.Provider{{ID: id, Spec: spec, Name: "Community vendor"}})
		provider.Changed()
	}
	installCommunity()
	if logins := provider.Logins(id); len(logins) != 2 {
		t.Fatalf("community accounts: %d", len(logins))
	}
	rawLogins, err := os.ReadFile(filepath.Join(filepath.Dir(provider.Path()), "logins.json"))
	var sourceLogins []struct{ Agent, Home, User string }
	if err != nil || json.Unmarshal(rawLogins, &sourceLogins) != nil {
		t.Fatalf("read community account metadata: %v", err)
	}
	users := make(map[string]string)
	for _, login := range sourceLogins {
		if login.Agent == "plugin:"+id {
			users[login.Home] = login.User
		}
	}
	selected, other := users[selectedKey], users[id]
	if selected == "" || other == "" || selected == other {
		t.Fatal("fixture did not generate distinct public account names")
	}
	order := []string{selected, other}
	if err := provider.SetAccountOrder(id, order); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetLoginOn(id, other, false); err != nil {
		t.Fatal(err)
	}
	models := []string{"fixture-model"}
	if err := provider.SetAccountModels(id, selected, models); err != nil {
		t.Fatal(err)
	}
	before, err := provider.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	before.AccountProxies = map[string]string{selected: proxy}
	if err := provider.Save(*before); err != nil {
		t.Fatal(err)
	}
	accounts := before.AccountIDs()
	allowed, err := provider.ResolveAccount(id, selected)
	if err != nil {
		t.Fatal(err)
	}
	checkProjection := func(active bool) {
		t.Helper()
		current, err := provider.Find(id)
		logins := provider.Logins(id)
		if err != nil || current.IsExtension() != active || current.IsPlugin() == active ||
			!slices.Equal(current.AccountIDs(), accounts) ||
			len(logins) != 2 || logins[0].User != selected || !logins[0].On || !logins[0].Active ||
			logins[1].User != other || logins[1].On {
			t.Fatalf("wrong subscription or account policy after transition: %v", err)
		}
		ref, err := provider.ResolveAccount(id, selected)
		if err != nil || ref != allowed {
			t.Fatalf("account access reference changed: %v", err)
		}
		_, savedModels, err := provider.AccountModelsOf(id, selected)
		if err != nil || !slices.Equal(savedModels, models) || current.AccountServes(selected, "outside-allowlist") ||
			netproxy.Choice(current.Via(t.Context())) != proxy {
			t.Fatalf("account model or proxy policy changed: %v", err)
		}
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		output, err := said(t, func() error {
			return sdkPluginCmd(t.Context(), append([]string{"auth"}, args...))
		})
		if strings.Contains(output, "PRIVATE-") || err != nil && strings.Contains(err.Error(), "PRIVATE-") {
			t.Fatal("CLI printed private authentication data")
		}
		return output, err
	}
	coordination := t.TempDir()
	trace := filepath.Join(coordination, "auth.jsonl")
	queueRelease := filepath.Join(coordination, "queue-release")
	executeRelease := filepath.Join(coordination, "execute-release")
	started, release := filepath.Join(coordination, "refresh-started"), filepath.Join(coordination, "refresh-release")
	config, err := json.Marshal(authfixture.Config{Trace: trace, RefreshStarted: started, RefreshRelease: release,
		RefreshOutcome: "not-started", QueueRelease: queueRelease, ExecuteRelease: executeRelease})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
		t.Fatal(err)
	}
	startSignIn := func(flags ...string) string {
		t.Helper()
		output, err := said(t, func() error {
			return sdkPluginCmd(t.Context(), append([]string{"auth", "start", manifest.ID, id, `{"method":"browser"}`}, flags...))
		})
		var result sdk.AuthStartResult
		if err != nil || json.Unmarshal([]byte(output), &result) != nil || len(result.Continuation) == 0 {
			t.Fatalf("start CLI sign-in: %v", err)
		}
		input, err := json.Marshal(sdk.AuthFinishRequest{Continuation: result.Continuation})
		if err != nil {
			t.Fatal(err)
		}
		return string(input)
	}
	beforeTransfer := startSignIn()
	prepare := func() []string {
		t.Helper()
		var tickets []string
		for _, key := range []string{selectedKey, id} {
			output, err := run("import", manifest.ID, id, key)
			var prepared extensionhost.PreparedSignIn
			if err != nil || json.Unmarshal([]byte(output), &prepared) != nil || prepared.Ticket == "" {
				t.Fatalf("prepare account: %v", err)
			}
			tickets = append(tickets, prepared.Ticket)
		}
		return tickets
	}
	tickets := prepare()
	checkProjection(false)
	transfer := []string{"transfer", manifest.ID, id}
	if output, err := run(append(slices.Clone(transfer), tickets[0])...); err == nil || output != "" {
		t.Fatal("partial transfer changed the owner")
	}
	transfer = append(transfer, tickets...)
	changed := bytes.Replace(source, []byte("PRIVATE-SAVED"), []byte("PRIVATE-CHANGED"), 1)
	if err := os.WriteFile(plugin.AuthPath(), changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run(transfer...); err == nil || output != "" {
		t.Fatal("changed source was transferred")
	}
	if err := os.WriteFile(plugin.AuthPath(), source, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := run(transfer...)
	var owner extensionhost.SubscriptionOwner
	if err != nil || json.Unmarshal([]byte(output), &owner) != nil || !owner.Active ||
		owner.Pending || owner.PublicID != id || owner.Revision == "" || len(owner.Accounts) != 2 {
		t.Fatalf("transfer receipt: %v", err)
	}
	checkProjection(true)
	beforeFinish, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := run("finish", manifest.ID, id, beforeTransfer); err == nil || output != "" {
		t.Fatal("CLI sign-in crossed the subscription owner transition")
	}
	if output, err := said(t, func() error {
		return sdkPluginCmd(t.Context(), []string{"call", manifest.ID, sdk.MethodAuthFinish, beforeTransfer, "--scope", `{"provider":"vendor"}`})
	}); err == nil || !strings.Contains(err.Error(), "use magpie plugin sdk auth") || output != "" {
		t.Fatal("generic SDK call bypassed the transferred subscription's sign-in flow")
	}
	if after, err := os.ReadFile(trace); err != nil || !bytes.Equal(beforeFinish, after) {
		t.Fatal("the stale CLI continuation reached the SDK worker")
	}
	if err := plugin.SignOut(t.Context(), id, ""); err == nil {
		t.Fatal("legacy sign-out deleted the rollback source")
	}
	if output, err := run("owners"); err != nil || !strings.Contains(output, owner.Revision) {
		t.Fatalf("CLI did not report the active owner: %v", err)
	}
	report := provider.RefreshReport(t.Context(), id, selected, time.Now())
	if !slices.ContainsFunc(report, func(quota provider.Quota) bool {
		return quota.Provider == id && quota.User == selected && quota.Error == "" &&
			len(quota.Windows) == 1 && quota.Windows[0].Used == 17
	}) {
		t.Fatalf("usage did not use the imported account under its public name: %+v", report)
	}
	if err := provider.RemovePlugin(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	checkProjection(true)
	if output, err := run("rollback", id, owner.Revision); err == nil || output != "" {
		t.Fatal("rollback activated an absent community package")
	}
	installCommunity()
	if err := provider.SetPluginOff(t.Context(), spec, true); err != nil {
		t.Fatal(err)
	}
	if output, err := run("rollback", id, owner.Revision); err == nil || output != "" {
		t.Fatal("rollback activated a disabled community package")
	}
	if err := provider.SetPluginOff(t.Context(), spec, false); err != nil {
		t.Fatal(err)
	}
	listPath := filepath.Join(filepath.Dir(plugin.AuthPath()), "plugins.json")
	list, err := os.ReadFile(listPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lastgood.Bak(listPath), list, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(listPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin.UseCached([]plugin.Provider{{ID: id, Spec: spec, Name: "Community vendor"}})
	if output, err := run("rollback", id, owner.Revision); err == nil || output != "" {
		t.Fatal("rollback accepted a community package from a backup of the unreadable installed list")
	}
	if err := os.WriteFile(listPath, list, 0o600); err != nil {
		t.Fatal(err)
	}
	plugin.UseCached([]plugin.Provider{{ID: id, Spec: spec, Name: "Community vendor"}})
	provider.Changed()
	checkProjection(true)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	scope, ok := provider.ResolveExtensionLogin(id, selected)
	if !ok || scope.Account != "chosen" || scope.Generation != 1 {
		t.Fatal("public account does not resolve to its imported generation")
	}
	done := make(chan struct{})
	var refreshErr error
	go func() {
		defer close(done)
		_, refreshErr = provider.RefreshExtensionAuth(ctx, scope)
	}()
	defer func() {
		_ = os.WriteFile(release, nil, 0o600)
		cancel()
		<-done
	}()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatalf("refresh terminated before dispatch: %v", refreshErr)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	output, err = said(t, func() error {
		commandHost := extensionhost.NewManager(manager.Namespace())
		return sdkPluginRun(t.Context(), []string{"auth", "rollback", id, owner.Revision}, commandHost, os.Stdout, sdkCLIAuth{
			rollback: func(ctx context.Context, publicID, revision string) (extensionhost.SubscriptionOwner, error) {
				return provider.RollbackSavedPluginSubscription(ctx, commandHost, publicID, revision)
			},
		})
	})
	if err == nil || output != "" {
		t.Fatal("rollback switched owners during a dispatched credential refresh")
	}
	checkProjection(true)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-done
	account, err := manager.Store().GetAccount(manifest.ID, id, scope.Account)
	var outcome *extensionhost.OutcomeError
	if !errors.As(refreshErr, &outcome) || outcome.Outcome != "not-started" || err != nil || account.Generation != 1 {
		t.Fatalf("refresh did not prove unchanged credentials: outcome %v, generation %d, read error %v", outcome, account.Generation, err)
	}
	checkSubscriptionRollbackAdmission(t, owner, spec, trace, queueRelease, executeRelease)
	output, err = run("rollback", id, owner.Revision)
	var rollback extensionhost.SubscriptionOwner
	if err != nil || json.Unmarshal([]byte(output), &rollback) != nil || rollback.Active ||
		rollback.Pending || rollback.Revision == "" || rollback.Revision == owner.Revision {
		t.Fatalf("rollback receipt: %v", err)
	}
	checkProjection(false)
	if output, err := run("rollback", id, owner.Revision); err != nil || !strings.Contains(output, rollback.Revision) {
		t.Fatalf("retry did not return the already committed rollback: %v", err)
	}
	if output, err := run("rollback", id, "unrelated-revision"); err == nil || output != "" {
		t.Fatal("rollback accepted an unrelated owner revision")
	}
	if current, err := os.ReadFile(plugin.AuthPath()); err != nil || !bytes.Equal(current, source) {
		t.Fatal("transfer or rollback changed the community credentials")
	}

	rotated := bytes.Replace(source, []byte("PRIVATE-SAVED"), []byte("PRIVATE-ROTATED-SAVED"), 1)
	if err := os.WriteFile(plugin.AuthPath(), rotated, 0o600); err != nil {
		t.Fatal(err)
	}
	checkProjection(false)
	output, err = run(append([]string{"transfer", manifest.ID, id}, prepare()...)...)
	if err != nil || json.Unmarshal([]byte(output), &owner) != nil || !owner.Active || owner.Revision == rollback.Revision {
		t.Fatalf("second transfer receipt: %v", err)
	}
	checkProjection(true)
	replaced, err := manager.Store().GetAccount(manifest.ID, id, scope.Account)
	if err != nil || replaced.Generation != 2 || string(replaced.Auth) != `{"token":"PRIVATE-ROTATED"}` {
		t.Fatal("the rotated sign-in did not replace the hidden SDK credentials")
	}
	if unchanged, err := manager.Store().GetAccount(manifest.ID, id, "other"); err != nil || unchanged.Generation != 1 {
		t.Fatal("retransfer changed an account whose credentials had not rotated")
	}
	flags := []string{"--scope", `{"account":"chosen"}`}
	current := startSignIn(flags...)
	if output, err := run(append([]string{"finish", manifest.ID, id, current}, flags...)...); err != nil || !strings.Contains(output, `"id": "chosen"`) {
		t.Fatalf("complete CLI sign-in under its original owner: %v", err)
	}
	if output, err := run("rollback", id, owner.Revision); err == nil || output != "" {
		t.Fatal("rollback reused community credentials after dispatching a new sign-in")
	}
	checkProjection(true)
}

func TestSDKCLIImportUsesTheSelectedSourceAccountProxy(t *testing.T) {
	manager, manifest, _ := savedSignInFixture(t)
	logins := []byte(`[
		{"agent":"plugin:vendor","home":"vendor","user":"Other","on":true},
		{"agent":"plugin:vendor","home":"vendor#selected","user":"Selected","on":true}
	]`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(provider.Path()), "logins.json"), logins, 0o600); err != nil {
		t.Fatal(err)
	}
	const selected = "http://127.0.0.1:15493"
	settings, err := json.Marshal(struct {
		Providers []provider.Provider `json:"providers"`
	}{[]provider.Provider{
		{ID: "vendor", Proxy: "direct", AccountProxies: map[string]string{
			"other": "direct", "selected": selected,
		}},
		{ID: provider.ExtensionProviderID(manifest.ID, "vendor"), Proxy: "direct"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provider.Path(), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(authfixture.Config{ImportProxy: selected})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
		t.Fatal(err)
	}
	out, err := said(t, func() error {
		return sdkPluginCmd(t.Context(), []string{"auth", "import", manifest.ID, "vendor", "vendor#selected"})
	})
	var prepared extensionhost.PreparedSignIn
	if err != nil || json.Unmarshal([]byte(out), &prepared) != nil || prepared.Account.ID != "chosen" {
		t.Fatalf("saved sign-in did not verify through the selected source account proxy: %v", err)
	}
}

func TestSDKSavedSignInDoesNotPublishAnAlternateSubscription(t *testing.T) {
	manager, manifest, source := savedSignInFixture(t)
	const spec = "@example/opencode-vendor-auth"
	installed, err := json.Marshal(plugin.List{Plugins: []plugin.Entry{{Spec: spec, Off: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plugin.ListPath(), installed, 0o600); err != nil {
		t.Fatal(err)
	}
	plugin.UseCached([]plugin.Provider{{ID: "vendor", Spec: spec}})
	t.Cleanup(func() { plugin.UseCached(nil) })
	prepared, err := provider.ImportSavedPluginAuth(t.Context(), manager, manifest.ID, "vendor", "vendor#selected")
	if err != nil {
		t.Fatal(err)
	}
	out, err := said(t, func() error {
		return sdkPluginCmd(t.Context(), []string{"auth", "activate", manifest.ID, "vendor", prepared.Ticket})
	})
	if err == nil || !strings.Contains(err.Error(), "requires a subscription owner transfer") || out != "" {
		t.Fatalf("activation published an alternate subscription while the source can still serve: %v", err)
	}
	accounts, err := manager.Store().AccountsContext(t.Context(), manifest.ID, "vendor")
	if err != nil || len(accounts) != 0 {
		t.Fatal("refused activation published SDK credentials")
	}
	current, err := os.ReadFile(plugin.AuthPath())
	if err != nil || !bytes.Equal(current, source) {
		t.Fatal("refused activation changed the community credentials")
	}
}

func TestSDKSavedSignInReportsPublishedAccountWhenTicketCleanupFails(t *testing.T) {
	manager, manifest, _ := savedSignInFixture(t)
	prepared, err := provider.ImportSavedPluginAuth(t.Context(), manager, manifest.ID, "vendor", "vendor#selected")
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(filepath.Dir(plugin.AuthPath()), "extensions", "private")
	ticketPath := filepath.Join(private, ".saved-signins", "records.json")
	backupPath := ticketPath + ".test-backup"
	source := func(ctx context.Context, providerID, key string, use func(json.RawMessage) error) error {
		if err := plugin.WithSavedAuth(ctx, providerID, key, use); err != nil {
			return err
		}
		if err := os.Rename(ticketPath, backupPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(ticketPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ticketPath, "occupied"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	var output bytes.Buffer
	err = sdkCLIAuthCommand(t.Context(), []string{"activate", manifest.ID, "vendor", prepared.Ticket}, manager, sdkCLIAuth{
		activate: func(ctx context.Context, moduleID, providerID, ticket string) (sdk.AccountSummary, error) {
			return manager.ActivateSavedSignIn(ctx, moduleID, providerID, ticket, source, func(sdk.AccountSummary) error { return nil })
		},
	}, &output)
	var receipt struct {
		sdk.AccountSummary
		Warning string `json:"warning"`
	}
	if err != nil || json.Unmarshal(output.Bytes(), &receipt) != nil || receipt.ID != "chosen" ||
		receipt.Generation != "1" || receipt.Warning == "" || strings.Contains(output.String(), "PRIVATE") {
		t.Fatalf("activation did not report its committed account and cleanup failure: %v", err)
	}
	if err := os.Remove(filepath.Join(ticketPath, "occupied")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ticketPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backupPath, ticketPath); err != nil {
		t.Fatal(err)
	}
	reopened := extensionhost.NewManager(filepath.Dir(private))
	account, err := provider.ActivateSavedPluginAuth(t.Context(), reopened, manifest.ID, "vendor", prepared.Ticket)
	if err != nil || account.ID != receipt.ID || account.Generation != receipt.Generation {
		t.Fatalf("retry after reopening did not acknowledge the same account: %v", err)
	}
	if _, err := os.Stat(ticketPath); !os.IsNotExist(err) {
		t.Fatal("successful retry retained the ticket and credential copy")
	}
}

func TestSDKSavedSignInActivationRechecksSelection(t *testing.T) {
	for _, change := range []string{"source", "lock", "config", "enablement", "generation"} {
		t.Run(change, func(t *testing.T) {
			manager, manifest, source := savedSignInFixture(t)
			prepared, err := provider.ImportSavedPluginAuth(t.Context(), manager, manifest.ID, "vendor", "vendor#selected")
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "source":
				if err := plugin.SignOut(t.Context(), "vendor", "vendor#selected"); err != nil {
					t.Fatal(err)
				}
			case "lock":
				lock := plugin.AuthPath() + ".lock"
				if err := os.WriteFile(lock, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(lock) })
			case "config":
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0",
					json.RawMessage(`{"changed":true}`)); err != nil {
					t.Fatal(err)
				}
			case "enablement":
				for _, enabled := range []bool{false, true} {
					if err := manager.SetEnabled(t.Context(), manifest.ID, enabled); err != nil {
						t.Fatal(err)
					}
				}
			case "generation":
				account, err := manager.Store().SaveAccount(extensionhost.Account{Plugin: manifest.ID, Provider: "vendor",
					ID: prepared.Account.ID, Auth: json.RawMessage(`{"token":"DIFFERENT"}`)}, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := manager.Store().DeleteAccount(manifest.ID, "vendor", account.ID, account.Generation); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := provider.ActivateSavedPluginAuth(t.Context(), manager, manifest.ID, "vendor", prepared.Ticket); err == nil {
				t.Fatal("activation accepted a stale or occupied selection")
			}
			accounts, err := manager.Store().Accounts(manifest.ID, "vendor")
			if err != nil || len(accounts) != 0 {
				t.Fatal("activation published a revoked sign-in")
			}
			if change != "source" {
				if after, err := os.ReadFile(plugin.AuthPath()); err != nil || !bytes.Equal(after, source) {
					t.Fatal("rejected activation changed the legacy sign-ins")
				}
			}
		})
	}
}

func TestSDKSavedSignInRejectsIllFormedRecordBeforeVerification(t *testing.T) {
	manager, manifest, source := savedSignInFixture(t)
	trace := filepath.Join(t.TempDir(), "calls.jsonl")
	config, _ := json.Marshal(authfixture.Config{Trace: trace})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
		t.Fatal(err)
	}
	body := bytes.Replace(source, []byte(`"PRIVATE-SAVED"`), []byte{'"', 0x80, '"'}, 1)
	if err := os.WriteFile(plugin.AuthPath(), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ImportSavedPluginAuth(t.Context(), manager, manifest.ID, "vendor", "vendor#selected"); err == nil {
		t.Fatal("import accepted invalid UTF-8 in the saved record")
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("invalid UTF-8 reached the provider verifier")
	}
}

func TestSDKSavedSignInSourceRevokedDuringVerification(t *testing.T) {
	testSavedSignInPublication(t, func(t *testing.T) {
		if err := plugin.SignOut(t.Context(), "vendor", "vendor#selected"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSDKSavedSignInRefusesPublicationDuringSourceWrite(t *testing.T) {
	testSavedSignInPublication(t, func(t *testing.T) {
		lock := plugin.AuthPath() + ".lock"
		file, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(lock) })
	})
}

func testSavedSignInPublication(t *testing.T, beforePublish func(*testing.T)) {
	t.Helper()
	manager, manifest, _ := savedSignInFixture(t)
	coordination := t.TempDir()
	started, release := filepath.Join(coordination, "started"), filepath.Join(coordination, "release")
	config, _ := json.Marshal(authfixture.Config{Started: started, Release: release})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	done := make(chan struct{})
	var importErr error
	var prepared extensionhost.PreparedSignIn
	go func() {
		defer close(done)
		prepared, importErr = provider.ImportSavedPluginAuth(ctx, manager, manifest.ID, "vendor", "vendor#selected")
	}()
	defer func() {
		cancel()
		<-done
	}()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatal("verification ended before entering the worker")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	beforePublish(t)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-done
	accounts, err := manager.Store().Accounts(manifest.ID, "vendor")
	if importErr == nil || prepared.Ticket != "" || strings.Contains(importErr.Error(), "PRIVATE") || err != nil || len(accounts) != 0 {
		t.Fatal("a revoked or locked saved sign-in published an account or exposed private data")
	}
	if state, err := manager.Store().StateGet(sdk.Scope{Plugin: manifest.ID, Provider: "vendor"}, "fixture.saved-sign-in"); err != nil ||
		state.Version != 0 || len(state.Value) != 0 {
		t.Fatal("refused preparation retained the verification state")
	}
}
