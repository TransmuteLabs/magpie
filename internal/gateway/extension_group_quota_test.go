//go:build cgo && (darwin || linux || freebsd || windows)

package gateway

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var nativeGroupQuotaChild = flag.Bool("native-group-quota-child", false, "run the SDK quota fixture in this test process")

func TestExtensionNativeGroupCapUsesProviderModelIDs(t *testing.T) {
	if !*nativeGroupQuotaChild {
		ctx, cancel := context.WithTimeout(t.Context(), 155*time.Second)
		defer cancel()
		child := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestExtensionNativeGroupCapUsesProviderModelIDs$",
			"-test.v", "-test.timeout=150s", "-native-group-quota-child")
		output, err := child.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated native quota fixture: %v\n%s", err, output)
		}
		t.Log(string(output))
		return
	}

	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_ADDR", "")
	previousAllowances := allowances
	allowances = provider.Allowances
	t.Cleanup(func() { allowances = previousAllowances })
	directory := buildNativeGroupQuotaFixture(t)
	fixture := t.TempDir()
	manager := extensionhost.Default()
	t.Cleanup(func() { closeSDKGatewayHook(t, manager) })
	installed, err := manager.Install(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	module, vendor := installed.Manifest.ID, installed.Manifest.Providers[0].ID
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), module, "0", mustJSON(map[string]string{"root": fixture})); err != nil {
		t.Fatal(err)
	}
	store := manager.Store()
	account, err := store.SaveAccount(extensionhost.Account{Plugin: module, Provider: vendor, ID: "alice",
		Auth: json.RawMessage(`{"access":"quota-fixture"}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: module, Provider: vendor, Account: account.ID, Generation: account.Generation}
	id := provider.ExtensionProviderID(module, vendor)
	provider.Changed()

	const paid, free = "paid-native", "free-native"
	const paidAlias, freeAlias = "paid-alias", "free-alias"
	for _, model := range []struct{ native, alias string }{{paid, paidAlias}, {free, freeAlias}} {
		if _, changed, err := provider.SetModelID(id+"/"+model.native, model.alias); err != nil || !changed {
			t.Fatalf("set model ID %s: changed=%t, err=%v", model.alias, changed, err)
		}
	}
	if err := provider.SetAccountCap(id, account.ID, 85); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetWindowCap(id, account.ID, "paid", 90); err != nil {
		t.Fatal(err)
	}
	const group = "fixture-sdk-quota"
	const requested = "group/" + group
	if err := provider.SaveGroup(provider.Group{ID: group, Routing: provider.Ordered,
		Members: []string{id + "/" + paidAlias, id + "/" + freeAlias}}); err != nil {
		t.Fatal(err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()

	reset := time.Now().Add(time.Hour).UnixMilli()
	paidWindow := sdk.UsageWindow{Name: "paid", Used: 90, Models: []string{paid}, Aside: true, ResetsAt: reset}
	freeWindow := sdk.UsageWindow{Name: "free", Used: 12, Models: []string{free}, ResetsAt: reset}
	belowWindow, exclusionWindow, fullFreeWindow := paidWindow, paidWindow, freeWindow
	belowWindow.Used = 89
	exclusionWindow.Models, exclusionWindow.NotModels = nil, []string{free}
	fullFreeWindow.Used = 85
	trace := filepath.Join(fixture, "dispatch.jsonl")
	for _, scenario := range []struct {
		name, model, alias string
		windows            []sdk.UsageWindow
	}{
		{name: "below-cap", model: paid, alias: paidAlias, windows: []sdk.UsageWindow{belowWindow, freeWindow}},
		{name: "models-cap", model: free, alias: freeAlias, windows: []sdk.UsageWindow{paidWindow, freeWindow}},
		{name: "not-models-cap", model: free, alias: freeAlias, windows: []sdk.UsageWindow{exclusionWindow, freeWindow}},
		{name: "own-quota-full", windows: []sdk.UsageWindow{paidWindow, fullFreeWindow}},
		{name: "shared-cap", windows: []sdk.UsageWindow{{Name: "shared", Used: 85, ResetsAt: reset}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			if err := os.WriteFile(filepath.Join(fixture, "windows.json"), mustJSON(scenario.windows), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(trace, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			dirty, err := store.InvalidateUsage(scope)
			if err != nil || !dirty.Dirty {
				t.Fatalf("fixture usage revision was not invalidated: %+v (%v)", dirty, err)
			}
			if !provider.ReadUsageNow(ctx, id, account.ID) {
				t.Fatal("no usage refresh completed")
			}
			stamp, err := store.UsageStampContext(ctx, scope)
			if err != nil || stamp.Dirty || stamp.Revision != dirty.Revision {
				t.Fatalf("usage refresh did not publish the selected revision: %+v (%v)", stamp, err)
			}

			session := "sdk-quota-" + scenario.name
			request := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1/v1/chat/completions",
				strings.NewReader(`{"model":"`+requested+`","messages":[{"role":"user","content":"hello"}]}`))
			request.RemoteAddr = "127.0.0.1:12345"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(SessionHeader, session)
			reply := httptest.NewRecorder()
			handler.ServeHTTP(reply, request)

			recorded, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			rejected := scenario.model == ""
			wantStatus := http.StatusOK
			if rejected {
				wantStatus = http.StatusTooManyRequests
				if len(recorded) != 0 || !strings.Contains(reply.Body.String(), "usage cap reached") {
					t.Fatalf("capped group: HTTP %d, executions=%s, body=%s", reply.Code, recorded, reply.Body)
				}
			} else {
				var dispatch struct {
					Scope                      sdk.Scope
					Model, Protocol, Operation string
					PID                        int
				}
				err := json.Unmarshal(recorded, &dispatch)
				got := dispatch.Scope
				if err != nil || dispatch.Model != scenario.model || dispatch.Protocol != "chat" || dispatch.PID != os.Getpid() ||
					got.Plugin != scope.Plugin || got.Provider != scope.Provider || got.Account != scope.Account ||
					got.Generation != scope.Generation || got.Operation == "" || dispatch.Operation != got.Operation ||
					!strings.Contains(reply.Body.String(), "answered "+scenario.model) {
					t.Fatalf("want exactly one native %s execution: HTTP %d, executions=%s (%v), body=%s",
						scenario.model, reply.Code, recorded, err, reply.Body)
				}
			}
			if reply.Code != wantStatus {
				t.Fatalf("HTTP %d, want %d: %s", reply.Code, wantStatus, reply.Body)
			}
			var rows []usage.Record
			usage.Visit(time.Time{}, func(row usage.Record) {
				if row.Session == session {
					rows = append(rows, row)
				}
			})
			if len(rows) != 1 || rows[0].Status != wantStatus || rows[0].Requested != requested || rows[0].Rejected != rejected {
				t.Fatalf("want one Usage row for the group request: %+v", rows)
			}
			row := rows[0]
			if rejected {
				if row.ProviderAccount != "" || row.Input != 0 || row.Output != 0 || row.CacheRead != 0 ||
					row.CacheWrite != 0 || row.Error == "" {
					t.Fatalf("cap refusal billed a provider or lost its error: %+v", row)
				}
			} else if row.Provider != id || row.ProviderAccount != account.ID || row.Model != scenario.alias ||
				row.Wire != scenario.model || row.Input != 13 || row.Output != 3 || row.Error != "" {
				t.Fatalf("native execution Usage: %+v", row)
			}
		})
	}
}

func buildNativeGroupQuotaFixture(t *testing.T) string {
	t.Helper()
	const source = "./testdata/extensionquotaworker"
	raw, err := os.ReadFile(filepath.Join(source, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	command := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-p=2", "-mod=readonly", "-trimpath", "-buildmode=c-shared", "-o", filepath.Join(directory, manifest.Entry), source)
	command.Env = append(os.Environ(), "CGO_ENABLED=1", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build native quota fixture: %v\n%s", err, output)
	}
	if err := os.Remove(filepath.Join(directory, "worker.h")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}
