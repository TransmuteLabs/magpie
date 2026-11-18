package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gwtool"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

type extensionToolAccountFake struct {
	entries []extension.Installed
	store   *extension.Store
	err     error
	listed  int
	stop    func()
}

func (fake *extensionToolAccountFake) List() ([]extension.Installed, error) {
	fake.listed++
	return fake.entries, fake.err
}

func (fake *extensionToolAccountFake) Store() *extension.Store { return fake.store }

func extensionToolGatewayFixture(t *testing.T) *extensionToolAccountFake {
	t.Helper()
	fresh(t)
	fake := &extensionToolAccountFake{
		entries: []extension.Installed{{Manifest: sdk.Manifest{
			Schema: sdk.ManifestSchema, ID: "tool.module", Version: "1.0.0", Runtime: "exec", Entry: "worker",
			Contract: sdk.Contract{
				Version: sdk.APIVersion{Major: 1},
				Capabilities: []sdk.Capability{
					sdk.ToolProvider, sdk.AccountAuth, sdk.ModelDiscovery,
				},
			},
			Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Vendor", Protocols: []protocolext.Protocol{"chat"}}},
			Tools: []sdk.ToolDescriptor{{
				Name: "echo", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"},"_magpie":{"type":"object","properties":{"provider":{"type":"string"},"account":{"type":"string"}},"required":["provider","account"]}}}`),
			}},
		}}},
	}
	manager := extension.Default()
	fake.store = manager.Store()
	source := t.TempDir()
	manifest, err := json.Marshal(fake.entries[0].Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "worker"), []byte("discovery fixture; never executed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := extension.NewCatalog(manager.Namespace()).Install(t.Context(), source,
		func(context.Context, extension.Installed) error { return nil }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := fake.store.SaveAccount(extension.Account{
		Plugin: "tool.module", Provider: "vendor", ID: "selected", Auth: json.RawMessage(`{"token":"synthetic-private-secret"}`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	previous := extensionToolAccounts
	extensionToolAccounts = func() extensionToolAccountSource { return fake }
	t.Cleanup(func() { extensionToolAccounts = previous })
	off := gwtool.Serve(gwtool.PluginTools)
	fake.stop = off
	t.Cleanup(off)
	return fake
}

func TestExtensionToolContextDerivesAuthority(t *testing.T) {
	fake := extensionToolGatewayFixture(t)
	account, err := fake.store.GetAccount("tool.module", "vendor", "selected")
	if err != nil {
		t.Fatal(err)
	}
	account.Auth = json.RawMessage(`{"token":"refreshed"}`)
	account, err = fake.store.SaveAccount(account, account.Generation)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), callerCtx{}, caller{session: "host-session"})
	ctx = extension.WithToolScope(ctx, sdk.Scope{Plugin: "other", Provider: "other", Account: "other", Generation: 99, Operation: "unrelated", Attempt: 99})
	ctx, args, err := extensionToolContext(ctx, "extension:tool.module",
		json.RawMessage(`{"value":"hello","_magpie":{"provider":"vendor","account":"selected"}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := sdk.Scope{Plugin: "tool.module", Provider: "vendor", Account: "selected", Generation: account.Generation, Session: "host-session"}
	if scope := extension.ToolScopeFrom(ctx); scope != want {
		t.Fatalf("host scope = %+v; want %+v", scope, want)
	}
	if string(args) != `{"value":"hello"}` {
		t.Fatalf("reserved arguments reached module: %s", args)
	}
	unbound, original, err := extensionToolContext(ctx, "extension:tool.module",
		json.RawMessage(`{"provider":"vendor","account":"selected","generation":999,"plugin":"other"}`))
	if err != nil || extension.ToolScopeFrom(unbound) != (sdk.Scope{Plugin: "tool.module", Session: "host-session"}) ||
		!strings.Contains(string(original), `"generation":999`) {
		t.Fatalf("ordinary args selected credentials: %+v, %s, %v", extension.ToolScopeFrom(unbound), original, err)
	}
}

func TestExtensionToolContextRefusesRestrictedDelegation(t *testing.T) {
	fake := extensionToolGatewayFixture(t)
	for _, who := range []access.Identity{
		{ReadOnly: true},
		{Models: []string{"vendor/model"}},
		{Accounts: []string{"vendor/selected"}},
		{Limit: &access.Limit{Period: "day", Tokens: 1}},
		{More: []access.Limit{{Period: "day", Cost: 1}}},
		{Issuer: &access.Identity{Limit: &access.Limit{Period: "day", Tokens: 1}}},
		{Issuer: &access.Identity{ReadOnly: true}},
	} {
		ctx := access.WithIdentity(t.Context(), who)
		_, _, err := extensionToolContext(ctx, "extension:tool.module",
			json.RawMessage(`{"_magpie":{"provider":"vendor","account":"selected"}}`))
		if err == nil {
			t.Fatalf("restricted caller delegated credentials: %+v", who)
		}
		if fake.listed != 0 {
			t.Fatal("restricted delegation read account declarations")
		}
		unbound, args, err := extensionToolContext(ctx, "extension:tool.module", json.RawMessage(`{"value":"unbound"}`))
		if err != nil || extension.ToolScopeFrom(unbound).Account != "" || string(args) != `{"value":"unbound"}` {
			t.Fatalf("ordinary tool changed policy: %+v, %s, %v", extension.ToolScopeFrom(unbound), args, err)
		}
	}
}

func TestExtensionToolNativeRequestReplayAfterReauth(t *testing.T) {
	fake := extensionToolGatewayFixture(t)
	groups := []plugin.PluginTools{{Spec: "extension:tool.module", Tools: []plugin.Tool{{Name: "echo"}}}}
	fakePluginTools(t, groups)
	previous := runExtensionPluginTool
	var scopes []sdk.Scope
	executed := 0
	runExtensionPluginTool = func(ctx context.Context, spec, name string, args json.RawMessage) (sdk.ToolResult, error) {
		scope := extension.ToolScopeFrom(ctx)
		if scope.Operation == "" || scope.Attempt != 1 {
			t.Fatalf("native tool lost its operation identity: %+v", scope)
		}
		scopes = append(scopes, scope)
		if _, err := fake.store.Journal(scope, "dispatched"); err != nil {
			return sdk.ToolResult{}, err
		}
		executed++
		if _, err := fake.store.Journal(scope, "completed"); err != nil {
			return sdk.ToolResult{}, err
		}
		return sdk.ToolResult{Content: json.RawMessage(`"done"`)}, nil
	}
	t.Cleanup(func() { runExtensionPluginTool = previous })
	ctx := context.WithValue(t.Context(), callerCtx{}, caller{session: "native-session"})
	ctx = extension.WithRequestMetadata(ctx, extension.RequestMetadata{
		Plugin: "tool.module", RequestID: "request-1", CallerID: "native-caller",
	})
	request := httptest.NewRequest(http.MethodPost, ToolsMCPPath, nil).WithContext(ctx)
	args := json.RawMessage(`{"_magpie":{"provider":"vendor","account":"selected"}}`)
	server := New()
	if _, err := server.toolsMCPRun(request, namedTools(groups)[0], args); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		if attempt == 1 {
			account, err := fake.store.GetAccount("tool.module", "vendor", "selected")
			if err != nil {
				t.Fatal(err)
			}
			account.Auth = json.RawMessage(`{"token":"replacement"}`)
			if _, err := fake.store.SaveAccount(account, account.Generation); err != nil {
				t.Fatal(err)
			}
		}
		_, err := server.toolsMCPRun(request, namedTools(groups)[0], args)
		var conflict *sdk.Error
		if !errors.As(err, &conflict) || conflict.Code != "operation_conflict" || conflict.Outcome != "completed" {
			t.Fatalf("native replay was not fenced: %v", err)
		}
	}
	if executed != 1 || len(scopes) != 3 || scopes[0].Operation != scopes[1].Operation ||
		scopes[0].Operation != scopes[2].Operation || scopes[0].Generation == scopes[2].Generation {
		t.Fatalf("replay executed %d times: %+v", executed, scopes)
	}
	entry, err := fake.store.Operation(scopes[0].Plugin, scopes[0].Operation)
	if err != nil || entry.Scope != scopes[0] || entry.State != "completed" {
		t.Fatalf("replay replaced the original journal: %+v, %v", entry, err)
	}
}

func TestExtensionToolNativeIdentityIsolation(t *testing.T) {
	extensionToolGatewayFixture(t)
	metadata := extension.RequestMetadata{Plugin: "tool.module", RequestID: "request-1", CallerID: "caller-1"}
	ctx := context.WithValue(t.Context(), callerCtx{}, caller{session: "session-1"})
	derive := func(ctx context.Context, metadata extension.RequestMetadata) sdk.Scope {
		t.Helper()
		scoped, _, err := extensionToolContext(extension.WithRequestMetadata(ctx, metadata), "extension:tool.module", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		return extension.ToolScopeFrom(scoped)
	}
	first := derive(ctx, metadata)
	if first.Operation == "" || first.Attempt != 1 || first != derive(ctx, metadata) {
		t.Fatalf("unstable native identity: %+v", first)
	}
	changed := metadata
	changed.RequestID = "request-2"
	if first.Operation == derive(ctx, changed).Operation {
		t.Fatal("different native requests shared an operation")
	}
	changed = metadata
	changed.CallerID = "caller-2"
	if first.Operation == derive(ctx, changed).Operation {
		t.Fatal("different native callers shared an operation")
	}
	if first.Operation == derive(context.WithValue(ctx, callerCtx{}, caller{session: "session-2"}), metadata).Operation {
		t.Fatal("different sessions shared an operation")
	}
	changed = metadata
	changed.Plugin = "other-module"
	if scope := derive(ctx, changed); scope.Operation != "" || scope.Attempt != 0 {
		t.Fatalf("foreign metadata selected an operation: %+v", scope)
	}
	changed = metadata
	changed.RequestID = ""
	if scope := derive(ctx, changed); scope.Operation != "" || scope.Attempt != 0 {
		t.Fatalf("empty request identity selected an operation: %+v", scope)
	}
}

func TestExtensionToolAccountNetworkContext(t *testing.T) {
	extensionToolGatewayFixture(t)
	config := settings.Load()
	config.Proxy = "http://127.0.0.1:28080"
	config.Proxies = []settings.NamedProxy{{ID: "tool-fixture", Name: "Tool fixture", URL: "socks5://127.0.0.1:1080",
		User: "proxy-user", Pass: "proxy-pass"}}
	if err := settings.Save(config); err != nil {
		t.Fatal(err)
	}
	type markerKey struct{}
	parent := context.WithValue(t.Context(), markerKey{}, "original parent")
	parent = netproxy.With(parent, "http://127.0.0.1:38080")
	parent, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	deadline, _ := parent.Deadline()
	for _, policy := range []struct {
		provider string
		account  string
		choice   string
		proxy    string
	}{
		{"http://127.0.0.1:18080", "@tool-fixture", "@tool-fixture", "socks5://proxy-user:proxy-pass@127.0.0.1:1080"},
		{"http://127.0.0.1:18080", "direct", "direct", ""},
		{"http://127.0.0.1:18080", "", "http://127.0.0.1:18080", "http://127.0.0.1:18080"},
		{"", "", "", "http://127.0.0.1:28080"},
	} {
		stored, err := json.Marshal(map[string]any{"providers": []provider.Provider{{
			ID: provider.ExtensionProviderID("tool.module", "vendor"), Proxy: policy.provider,
			AccountProxies: map[string]string{"selected": policy.account},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(provider.Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(provider.Path(), stored, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, _, err := extensionToolContext(parent, "extension:tool.module",
			json.RawMessage(`{"_magpie":{"provider":"vendor","account":"selected"}}`))
		if err != nil {
			t.Fatal(err)
		}
		actual, hasDeadline := ctx.Deadline()
		if netproxy.Choice(ctx) != policy.choice || ctx.Value(markerKey{}) != "original parent" || !hasDeadline || actual != deadline {
			t.Fatalf("account policy lost parent authority: proxy=%q deadline=%v", netproxy.Choice(ctx), actual)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://203.0.113.10/resource", nil)
		if err != nil {
			t.Fatal(err)
		}
		proxy, err := netproxy.Func(request)
		effective := ""
		if proxy != nil {
			effective = proxy.String()
		}
		if err != nil || effective != policy.proxy {
			t.Fatalf("effective proxy = %q, %v; want %q", effective, err, policy.proxy)
		}
	}
}

func TestExtensionToolNetworkHoldPreventsDispatch(t *testing.T) {
	extensionToolGatewayFixture(t)
	groups := []plugin.PluginTools{{Spec: "extension:tool.module", Tools: []plugin.Tool{{Name: "echo"}}}}
	fakePluginTools(t, groups)
	previous := runExtensionPluginTool
	calls := 0
	runExtensionPluginTool = func(context.Context, string, string, json.RawMessage) (sdk.ToolResult, error) {
		calls++
		return sdk.ToolResult{Content: json.RawMessage(`"unexpected"`)}, nil
	}
	t.Cleanup(func() { runExtensionPluginTool = previous })
	held := errors.New("explicit network hold")
	ctx := netproxy.Hold(t.Context(), held)
	request := httptest.NewRequest(http.MethodPost, ToolsMCPPath, nil).WithContext(ctx)
	_, err := New().toolsMCPRun(request, namedTools(groups)[0],
		json.RawMessage(`{"_magpie":{"provider":"vendor","account":"selected"}}`))
	if !errors.Is(err, held) || calls != 0 {
		t.Fatalf("held account tool dispatched: calls=%d error=%v", calls, err)
	}
}

func TestExtensionToolContextRequiresExplicitDeclaredAccount(t *testing.T) {
	fake := extensionToolGatewayFixture(t)
	for _, args := range []string{
		`{"_magpie":null}`, `{"_magpie":{}}`, `{"_magpie":[]}`,
		`{"_magpie":{"provider":"vendor"}}`,
		`{"_magpie":{"account":"selected"}}`,
		`{"_magpie":{"provider":"vendor","account":"missing"}}`,
		`{"_magpie":{"provider":"undeclared","account":"selected"}}`,
		`{"_magpie":{"provider":"vendor","account":"selected","generation":999}}`,
		`{"_magpie":{"provider":"vendor","account":"selected","plugin":"other"}}`,
		`[]`, `{`,
	} {
		if _, _, err := extensionToolContext(t.Context(), "extension:tool.module", json.RawMessage(args)); err == nil {
			t.Fatalf("invalid authority was accepted: %s", args)
		} else if strings.Contains(err.Error(), "synthetic-private-secret") {
			t.Fatal("credentials escaped in an error")
		}
	}
	args := json.RawMessage(`{"_magpie":{"provider":"vendor","account":"selected"}}`)
	fake.entries[0].Manifest.Contract.Capabilities = []sdk.Capability{sdk.ToolProvider}
	if _, _, err := extensionToolContext(t.Context(), "extension:tool.module", args); err == nil {
		t.Fatal("tool without account-auth received credentials")
	}
	fake.entries[0].Manifest.Contract.Capabilities = []sdk.Capability{sdk.ToolProvider, sdk.AccountAuth}
	fake.entries[0].Disabled = true
	if _, _, err := extensionToolContext(t.Context(), "extension:tool.module", args); err == nil {
		t.Fatal("disabled module received credentials")
	}
	fake.entries[0].Disabled = false
	fake.err = errors.New("unknown catalog")
	if _, _, err := extensionToolContext(t.Context(), "extension:tool.module", args); err == nil {
		t.Fatal("unknown catalog granted credentials")
	}
	fake.err = nil
	if err := fake.store.DeleteAccount("tool.module", "vendor", "selected", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := extensionToolContext(t.Context(), "extension:tool.module", args); err == nil {
		t.Fatal("deleted account received credentials")
	}
}

func TestExtensionToolsMCPPreservesRichResultAndScope(t *testing.T) {
	fake := extensionToolGatewayFixture(t)
	descriptor := fake.entries[0].Manifest.Tools[0]
	groups := []plugin.PluginTools{{Spec: "extension:tool.module", Tools: []plugin.Tool{
		{Name: descriptor.Name, Schema: descriptor.InputSchema},
	}}}
	fakePluginTools(t, groups)
	calls := 0
	previous := runExtensionPluginTool
	runExtensionPluginTool = func(ctx context.Context, spec, name string, args json.RawMessage) (sdk.ToolResult, error) {
		calls++
		scope := extension.ToolScopeFrom(ctx)
		if spec != "extension:tool.module" || name != "echo" || scope.Plugin != "tool.module" ||
			scope.Provider != "vendor" || scope.Account != "selected" || scope.Generation != 1 ||
			strings.Contains(string(args), "_magpie") {
			t.Errorf("MCP dispatch = %s %s %+v %s", spec, name, scope, args)
		}
		return sdk.ToolResult{
			Content: json.RawMessage(`[{"type":"text","text":"hello"},{"type":"image","data":"aW1hZ2U=","mimeType":"image/png"}]`),
			IsError: true,
		}, nil
	}
	t.Cleanup(func() { runExtensionPluginTool = previous })
	server := New()
	handler := server.Front()
	for _, route := range []struct {
		path string
		name string
	}{
		{ToolsMCPPath, namedTools(groups)[0].name},
		{ToolsMCPOne + toolPrefix(groups[0].Spec), "echo"},
	} {
		reply := toolsPostTo(handler, route.path, toolsCall(route.name, `{"value":"hello","_magpie":{"provider":"vendor","account":"selected"}}`))
		var message struct {
			Result sdk.ToolResult `json:"result"`
		}
		if err := json.Unmarshal(reply.Body.Bytes(), &message); err != nil || !message.Result.IsError ||
			!bytes.Contains(message.Result.Content, []byte(`"mimeType":"image/png"`)) {
			t.Fatalf("rich MCP result = %s, %v", reply.Body, err)
		}
	}
	if calls != 2 {
		t.Fatalf("MCP calls = %d; want 2", calls)
	}
}

func TestExtensionToolsHubPreparesSameScope(t *testing.T) {
	extensionToolGatewayFixture(t)
	groups := []plugin.PluginTools{{Spec: "extension:tool.module", Tools: []plugin.Tool{{Name: "echo"}}}}
	fakePluginTools(t, groups)
	called := 0
	runPluginTool = func(ctx context.Context, spec, name string, args json.RawMessage, agent string) (string, error) {
		called++
		if scope := extension.ToolScopeFrom(ctx); scope.Plugin != "tool.module" || scope.Provider != "vendor" ||
			scope.Account != "selected" || scope.Generation != 1 || scope.Session != "hub-session" {
			t.Errorf("hub scope = %+v", scope)
		}
		if string(args) != `{"value":1}` || agent != "claude" {
			t.Errorf("hub arguments = %s, agent %s", args, agent)
		}
		return "hub answer", nil
	}
	ctx := context.WithValue(t.Context(), callerCtx{}, caller{session: "hub-session"})
	text, err := RunPluginToolOf(ctx, groups[0].Spec, "echo",
		json.RawMessage(`{"value":1,"_magpie":{"provider":"vendor","account":"selected"}}`), "claude")
	if err != nil || text != "hub answer" || called != 1 {
		t.Fatalf("hub result = %q, %v; calls %d", text, err, called)
	}
	ctx = access.WithIdentity(ctx, access.Identity{Models: []string{"vendor/model"}})
	if _, err := RunPluginToolOf(ctx, groups[0].Spec, "echo",
		json.RawMessage(`{"_magpie":{"provider":"vendor","account":"selected"}}`), "claude"); err == nil || called != 1 {
		t.Fatalf("restricted hub delegated credentials: %v, calls %d", err, called)
	}
}

func TestExtensionToolsMCPKeepsReservationAndKeyChecks(t *testing.T) {
	extensionToolGatewayFixture(t)
	groups := []plugin.PluginTools{{Spec: "extension:tool.module", Tools: []plugin.Tool{{Name: "echo"}}}}
	fakePluginTools(t, groups)
	called := 0
	previous := runExtensionPluginTool
	runExtensionPluginTool = func(ctx context.Context, spec, name string, args json.RawMessage) (sdk.ToolResult, error) {
		called++
		if extension.ToolScopeFrom(ctx).Account != "" {
			t.Fatal("unbound call delegated an account")
		}
		return sdk.ToolResult{Content: json.RawMessage(`"ordinary"`)}, nil
	}
	t.Cleanup(func() { runExtensionPluginTool = previous })
	server := New()
	tool := namedTools(groups)[0]
	request := httptest.NewRequest("POST", ToolsMCPPath, nil).WithContext(
		access.WithIdentity(t.Context(), access.Identity{Models: []string{"vendor/model"}}))
	if result, err := server.toolsMCPRun(request, tool, json.RawMessage(`{}`)); err != nil || result["isError"] != false || called != 1 {
		t.Fatalf("ordinary restricted call = %+v, %v; calls %d", result, err, called)
	}
	request = httptest.NewRequest(http.MethodPost, ToolsMCPPath,
		strings.NewReader(toolsCall(tool.name, `{}`))).WithContext(
		access.WithIdentity(t.Context(), access.Identity{MCP: []string{"other-server"}}))
	request.RemoteAddr = "127.0.0.1:5000"
	reply := httptest.NewRecorder()
	server.toolsMCP(reply, request)
	if reply.Code != http.StatusForbidden || called != 1 {
		t.Fatalf("MCP key restriction did not block SDK call: HTTP %d, calls %d", reply.Code, called)
	}
	withoutLimits(t)
	request = request.WithContext(access.WithIdentity(t.Context(), access.Identity{
		KeyID: "limited", Limit: &access.Limit{Period: "day", Tokens: 1},
	}))
	if _, err := server.toolsMCPRun(request, tool, json.RawMessage(`{}`)); err == nil || called != 1 {
		t.Fatalf("reservation did not block SDK call: %v, calls %d", err, called)
	}
}

func TestExtensionToolMCPContentValidation(t *testing.T) {
	content := json.RawMessage(`[{"type":"text","text":"hello"},{"type":"resource","resource":{"uri":"fixture://document","text":"body","mimeType":"text/plain"}}]`)
	result, err := extensionMCPResult(sdk.ToolResult{Content: content, IsError: true})
	if err != nil || result["isError"] != true || !bytes.Equal(result["content"].(json.RawMessage), content) {
		t.Fatalf("rich content changed: %+v, %v", result, err)
	}
	for _, content := range []json.RawMessage{nil, json.RawMessage(`{`), json.RawMessage(`[{}]`), json.RawMessage(`[null]`)} {
		if _, err := extensionMCPResult(sdk.ToolResult{Content: content}); err == nil {
			t.Fatalf("malformed MCP content was forwarded: %s", content)
		}
	}
}

func TestExtensionToolsHubRichPathKeepsScopeAndGuards(t *testing.T) {
	fixture := extensionToolGatewayFixture(t)
	groups := []plugin.PluginTools{{Spec: "extension:tool.module", Tools: []plugin.Tool{{Name: "echo"}}}}
	fakePluginTools(t, groups)
	previous := runExtensionPluginTool
	calls := 0
	content := json.RawMessage(`[{"type":"image","data":"aW1hZ2U=","mimeType":"image/png"},{"type":"resource","resource":{"uri":"fixture://document","text":"body"}}]`)
	runExtensionPluginTool = func(ctx context.Context, spec, name string, args json.RawMessage) (sdk.ToolResult, error) {
		calls++
		scope := extension.ToolScopeFrom(ctx)
		if spec != groups[0].Spec || name != "echo" || scope.Plugin != "tool.module" ||
			scope.Account != "selected" || scope.Generation != 1 || scope.Session != "library-session" ||
			scope.Operation == "" || scope.Attempt != 1 || string(args) != `{"value":1}` {
			t.Errorf("rich hub dispatch: %s %s %+v %s", spec, name, scope, args)
		}
		return sdk.ToolResult{Content: content, IsError: true}, nil
	}
	t.Cleanup(func() { runExtensionPluginTool = previous })
	ctx := WithHubCaller(t.Context(), HubCaller{Session: "library-session"})
	ctx = extension.WithRequestMetadata(ctx, extension.RequestMetadata{Plugin: "tool.module", RequestID: "hub-request", CallerID: "hub-caller"})
	args := json.RawMessage(`{"value":1,"_magpie":{"provider":"vendor","account":"selected"}}`)
	body, err := RunPluginToolResultOf(ctx, groups[0].Spec, "echo", args, "claude")
	var result sdk.ToolResult
	if err != nil || json.Unmarshal(body, &result) != nil || !result.IsError || !bytes.Equal(result.Content, content) {
		t.Fatalf("rich hub result changed: %s, %v", body, err)
	}
	restricted := access.WithIdentity(ctx, access.Identity{Models: []string{"vendor/model"}})
	if _, err := RunPluginToolResultOf(restricted, groups[0].Spec, "echo", args, "claude"); err == nil {
		t.Fatal("rich hub delegated restricted credentials")
	}
	if _, err := RunPluginToolResultOf(ctx, groups[0].Spec, "undeclared", args, "claude"); err == nil {
		t.Fatal("rich hub ran an undeclared tool")
	}
	fixture.stop()
	stop := gwtool.Gate()
	if _, err := RunPluginToolResultOf(ctx, groups[0].Spec, "echo", args, "claude"); err == nil {
		t.Error("rich hub ran while Plugin tools was off")
	}
	stop()
	pluginTools = func(context.Context) ([]plugin.PluginTools, error) { return nil, nil }
	if _, err := RunPluginToolResultOf(ctx, groups[0].Spec, "echo", args, "claude"); err == nil {
		t.Fatal("rich hub ran a disabled or removed module")
	}
	if calls != 1 {
		t.Fatalf("guarded hub calls invoked SDK %d times; want 1", calls)
	}
}

func TestExtensionToolsCatalogLifecycleWithoutBun(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_BUN", filepath.Join(t.TempDir(), "missing-bun"))
	stop := gwtool.Serve(gwtool.PluginTools)
	defer stop()
	source := t.TempDir()
	manifest := sdk.Manifest{
		Schema: 1, ID: "catalog-tools", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.ToolProvider}},
		Tools:    []sdk.ToolDescriptor{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "worker"), []byte("not executed by discovery"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := extension.NewCatalog(filepath.Join(appdir.Config(), "extensions"))
	if _, err := catalog.Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil }); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if tools, err := servedTools(t.Context()); err != nil || len(tools) != 1 || !anyPluginOn() {
		t.Fatalf("Go-only tools = %+v, %v", tools, err)
	}
	if count, err := plugin.ToolsOn(); err != nil || count != 1 {
		t.Fatalf("Go-only tool count = %d, %v", count, err)
	}
	if err := manager.SetEnabled(t.Context(), manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if tools, err := servedTools(t.Context()); err != nil || len(tools) != 0 {
		t.Fatalf("disabled tools = %+v, %v", tools, err)
	}
	if list, err := plugin.ReadList(); err != nil || len(list.Plugins) != 1 || !list.Plugins[0].Off {
		t.Fatalf("disabled server presence = %+v, %v", list, err)
	}
	if err := manager.Remove(t.Context(), manifest.ID); err != nil {
		t.Fatal(err)
	}
	if list, err := plugin.ReadList(); err != nil || len(list.Plugins) != 0 || anyPluginOn() {
		t.Fatalf("removed server presence = %+v, %v", list, err)
	}
}
