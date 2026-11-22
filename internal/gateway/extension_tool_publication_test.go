package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionToolPublicationKeepsSelectedAuthority(test *testing.T) {
	for _, route := range []string{"MCP", "Library text", "Library result"} {
		test.Run(route, func(test *testing.T) {
			for _, mode := range []string{"unchanged", "account off", "manual replacement", "configuration", "module off", "module reenabled", "accepted unchanged", "accepted account off", "uncertain unchanged", "uncertain account off"} {
				test.Run(mode, func(test *testing.T) {
					fixture := installToolRefreshFixture(test, nil)
					started, release := make(chan sdk.Scope, 1), make(chan struct{})
					var releaseOnce sync.Once
					defer releaseOnce.Do(func() { close(release) })
					server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						if request.Method != http.MethodPost || (request.URL.Path != "/" && request.URL.Path != "/tool") {
							http.NotFound(writer, request)
							return
						}
						if request.URL.Path == "/" {
							test.Error("fresh tool unexpectedly attempted credential refresh")
							writer.WriteHeader(http.StatusBadRequest)
							return
						}
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
					}))
					test.Cleanup(server.Close)
					if mode == "module off" {
						client := &http.Client{Timeout: 5 * time.Second}
						for _, endpoint := range []string{fixture.server.URL, server.URL} {
							probe, err := client.Get(endpoint + "/")
							if err != nil {
								test.Fatal(err)
							}
							probe.Body.Close()
							if probe.StatusCode != http.StatusNotFound {
								test.Errorf("listener probe status = %d, want 404", probe.StatusCode)
							}
						}
					}
					account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
					if err != nil {
						test.Fatal(err)
					}
					account.Auth, err = json.Marshal(map[string]string{"url": server.URL, "token": "publication-original"})
					if err != nil {
						test.Fatal(err)
					}
					account.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
					if _, err := fixture.manager.Store().SaveAccount(account, account.Generation); err != nil {
						test.Fatal(err)
					}
					arguments := refreshToolArgs
					if strings.HasPrefix(mode, "accepted") {
						arguments = json.RawMessage(`{"value":"fixture-accepted","_magpie":{"provider":"vendor","account":"selected"}}`)
					}
					if strings.HasPrefix(mode, "uncertain") {
						arguments = json.RawMessage(`{"value":"fixture-uncertain","_magpie":{"provider":"vendor","account":"selected"}}`)
					}
					ctx, cancel := context.WithTimeout(test.Context(), time.Minute)
					defer cancel()
					type outcome struct {
						output any
						err    error
					}
					done := make(chan outcome, 1)
					joined := false
					go func() {
						var result outcome
						switch route {
						case "MCP":
							result.output, result.err = fixture.run(ctx, arguments)
						case "Library text":
							result.output, result.err = RunPluginToolOf(ctx, refreshToolSpec, "echo", arguments, "fixture")
						case "Library result":
							result.output, result.err = RunPluginToolResultOf(ctx, refreshToolSpec, "echo", arguments, "fixture")
						}
						done <- result
					}()
					defer func() {
						releaseOnce.Do(func() { close(release) })
						cancel()
						if !joined {
							select {
							case <-done:
							case <-time.After(30 * time.Second):
								test.Error("held caller did not join during cleanup")
							}
						}
					}()
					var dispatched sdk.Scope
					select {
					case dispatched = <-started:
					case <-ctx.Done():
						test.Fatal("tool did not reach held endpoint")
					}
					receipt, err := fixture.manager.Store().Operation(dispatched.Plugin, dispatched.Operation)
					before := "dispatched"
					if strings.HasPrefix(mode, "accepted") {
						before = "accepted"
					}
					for err == nil && before == "accepted" && receipt.State == "dispatched" {
						select {
						case <-ctx.Done():
							test.Fatal("accepted event did not reach its durable receipt")
						case <-time.After(time.Millisecond):
						}
						receipt, err = fixture.manager.Store().Operation(dispatched.Plugin, dispatched.Operation)
					}
					if err != nil || receipt.State != before {
						test.Fatalf("unexpected in-flight receipt: %+v %v", receipt, err)
					}
					switch mode {
					case "account off", "accepted account off", "uncertain account off":
						err = provider.SetLoginOn(provider.ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider), fixture.scope.Account, false)
					case "manual replacement":
						account, err = fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
						if err == nil {
							account.Auth = json.RawMessage(`{"token":"publication-replacement"}`)
							_, err = fixture.manager.Store().SaveAccount(account, account.Generation)
						}
					case "configuration":
						_, err = fixture.manager.CompareAndSwapModuleConfig(ctx, fixture.scope.Plugin, "0", json.RawMessage(`{"publication":"changed"}`))
					case "module off":
						err = extension.NewCatalog(fixture.manager.Namespace()).SetEnabled(ctx, fixture.scope.Plugin, false)
					case "module reenabled":
						catalog := extension.NewCatalog(fixture.manager.Namespace())
						err = catalog.SetEnabled(ctx, fixture.scope.Plugin, false)
						if err == nil {
							err = catalog.SetEnabled(ctx, fixture.scope.Plugin, true)
						}
					}
					releaseOnce.Do(func() { close(release) })
					if err != nil {
						test.Fatal(err)
					}
					select {
					case result := <-done:
						joined = true
						successful := mode == "unchanged" || mode == "accepted unchanged"
						if successful {
							if result.err != nil {
								test.Errorf("unchanged authority failed: %v", result.err)
							}
						} else {
							var terminal *extension.OutcomeError
							if !errors.As(result.err, &terminal) || terminal.Outcome != "uncertain" {
								test.Errorf("revoked/uncertain reply became success or replayable: %v, %v", result.output, result.err)
							}
							if strings.HasPrefix(mode, "uncertain") {
								var refusal *sdk.Error
								if !errors.As(result.err, &refusal) || refusal.Code != "fixture" {
									test.Errorf("existing uncertainty was replaced: %v", result.err)
								}
							}
						}
						receipt, err = fixture.manager.Store().Operation(dispatched.Plugin, dispatched.Operation)
						after := "uncertain"
						if successful {
							after = "completed"
						}
						if err != nil || receipt.State != after || receipt.Scope != dispatched {
							test.Errorf("publication changed original receipt authority: %+v %v; want %s %+v", receipt, err, after, dispatched)
						}
					case <-ctx.Done():
						test.Fatal("tool did not join after releasing reply")
					}
				})
			}
		})
	}
}
