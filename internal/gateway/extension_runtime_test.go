package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestEmptyPluginCatalogsUseOrdinaryGateway(t *testing.T) {
	fresh(t)
	t.Setenv("PATH", os.Getenv(standInsEnv))
	t.Setenv("MAGPIE_BUN", "")
	plugin.Restart()
	t.Cleanup(plugin.Settle)
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	checkEmpty := func() {
		t.Helper()
		if entries, err := manager.List(); err != nil || len(entries) != 0 {
			t.Fatalf("SDK catalog: entries=%v err=%v", entries, err)
		}
		if list, err := plugin.ReadList(); err != nil || len(list.Plugins) != 0 {
			t.Fatalf("plugin catalog: list=%v err=%v", list, err)
		}
		registry, err := manager.Protocols()
		if err != nil {
			t.Fatal(err)
		}
		if paths := registry.Paths(); len(paths) != 0 {
			t.Fatalf("empty SDK catalog registered native routes: %v", paths)
		}
		if proc.FindTool("bun") != "" || plugin.HasBun() || plugin.Running() {
			t.Fatal("plugin-free gateway found Bun or started a legacy host")
		}
	}
	checkEmpty()

	type observation struct {
		path, body, authorization string
	}
	observed := make(chan observation, 2)
	serveOn(t, "base", "builtin-key", []string{"m1"}, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		select {
		case observed <- observation{request.URL.Path, string(body), request.Header.Get("Authorization")}:
		default:
			t.Error("plugin-free request reached the upstream more than once")
		}
		writer.Header().Set("Content-Type", "application/json")
		io.WriteString(writer, `{"id":"builtin","choices":[{"message":{"role":"assistant","content":"builtin reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	server := New()
	t.Cleanup(server.idle.stop)
	for _, path := range []string{"/v1/models", "/v1/chat/completions"} {
		if !server.ClientPath(path) {
			t.Fatalf("empty registry lost core route %s", path)
		}
	}
	if server.ClientPath("/fixture/rpc") {
		t.Fatal("empty registry claimed an uninstalled native route")
	}
	web := httptest.NewServer(server.Front())
	t.Cleanup(web.Close)
	client := web.Client()
	client.Timeout = 20 * time.Second
	call := func(method, path, body string, status int) []byte {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, web.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("%s %s with empty plugin catalogs: HTTP %d: %s", method, path, response.StatusCode, answer)
		}
		return answer
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body := call(http.MethodGet, "/v1/models", "", http.StatusOK)
	if err := json.Unmarshal(body, &models); err != nil {
		t.Fatal(err)
	}
	if len(models.Data) != 1 || models.Data[0].ID != "base/m1" {
		t.Fatalf("builtin model catalog with no plugins: %s", body)
	}
	call(http.MethodPost, "/fixture/rpc", `{}`, http.StatusNotFound)
	body = call(http.MethodPost, "/v1/chat/completions", `{"model":"base/m1","messages":[{"role":"user","content":"hello"}]}`, http.StatusOK)
	if !strings.Contains(string(body), `"content":"builtin reply"`) {
		t.Fatalf("builtin reply with no plugins: %s", body)
	}
	select {
	case got := <-observed:
		if got.path != "/v1/chat/completions" || modelOf([]byte(got.body)) != "m1" || got.authorization != "Bearer builtin-key" {
			t.Fatalf("builtin HTTP provider request: %+v", got)
		}
	default:
		t.Fatal("ordinary gateway never reached the builtin HTTP provider")
	}
	if len(observed) != 0 {
		t.Fatal("model discovery or an uninstalled native route called the HTTP provider")
	}
	checkEmpty()
}

func compiledSDKExample(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dist := t.TempDir()
	entry := "plugin"
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		entry += ".exe"
		goBinary += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := proc.CommandContext(ctx, goBinary, "build", "-o", filepath.Join(dist, entry), "./examples/extensions/echo-go")
	command.Dir = root
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build standalone public-SDK example: %v\n%s", err, output)
	}
	body, err := os.ReadFile(filepath.Join(root, "examples", "extensions", "echo-go", sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err = json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Entry = entry
	body, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dist, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return dist
}

func TestInstalledGoSDKUsesOrdinaryGateway(t *testing.T) {
	fresh(t)
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := manager.Install(ctx, compiledSDKExample(t)); err != nil {
		t.Fatal(err)
	}
	if manager.Running("sdk-echo") {
		t.Fatal("installation left its handshake worker running")
	}
	server := New()
	if !server.ClientPath("/sdk-echo/rpc") {
		t.Fatal("ordinary Server did not discover the installed native endpoint")
	}
	denied := httptest.NewRequest(http.MethodPost, "/sdk-echo/rpc", strings.NewReader(`{"prompt":"forbidden"}`))
	denied.RemoteAddr = "192.0.2.44:1234"
	w := httptest.NewRecorder()
	server.Front().ServeHTTP(w, denied)
	if w.Code == 200 || manager.Running("sdk-echo") {
		t.Fatalf("denied native call started a worker: HTTP %d", w.Code)
	}

	web := httptest.NewServer(server.Front())
	t.Cleanup(web.Close)
	client := web.Client()
	client.Timeout = 20 * time.Second
	model := provider.ExtensionProviderID("sdk-echo", "echo") + "/echo"
	post := func(payload map[string]any) (int, string, string) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, web.URL+"/sdk-echo/rpc", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(answer), response.Header.Get("Content-Type")
	}
	code, body, _ := post(map[string]any{"model": model, "prompt": "native through Go", "requestId": "one"})
	if code != 200 || !strings.Contains(body, `"content":"native through Go"`) {
		t.Fatalf("native->provider->native: %d %s", code, body)
	}
	if !manager.Running("sdk-echo") {
		t.Fatal("native endpoint never started its installed worker")
	}
	code, body, _ = post(map[string]any{"model": model, "prompt": "native through Go", "requestId": "one"})
	if code != http.StatusConflict {
		t.Fatalf("completed native request was replayed: %d %s", code, body)
	}
	code, body, contentType := post(map[string]any{"model": model, "prompt": "streamed", "stream": true, "requestId": "two"})
	if code != 200 || !strings.Contains(contentType, "text/event-stream") ||
		!strings.Contains(body, `"content":"streamed"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("streamed native roundtrip: %d %s %s", code, contentType, body)
	}
	code, body, _ = post(map[string]any{"models": true})
	if code != 200 || !strings.Contains(body, model) {
		t.Fatalf("authorized native catalog: %d %s", code, body)
	}
	tool := map[string]any{"tool": "echo", "arguments": map[string]string{"text": "native tool"}, "requestId": "tool-one"}
	code, body, _ = post(tool)
	if code != 200 || !strings.Contains(body, "native tool") || strings.Contains(body, `"isError":true`) {
		t.Fatalf("native control through scoped MCP: %d %s", code, body)
	}
	code, body, _ = post(tool)
	if code != 200 || !strings.Contains(body, `"isError":true`) {
		t.Fatalf("completed native tool was replayed: %d %s", code, body)
	}
	if err := manager.SetEnabled(ctx, "sdk-echo", false); err != nil {
		t.Fatal(err)
	}
	if server.ClientPath("/sdk-echo/rpc") {
		t.Fatal("disabled extension path remained discoverable")
	}
	code, _, _ = post(map[string]any{"model": model, "prompt": "disabled"})
	if code == 200 {
		t.Fatal("disabled extension was still called")
	}
}
