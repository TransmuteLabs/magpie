package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func extensionGatewayManifest() sdk.Manifest {
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	return sdk.Manifest{Schema: sdk.ManifestSchema, ID: "fixture.gateway", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.OutboundExecutor}},
		Providers: []sdk.ProviderDescriptor{{ID: "fixture-outbound", Name: "Extension fixture", Protocols: []protocolext.Protocol{"chat"},
			Models: []sdk.Model{{ID: "m1", Protocols: []protocolext.Protocol{"chat"}, ContextWindow: 32768, MaxOutput: 4096}}}}}
}

func TestExtensionProviderNeverFallsBackAfterAcceptance(t *testing.T) {
	worker := filepath.Join(t.TempDir(), extensionGatewayManifest().Entry)
	buildContext, stopBuild := context.WithTimeout(context.Background(), time.Minute)
	defer stopBuild()
	build := exec.CommandContext(buildContext, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", worker, "./testdata/extensionworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build external worker fixture: %v\n%s", err, output)
	}
	for _, scenario := range []struct {
		name      string
		status    int
		fallbacks int32
		noRetry   bool
		completed bool
	}{
		{name: "death", status: http.StatusConflict, noRetry: true},
		{name: "accepted failure", status: http.StatusConflict, noRetry: true},
		{name: "completed success", status: http.StatusOK, completed: true},
		{name: "not-started", status: http.StatusOK, fallbacks: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			t.Setenv("MAGPIE_ADDR", "")
			directory := t.TempDir()
			input, err := os.Open(worker)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			output, err := os.OpenFile(filepath.Join(directory, extensionGatewayManifest().Entry), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
			if err != nil {
				t.Fatal(err)
			}
			_, copyErr := io.Copy(output, input)
			closeErr := output.Close()
			if copyErr != nil || closeErr != nil {
				t.Fatalf("worker copy: %v %v", copyErr, closeErr)
			}
			manifest := extensionGatewayManifest()
			raw, _ := json.Marshal(manifest)
			if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			manager := extensionhost.Default()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			t.Cleanup(func() {
				stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := manager.Close(stop); err != nil {
					t.Errorf("extension manager cleanup: %v", err)
				}
			})
			if _, err := manager.Install(ctx, directory); err != nil {
				t.Fatal(err)
			}
			provider.Changed()
			var fallbacks atomic.Int32
			vendor := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				fallbacks.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				io.WriteString(writer, `{"id":"fallback","choices":[{"message":{"role":"assistant","content":"fallback answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			t.Cleanup(vendor.Close)
			if err := provider.Save(provider.Provider{ID: "fallback", Name: "Fallback", Chat: vendor.URL + "/v1", Key: "fixture", Models: []string{"m1"}}); err != nil {
				t.Fatal(err)
			}
			id := provider.ExtensionProviderID(manifest.ID, manifest.Providers[0].ID)
			if err := provider.SaveGroup(provider.Group{ID: "external", Name: "External", Routing: "ordered",
				Members: []string{id + "/m1", "fallback/m1"}}); err != nil {
				t.Fatal(err)
			}
			requestContext := extensionhost.WithRequestMetadata(ctx, extensionhost.RequestMetadata{
				Plugin: manifest.ID, RequestID: "fixture-request", CallerID: "host-fixture-caller",
				Value: json.RawMessage(`{"fixture":true}`),
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"group/external","messages":[{"role":"user","content":"`+scenario.name+`"}]}`)).WithContext(requestContext)
			request.RemoteAddr = "127.0.0.1:12345"
			reply := httptest.NewRecorder()
			New().Front().ServeHTTP(reply, request)
			if reply.Code != scenario.status || fallbacks.Load() != scenario.fallbacks {
				t.Fatalf("status=%d fallback calls=%d body=%s", reply.Code, fallbacks.Load(), reply.Body)
			}
			if scenario.noRetry && reply.Header().Get("X-Should-Retry") != "false" {
				t.Fatalf("uncertain outcome did not forbid replay: %v", reply.Header())
			}
			if scenario.noRetry || scenario.completed {
				operation := reply.Header().Get("X-Magpie-Operation")
				if scenario.completed {
					var response struct {
						Choices []struct {
							Message struct {
								Content string `json:"content"`
							} `json:"message"`
						} `json:"choices"`
					}
					if err := json.Unmarshal(reply.Body.Bytes(), &response); err != nil || len(response.Choices) != 1 {
						t.Fatalf("successful completion changed shape: %s (%v)", reply.Body, err)
					}
					content := response.Choices[0].Message.Content
					if !strings.HasPrefix(content, "native success: ") {
						t.Fatalf("successful completion was changed: %s", reply.Body)
					}
					operation = strings.TrimPrefix(content, "native success: ")
				}
				journal, err := manager.Store().Operation(manifest.ID, operation)
				if operation == "" || err != nil || journal.State == "" || journal.State == "dispatched" {
					t.Fatalf("operation was not durably terminal: %+v %v", journal, err)
				}
				if scenario.completed && journal.State != "completed" {
					t.Fatalf("successful operation was not completed: %+v", journal)
				}
				retry := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
					strings.NewReader(`{"model":"group/external","messages":[{"role":"user","content":"`+scenario.name+`"}]}`)).WithContext(requestContext)
				retry.RemoteAddr = "127.0.0.1:12345"
				retried := httptest.NewRecorder()
				New().Front().ServeHTTP(retried, retry)
				if retried.Code != http.StatusConflict || fallbacks.Load() != 0 || retried.Header().Get("X-Magpie-Operation") != operation {
					t.Fatalf("native request retry escaped its operation guard: %d calls=%d headers=%v body=%s",
						retried.Code, fallbacks.Load(), retried.Header(), retried.Body)
				}
				if retried.Header().Get("X-Should-Retry") != "false" || !strings.Contains(retried.Body.String(), "inspect the operation") {
					t.Fatalf("native request retry did not require status inspection: headers=%v body=%s", retried.Header(), retried.Body)
				}
			}
		})
	}
}
