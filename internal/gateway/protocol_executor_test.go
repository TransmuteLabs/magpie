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

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

func installExecutorFixture(t *testing.T, manager *extension.Manager, binary, id string, minor int, inbound bool) sdk.Manifest {
	t.Helper()
	directory := t.TempDir()
	filename := filepath.Base(binary)
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, filename), body, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: id, Version: "1.0.0", Runtime: "exec", Entry: filename,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: minor}, Capabilities: []sdk.Capability{sdk.OutboundExecutor}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Executor fixture", Protocols: []codec.Protocol{codec.Chat},
			Models: []sdk.Model{{ID: "m1", Protocols: []codec.Protocol{codec.Chat}, ContextWindow: 32768}}}},
	}
	if inbound {
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.InboundCodec)
		manifest.Inbounds = []sdk.InboundDescriptor{{ID: "executor", Path: "/executor.fixture/Call"}}
	} else {
		manifest.Providers[0].ID = "foreign-vendor"
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestNativeExecutorConstraintSurvivesOrdinaryRouting(t *testing.T) {
	filename := "executorworker"
	if runtime.GOOS == "windows" {
		filename += ".exe"
	}
	binary := filepath.Join(t.TempDir(), filename)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, "./testdata/executorworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build paired executor: %v\n%s", err, output)
	}
	for _, scenario := range []string{"self", "foreign-first", "self-refused", "foreign-only", "alias", "unrestricted", "invalid", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			t.Setenv("MAGPIE_ADDR", "")
			manager := extension.Default()
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := manager.Close(ctx); err != nil {
					t.Error(err)
				}
			})
			minor := 4
			if scenario == "legacy" {
				minor = 3
			}
			manifest := installExecutorFixture(t, manager, binary, "fixture.executor", minor, true)
			installExecutorFixture(t, manager, binary, "fixture.foreign", 4, false)
			var plainCalls atomic.Int32
			vendor := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" {
					writer.WriteHeader(http.StatusNoContent)
					return
				}
				plainCalls.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"plain answer"},"finish_reason":"stop"}]}`)
			}))
			t.Cleanup(vendor.Close)
			if err := provider.Save(provider.Provider{ID: "plain-executor", Name: "Plain", Key: "fixture", Chat: vendor.URL + "/v1", Models: []string{"m1"}}); err != nil {
				t.Fatal(err)
			}
			self := provider.ExtensionProviderID(manifest.ID, "vendor") + "/m1"
			foreign := provider.ExtensionProviderID("fixture.foreign", "foreign-vendor") + "/m1"
			members := []string{"plain-executor/m1", foreign, self}
			if scenario == "self-refused" {
				members = []string{self, foreign, "plain-executor/m1"}
			}
			if err := provider.SaveGroup(provider.Group{ID: "executor-fixture", Name: "Executor fixture", Routing: "order", Members: members}); err != nil {
				t.Fatal(err)
			}
			model, content, policy := "group/executor-fixture", "complete", "same-module"
			switch scenario {
			case "self", "legacy":
				model = self
			case "foreign-only":
				model = "plain-executor/m1"
			case "self-refused":
				content = "refuse"
			case "unrestricted":
				policy = ""
			case "invalid":
				model = self
				policy = "another-module"
			}
			secret := ""
			if scenario == "alias" {
				keys, secrets := newCaller(t, "Executor alias")
				if _, err := access.Update("aliases-key", access.Change{Key: keys[0].ID,
					Aliases: map[string]string{"assistant": model}}); err != nil {
					t.Fatal(err)
				}
				model, secret = "assistant", secrets[0]
			}
			input, _ := json.Marshal(map[string]string{"model": model, "content": content, "executor": policy})
			reply := protocolPost(New().Front(), "/executor.fixture/Call", string(input), secret)
			switch scenario {
			case "unrestricted":
				if reply.Code != http.StatusOK || plainCalls.Load() != 1 || !strings.Contains(reply.Body.String(), "plain answer") {
					t.Fatalf("ordinary fallback changed: %d calls=%d %s", reply.Code, plainCalls.Load(), reply.Body)
				}
			case "foreign-only", "self-refused", "invalid", "legacy":
				if reply.Code < 400 || plainCalls.Load() != 0 || strings.Contains(reply.Body.String(), "fixture.foreign") {
					t.Fatalf("native-only request escaped: %d calls=%d %s", reply.Code, plainCalls.Load(), reply.Body)
				}
			default:
				if reply.Code != http.StatusOK || plainCalls.Load() != 0 || !strings.Contains(reply.Body.String(), manifest.ID) {
					t.Fatalf("paired executor lost routing or metadata: %d calls=%d %s", reply.Code, plainCalls.Load(), reply.Body)
				}
			}
		})
	}
}
