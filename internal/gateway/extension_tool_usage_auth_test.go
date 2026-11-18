package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func installToolUsageAuthFixture(test *testing.T) *toolRefreshFixture {
	test.Helper()
	fixture := installToolRefreshFixture(test, nil)
	source, err := os.ReadFile("testdata/extensiontoolrefreshworker/main.go")
	if err != nil {
		test.Fatal(err)
	}
	body := string(source)
	for _, insertion := range []struct{ marker, replacement string }{
		{"Handlers: map[string]sdk.Handler{", `Handlers: map[string]sdk.Handler{
			sdk.MethodProviderUsage: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return nil, &sdk.Error{Code: "unauthenticated", Message: "synthetic refusal", Outcome: "not-started"}
			},`},
		{"body, _ := json.Marshal(struct {", `if strings.Contains(string(call.Params), "fixture-auth-callback") {
		var proxy sdk.NetworkProxyResult
		callbackErr := stream.HostCall(ctx, sdk.MethodNetworkProxy,
			sdk.NetworkProxyRequest{URL: auth.URL + "/authority"}, &proxy)
		path := "/callback-allowed"
		if callbackErr != nil {
			path = "/callback-denied"
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.URL + path, nil)
		if err != nil {
			return nil, err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return nil, err
		}
		response.Body.Close()
	}
	body, _ := json.Marshal(struct {`},
	} {
		if strings.Count(body, insertion.marker) != 1 {
			test.Fatal("tool authentication fixture insertion marker is not unique")
		}
		body = strings.Replace(body, insertion.marker, insertion.replacement, 1)
	}
	directory := test.TempDir()
	path := filepath.Join(test.TempDir(), "worker.go")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		test.Fatal(err)
	}
	build := proc.CommandContext(testenv.DeadlineContext(test, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-o", filepath.Join(directory, "worker"), path)
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		test.Fatalf("build tool authentication fixture: %v\n%s", err, output)
	}
	manifest, err := fixture.manager.Manifest(fixture.scope.Plugin)
	if err != nil {
		test.Fatal(err)
	}
	manifest.Version, manifest.Entry = "1.0.1", "worker"
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.UsageReporter)
	bodyJSON, err := json.Marshal(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), bodyJSON, 0600); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.manager.Install(test.Context(), directory); err != nil {
		test.Fatal(err)
	}
	account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
	if err != nil {
		test.Fatal(err)
	}
	account.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	account, err = fixture.manager.Store().SaveAccount(account, account.Generation)
	if err != nil {
		test.Fatal(err)
	}
	fixture.scope.Generation = account.Generation
	provider.Changed()
	return fixture
}

func refuseToolUsageAuth(test *testing.T, fixture *toolRefreshFixture) {
	test.Helper()
	id := provider.ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider)
	quota := provider.LoginUsage(test.Context(), id)[fixture.scope.Account]
	logins := provider.Logins(id)
	if quota.Error == "" || len(logins) != 1 || logins[0].Lapsed == "" {
		test.Fatal("installed-worker refusal did not reach the selected ordinary login")
	}
}

func TestExtensionToolUsageAuthRefusalRechecksPreparedDispatch(test *testing.T) {
	fixture := installToolUsageAuthFixture(test)
	ctx, arguments, err := extensionToolContext(test.Context(), refreshToolSpec, refreshToolArgs)
	if err != nil {
		test.Fatal(err)
	}
	refuseToolUsageAuth(test, fixture)
	if _, err := plugin.RunExtensionTool(ctx, refreshToolSpec, "echo", arguments); err == nil {
		test.Error("usage refusal after preflight retained prepared tool authority")
	}
	if fixture.toolCalls.Load() != 0 || fixture.refreshes.Load() != 0 {
		test.Fatalf("revoked prepared selection dispatched: sends=%d refreshes=%d", fixture.toolCalls.Load(), fixture.refreshes.Load())
	}
}

func TestExtensionToolUsageAuthRefusalBeforeHostCallback(test *testing.T) {
	for _, revoked := range []bool{false, true} {
		name := "unchanged"
		if revoked {
			name = "usage refusal"
		}
		test.Run(name, func(test *testing.T) {
			fixture := installToolUsageAuthFixture(test)
			started, release := make(chan sdk.Scope, 1), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var allowed, denied atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/tool":
					fixture.toolCalls.Add(1)
					var call sdk.Call
					if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
						test.Error(err)
					}
					started <- call.Scope
					select {
					case <-release:
						writer.WriteHeader(http.StatusOK)
					case <-request.Context().Done():
					}
				case "/callback-allowed":
					allowed.Add(1)
					writer.WriteHeader(http.StatusOK)
				case "/callback-denied":
					denied.Add(1)
					writer.WriteHeader(http.StatusOK)
				default:
					fixture.refreshes.Add(1)
					writer.WriteHeader(http.StatusBadRequest)
				}
			}))
			test.Cleanup(server.Close)
			account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
			if err != nil {
				test.Fatal(err)
			}
			account.Auth, err = json.Marshal(map[string]string{"url": server.URL, "token": "synthetic-callback"})
			if err != nil {
				test.Fatal(err)
			}
			if _, err := fixture.manager.Store().SaveAccount(account, account.Generation); err != nil {
				test.Fatal(err)
			}
			provider.Changed()
			ctx, cancel := context.WithTimeout(test.Context(), time.Minute)
			defer cancel()
			done := make(chan error, 1)
			joined := false
			go func() {
				_, err := fixture.run(ctx, json.RawMessage(`{"value":"fixture-auth-callback","_magpie":{"provider":"vendor","account":"selected"}}`))
				done <- err
			}()
			defer func() {
				releaseOnce.Do(func() { close(release) })
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(30 * time.Second):
						test.Error("held tool caller did not join")
					}
				}
			}()
			var scope sdk.Scope
			select {
			case scope = <-started:
			case <-ctx.Done():
				test.Fatal("tool did not reach its held endpoint")
			}
			if revoked {
				refuseToolUsageAuth(test, fixture)
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case err := <-done:
				joined = true
				if revoked {
					var outcome *extension.OutcomeError
					if !errors.As(err, &outcome) || outcome.Outcome != "uncertain" {
						test.Errorf("revoked tool publication lost its uncertainty fence: %v", err)
					}
				} else if err != nil {
					test.Errorf("healthy callback failed: %v", err)
				}
			case <-ctx.Done():
				test.Fatal("held tool did not finish")
			}
			if fixture.toolCalls.Load() != 1 || fixture.refreshes.Load() != 0 {
				test.Errorf("callback control changed dispatch: sends=%d refreshes=%d", fixture.toolCalls.Load(), fixture.refreshes.Load())
			}
			wantState := "completed"
			if revoked {
				wantState = "uncertain"
				if allowed.Load() != 0 || denied.Load() != 1 {
					test.Errorf("revoked host callback retained authority: allowed=%d denied=%d", allowed.Load(), denied.Load())
				}
				if _, err := fixture.run(test.Context(), refreshToolArgs); err == nil {
					test.Error("refused selection admitted a later tool request")
				}
				if fixture.toolCalls.Load() != 1 || fixture.refreshes.Load() != 0 {
					test.Error("refused selection retried or refreshed")
				}
			} else if allowed.Load() != 1 || denied.Load() != 0 {
				test.Errorf("healthy host callback was not exercised: allowed=%d denied=%d", allowed.Load(), denied.Load())
			}
			receipt, err := fixture.manager.Store().Operation(scope.Plugin, scope.Operation)
			if err != nil || receipt.State != wantState || receipt.Scope != scope {
				test.Errorf("callback changed selected journal authority: state=%s err=%v", receipt.State, err)
			}
		})
	}
}
