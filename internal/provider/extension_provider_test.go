package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

type fixtureExtensionProviderHost struct {
	manifest  sdk.Manifest
	store     *extensionhost.Store
	namespace string
	disabled  bool
	call      func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error)
}

func (host *fixtureExtensionProviderHost) List() ([]extensionhost.Installed, error) {
	return []extensionhost.Installed{{Manifest: host.manifest, Disabled: host.disabled, Revision: "fixture"}}, nil
}

func (host *fixtureExtensionProviderHost) Manifest(id string) (sdk.Manifest, error) {
	if host.disabled || id != host.manifest.ID {
		return sdk.Manifest{}, errors.New("extension is not enabled")
	}
	return host.manifest, nil
}

func (host *fixtureExtensionProviderHost) Store() *extensionhost.Store { return host.store }

func (host *fixtureExtensionProviderHost) Namespace() string { return host.namespace }

func (host *fixtureExtensionProviderHost) Call(ctx context.Context, id, method string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
	if id != host.manifest.ID || scope.Plugin != id {
		return nil, errors.New("wrong module scope")
	}
	if host.call == nil {
		return nil, errors.New("unexpected extension invocation")
	}
	return host.call(ctx, method, scope, params, consume)
}

func fixtureExtensionProvider(t *testing.T, authenticated bool) *fixtureExtensionProviderHost {
	t.Helper()
	providerFileHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	root := t.TempDir()
	host := &fixtureExtensionProviderHost{store: extensionhost.NewStore(root), namespace: root, manifest: sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "fixture.external", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.ModelDiscovery, sdk.UsageReporter}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture vendor", Protocols: []protocolext.Protocol{"chat", "messages"},
			Models: []sdk.Model{{ID: "m1", Name: "Model", ContextWindow: 32768, MaxOutput: 4096, Protocols: []protocolext.Protocol{"chat"}}}}},
	}}
	if authenticated {
		host.manifest.Contract.Capabilities = append(host.manifest.Contract.Capabilities, sdk.AccountAuth)
		for _, id := range []string{"alice", "bob"} {
			if _, err := host.store.SaveAccount(extensionhost.Account{Plugin: host.manifest.ID, Provider: "vendor", ID: id,
				Auth: json.RawMessage(`{"access":"private-` + id + `"}`)}, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := providerExtensionHost
	providerExtensionHost = func() extensionProviderHost { return host }
	t.Cleanup(func() {
		providerExtensionHost = before
		Changed()
	})
	return host
}

func TestExtensionProviderAccountsModelsAndPolicy(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	all := extensionAccounts()
	if len(all) != 1 || !all[0].IsExtension() || all[0].IsPlugin() || all[0].Account.User != "alice" {
		t.Fatalf("external provider projection: %+v", all)
	}
	first := all[0]
	scope, ok := first.ExtensionScope()
	if !ok || scope.Plugin != host.manifest.ID || scope.Provider != "vendor" || scope.Account != "alice" || scope.Generation != 1 {
		t.Fatalf("account scope: %+v", scope)
	}
	if apis := first.ListedAPIs("m1"); len(apis) != 1 || apis[0] != Chat {
		t.Fatalf("per-model protocols: %v", apis)
	}
	others := first.AlsoOn()
	if len(others) != 1 || others[0].Account.User != "bob" {
		t.Fatalf("additional accounts: %+v", others)
	}
	if err := SetLoginOn(first.ID, "alice", false); err != nil {
		t.Fatal(err)
	}
	if selected := extensionAccounts(); len(selected) != 1 || selected[0].Account.User != "bob" || len(selected[0].AlsoOn()) != 0 {
		t.Fatalf("paused account remains routable: %+v", selected)
	}
	t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
	if providers, err := ExtensionProviders(); err != nil || len(providers) != 0 {
		t.Fatalf("account discovery off: %+v %v", providers, err)
	}
}

func TestExtensionProviderGenerationAndDisabledFence(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	provider := extensionAccounts()[0]
	scope, _ := provider.ExtensionScope()
	if _, err := host.store.SaveAccount(extensionhost.Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
		Auth: json.RawMessage(`{"access":"replacement"}`)}, scope.Generation); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, provider.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
	if _, err := provider.Do(http.DefaultClient, request); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("stale generation was executed: %v", err)
	}
	current := extensionAccounts()[0]
	host.disabled = true
	request, _ = http.NewRequest(http.MethodPost, current.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
	if _, err := current.Do(http.DefaultClient, request); err == nil {
		t.Fatal("disabled extension remained executable")
	}
}

func TestExtensionProviderModelsAndQuotaUseAccountScope(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	provider := extensionAccounts()[0]
	host.call = func(_ context.Context, method string, scope sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if scope.Provider != "vendor" || scope.Account != "alice" || scope.Generation != 1 {
			t.Fatalf("scope lost: %+v", scope)
		}
		wire, _ := json.Marshal(params)
		if strings.Contains(string(wire), "private-alice") {
			t.Fatal("adapter copied credentials into params")
		}
		if method == sdk.MethodProviderModels {
			return json.RawMessage(`{"models":[{"id":"m2","protocols":["messages"],"contextWindow":64000,"maxOutput":8000}]}`), nil
		}
		if method == sdk.MethodProviderUsage {
			return json.RawMessage(`{"usage":{},"windows":[{"name":"Weekly","used":62,"resetsAt":1800000000000}]}`), nil
		}
		t.Fatalf("unexpected method %s", method)
		return nil, nil
	}
	models, err := provider.Account.fetch(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "m2" || models[0].APIs[0] != string(Anthropic) {
		t.Fatalf("model discovery: %+v %v", models, err)
	}
	if listed := provider.Account.models(); len(listed) != 2 || listed[0].ID != "m2" || listed[1].ID != "m1" {
		t.Fatalf("refreshed models were not published: %+v", listed)
	}
	logins, _ := extensionLogins(provider.ID)
	quota := loginQuota(context.Background(), logins[0])
	if quota.Error != "" || len(quota.Windows) != 1 || quota.Windows[0].Used != 62 || quota.Windows[0].ResetsAt == nil {
		t.Fatalf("account quota: %+v", quota)
	}
}

func TestExtensionProviderStreamAndAcceptedReplayFence(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	provider := extensionAccounts()[0]
	var calls atomic.Int32
	host.call = func(_ context.Context, method string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
		calls.Add(1)
		request := params.(sdk.ExecuteRequest)
		if method != sdk.MethodProviderExecute || request.Model != "m1" || request.Protocol != "chat" ||
			scope.Operation == "" || scope.Operation != request.OperationID || scope.Account != "alice" || scope.Generation != 1 {
			return nil, errors.New("execution scope was not preserved")
		}
		if request.Headers.Get("Authorization") != "" || request.Credentials != nil {
			return nil, errors.New("credential escaped the auth frame")
		}
		for _, event := range []extensionhost.Event{
			{Name: "accepted", Data: json.RawMessage(`{"operationId":"` + scope.Operation + `"}`)},
			{Name: "headers", Data: json.RawMessage(`{"status":200,"headers":{"Content-Type":["application/json"]}}`)},
			{Name: "data", Data: json.RawMessage(`"eyJvayI6dHJ1ZX0="`)},
			{Name: "usage", Data: json.RawMessage(`{"inputTokens":12,"outputTokens":3}`)},
		} {
			if err := consume(event); err != nil {
				return nil, err
			}
		}
		return json.RawMessage(`{"status":200,"outcome":"completed"}`), nil
	}
	ctx, attempt := WithExtensionAttempt(context.Background())
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, provider.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
	request.Header.Set("Authorization", "Bearer gateway-secret")
	response, err := provider.Do(http.DefaultClient, request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("stream body: %s %v", body, err)
	}
	if totals := attempt.Usage(); totals == nil || totals.InputTokens != 12 || totals.OutputTokens != 3 {
		t.Fatalf("usage event was lost: %+v", totals)
	}
	second, _ := http.NewRequestWithContext(ctx, http.MethodPost, provider.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
	if _, err := provider.Do(http.DefaultClient, second); err == nil || calls.Load() != 1 {
		t.Fatalf("accepted operation was replayed: calls=%d error=%v", calls.Load(), err)
	}
}

func TestExtensionProviderTerminalOutcomePreservesTypedError(t *testing.T) {
	for _, outcome := range []string{"accepted", "uncertain", "completed", "not-started"} {
		for _, rpc := range []bool{false, true} {
			name := outcome + "/host"
			if rpc {
				name = outcome + "/rpc"
			}
			t.Run(name, func(t *testing.T) {
				host := fixtureExtensionProvider(t, true)
				provider := extensionAccounts()[0]
				var original error = &extensionhost.OutcomeError{Outcome: outcome, Cause: io.ErrUnexpectedEOF}
				if rpc {
					original = &sdk.Error{Code: "operation_conflict", Outcome: outcome}
				}
				host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
					return nil, original
				}
				ctx, attempt := WithExtensionAttempt(context.Background())
				request, _ := http.NewRequestWithContext(ctx, http.MethodPost, provider.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
				_, err := provider.Do(http.DefaultClient, request)
				if !errors.Is(err, original) {
					t.Fatalf("operation lost its typed error: %v", err)
				}
				terminal := outcome != "not-started"
				if got := attempt.Outcome(false); (got != nil) != terminal {
					t.Fatalf("outcome %s replay fence: %v", outcome, got)
				}
			})
		}
	}
}

func TestExtensionAuthSavesWithGenerationCAS(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	scope := sdk.Scope{Plugin: host.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1}
	host.call = func(_ context.Context, method string, got sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodAuthRefresh || got != scope {
			t.Fatalf("refresh scope: %s %+v", method, got)
		}
		return json.RawMessage(`{"id":"alice","credentials":{"access":"new-access"}}`), nil
	}
	result, err := RefreshExtensionAuth(context.Background(), scope)
	if err != nil || result.ID != "alice" || result.Credentials != nil {
		t.Fatalf("refresh: %+v %v", result, err)
	}
	if _, err := RefreshExtensionAuth(context.Background(), scope); err == nil {
		t.Fatal("stale refresh overwrote the newer credential generation")
	}
	account, err := host.store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || account.Generation != 2 || string(account.Auth) != `{"access":"new-access"}` {
		t.Fatalf("saved account: %+v %v", account, err)
	}
}

func TestExtensionProviderNativeMetadataAndStableOperationIdentity(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	provider := extensionAccounts()[0]
	type captured struct {
		scope     sdk.Scope
		native    json.RawMessage
		requestID string
		callerID  string
	}
	var seen []captured
	host.call = func(_ context.Context, _ string, scope sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		request := params.(sdk.ExecuteRequest)
		var options struct {
			Native    json.RawMessage `json:"native"`
			RequestID string          `json:"requestId"`
			CallerID  string          `json:"callerId"`
		}
		if err := json.Unmarshal(request.Options, &options); err != nil {
			return nil, err
		}
		seen = append(seen, captured{scope, options.Native, options.RequestID, options.CallerID})
		return json.RawMessage(`{"status":204,"outcome":"completed"}`), nil
	}
	send := func(plugin, requestID, callerID, session string) {
		t.Helper()
		ctx := extensionhost.WithRequestMetadata(context.Background(), extensionhost.RequestMetadata{
			Plugin: plugin, RequestID: requestID, CallerID: callerID,
			Value: json.RawMessage(`{"nativeTag":"private-to-owner","callerId":"raw-spoofed"}`),
		})
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, provider.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
		request.Header.Set(ConversationHeader, session)
		request.Header.Set(SessionToPluginHeader, "session-a")
		request.Header.Set("X-Magpie-Caller-ID", "client-spoofed")
		response, err := provider.Do(http.DefaultClient, request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	send(host.manifest.ID, "native-1", "host-caller-a", "session-a")
	send(host.manifest.ID, "native-1", "host-caller-a", "session-a")
	send("another.module", "native-1", "host-caller-a", "session-a")
	send(host.manifest.ID, "", "host-caller-a", "session-a")
	send(host.manifest.ID, "", "host-caller-a", "session-a")
	if seen[0].scope.Operation != seen[1].scope.Operation || seen[0].requestID != "native-1" ||
		seen[0].scope.Session != "session-a" ||
		!strings.Contains(string(seen[0].native), "private-to-owner") {
		t.Fatalf("native request identity was not preserved: %+v", seen[:2])
	}
	if seen[2].requestID != "" || seen[2].callerID != "" || len(seen[2].native) != 0 || seen[2].scope.Operation == seen[0].scope.Operation {
		t.Fatalf("another module's native metadata escaped: %+v", seen[2])
	}
	if seen[3].scope.Operation == seen[4].scope.Operation {
		t.Fatal("independent calls without a native request ID reused an operation")
	}
	send(host.manifest.ID, "native-1", "host-caller-b", "session-a")
	if seen[5].scope.Operation == seen[0].scope.Operation {
		t.Fatal("different callers sharing a native request ID collided")
	}
	if seen[0].callerID != "host-caller-a" || seen[5].callerID != "host-caller-b" {
		t.Fatal("native caller options lost their trusted host identity")
	}
	send(host.manifest.ID, "native-1", "host-caller-a", "session-b")
	if seen[6].scope.Operation == seen[0].scope.Operation || seen[6].scope.Session != "session-b" {
		t.Fatal("different sessions sharing a native request ID collided")
	}
	send(host.manifest.ID, "native-1", "host-caller-a", "")
	if seen[7].scope.Operation != seen[0].scope.Operation || seen[7].scope.Session != "session-a" {
		t.Fatal("the effective session changed the native operation identity")
	}
	scope, _ := provider.ExtensionScope()
	if _, err := host.store.SaveAccount(extensionhost.Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
		Auth: json.RawMessage(`{"access":"rotated"}`)}, scope.Generation); err != nil {
		t.Fatal(err)
	}
	provider = extensionAccounts()[0]
	send(host.manifest.ID, "native-1", "host-caller-a", "session-a")
	if seen[8].scope.Operation != seen[0].scope.Operation || seen[8].scope.Generation == seen[0].scope.Generation {
		t.Fatal("reauthentication changed the logical native operation or lost its account generation")
	}
}

func TestExtensionProviderCacheIsolatedBetweenStores(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	first := extensionAccounts()[0].Account.extension
	extensionModelCache.Store(first.cacheKey(), []sdk.Model{{ID: "m1", Price: &sdk.ModelPrice{Input: 91, Output: 92}, Efforts: []string{"high"}}})
	if models, err := first.models(); err != nil || len(models) != 1 || models[0].Price == nil ||
		models[0].Price.Input != 91 || !slices.Equal(models[0].Efforts, []string{"high"}) {
		t.Fatalf("cached model metadata was lost: %+v %v", models, err)
	}
	root := t.TempDir()
	other := &fixtureExtensionProviderHost{manifest: host.manifest, store: extensionhost.NewStore(root), namespace: root}
	second := &extensionAccount{host: other, manifest: host.manifest, descriptor: first.descriptor, scope: first.scope, modelKey: first.modelKey}
	if first.cacheKey() == second.cacheKey() {
		t.Fatal("different stores share a model-cache namespace")
	}
	models, err := second.models()
	if err != nil || len(models) != 1 || models[0].ID != "m1" || models[0].Price != nil || len(models[0].Efforts) != 0 {
		t.Fatalf("another configuration's cached models escaped: %+v %v", models, err)
	}
}

func TestExtensionProviderGenerationInvalidatesRoutingFreshness(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	provider := extensionAccounts()[0]
	UsageRead(provider.ID, "alice")
	usedCache.Lock()
	if usedCache.m == nil {
		usedCache.m = map[string]map[string]Allowance{}
	}
	if usedCache.read == nil {
		usedCache.read = map[string]readState{}
	}
	usedCache.m[provider.ID] = map[string]Allowance{"alice": {}}
	usedCache.read[provider.ID+"/alice"] = readState{at: time.Now()}
	usedCache.Unlock()
	scope, _ := provider.ExtensionScope()
	if _, err := host.store.SaveAccount(extensionhost.Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
		Auth: json.RawMessage(`{"access":"new-generation"}`)}, scope.Generation); err != nil {
		t.Fatal(err)
	}
	if _, _, known := UsageRead(provider.ID, "alice"); known {
		t.Fatal("new generation inherited old routing freshness")
	}
	usedCache.Lock()
	_, retained := usedCache.m[provider.ID]
	usedCache.Unlock()
	if retained {
		t.Fatal("new generation inherited the old routing allowance")
	}
}

func TestExtensionProviderAccountModelUnionAndAllPaused(t *testing.T) {
	fixtureExtensionProvider(t, true)
	all, err := ExtensionProviders()
	if err != nil {
		t.Fatal(err)
	}
	extensionModelCache.Store(all[0].Account.extension.cacheKey(), []sdk.Model{{ID: "alice-only", Protocols: []protocolext.Protocol{"chat"}}})
	extensionModelCache.Store(all[1].Account.extension.cacheKey(), []sdk.Model{{ID: "bob-only", Protocols: []protocolext.Protocol{"messages"}}})
	primary := extensionAccounts()[0]
	if models := primary.Account.models(); len(models) != 2 {
		t.Fatalf("provider did not expose the in-use accounts' union: %+v", models)
	}
	if primary.Account.Lists("bob-only") || !primary.Account.Lists("alice-only") || !primary.AlsoOn()[0].Account.Lists("bob-only") {
		t.Fatal("model union erased account-specific model restrictions")
	}
	if err := SetLoginOn(primary.ID, "alice", false); err != nil {
		t.Fatal(err)
	}
	if err := SetLoginOn(primary.ID, "bob", false); err != nil {
		t.Fatal(err)
	}
	paused := extensionAccounts()
	if len(paused) != 1 || !paused[0].OwnPaused() || len(paused[0].AlsoOn()) != 0 {
		t.Fatalf("all-paused provider remained routable or disappeared: %+v", paused)
	}
}

func TestExtensionProviderPartialUsageDoesNotInventZero(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	provider := extensionAccounts()[0]
	logins, _ := extensionLogins(provider.ID)
	raw := json.RawMessage(`{"usage":{},"windows":[
		{"name":"real-zero","used":0},
		{"name":"absent"},
		{"name":"null","used":null},
		{"name":"flagged","used":0,"unknown":true,"error":"unavailable\n detail","asOf":1794470340000},
		{"name":"cached","used":70,"asOf":1794470340000}
	]}`)
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return raw, nil
	}
	quota := extensionQuota(context.Background(), logins[0])
	if quota.Error != "" || len(quota.Windows) != 5 || quota.Windows[0].Unknown || quota.Windows[0].Text || quota.Windows[0].Used != 0 {
		t.Fatalf("real zero or partial windows changed: %+v", quota)
	}
	for _, index := range []int{1, 2, 3} {
		window := quota.Windows[index]
		if !window.Unknown || !window.Text || window.Display == "" {
			t.Fatalf("missing reading became zero percent: %+v", window)
		}
		if _, recorded := pointOf(window, time.Now(), time.Now()); recorded {
			t.Fatalf("unknown reading became history: %+v", window)
		}
	}
	if strings.Contains(quota.Windows[3].Display, "\n") || quota.Windows[3].AsOf == nil || quota.Windows[3].Error == "" {
		t.Fatalf("window error or provenance lost: %+v", quota.Windows[3])
	}
	if !quota.Windows[4].Text || quota.Windows[4].AsOf == nil || !strings.Contains(quota.Windows[4].Display, "70%") {
		t.Fatalf("cached window became a fresh percentage: %+v", quota.Windows[4])
	}
	if _, known := routedAllowance(provider.ID, "alice", quota, time.Now()); known {
		t.Fatal("an incomplete subset became an authoritative account allowance")
	}
	if state := (loginUsageEntry{read: quota, at: time.Now()}).state(); !state.failed {
		t.Fatal("partial usage became a verified fresh routing reading")
	}
	raw = json.RawMessage(`{"usage":{},"asOf":1794470400123,"readDeferred":true,"error":"partial read","windows":[{"name":"real-zero","used":0}]}`)
	deferred := extensionQuota(context.Background(), logins[0])
	if deferred.Error != "partial read" || !deferred.ReadDeferred || deferred.AsOf == nil ||
		deferred.AsOf.UnixMilli() != 1794470400123 || len(deferred.Windows) != 1 || deferred.Windows[0].Used != 0 {
		t.Fatalf("global freshness or partial data lost: %+v", deferred)
	}
	raw = json.RawMessage(`{"usage":{},"readDeferred":true,"windows":[{"name":"quota","used":90}]}`)
	deferred = readNow(extensionQuota(context.Background(), logins[0]))
	if deferred.ReadAt != nil || deferred.AsOf != nil || !deferred.ReadDeferred || deferred.Error != "" {
		t.Fatalf("deferred read acquired invented provenance: %+v", deferred)
	}
	if alerts := dueAlerts([]SubscriptionQuota{deferred}, map[string]alertMark{}, 80, 0, time.Now()); len(alerts) != 0 {
		t.Fatalf("deferred reading raised a fresh alert: %+v", alerts)
	}
}

func TestExtensionProviderUsageRejectsInvalidNumbers(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	provider := extensionAccounts()[0]
	logins, _ := extensionLogins(provider.ID)
	var raw json.RawMessage
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return raw, nil
	}
	for _, number := range []string{"NaN", "Infinity", "1e309", "-1", "101"} {
		raw = json.RawMessage(`{"usage":{},"windows":[{"name":"quota","used":` + number + `}]}`)
		quota := extensionQuota(context.Background(), logins[0])
		if quota.Error == "" || len(quota.Windows) != 0 {
			t.Fatalf("invalid quota %s became a usable reading: %+v", number, quota)
		}
	}
}

func TestExtensionProviderRefusesCaseAmbiguousAccountIDs(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	if _, err := host.store.SaveAccount(extensionhost.Account{Plugin: host.manifest.ID, Provider: "vendor", ID: "Alice",
		Auth: json.RawMessage(`{"access":"another-account"}`)}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtensionProviders(); err == nil {
		t.Fatal("case-ambiguous accounts would share core policy identities")
	}
}
