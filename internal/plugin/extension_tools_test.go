package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionToolsFake struct {
	entries  []extension.Installed
	listErr  error
	callErr  error
	result   json.RawMessage
	calls    int
	scope    sdk.Scope
	method   string
	module   string
	request  sdk.ToolRequest
	deadline bool
}

func (fake *extensionToolsFake) List() ([]extension.Installed, error) {
	return fake.entries, fake.listErr
}

func (fake *extensionToolsFake) Call(ctx context.Context, module, method string, scope sdk.Scope, params any, consume func(extension.Event) error) (json.RawMessage, error) {
	fake.calls++
	fake.scope, fake.module, fake.method = scope, module, method
	fake.request, _ = params.(sdk.ToolRequest)
	_, fake.deadline = ctx.Deadline()
	return bytes.Clone(fake.result), fake.callErr
}

func extensionToolsFixture(t *testing.T) *extensionToolsFake {
	t.Helper()
	storeSandbox(t)
	fake := &extensionToolsFake{
		entries: []extension.Installed{{Manifest: sdk.Manifest{
			ID: "tool.module", Contract: sdk.Contract{Capabilities: []sdk.Capability{sdk.ToolProvider}},
			Tools: []sdk.ToolDescriptor{
				{Name: "echo", Description: "Echo a value", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)},
				{Name: "another", InputSchema: json.RawMessage(`{"type":"object"}`)},
			},
		}}},
		result: json.RawMessage(`{"content":[{"type":"text","text":"hello"},{"type":"text","text":"again"}]}`),
	}
	previousRuntime, previousLegacy := extensionToolsRuntime, legacyToolPlugins
	extensionToolsRuntime = func() extensionToolRuntime { return fake }
	legacyToolPlugins = func(context.Context) ([]Loaded, error) {
		t.Error("SDK-only tools started the legacy host")
		return nil, errors.New("unexpected legacy host")
	}
	t.Cleanup(func() { extensionToolsRuntime, legacyToolPlugins = previousRuntime, previousLegacy })
	return fake
}

func TestExtensionToolsDiscoverWithoutWorkers(t *testing.T) {
	fake := extensionToolsFixture(t)
	disabled := fake.entries[0]
	disabled.Manifest.ID, disabled.Disabled = "disabled", true
	fake.entries = append(fake.entries, disabled, extension.Installed{Manifest: sdk.Manifest{ID: "without-tools"}})
	writeList(t, `{"plugins":[{"spec":"old-tools","off":true}]}`)
	if err := os.WriteFile(toolsPath(), []byte(`{"old-tools":["old"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tools, err := Tools(t.Context())
	if err != nil || len(tools) != 1 || tools[0].Spec != "extension:tool.module" || len(tools[0].Tools) != 2 {
		t.Fatalf("catalog tools = %+v, %v", tools, err)
	}
	tools[0].Tools[0].Schema[0] = 'x'
	tools, err = Tools(t.Context())
	if err != nil || !json.Valid(tools[0].Tools[0].Schema) {
		t.Fatalf("tool descriptors share mutable schemas: %+v, %v", tools, err)
	}
	list, err := ReadList()
	if err != nil || len(list.Plugins) != 4 || !list.Plugins[2].Off {
		t.Fatalf("merged installed list = %+v, %v", list, err)
	}
	if names := ToolNames(); strings.Join(names["extension:tool.module"], ",") != "another,echo" ||
		len(names["extension:disabled"]) != 2 || len(names["old-tools"]) != 1 {
		t.Fatalf("merged names = %+v", names)
	}
	if count, err := ToolsOn(); err != nil || count != 1 {
		t.Fatalf("enabled tool modules = %d, %v", count, err)
	}
	fake.entries[0].Disabled = true
	if tools, err := Tools(t.Context()); err != nil || len(tools) != 0 {
		t.Fatalf("disabled tools = %+v, %v", tools, err)
	}
	if count, err := ToolsOn(); err != nil || count != 0 {
		t.Fatalf("disabled count = %d, %v", count, err)
	}
	if list, err := ReadList(); err != nil || len(list.Plugins) != 4 || !list.Plugins[1].Off {
		t.Fatalf("disabled module disappeared from installed list: %+v, %v", list, err)
	}
	fake.entries = nil
	if list, err := ReadList(); err != nil || len(list.Plugins) != 1 {
		t.Fatalf("removed module remains installed: %+v, %v", list, err)
	}
	if names := ToolNames(); len(names["extension:tool.module"]) != 0 {
		t.Fatalf("removed tools remain noted: %+v", names)
	}
	if fake.calls != 0 {
		t.Fatalf("metadata started %d workers", fake.calls)
	}
}

func TestExtensionToolsMergeLegacy(t *testing.T) {
	extensionToolsFixture(t)
	writeList(t, `{"plugins":[{"spec":"old-tools"}]}`)
	loaded := 0
	legacyToolPlugins = func(context.Context) ([]Loaded, error) {
		loaded++
		return []Loaded{{Spec: "old-tools", Tools: []Tool{{Name: "old"}}}}, nil
	}
	tools, err := Tools(t.Context())
	if err != nil || loaded != 1 || len(tools) != 2 || tools[0].Spec != "old-tools" || tools[1].Spec != "extension:tool.module" {
		t.Fatalf("legacy + SDK tools = %+v, %v; host calls %d", tools, err, loaded)
	}
}

func TestExtensionToolsWithoutLegacyNotes(t *testing.T) {
	fake := extensionToolsFixture(t)
	if names := ToolNames(); strings.Join(names["extension:tool.module"], ",") != "another,echo" {
		t.Fatalf("SDK names need a legacy note: %+v", names)
	}
	if count, err := ToolsOn(); err != nil || count != 1 {
		t.Fatalf("SDK count needs a legacy note: %d, %v", count, err)
	}
	fake.entries = nil
	if names := ToolNames(); names != nil {
		t.Fatalf("legacy missing-note result changed: %+v", names)
	}
	if count, err := ToolsOn(); err != nil || count != 0 {
		t.Fatalf("no plugins = %d, %v", count, err)
	}
}

func TestExtensionToolsUnknownMetadataFailsClosed(t *testing.T) {
	fake := extensionToolsFixture(t)
	fake.listErr = errors.New("catalog unreadable")
	if _, err := Tools(t.Context()); err == nil {
		t.Fatal("unknown catalog listed tools")
	}
	if _, err := ReadList(); err == nil {
		t.Fatal("unknown catalog became no installed modules")
	}
	if _, err := ToolsOn(); err == nil {
		t.Fatal("unknown catalog became zero active modules")
	}
	if names := ToolNames(); names != nil {
		t.Fatalf("unknown catalog returned partial names: %+v", names)
	}
	fake.listErr = nil
	writeList(t, "{")
	if _, err := Tools(t.Context()); err == nil {
		t.Fatal("malformed legacy list started the host")
	}
	if _, err := ReadList(); err == nil {
		t.Fatal("malformed installed list became empty")
	}
	if _, err := ToolsOn(); err == nil {
		t.Fatal("malformed installed list became zero")
	}
	writeList(t, `{"plugins":[]}`)
	if err := os.WriteFile(toolsPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ToolsOn(); err == nil {
		t.Fatal("unknown legacy note became zero")
	}
	if names := ToolNames(); names != nil {
		t.Fatalf("unknown note returned partial names: %+v", names)
	}
}

func TestExtensionToolUsesHostScope(t *testing.T) {
	fake := extensionToolsFixture(t)
	scope := sdk.Scope{Plugin: "tool.module", Provider: "vendor", Account: "chosen", Generation: 7, Session: "session", Operation: "operation", Attempt: 2}
	ctx := extension.WithToolScope(t.Context(), scope)
	text, err := RunTool(ctx, "extension:tool.module", "echo", json.RawMessage(`{"value":"hello"}`), "claude")
	if err != nil || text != "hello\nagain" {
		t.Fatalf("SDK text tool = %q, %v", text, err)
	}
	if fake.calls != 1 || fake.scope != scope || fake.module != "tool.module" || fake.method != sdk.MethodToolCall ||
		fake.request.ID != "echo" || string(fake.request.Arguments) != `{"value":"hello"}` || !fake.deadline {
		t.Fatalf("SDK dispatch = %+v", fake)
	}
	fake.result = json.RawMessage(`{"content":[{"type":"image","data":"aW1hZ2U=","mimeType":"image/png"}],"isError":true}`)
	result, err := RunExtensionTool(ctx, "extension:tool.module", "echo", json.RawMessage(`{}`))
	if err != nil || !result.IsError || !strings.Contains(string(result.Content), `"type":"image"`) {
		t.Fatalf("rich result = %+v, %v", result, err)
	}
	fake.result = json.RawMessage(`{"content":"tool failed","isError":true}`)
	if _, err := RunTool(ctx, "extension:tool.module", "echo", nil, ""); err == nil || err.Error() != "tool failed" {
		t.Fatalf("tool's error flag was lost: %v", err)
	}
	fake.result = json.RawMessage(`{"content":"unbound"}`)
	if _, err := RunTool(t.Context(), "extension:tool.module", "echo", nil, ""); err != nil {
		t.Fatal(err)
	}
	if fake.scope != (sdk.Scope{Plugin: "tool.module"}) || string(fake.request.Arguments) != "{}" {
		t.Fatalf("unbound call inherited an account: %+v, %s", fake.scope, fake.request.Arguments)
	}
}

func TestExtensionToolCannotChooseAuthorityOrRunOff(t *testing.T) {
	fake := extensionToolsFixture(t)
	for _, args := range []json.RawMessage{
		json.RawMessage(`{"_magpie":{"provider":"vendor","account":"other","generation":99}}`),
		json.RawMessage(`[]`), json.RawMessage(`{`),
	} {
		if _, err := RunTool(t.Context(), "extension:tool.module", "echo", args, ""); err == nil {
			t.Fatalf("unprepared arguments ran: %s", args)
		}
	}
	ctx := extension.WithToolScope(t.Context(), sdk.Scope{Plugin: "another-module"})
	if _, err := RunTool(ctx, "extension:tool.module", "echo", nil, ""); err == nil {
		t.Fatal("another module's scope ran")
	}
	if _, err := RunTool(t.Context(), "extension:../escape", "echo", nil, ""); err == nil {
		t.Fatal("invalid extension spec reached a runtime")
	}
	if _, err := RunTool(t.Context(), "extension:tool.module", "unknown", nil, ""); err == nil {
		t.Fatal("undeclared tool ran")
	}
	fake.entries[0].Disabled = true
	if _, err := RunTool(t.Context(), "extension:tool.module", "echo", nil, ""); err == nil {
		t.Fatal("disabled tool ran")
	}
	fake.entries = nil
	if _, err := RunTool(t.Context(), "extension:tool.module", "echo", nil, ""); err == nil {
		t.Fatal("removed tool ran")
	}
	if fake.calls != 0 {
		t.Fatalf("refused arguments reached %d workers", fake.calls)
	}
}

func TestExtensionToolDirectCallPreservesNetworkHold(t *testing.T) {
	fake := extensionToolsFixture(t)
	held := errors.New("explicit tool network hold")
	ctx := netproxy.Hold(t.Context(), held)
	if _, err := RunTool(ctx, "extension:tool.module", "echo", nil, "claude"); !errors.Is(err, held) {
		t.Fatalf("direct SDK call lost network hold: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("held direct tool invoked SDK %d times", fake.calls)
	}
}
