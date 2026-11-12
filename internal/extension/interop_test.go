package extension

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

func bunFixturePackage(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	source := filepath.Join("..", "..", "sdk", "js")
	files, err := filepath.Glob(filepath.Join(source, "*.js"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join(source, "package.json"), filepath.Join(source, "test", "fixtures", "worker.js"))
	for _, file := range files {
		relative, err := filepath.Rel(source, file)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(directory, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "conformance", Version: "1.0.0", Runtime: "bun", Entry: "test/fixtures/worker.js",
		Contract:  sdk.FullContract(),
		Inbounds:  []sdk.InboundDescriptor{{ID: "native", Path: "/conformance/native"}},
		Providers: []sdk.ProviderDescriptor{{ID: "sample", Name: "Sample", Protocols: []codec.Protocol{codec.Chat, codec.Responses, codec.Messages, codec.Gemini}, Models: []sdk.Model{{ID: "echo"}}}},
		Agents:    []sdk.AgentDescriptor{{ID: "editor", Name: "Editor"}},
		Sessions:  []sdk.SessionDescriptor{{ID: "text", Name: "Text"}},
		Tools:     []sdk.ToolDescriptor{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestBunWorkerInstalledThroughGoHost(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("Bun runtime is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	manager := NewManager(t.TempDir())
	manager.Bun = bun
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := manager.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(ctx, bunFixturePackage(t)); err != nil {
		t.Fatal(err)
	}
	var tool sdk.ToolResult
	if err := manager.Invoke(ctx, "conformance", sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "echo", Arguments: json.RawMessage(`{"language":"bun"}`)}, &tool, nil); err != nil {
		t.Fatal(err)
	}
	if string(tool.Content) != `{"language":"bun"}` {
		t.Fatalf("cross-language tool result: %s", tool.Content)
	}
	var state sdk.StateValue
	if err := manager.Invoke(ctx, "conformance", sdk.MethodOperationStatus, sdk.Scope{},
		map[string]string{"mode": "host"}, &state, nil); err != nil {
		t.Fatal(err)
	}
	if state.Found || state.Version != "0" {
		t.Fatalf("cross-language scoped state: %+v", state)
	}

	var auth sdk.AuthResult
	if err := manager.Invoke(ctx, "conformance", sdk.MethodAuthFinish, sdk.Scope{Provider: "sample"},
		sdk.AuthFinishRequest{}, &auth, nil); err != nil {
		t.Fatal(err)
	}
	account, err := manager.Store().SaveAccount(Account{Plugin: "conformance", Provider: "sample", ID: auth.ID, Auth: auth.Credentials}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var result sdk.OperationResult
	if err := manager.Invoke(ctx, "conformance", sdk.MethodOperationStatus,
		sdk.Scope{Provider: "sample", Account: account.ID, Generation: account.Generation}, map[string]string{},
		&result, nil); err != nil {
		t.Fatal(err)
	}
	if string(result.Result) != `{"token":"fixture-token"}` {
		t.Fatalf("selected credentials did not reach their worker: %s", result.Result)
	}
	if err := manager.Invoke(ctx, "conformance", sdk.MethodToolCall,
		sdk.Scope{Provider: "sample", Account: account.ID, Generation: account.Generation},
		sdk.ToolRequest{ID: "echo", Arguments: json.RawMessage(`{"replace":true}`)}, &tool, nil); err != nil {
		t.Fatal(err)
	}
	current, err := manager.Store().GetAccount("conformance", "sample", account.ID)
	if err != nil || current.Generation != account.Generation+1 || string(current.Auth) != `{"token":"fixture-replaced"}` {
		t.Fatalf("Bun account replacement did not commit: generation=%d auth=%s err=%v", current.Generation, current.Auth, err)
	}

	registry, err := manager.Protocols()
	if err != nil {
		t.Fatal(err)
	}
	inbound, found := registry.Lookup("/conformance/native")
	if !found {
		t.Fatal("installed native endpoint was not registered")
	}
	inbound = codec.Bind(inbound, ctx)
	replyTo := httptest.NewRecorder()
	reply := inbound.Reply(replyTo)
	body := []byte(`{"model":"echo","messages":[]}`)
	decoded, err := inbound.Decode(ctx, http.Header{"Authorization": {"not-a-worker-credential"}}, body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Protocol != codec.Chat || string(decoded.Body) != string(body) {
		t.Fatalf("native request: %+v", decoded)
	}
	reply.WriteHeader(http.StatusOK)
	if _, err = reply.Write(body); err != nil {
		t.Fatal(err)
	}
	if err = reply.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if replyTo.Code != 200 || replyTo.Body.String() != string(body) {
		t.Fatalf("native response: %d %q", replyTo.Code, replyTo.Body.String())
	}
}
