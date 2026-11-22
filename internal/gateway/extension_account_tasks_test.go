package gateway

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func taskGatewayFixture(t *testing.T, mode string, private ...string) (*Server, *extensionhost.Manager, string, <-chan sdk.Scope, *atomic.Int32) {
	t.Helper()
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	paid := make(chan sdk.Scope, 8)
	count := new(atomic.Int32)
	vendor := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if mode == "snapshots" && request.Method == http.MethodPost && request.URL.Path == "/snapshots-ready" {
			var scope sdk.Scope
			if json.NewDecoder(request.Body).Decode(&scope) != nil {
				writer.WriteHeader(400)
				return
			}
			paid <- scope
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != "/paid" {
			http.NotFound(writer, request)
			return
		}
		var input struct {
			Scope sdk.Scope              `json:"scope"`
			Task  sdk.AccountTaskRequest `json:"task"`
			Token string                 `json:"token"`
		}
		wantGeneration := uint64(1)
		if mode == "refresh" {
			wantGeneration = 2
		}
		if json.NewDecoder(request.Body).Decode(&input) != nil || input.Scope.Account != "selected" ||
			input.Scope.Generation != wantGeneration || input.Task.Model != "m1" || input.Token != "synthetic" ||
			input.Task.Job != "opaque-fixture-template" && (mode != "chain" || input.Task.Job != "opaque-fixture-template-next") ||
			input.Task.DueAt <= 0 {
			t.Error("task escaped its actual selected authority", input)
			writer.WriteHeader(400)
			return
		}
		count.Add(1)
		if mode != "snapshots" {
			paid <- input.Scope
		}
		if mode == "snapshots" {
			for _, output := range []int64{2, 3} {
				inputTokens, cached, cacheWrite, cacheHour := int64(23), int64(19), int64(5), int64(2)
				response := "native-response"
				if err := json.NewEncoder(writer).Encode(sdk.OwnCall{Kind: "cache_keepalive", Model: "m1", Status: 200,
					Usage: sdk.Usage{Facts: &sdk.ExecutionUsageFacts{ExecutionCounts: sdk.ExecutionCounts{
						InputTokens: &inputTokens, OutputTokens: &output, CachedTokens: &cached,
						CacheWriteTokens: &cacheWrite, CacheWrite1hTokens: &cacheHour}, ResponseID: &response}}}); err != nil {
					return
				}
				writer.(http.Flusher).Flush()
			}
			<-request.Context().Done()
			return
		}
		if mode == "hold" || mode == "hold-usage-deferred" {
			<-request.Context().Done()
			return
		}
		_ = json.NewEncoder(writer).Encode(sdk.OwnCall{Kind: "cache_keepalive", Model: "m1",
			Status: 200, Usage: sdk.Usage{InputTokens: 7, OutputTokens: 1}})
	}))
	t.Cleanup(vendor.Close)
	manager := extensionhost.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	directory, entry := t.TempDir(), "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o",
		filepath.Join(directory, entry), "../extension/testdata/taskworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build task fixture: %v\n%s", err, output)
	}
	raw := []byte(`{"schema":1,"id":"tasks.fixture","version":"1.0.0","runtime":"exec",
		"contract":{"version":{"major":1,"minor":9},"capabilities":["outbound-executor","account-auth","module-rpc","usage-reporter"]},
		"providers":[{"id":"vendor","name":"Fixture","protocols":["chat"],
			"models":[{"id":"m1","protocols":["chat"]}],"accountTasks":[{"id":"ping","action":"paid.ping"}]}],
		"actions":[{"id":"paid.ping","effect":"write","inputSchema":{"type":"object"}}]}`)
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Entry = entry
	if len(private) != 0 {
		manifest.Actions[0].Component = "paid"
		service := sdk.ServiceDescriptor{ID: "paid", Runtime: "exec", Entry: entry, Args: []string{"paid"},
			Contract: sdk.ServiceContract{ID: "paid", Version: 1}, Methods: []string{"paid.ping"}, Start: "lazy"}
		if private[0] == "js" {
			if runtime.GOOS == "windows" {
				t.Skip("Node executable fixture requires a Unix interpreter directive")
			}
			if _, err := exec.LookPath("node"); err != nil {
				t.Skip("Node executable fixture requires Node on PATH")
			}
			script, err := os.ReadFile("../extension/testdata/taskworker/sidecar.mjs")
			if err != nil {
				t.Fatal(err)
			}
			service.Entry, service.Args = "sidecar.mjs", nil
			if err := os.WriteFile(filepath.Join(directory, service.Entry), script, 0700); err != nil {
				t.Fatal(err)
			}
			sdkDirectory := filepath.Join(directory, "sdk")
			if err := os.Mkdir(sdkDirectory, 0700); err != nil {
				t.Fatal(err)
			}
			files, err := os.ReadDir("../../sdk/js")
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if file.IsDir() || filepath.Ext(file.Name()) != ".js" && file.Name() != "package.json" {
					continue
				}
				raw, err := os.ReadFile(filepath.Join("../../sdk/js", file.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sdkDirectory, file.Name()), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		manifest.Services = []sdk.ServiceDescriptor{service}
	}
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(map[string]string{"url": vendor.URL, "token": "synthetic", "mode": mode})
	account, err := manager.Store().SaveAccount(extensionhost.Account{Plugin: manifest.ID, Provider: "vendor",
		ID: "selected", Auth: auth}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.RegisterExtensionLogin(t.Context(), sdk.Scope{
		Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ExtensionProviders(); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	server := New()
	t.Cleanup(server.idle.stop)
	return server, manager, provider.ExtensionProviderID(manifest.ID, "vendor"), paid, count
}

func scheduleGatewayTask(t *testing.T, server *Server, id string, ctx context.Context, message ...string) string {
	t.Helper()
	content := "schedule"
	if len(message) > 0 {
		content = message[0]
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+id+`/m1","messages":[{"role":"user","content":"`+content+`"}]}`)).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:12442"
	reply := httptest.NewRecorder()
	server.Front().ServeHTTP(reply, request)
	if reply.Code != 200 {
		t.Fatalf("ordinary task origin: %d %s", reply.Code, reply.Body)
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(reply.Body.Bytes(), &response) != nil || len(response.Choices) != 1 {
		t.Fatal(reply.Body)
	}
	return response.Choices[0].Message.Content
}

func taskReceipt(t *testing.T, manager *extensionhost.Manager, operation, state string) extensionhost.JournalEntry {
	t.Helper()
	var receipt extensionhost.JournalEntry
	waitFor(t, "paid task receipt "+state, func() bool {
		var err error
		receipt, err = manager.Store().Operation("tasks.fixture", operation)
		return err == nil && receipt.State == state
	})
	return receipt
}

func TestExtensionAccountTaskStopsOnCurrentSelectionChanges(t *testing.T) {
	for _, change := range []string{"account", "configuration", "disabled", "off", "model", "disabled-model", "unknown-policy", "foreground", "shutdown"} {
		t.Run(change, func(t *testing.T) {
			server, manager, id, _, count := taskGatewayFixture(t, "")
			cancel, done := serveKeepAliveGateway(t, server)
			waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
			operation := scheduleGatewayTask(t, server, id, t.Context())
			switch change {
			case "account":
				account, err := manager.Store().GetAccount("tasks.fixture", "vendor", "selected")
				if err != nil {
					t.Fatal(err)
				}
				account.Auth = json.RawMessage(`{"token":"manual-replacement"}`)
				if _, err := manager.Store().SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			case "configuration":
				config, err := manager.ModuleConfig(t.Context(), "tasks.fixture")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), "tasks.fixture", config.Version, json.RawMessage(`{"changed":true}`)); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := manager.SetEnabled(t.Context(), "tasks.fixture", false); err != nil {
					t.Fatal(err)
				}
			case "off":
				if err := provider.SetLoginOn(id, "selected", false); err != nil {
					t.Fatal(err)
				}
			case "model":
				selected, err := provider.Find(id)
				if err != nil {
					t.Fatal(err)
				}
				selected.AccountModels = map[string][]string{"selected": {"other"}}
				if err := provider.Save(*selected); err != nil {
					t.Fatal(err)
				}
			case "disabled-model":
				if err := provider.DisableModels(id + "/m1"); err != nil {
					t.Fatal(err)
				}
			case "unknown-policy":
				if err := os.WriteFile(provider.Path(), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreground":
				scheduleGatewayTask(t, server, id, t.Context(), "foreground")
			case "shutdown":
				cancel()
			}
			taskReceipt(t, manager, operation, "not-started")
			if count.Load() != 0 {
				t.Fatal("revoked task paid vendor", change, count.Load())
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtensionAccountTaskFreshQuotaAndUsageEvidence(t *testing.T) {
	for _, mode := range []string{"quota-exhausted", "quota-unknown", "missing-usage", "uncertain", "refresh"} {
		t.Run(mode, func(t *testing.T) {
			server, manager, id, _, count := taskGatewayFixture(t, mode)
			cancel, done := serveKeepAliveGateway(t, server)
			waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
			operation := scheduleGatewayTask(t, server, id, t.Context())
			if mode == "refresh" {
				scope := sdk.Scope{Plugin: "tasks.fixture", Provider: "vendor", Account: "selected", Generation: 1}
				if _, err := manager.RefreshAccount(t.Context(), scope.Plugin, scope); err != nil {
					t.Fatal(err)
				}
			}
			state := "uncertain"
			expectedCalls, expectedRows := int32(1), 1
			switch mode {
			case "quota-exhausted", "quota-unknown":
				state, expectedCalls, expectedRows = "not-started", 0, 0
			case "missing-usage":
				expectedRows = 0
			case "refresh":
				state = "completed"
			}
			receipt := taskReceipt(t, manager, operation, state)
			if count.Load() != expectedCalls {
				t.Fatal("quota/evidence admission paid wrong count", count.Load())
			}
			rows := 0
			for _, row := range usage.Load(time.Time{}) {
				if row.Kind == "cache_keepalive" {
					rows++
				}
			}
			if rows != expectedRows {
				t.Fatal("paid task lost or duplicated normal billing", rows, expectedRows)
			}
			if mode == "refresh" && receipt.Scope.Generation != 2 {
				t.Fatal("refresh did not carry its actual receipt generation", receipt)
			}
			if expectedCalls > 0 {
				input, _ := json.Marshal(sdk.ModuleCallRequest{Action: "paid.ping", Input: json.RawMessage(`{}`)})
				if _, err := manager.Call(t.Context(), "tasks.fixture", sdk.MethodModuleCall, receipt.Scope, json.RawMessage(input), nil); err == nil {
					t.Fatal("terminal task replayed through ordinary module.call")
				}
				if count.Load() != expectedCalls {
					t.Fatal("terminal intent paid the vendor twice")
				}
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtensionAccountTaskInFlightCancellationJoinsAcceptedWrite(t *testing.T) {
	server, manager, id, paid, count := taskGatewayFixture(t, "hold")
	cancel, done := serveKeepAliveGateway(t, server)
	waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
	operation := scheduleGatewayTask(t, server, id, t.Context())
	select {
	case <-paid:
	case <-time.After(10 * time.Second):
		t.Fatal("accepted paid task did not reach synthetic vendor")
	}
	scheduleGatewayTask(t, server, id, t.Context(), "foreground")
	taskReceipt(t, manager, operation, "uncertain")
	if count.Load() != 1 {
		t.Fatal("foreground cancellation retried paid work", count.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExtensionAccountTaskDeferredUsageCannotAdmitForeground(t *testing.T) {
	server, manager, id, paid, count := taskGatewayFixture(t, "hold-usage-deferred")
	cancel, done := serveKeepAliveGateway(t, server)
	waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
	operation := scheduleGatewayTask(t, server, id, t.Context())
	select {
	case <-paid:
	case <-time.After(10 * time.Second):
		t.Fatal("accepted paid task did not reach synthetic vendor")
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+id+`/m1","messages":[{"role":"user","content":"foreground"}]}`))
	request.RemoteAddr = "127.0.0.1:12442"
	reply := httptest.NewRecorder()
	server.Front().ServeHTTP(reply, request.WithContext(t.Context()))
	if reply.Code != 502 || !strings.Contains(reply.Body.String(), "fresh account usage acknowledgment is required") {
		t.Fatal("deferred usage bypassed the ordinary write barrier", reply.Code, reply.Body)
	}
	scope := taskReceipt(t, manager, operation, "accepted").Scope
	if err := manager.Store().UsageReady(scope); err == nil {
		t.Fatal("deferred usage acknowledged dirty account state")
	}
	if count.Load() != 1 {
		t.Fatal("deferred usage dispatched another paid task", count.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	taskReceipt(t, manager, operation, "uncertain")
}

func TestExtensionAccountTaskCancellationKeepsLastMeasuredFactsOnce(t *testing.T) {
	for _, change := range []string{"foreground", "account", "configuration", "shutdown", "owner"} {
		t.Run(change, func(t *testing.T) {
			server, manager, id, paid, count := taskGatewayFixture(t, "snapshots")
			observer := &fakeObserver{}
			t.Cleanup(usage.Observe(observer))
			cancel, done := serveKeepAliveGateway(t, server)
			waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
			secret, err := access.Update("add-key", access.Change{Name: "snapshot-task-key"})
			if err != nil {
				t.Fatal(err)
			}
			who, err := access.Check(secret)
			if err != nil {
				t.Fatal(err)
			}
			ctx := access.WithIdentity(t.Context(), who)
			operation := scheduleGatewayTask(t, server, id, ctx)
			var selected sdk.Scope
			select {
			case selected = <-paid:
				if selected.Operation != operation || selected.Generation != 1 {
					t.Fatal("usage barrier belongs to another paid tick", selected)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("task did not acknowledge both measured usage snapshots")
			}
			for _, row := range usage.Load(time.Time{}) {
				if row.Kind == "cache_keepalive" {
					t.Fatal("intermediate snapshots appended before task termination", row)
				}
			}
			switch change {
			case "foreground":
				scheduleGatewayTask(t, server, id, ctx, "foreground")
			case "account":
				account, err := manager.Store().GetAccount("tasks.fixture", "vendor", "selected")
				if err != nil {
					t.Fatal(err)
				}
				account.Auth = json.RawMessage(`{"token":"manual-replacement"}`)
				if _, err := manager.Store().SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			case "configuration":
				config, err := manager.ModuleConfig(t.Context(), "tasks.fixture")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), "tasks.fixture",
					config.Version, json.RawMessage(`{"changed":true}`)); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				cancel()
			case "owner":
				saveTaskSubscriptionOwner(t, manager, selected, "saved-vendor", "saved-user", "saved-login")
			}
			taskReceipt(t, manager, operation, "uncertain")
			var rows []usage.Record
			for _, row := range usage.Load(time.Time{}) {
				if row.Kind == "cache_keepalive" {
					rows = append(rows, row)
				}
			}
			if len(rows) != 1 || count.Load() != 1 {
				t.Fatal("cancellation lost measured accounting or replayed paid work", rows, count.Load())
			}
			row := rows[0]
			if row.Input != 23 || row.Output != 3 || row.CacheRead != 19 || row.CacheWrite != 5 ||
				row.CacheWrite1h != 2 || row.UpstreamResponseID != "native-response" ||
				row.ResponseID != "" || row.CallerKeyID != who.KeyID || row.Operation != operation ||
				row.Provider != id || row.ProviderAccount != selected.Account {
				t.Fatal("cancellation changed the last measured facts or their original caller", row)
			}
			observer.mu.Lock()
			observed := 0
			for _, row := range observer.rows {
				if row.Kind == "cache_keepalive" {
					observed++
				}
			}
			observer.mu.Unlock()
			if observed != 1 {
				t.Fatal("paid cancellation did not reach the ordinary ledger observer once", observed)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtensionAccountTaskOrdinaryLifecycleAndAppendOnce(t *testing.T) {
	server, manager, id, paid, count := taskGatewayFixture(t, "duplicate")
	cancel, done := serveKeepAliveGateway(t, server)
	waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
	operation := scheduleGatewayTask(t, server, id, t.Context())
	if !strings.HasPrefix(operation, "account-task-") {
		t.Fatal("host returned no durable task identity", operation)
	}
	select {
	case <-paid:
	case <-time.After(10 * time.Second):
		t.Fatal("ordinary gateway did not dispatch its selected paid task")
	}
	waitFor(t, "ordinary task accounting", func() bool {
		rows := usage.Load(time.Time{})
		found := 0
		for _, row := range rows {
			if row.Kind == "cache_keepalive" {
				found++
			}
		}
		return found == 1
	})
	receipt, err := manager.Store().Operation("tasks.fixture", operation)
	waitFor(t, "terminal paid receipt", func() bool {
		receipt, err = manager.Store().Operation("tasks.fixture", operation)
		return err == nil && receipt.State == "completed"
	})
	if receipt.Scope.Account != "selected" || count.Load() != 1 {
		t.Fatal("task was duplicated or changed accounts", receipt, count.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func saveTaskSubscriptionOwner(t *testing.T, manager *extensionhost.Manager, scope sdk.Scope, publicProvider, publicUser, stableLogin string) {
	t.Helper()
	owner := extensionhost.SubscriptionOwner{
		Module: scope.Plugin, Provider: scope.Provider, PublicID: publicProvider, Revision: "fixture-owner",
		Active: true, Accounts: []extensionhost.SubscriptionAccount{
			{Key: "saved-key", ID: scope.Account, User: publicUser, LoginID: stableLogin},
		},
	}
	raw, err := json.Marshal([]any{struct {
		extensionhost.SubscriptionOwner
		GrantRevision string            `json:"grantRevision"`
		Digests       map[string]string `json:"digests"`
		Tickets       []string          `json:"tickets"`
	}{owner, strings.Repeat("a", 64), map[string]string{"saved-key": strings.Repeat("b", 64)}, []string{"fixture-ticket"}}})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(manager.Namespace(), "private", ".saved-signins")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "subscription-owners.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
}

func TestExtensionAccountTaskHonorsTransferredAccountACL(t *testing.T) {
	server, manager, _, _, count := taskGatewayFixture(t, "")
	const publicProvider, publicUser, stableLogin = "saved-vendor", "saved-user", "saved-login"
	scope := sdk.Scope{Plugin: "tasks.fixture", Provider: "vendor", Account: "selected", Generation: 1}
	saveTaskSubscriptionOwner(t, manager, scope, publicProvider, publicUser, stableLogin)
	if err := provider.RegisterExtensionLogin(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	if got := provider.LoginID(publicProvider, publicUser); got != stableLogin {
		t.Fatalf("public account did not retain its original login ID: %q", got)
	}
	secret, err := access.Update("add-key", access.Change{Name: "transferred-task"})
	if err != nil {
		t.Fatal(err)
	}
	who, err := access.Check(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.Update("accounts-key", access.Change{Key: who.KeyID, Accounts: []string{publicProvider + "/" + stableLogin}}); err != nil {
		t.Fatal(err)
	}
	who, err = access.Check(secret)
	if err != nil {
		t.Fatal(err)
	}
	cancel, done := serveKeepAliveGateway(t, server)
	waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
	operation := scheduleGatewayTask(t, server, publicProvider, access.WithIdentity(t.Context(), who), "schedule:overdue")
	var receipt extensionhost.JournalEntry
	waitFor(t, "terminal transferred account task", func() bool {
		receipt, err = manager.Store().Operation(scope.Plugin, operation)
		return err == nil && (receipt.State == "completed" || receipt.State == "not-started")
	})
	if receipt.State != "completed" || receipt.Scope.Account != scope.Account || count.Load() != 1 {
		t.Fatalf("public grant did not admit exactly one paid call with a private receipt: %+v, calls=%d", receipt, count.Load())
	}
	var measured int
	for _, row := range usage.Load(time.Time{}) {
		if row.Operation == operation && row.Provider == publicProvider && row.ProviderAccount == publicUser && row.Kind == "cache_keepalive" {
			measured++
		}
	}
	if measured != 1 {
		t.Fatal("transferred paid call did not use its admitted public login", measured)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExtensionAccountTasksReacquireAfterDrain(t *testing.T) {
	server, manager, id, _, count := taskGatewayFixture(t, "")
	previousCtx, stopPrevious := context.WithCancel(t.Context())
	defer stopPrevious()
	previous, err := manager.BeginAccountTasks(previousCtx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := previous.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	barrier := &extensionLedgerDrainBarrier{rows: make(chan usage.Record, 4), release: release, kind: "cache_keepalive"}
	t.Cleanup(usage.Observe(barrier))
	operation := scheduleGatewayTask(t, server, id, t.Context())
	select {
	case row := <-barrier.rows:
		if row.Operation != operation {
			t.Fatalf("another task reached the ledger: %+v", row)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("accepted task did not reach ledger")
	}
	stopPrevious()

	ready := make(chan struct{})
	stop := serveExtensionOwner(t.Context(), "account tasks", func(ctx context.Context) (*extensionhost.AccountTasks, error) {
		owner, err := manager.BeginAccountTasks(ctx)
		if err == nil {
			close(ready)
		}
		return owner, err
	})
	t.Cleanup(stop)
	select {
	case <-ready:
		t.Fatal("successor took ownership while the previous task held accounting")
	default:
	}
	unblock()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("successor did not take ownership after ledger drain")
	}
	next := New()
	t.Cleanup(next.idle.stop)
	second := scheduleGatewayTask(t, next, id, t.Context(), "schedule:other")
	if operation == second {
		t.Fatal("new request reused the predecessor's task")
	}
	taskReceipt(t, manager, second, "completed")
	if count.Load() != 2 {
		t.Fatalf("expected one paid request per listener owner, got %d", count.Load())
	}
}

func TestExtensionAccountTaskRechecksCallerBeforePaidDispatch(t *testing.T) {
	for _, change := range []string{"revoked", "rotated", "model", "account", "budget", "unreadable"} {
		t.Run(change, func(t *testing.T) {
			server, manager, id, paid, count := taskGatewayFixture(t, "")
			budget.Forget()
			t.Cleanup(budget.Forget)
			cancel, done := serveKeepAliveGateway(t, server)
			waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
			secret, err := access.Update("add-key", access.Change{Name: "task-key"})
			if err != nil {
				t.Fatal(err)
			}
			who, err := access.Check(secret)
			if err != nil {
				t.Fatal(err)
			}
			operation := scheduleGatewayTask(t, server, id, access.WithIdentity(t.Context(), who))
			update := access.Change{Key: who.KeyID}
			action := ""
			switch change {
			case "revoked":
				action = "off-key"
			case "rotated":
				action = "rotate-key"
			case "model":
				action, update.Models = "models-key", []string{"unavailable/model"}
			case "account":
				account, err := manager.Store().GetAccount("tasks.fixture", "vendor", "selected")
				if err != nil {
					t.Fatal(err)
				}
				account.ID, account.Generation = "other", 0
				if _, err := manager.Store().SaveAccount(account, 0); err != nil {
					t.Fatal(err)
				}
				provider.Changed()
				action, update.Accounts = "accounts-key", []string{id + "/" + provider.LoginID(id, "other")}
			case "budget":
				action, update.Limit = "limit-key", &access.Limit{Period: "day", Tokens: 1}
			case "unreadable":
				if err := os.WriteFile(access.Path(), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if action != "" {
				if _, err := access.Update(action, update); err != nil {
					t.Fatal(err)
				}
			}
			if change == "budget" {
				budget.Append(usage.Record{CallerKeyID: who.KeyID, Provider: id, Model: "m1", Input: 1})
			}
			taskReceipt(t, manager, operation, "not-started")
			if count.Load() != 0 {
				t.Fatal("current caller policy permitted a paid task", change, count.Load())
			}
			select {
			case actual := <-paid:
				t.Fatal("denied caller dispatched", change, actual)
			default:
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtensionAccountTaskMeasuredUsageSpendsCallerBudget(t *testing.T) {
	server, manager, id, _, count := taskGatewayFixture(t, "")
	budget.Forget()
	t.Cleanup(budget.Forget)
	cancel, done := serveKeepAliveGateway(t, server)
	waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
	secret, err := access.Update("add-key", access.Change{Name: "paid-task-budget"})
	if err != nil {
		t.Fatal(err)
	}
	who, err := access.Check(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.Update("limit-key", access.Change{Key: who.KeyID,
		Limit: &access.Limit{Period: "day", Tokens: 8}}); err != nil {
		t.Fatal(err)
	}
	who, err = access.Check(secret)
	if err != nil {
		t.Fatal(err)
	}
	ctx := access.WithIdentity(t.Context(), who)
	operation := scheduleGatewayTask(t, server, id, ctx)
	taskReceipt(t, manager, operation, "completed")
	key := access.Key{ID: who.KeyID, Limit: who.Limit}
	waitFor(t, "paid task budget reservation released", func() bool {
		state := budget.All(key, limitClock())
		return len(state) == 1 && state[0].InFlight == 0
	})
	if state := budget.All(key, limitClock()); len(state) != 1 || state[0].Tokens != 8 {
		t.Fatal("measured paid task did not spend the ordinary caller budget", state)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+id+`/m1","messages":[{"role":"user","content":"foreground"}]}`))
	request.RemoteAddr = "127.0.0.1:12442"
	reply := httptest.NewRecorder()
	server.Front().ServeHTTP(reply, request.WithContext(ctx))
	if reply.Code != http.StatusTooManyRequests {
		t.Fatal("measured paid task did not spend the ordinary caller budget", reply.Code, reply.Body)
	}
	if count.Load() != 1 {
		t.Fatal("spent task budget paid another vendor call", count.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExtensionAccountTaskConversationAndCallerIsolation(t *testing.T) {
	server, manager, id, _, count := taskGatewayFixture(t, "isolation")
	cancel, done := serveKeepAliveGateway(t, server)
	waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
	callers := make([]context.Context, 0, 2)
	for _, name := range []string{"first-task-caller", "second-task-caller"} {
		secret, err := access.Update("add-key", access.Change{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		who, err := access.Check(secret)
		if err != nil {
			t.Fatal(err)
		}
		callers = append(callers, access.WithIdentity(t.Context(), who))
	}
	first := scheduleGatewayTask(t, server, id, callers[0], "schedule")
	second := scheduleGatewayTask(t, server, id, callers[0], "schedule:other")
	scheduleGatewayTask(t, server, id, callers[0], "foreground:other")
	taskReceipt(t, manager, second, "not-started")
	if receipt, err := manager.Store().Operation("tasks.fixture", first); err != nil || receipt.State != "reserved" {
		t.Fatal("another conversation canceled the selected task", receipt, err)
	}
	scheduleGatewayTask(t, server, id, callers[1], "foreground")
	if receipt, err := manager.Store().Operation("tasks.fixture", first); err != nil || receipt.State != "reserved" {
		t.Fatal("another caller canceled the selected task with the same opaque key", receipt, err)
	}
	scheduleGatewayTask(t, server, id, callers[0], "foreground")
	taskReceipt(t, manager, first, "not-started")
	if count.Load() != 0 {
		t.Fatal("conversation cancellation dispatched a future task", count.Load())
	}
	immediate := scheduleGatewayTask(t, server, id, callers[0], "schedule:overdue")
	taskReceipt(t, manager, immediate, "completed")
	if count.Load() != 1 {
		t.Fatal("overdue task did not execute exactly once after its foreground terminal", count.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExtensionAccountTaskHonorsCurrentAccountsAtOnce(t *testing.T) {
	server, manager, id, _, count := taskGatewayFixture(t, "")
	cancel, done := serveKeepAliveGateway(t, server)
	waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
	operation := scheduleGatewayTask(t, server, id, t.Context())
	account, err := manager.Store().GetAccount("tasks.fixture", "vendor", "selected")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Store().SaveAccount(extensionhost.Account{
		Plugin: account.Plugin, Provider: account.Provider, ID: "other", Auth: account.Auth,
	}, 0); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	if err := provider.SetAccountOrder(id, []string{"other", "selected"}); err != nil {
		t.Fatal(err)
	}
	one := 1
	if err := provider.SetAtOnce(id, &one); err != nil {
		t.Fatal(err)
	}
	taskReceipt(t, manager, operation, "not-started")
	if count.Load() != 0 {
		t.Fatal("scheduled account bypassed the current accounts-at-once pool", count.Load())
	}
	if err := provider.SetAccountOrder(id, []string{"selected", "other"}); err != nil {
		t.Fatal(err)
	}
	next := scheduleGatewayTask(t, server, id, t.Context(), "schedule:overdue")
	taskReceipt(t, manager, next, "completed")
	if count.Load() != 1 {
		t.Fatal("account returned to its current pool did not execute once", count.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExtensionAccountTaskSuccessorRunsAfterItsCompletedTick(t *testing.T) {
	server, manager, id, paid, count := taskGatewayFixture(t, "chain")
	cancel, done := serveKeepAliveGateway(t, server)
	waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
	first := scheduleGatewayTask(t, server, id, t.Context(), "schedule:overdue")
	var successor string
	for index := range 2 {
		select {
		case scope := <-paid:
			if index == 0 {
				if scope.Operation != first {
					t.Fatal("first paid tick changed its reserved operation", scope)
				}
			} else {
				successor = scope.Operation
				if successor == first || scope.Account != "selected" || scope.Generation != 1 {
					t.Fatal("successor borrowed the previous tick or another account", scope)
				}
				if receipt, err := manager.Store().Operation("tasks.fixture", first); err != nil || receipt.State != "completed" {
					t.Fatal("overdue successor paid before its parent tick completed", receipt, err)
				}
			}
		case <-time.After(10 * time.Second):
			t.Fatal("live successful task did not schedule its one-shot successor", index)
		}
	}
	taskReceipt(t, manager, successor, "completed")
	rows := map[string]int{}
	for _, row := range usage.Load(time.Time{}) {
		if row.Kind == "cache_keepalive" {
			rows[row.Operation]++
		}
	}
	if count.Load() != 2 || len(rows) != 2 || rows[first] != 1 || rows[successor] != 1 {
		t.Fatal("successive ticks lost or duplicated normal accounting", count.Load(), rows)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
