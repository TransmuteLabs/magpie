package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var replacementToolArgs = json.RawMessage(`{"fixture-accepted":true,"fixture-replace":true,"_magpie":{"provider":"vendor","account":"selected"}}`)

func TestExtensionToolReplacementOrdinaryPaths(test *testing.T) {
	for _, route := range []string{"MCP", "Library text", "Library result"} {
		test.Run(route, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			var output any
			var err error
			switch route {
			case "MCP":
				output, err = fixture.run(test.Context(), replacementToolArgs)
			case "Library text":
				output, err = RunPluginToolOf(test.Context(), refreshToolSpec, "echo", replacementToolArgs, "fixture")
			case "Library result":
				output, err = RunPluginToolResultOf(test.Context(), refreshToolSpec, "echo", replacementToolArgs, "fixture")
			}
			if err != nil {
				saved, readErr := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if readErr != nil || saved.Generation != 3 || string(saved.Auth) != `{"token":"synthetic-replaced"}` {
					test.Fatalf("ordinary replacement failed before its own CAS: %+v %v, call error %v", saved, readErr, err)
				}
				test.Fatalf("ordinary tool saved its accepted CAS generation %d but lost its terminal result: %v", saved.Generation, err)
			}
			text, direct := output.(string)
			if !direct {
				raw, err := json.Marshal(output)
				if err != nil {
					test.Fatal(err)
				}
				var result mcpToolResult
				if err := json.Unmarshal(raw, &result); err != nil {
					test.Fatal(err)
				}
				text = resultText(result)
			}
			var observation struct {
				Scope           sdk.Scope                `json:"scope"`
				Auth            json.RawMessage          `json:"auth"`
				Params          sdk.ToolRequest          `json:"params"`
				Replacement     sdk.AccountReplaceResult `json:"replacement"`
				CallbacksDenied bool                     `json:"callbacksDenied"`
			}
			if err := json.Unmarshal([]byte(text), &observation); err != nil {
				test.Fatal(err)
			}
			if observation.Scope.Plugin != fixture.scope.Plugin || observation.Scope.Provider != fixture.scope.Provider ||
				observation.Scope.Account != fixture.scope.Account || observation.Scope.Generation != 2 ||
				observation.Scope.Operation == "" || observation.Scope.Attempt != 1 ||
				!strings.Contains(string(observation.Auth), "synthetic-rotated") || !observation.CallbacksDenied ||
				observation.Replacement.ID != fixture.scope.Account || observation.Replacement.Generation != 3 ||
				strings.Contains(string(observation.Params.Arguments), "_magpie") {
				test.Fatalf("replacement changed original wire/callback authority: %+v", observation)
			}
			account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
			if err != nil || account.Generation != 3 || string(account.Auth) != `{"token":"synthetic-replaced"}` {
				test.Fatalf("own CAS was not durably published: %+v %v", account, err)
			}
			record, err := fixture.manager.Store().Operation(fixture.scope.Plugin, observation.Scope.Operation)
			if err != nil || record.Scope != observation.Scope || record.State != "completed" {
				test.Fatalf("completed journal lost original wire identity: %+v %v", record, err)
			}
			replay := observation.Scope
			replay.Generation = account.Generation
			if _, err := fixture.manager.Call(test.Context(), fixture.scope.Plugin, sdk.MethodToolCall, replay,
				sdk.ToolRequest{ID: "echo", Arguments: json.RawMessage(`{}`)}, nil); err == nil {
				test.Fatal("successor replayed the completed operation")
			}
			if fixture.refreshes.Load() != 1 || fixture.toolCalls.Load() != 1 {
				test.Fatalf("ordinary tool replayed auth/business work: refresh=%d tool=%d", fixture.refreshes.Load(), fixture.toolCalls.Load())
			}
		})
	}
}

func TestExtensionToolReplacementStillHonorsRevocation(test *testing.T) {
	for _, mode := range []string{"manual replacement", "account off", "module off", "configuration", "package", "network", "post-replacement event"} {
		test.Run(mode, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			ctx, cancel := context.WithTimeout(test.Context(), time.Minute)
			defer cancel()
			release := make(chan struct{})
			observed := make(chan sdk.Call, 1)
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var call sdk.Call
				if request.URL.Path != "/tool" || json.NewDecoder(request.Body).Decode(&call) != nil {
					test.Error("replacement used an unexpected fixture request")
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				calls.Add(1)
				select {
				case observed <- call:
				default:
					test.Error("replacement business call was replayed")
				}
				select {
				case <-release:
					writer.WriteHeader(http.StatusOK)
				case <-request.Context().Done():
				}
			}))
			test.Cleanup(server.Close)
			account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
			if err != nil {
				test.Fatal(err)
			}
			account.Auth, _ = json.Marshal(map[string]string{"url": server.URL, "token": "original"})
			account.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
			account, err = fixture.manager.Store().SaveAccount(account, account.Generation)
			if err != nil {
				test.Fatal(err)
			}
			args := replacementToolArgs
			if mode == "post-replacement event" {
				args = json.RawMessage(`{"fixture-accepted":true,"fixture-replace":true,"fixture-after-event":true,"_magpie":{"provider":"vendor","account":"selected"}}`)
			}
			done := make(chan error, 1)
			joined := false
			go func() {
				_, err := fixture.run(ctx, args)
				done <- err
			}()
			defer func() {
				unblock()
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(15 * time.Second):
						test.Error("held replacement invocation did not join")
					}
				}
			}()
			var wire sdk.Scope
			select {
			case call := <-observed:
				wire = call.Scope
			case <-ctx.Done():
				test.Fatal("replacement did not reach its held response")
			}
			current, err := fixture.manager.Store().GetAccount(wire.Plugin, wire.Provider, wire.Account)
			if err != nil || current.Generation != account.Generation+1 || wire.Generation != account.Generation ||
				string(current.Auth) != `{"token":"synthetic-replaced"}` {
				test.Fatalf("revocation did not follow a successful own CAS: %+v %+v %v", wire, current, err)
			}
			record, err := fixture.manager.Store().Operation(wire.Plugin, wire.Operation)
			if err != nil || record.Scope != wire || record.State != "accepted" {
				test.Fatalf("held replacement lost durable acceptance: %+v %v", record, err)
			}
			switch mode {
			case "manual replacement":
				current.Auth = json.RawMessage(`{"token":"manual"}`)
				_, err = fixture.manager.Store().SaveAccount(current, current.Generation)
			case "account off":
				err = provider.SetLoginOn(provider.ExtensionProviderID(wire.Plugin, wire.Provider), wire.Account, false)
			case "module off":
				err = extension.NewCatalog(fixture.manager.Namespace()).SetEnabled(ctx, wire.Plugin, false)
			case "configuration":
				_, err = fixture.manager.CompareAndSwapModuleConfig(ctx, wire.Plugin, "0", json.RawMessage(`{"changed":true}`))
			case "package":
				entries, listErr := fixture.manager.List()
				if listErr != nil || len(entries) != 1 {
					test.Fatalf("replacement package unavailable: %+v %v", entries, listErr)
				}
				entry := entries[0]
				source := test.TempDir()
				binary, readErr := os.ReadFile(filepath.Join(fixture.manager.Namespace(), filepath.FromSlash(entry.Directory), entry.Manifest.Entry))
				if readErr != nil {
					test.Fatal(readErr)
				}
				if err := os.WriteFile(filepath.Join(source, entry.Manifest.Entry), binary, 0o700); err != nil {
					test.Fatal(err)
				}
				entry.Manifest.Version = "2.0.0"
				raw, _ := json.Marshal(entry.Manifest)
				if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
					test.Fatal(err)
				}
				_, err = fixture.manager.Install(ctx, source)
			case "network":
				entries, listErr := provider.ExtensionProviders()
				if listErr != nil || len(entries) != 1 {
					test.Fatalf("replacement provider unavailable: %+v %v", entries, listErr)
				}
				selected := entries[0]
				selected.Proxy = "direct"
				err = provider.Save(selected)
			}
			if err != nil {
				test.Fatal(err)
			}
			unblock()
			select {
			case err := <-done:
				joined = true
				var outcome *extension.OutcomeError
				if !errors.As(err, &outcome) || outcome.Outcome != "uncertain" {
					test.Fatalf("revoked replacement was not fenced as uncertain: %v", err)
				}
			case <-ctx.Done():
				test.Fatal("revoked replacement did not return")
			}
			record, err = fixture.manager.Store().Operation(wire.Plugin, wire.Operation)
			if err != nil || record.Scope != wire || record.State != "uncertain" || calls.Load() != 1 {
				test.Fatalf("revoked replacement lost original uncertain/replay fence: %+v calls=%d %v", record, calls.Load(), err)
			}
			current, err = fixture.manager.Store().GetAccount(wire.Plugin, wire.Provider, wire.Account)
			if err != nil {
				test.Fatal(err)
			}
			replay := wire
			replay.Generation = current.Generation
			if _, err := fixture.manager.Call(ctx, wire.Plugin, sdk.MethodToolCall, replay,
				sdk.ToolRequest{ID: "echo", Arguments: json.RawMessage(`{}`)}, nil); err == nil {
				test.Fatal("successor replayed a revoked accepted operation")
			}
			if calls.Load() != 1 {
				test.Fatal("revoked operation repeated its business request")
			}
		})
	}
}
