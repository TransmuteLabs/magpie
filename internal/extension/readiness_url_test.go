package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// This probes real public SDK control frames through the ordinary component
// group's eager-service admission. Only process construction is substituted.
func TestComponentReadinessMatchesSDKs(t *testing.T) {
	var cases []struct {
		Name    string
		URL     string
		Valid   bool
		Startup bool
	}
	body, err := os.ReadFile("../../pkg/extension/testdata/ready-urls.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &cases); err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"go", "js"} {
		for _, test := range cases {
			if !test.Startup {
				continue
			}
			t.Run(language+"/"+test.Name, func(t *testing.T) {
				if language == "js" {
					if _, err := exec.LookPath("node"); err != nil {
						t.Skip("Node is not installed")
					}
				}
				manifest := sdk.Manifest{Schema: 1, ID: "ready-parity", Version: "1.0.0", Runtime: "exec", Entry: "primary",
					Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 1}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
					Services: []sdk.ServiceDescriptor{{ID: "helper", Runtime: "exec", Entry: "helper", Start: "eager", Contract: sdk.ServiceContract{ID: "helper", Version: 1}, Ready: sdk.ServiceReady{HTTPEndpoints: []string{"ui"}}}},
				}
				group := newComponentGroup(NewManager(t.TempDir()), Installed{Manifest: manifest}, sdk.ConfigSnapshot{Version: "0", Values: json.RawMessage(`{}`)})
				group.verify = func(ctx context.Context, _ Installed) error { return ctx.Err() }
				type peer struct {
					in     io.Writer
					out    *json.Decoder
					close  func()
					helper bool
				}
				peers := map[*workerClient]peer{}
				connect := func(module sdk.Module, helper bool) *workerClient {
					t.Helper()
					if err := module.Validate(); err != nil {
						t.Fatal(err)
					}
					life, cancel := context.WithCancel(context.Background())
					var in io.Writer
					var out io.Reader
					var stop func()
					if helper && language == "js" {
						encoded, _ := json.Marshal(module.Manifest)
						cmd := proc.CommandContext(life, "node", filepath.Join("testdata", "ready-worker.mjs"), string(encoded), test.URL)
						input, e := cmd.StdinPipe()
						if e != nil {
							t.Fatal(e)
						}
						output, e := cmd.StdoutPipe()
						if e != nil {
							t.Fatal(e)
						}
						cmd.Stderr = os.Stderr
						if err := cmd.Start(); err != nil {
							t.Fatal(err)
						}
						in, out = input, output
						stop = func() { input.Close(); cancel(); _ = cmd.Wait() }
					} else {
						connection, remote := net.Pipe()
						done := make(chan struct{})
						go func() { defer close(done); defer remote.Close(); _ = sdk.Serve(life, remote, remote, module) }()
						in, out = connection, connection
						stop = func() { cancel(); connection.Close(); <-done }
					}
					var once sync.Once
					client := &workerClient{}
					peers[client] = peer{in: in, out: json.NewDecoder(out), close: func() { once.Do(stop) }, helper: helper}
					return client
				}
				sdkAccepted := false
				sequence := 0
				group.invoke = func(client *workerClient, ctx context.Context, method string, scope sdk.Scope, auth json.RawMessage, params any, _ func(Event) error) (json.RawMessage, error) {
					p := peers[client]
					encoded, err := json.Marshal(params)
					if err != nil {
						return nil, err
					}
					sequence++
					deadline, ok := ctx.Deadline()
					if !ok {
						deadline = time.Now().Add(5 * time.Second)
					}
					request := sdk.Frame{Kind: sdk.KindCall, ID: fmt.Sprint(sequence), Method: method, Scope: scope, Auth: auth, Params: encoded, Deadline: deadline.UnixMilli()}
					if err := json.NewEncoder(p.in).Encode(request); err != nil {
						return nil, err
					}
					var result sdk.Frame
					if err := p.out.Decode(&result); err != nil {
						return nil, err
					}
					if result.Kind != sdk.KindResult || result.ID != request.ID {
						return nil, fmt.Errorf("unexpected frame: %+v", result)
					}
					if p.helper && method == sdk.MethodComponentConfigure {
						sdkAccepted = result.Error == nil
					}
					if result.Error != nil {
						return nil, result.Error
					}
					return result.Result, nil
				}
				group.stop = func(c *workerClient) { peers[c].close() }
				group.start = func(_ context.Context, _ string, entry Installed, _ string, _ func(context.Context, sdk.Scope, string, json.RawMessage) (any, error)) (*workerClient, error) {
					return connect(sdk.Module{Manifest: entry.Manifest, Start: func(context.Context, sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
						return sdk.ModuleReady{Endpoints: map[string]string{"ui": test.URL}}, nil, nil
					}}, true), nil
				}
				primary := connect(sdk.Module{Manifest: manifest}, false)
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					_ = group.Close(ctx)
					for _, p := range peers {
						p.close()
					}
				})
				err := group.prepare(t.Context(), primary)
				t.Logf("URL=%q SDK configure accepted=%t host group admitted=%t host error=%v", test.URL, sdkAccepted, err == nil, err)
				if sdkAccepted != test.Valid {
					t.Errorf("%s SDK accepted=%t, want %t", language, sdkAccepted, test.Valid)
				}
				if (err == nil) != test.Valid {
					t.Errorf("host admitted=%t, want %t", err == nil, test.Valid)
				}
				if sdkAccepted != (err == nil) {
					t.Error("public SDK readiness success cannot be admitted by ordinary host")
				}
			})
		}
	}
}

func TestComponentReadyEndpointUsesSDKPolicy(t *testing.T) {
	body, err := os.ReadFile("../../pkg/extension/testdata/ready-urls.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string
		URL   string
		Valid bool
	}
	if err := json.Unmarshal(body, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			if got := componentLoopbackEndpoint(test.URL); got != test.Valid {
				t.Fatalf("host readiness=%t, want %t", got, test.Valid)
			}
		})
	}
}
