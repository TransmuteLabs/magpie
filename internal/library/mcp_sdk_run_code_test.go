package library

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/gwtool"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const codeSDKEndpoint = gateway.HubMCPPath + "claude"

type codeSDKReply struct {
	Result struct {
		Content []struct{ Text string }
		IsError bool
	}
	Error *struct {
		Code    int
		Message string
	}
}

func codeSDKFixture(t *testing.T) (func(string, string, any) codeSDKReply, string, access.Key) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX worker launcher")
	}
	hubSandbox(t)
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	t.Setenv("MAGPIE_PUBLIC_URL", "")
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(gwtool.Serve(gwtool.PluginTools))
	source := t.TempDir()
	journal := filepath.Join(t.TempDir(), "calls")
	manifest := sdk.Manifest{
		Schema: 1, ID: "hubcode-fixture", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.ToolProvider}},
		Tools: []sdk.ToolDescriptor{
			{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "blocked", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	worker := "#!/bin/sh\nexec " + quote(exe) + " -test.run='^TestHubCodeSDKWorker$' -- " + quote(journal) + "\n"
	for name, content := range map[string][]byte{sdk.ManifestFile: body, "worker": []byte(worker)} {
		if err := os.WriteFile(filepath.Join(source, name), content, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if _, err := change(func(library *Library) error {
		library.MCPHub = true
		library.MCP = append(library.MCP, &Server{
			Name: "sdkfixture", Plugin: "extension:" + manifest.ID, Transport: "http",
			URL: gateway.URL() + gateway.ToolsMCPOne + manifest.ID, Agents: []string{"claude"},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := access.ConfigureLAN(true, false); err != nil {
		t.Fatal(err)
	}
	key, err := access.Update("add-key", access.Change{Name: "SDK echo only"})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := access.List()
	if err != nil || len(keys) == 0 {
		t.Fatalf("fixture keys: %d %v", len(keys), err)
	}
	identity := keys[len(keys)-1]
	if _, err := access.Update("mcp-key", access.Change{Key: identity.ID, MCP: []string{"sdkfixture/echo"}}); err != nil {
		t.Fatal(err)
	}
	handler := gateway.New().Front()
	return func(tool, code string, args any) codeSDKReply {
		t.Helper()
		if tool == "run_code" {
			args = map[string]string{"code": code}
		}
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": args},
		})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, codeSDKEndpoint, strings.NewReader(string(body)))
		request.RemoteAddr = "127.0.0.1:50000"
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var out codeSDKReply
		if err := json.Unmarshal(response.Body.Bytes(), &out); response.Code != http.StatusOK || err != nil {
			t.Fatalf("HTTP %d %s (%v)", response.Code, response.Body, err)
		}
		return out
	}, journal, identity
}

func codeSDKRows(t *testing.T, identity access.Key) []usage.Record {
	t.Helper()
	body, err := os.ReadFile(usage.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "PRIVATE_SDK_ARG") {
		t.Fatal("Usage retained a tool argument")
	}
	var rows []usage.Record
	for line := range strings.Lines(string(body)) {
		var row usage.Record
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row.Source != usage.SourceMCP {
			continue
		}
		if row.Kind != usage.KindMCP || row.Operation != "execute_tool" || !row.IsProbe() ||
			row.Endpoint != codeSDKEndpoint || row.CallerKeyID != identity.ID || row.CallerKeyName != identity.Name || row.Time.IsZero() {
			t.Fatalf("incorrect Usage attribution: %+v", row)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestHubCodeUsesSDKToolsAndAuditsCalls(t *testing.T) {
	call, journal, identity := codeSDKFixture(t)
	got := call("run_code", `
const first = await tools.sdkfixture__echo({value: "PRIVATE_SDK_ARG"});
const second = await tools.echo({value: "second"});
try {
	await tools.sdkfixture__echo({fail: true});
} catch (error) {
	return [first, second, error.message];
}`, nil)
	var result []string
	if got.Error != nil || got.Result.IsError || len(got.Result.Content) != 1 ||
		json.Unmarshal([]byte(got.Result.Content[0].Text), &result) != nil ||
		!slices.Equal(result, []string{"PRIVATE_SDK_ARG", "second", "SDK fixture error"}) {
		t.Fatalf("run_code did not return actual SDK results: %+v", got)
	}
	if got := read(t, journal); got != "echo\necho\necho\n" {
		t.Fatalf("SDK calls: %q", got)
	}
	rows := codeSDKRows(t, identity)
	if len(rows) != 4 {
		t.Fatalf("expected three SDK rows and one run_code row: %+v", rows)
	}
	for index, row := range rows[:3] {
		if row.Requested != "sdkfixture__echo" || row.Host != "sdkfixture" || row.Agent != "claude" ||
			(index < 2 && row.Error != "") || (index == 2 && row.Error != "SDK fixture error") {
			t.Fatalf("SDK row %d: %+v", index, row)
		}
	}
	if outer := rows[3]; outer.Requested != "run_code" || outer.Host != "magpie" || outer.Agent != "claude" || outer.Error != "" {
		t.Fatalf("run_code row: %+v", outer)
	}
}

func TestHubCodeAuditsDeniedSDKName(t *testing.T) {
	call, journal, identity := codeSDKFixture(t)
	const blocked = "sdkfixture__blocked"
	direct := call(blocked, "", map[string]string{"value": "PRIVATE_SDK_ARG"})
	if direct.Error == nil || direct.Error.Code != -32602 || !strings.Contains(direct.Error.Message, blocked) {
		t.Fatalf("direct call of denied tool: %+v", direct)
	}
	directRows := codeSDKRows(t, identity)
	if len(directRows) != 1 || directRows[0].Requested != blocked || directRows[0].Error == "" {
		t.Fatalf("expected a failed row for the direct refusal: %+v", directRows)
	}
	got := call("run_code", `
const allowed = await tools.sdkfixture__echo({value: "allowed"});
try {
	await tools.sdkfixture__blocked({value: "PRIVATE_SDK_ARG"});
} catch (error) {
	return allowed + ": " + error.message;
}
return "denied tool ran";`, nil)
	if got.Error != nil || got.Result.IsError || len(got.Result.Content) != 1 ||
		!strings.HasPrefix(got.Result.Content[0].Text, "allowed: no tool is called "+blocked) {
		t.Fatalf("restricted run_code: %+v", got)
	}
	if got := read(t, journal); got != "echo\n" {
		t.Fatalf("denied tool reached SDK runtime: %q", got)
	}
	rows := codeSDKRows(t, identity)
	var names []string
	for _, row := range rows {
		names = append(names, row.Requested)
	}
	if !slices.Equal(names, []string{blocked, "sdkfixture__echo", blocked, "run_code"}) {
		t.Fatalf("missing or duplicated Usage rows after nested refusal: %v", names)
	}
	if nested := rows[2]; nested.Host != "magpie" || nested.Agent != "claude" ||
		!strings.Contains(nested.Error, "no tool is called "+blocked) || rows[3].Error != "" {
		t.Fatalf("nested refusal and its successful run_code row: %+v", rows[2:])
	}
}

func TestHubCodeSDKWorker(t *testing.T) {
	if len(os.Args) != 4 || os.Args[2] != "--" {
		return
	}
	body, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodToolCall: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.ToolRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			file, err := os.OpenFile(os.Args[3], os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
			if err != nil {
				return nil, err
			}
			_, err = fmt.Fprintln(file, request.ID)
			closeErr := file.Close()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, closeErr
			}
			var args struct {
				Value string
				Fail  bool
			}
			if err := json.Unmarshal(request.Arguments, &args); err != nil {
				return nil, err
			}
			if args.Fail {
				return sdk.ToolResult{Content: json.RawMessage(`"SDK fixture error"`), IsError: true}, nil
			}
			text, err := json.Marshal(args.Value)
			return sdk.ToolResult{Content: text}, err
		},
	}}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
