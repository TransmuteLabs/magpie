package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func usageExtension(t *testing.T, minor int) (*extensionhost.Manager, sdk.Scope) {
	t.Helper()
	reportedHome(t)
	directory := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	command := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), "go", "build", "-mod=readonly", "-o", filepath.Join(directory, entry), "./testdata/extensionusageworker")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build usage fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "usagefixture", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: minor},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth, sdk.UsageReporter, sdk.ModuleRPC}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture vendor", Protocols: []protocolext.Protocol{protocolext.Chat}}},
		Actions: []sdk.ActionDescriptor{
			{ID: "usagefixture.own", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{ID: "usagefixture.history", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if minor >= 8 {
		var descriptor map[string]any
		if err := json.Unmarshal(raw, &descriptor); err != nil {
			t.Fatal(err)
		}
		descriptor["providers"].([]any)[0].(map[string]any)["calls"] = true
		raw, err = json.Marshal(descriptor)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := extensionhost.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := manager.Install(ctx, directory); err != nil {
		t.Fatal(err)
	}
	account, err := manager.Store().SaveAccount(extensionhost.Account{
		Plugin: manifest.ID, Provider: "vendor", ID: "alice", Auth: json.RawMessage(`{"token":"private-fixture-token"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return manager, sdk.Scope{Plugin: manifest.ID, Provider: "vendor", Account: account.ID,
		Generation: account.Generation, Session: "host-session"}
}

func invokeUsageReport(t *testing.T, manager *extensionhost.Manager, scope sdk.Scope, action, method string,
	params any, accept bool, repeat int, consume func(extensionhost.Event) error,
) ([]json.RawMessage, error) {
	t.Helper()
	input, err := json.Marshal(map[string]any{"method": method, "params": params, "accept": accept, "repeat": repeat})
	if err != nil {
		t.Fatal(err)
	}
	var results []json.RawMessage
	err = manager.Invoke(t.Context(), scope.Plugin, sdk.MethodModuleCall, scope,
		sdk.ModuleCallRequest{Action: action, Input: input}, &results, consume)
	return results, err
}

func ownUsageParams() map[string]any {
	return map[string]any{"call": map[string]any{
		"kind": "cache_keepalive", "model": "fixture-model", "status": 200, "millis": 12,
		"usage": map[string]any{"facts": map[string]any{
			"inputTokens": 11, "outputTokens": 3, "cachedTokens": 5, "cacheWriteTokens": 2,
			"cacheWrite1hTokens": 1, "reasoningTokens": 1, "credits": 0.125,
			"deferred": map[string]any{"inputTokens": 2, "outputTokens": 1, "cachedTokens": 1,
				"cacheWriteTokens": 1, "cacheWrite1hTokens": 1, "reasoningTokens": 1},
			"requestId": "vendor-own", "responseId": "vendor-response-own",
			"servedModel": "actual-model", "upstreamProvider": "upstream-fixture",
			"tier": "priority", "cost": map[string]any{"amount": 0.12, "currency": "USD"},
		}},
	}}
}

func outsideUsageParams() map[string]any {
	return map[string]any{"calls": []any{map[string]any{
		"id": "vendor-history", "time": time.Now().Add(-10 * time.Minute).UnixMilli(), "session": "outside-session",
		"model": "fixture-model", "status": 200,
		"usage": map[string]any{"facts": map[string]any{
			"inputTokens": 7, "outputTokens": 2, "credits": 0.5,
			"cost": map[string]any{"amount": 0.8, "currency": "EUR"},
		}},
	}}}
}

func setOutsideUsageFixture(t *testing.T, manager *extensionhost.Manager, scope sdk.Scope, pages map[string]any, since *int64) {
	t.Helper()
	value, err := manager.Store().StateGet(scope, "calls-fixture")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"pages": pages, "since": since})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Store().StateCAS(scope, "calls-fixture", value.Version, raw); err != nil {
		t.Fatal(err)
	}
}

func TestSDKOwnCallUsesAppendWhileOutsideHistoryUsesIngestReported(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	observer := &fakeObserver{}
	remove := Observe(observer)
	defer remove()
	scope.Operation, scope.Attempt = "own-intent", 1
	results, err := invokeUsageReport(t, manager, scope, "usagefixture.own", "usage.append", ownUsageParams(), true, 2, nil)
	if err != nil || len(results) != 2 {
		t.Fatalf("ordinary SDK own call did not reach Append: results=%s err=%v", results, err)
	}
	if string(results[0]) != `{"accepted":true}` || string(results[1]) != `{"accepted":false}` {
		t.Fatalf("one accepted intent appended twice: %s", results)
	}
	own := Load(time.Time{})
	if len(own) != 1 {
		t.Fatalf("own usage ledger has %d rows, want one", len(own))
	}
	row := own[0]
	if row.Source != "" || row.Agent != "magpie" || row.Provider != provider.ExtensionProviderID(scope.Plugin, scope.Provider) ||
		row.ProviderAccount != scope.Account || row.Session != scope.Session || row.Kind != KindKeepalive ||
		row.Input != 13 || row.Output != 4 || row.CacheRead != 6 || row.CacheWrite != 3 || row.CacheWrite1h != 2 ||
		row.Reasoning != 2 || row.RequestID != "vendor-own" || row.Served != "actual-model" ||
		row.ResponseID != "" || row.UpstreamResponseID != "vendor-response-own" ||
		row.Upstream != "upstream-fixture" || row.TierServed != "priority" || row.Credits == nil || *row.Credits != 0.125 ||
		row.OwnCost == nil || *row.OwnCost != 0.12 || row.Watched || row.CallerKeyID != "" || row.ProviderKeyID != "" {
		t.Fatalf("own-call identity, accounting facts or ordinary semantics changed: %+v", row)
	}
	if _, _, count := observer.counts(); count != 1 {
		t.Fatalf("own call bypassed ordinary ledger observers: %d", count)
	}
	if _, err := invokeUsageReport(t, manager, scope, "usagefixture.own", "usage.append", ownUsageParams(), true, 1, nil); err == nil {
		t.Fatal("completed own intent was replayed")
	}
	scope.Operation, scope.Attempt = "", 0
	history := outsideUsageParams()
	setOutsideUsageFixture(t, manager, scope, map[string]any{"": history}, nil)
	for index := range 3 {
		if index == 2 {
			forgetReported()
		}
		ReadOutsideCalls(t.Context())
		if rows := Load(time.Time{}); len(rows) != 2 {
			t.Fatalf("ordinary SDK history import/dedup pass %d wrote %d rows, want two", index, len(rows))
		}
	}
	rows := Load(time.Time{})
	if len(rows) != 2 {
		t.Fatalf("history import repeated billing: %+v", rows)
	}
	outside := rows[1]
	if outside.Source != SourceReported || outside.Agent != scope.Plugin || outside.Provider != row.Provider ||
		outside.ProviderAccount != scope.Account || outside.Session != "outside-session" ||
		!strings.Contains(outside.RequestID, "vendor-history") || outside.Input != 7 || outside.Output != 2 ||
		outside.Credits == nil || *outside.Credits != 0.5 || outside.OwnCost != nil || outside.ReportedCost == nil ||
		outside.ReportedCost.Amount != 0.8 || outside.ReportedCost.Currency != "EUR" ||
		outside.CallerKeyID != "" || outside.ProviderKeyID != "" {
		t.Fatalf("outside history lost its separate identity or invented USD: %+v", outside)
	}
	if _, _, count := observer.counts(); count != 1 {
		t.Fatalf("imported history reached live-call observers: %d", count)
	}
	account, err := manager.Store().SaveAccount(extensionhost.Account{
		Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"token":"refreshed"}`),
	}, scope.Generation)
	if err != nil {
		t.Fatal(err)
	}
	scope.Generation = account.Generation
	setOutsideUsageFixture(t, manager, scope, map[string]any{"": history}, nil)
	ReadOutsideCalls(t.Context())
	if rows := Load(time.Time{}); len(rows) != 2 {
		t.Fatalf("credential refresh duplicated the same account's history: %+v", rows)
	}
	account, err = manager.Store().SaveAccount(extensionhost.Account{
		Plugin: scope.Plugin, Provider: scope.Provider, ID: "bob", Auth: json.RawMessage(`{"token":"other"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope.Account, scope.Generation = account.ID, account.Generation
	bobHistory := outsideUsageParams()
	bobHistory["calls"].([]any)[0].(map[string]any)["usage"] = map[string]any{"inputTokens": 17, "outputTokens": 4}
	setOutsideUsageFixture(t, manager, scope, map[string]any{"": bobHistory}, nil)
	ReadOutsideCalls(t.Context())
	if rows := Load(time.Time{}); len(rows) != 3 || rows[2].ProviderAccount != "bob" || rows[2].RequestID == rows[1].RequestID ||
		rows[2].Input != 17 || rows[2].Output != 4 {
		t.Fatalf("one account's vendor id hid another account's history: %+v", rows)
	}
}

func TestSDKUsageReportsRejectForgedInvalidAndUnacceptedCalls(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	for _, scenario := range []struct {
		name   string
		action string
		method string
		params any
		accept bool
	}{
		{"unaccepted", "usagefixture.own", "usage.append", ownUsageParams(), false},
		{"read append", "usagefixture.history", "usage.append", ownUsageParams(), false},
		{"account selector", "usagefixture.own", "usage.append", map[string]any{"account": "bob", "call": ownUsageParams()["call"]}, true},
		{"forged source", "usagefixture.own", "usage.append", map[string]any{"call": map[string]any{
			"kind": "cache_keepalive", "model": "fixture-model", "source": "reported",
		}}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			scope.Operation, scope.Attempt = "refuse-"+strings.ReplaceAll(scenario.name, " ", "-"), 1
			if _, err := invokeUsageReport(t, manager, scope, scenario.action, scenario.method, scenario.params, scenario.accept, 1, nil); err == nil {
				t.Fatal("unauthorized usage report reached the ledger")
			}
			if rows := Load(time.Time{}); len(rows) != 0 {
				t.Fatalf("refused report wrote usage: %+v", rows)
			}
		})
	}
}

func TestSDKOutsideHistoryKeepsSelectedAccountAcrossPagesAndKnownCalls(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	now := time.Now()
	previous := outsideIO.now
	outsideIO.now = func() time.Time { return now }
	t.Cleanup(func() { outsideIO.now = previous })
	scope.Operation, scope.Attempt = "paged-own", 1
	if _, err := invokeUsageReport(t, manager, scope, "usagefixture.own", "usage.append", ownUsageParams(), true, 1, nil); err != nil {
		t.Fatal(err)
	}
	scope.Operation, scope.Attempt = "", 0
	first := outsideUsageParams()["calls"].([]any)[0].(map[string]any)
	second := outsideUsageParams()["calls"].([]any)[0].(map[string]any)
	second["id"], second["model"] = "vendor-second", "second-model"
	through := outsideUsageParams()["calls"].([]any)[0].(map[string]any)
	through["id"] = "vendor-own"
	pages := map[string]any{
		"":       map[string]any{"calls": []any{through, first}, "nextCursor": "second"},
		"second": map[string]any{"calls": []any{second}},
	}
	since := now.Add(-outsideBack).UnixMilli()
	setOutsideUsageFixture(t, manager, scope, pages, &since)
	ReadOutsideCalls(t.Context())
	rows := Load(time.Time{})
	if len(rows) != 3 || rows[0].IsReported() || !rows[1].IsReported() || !rows[2].IsReported() ||
		rows[2].Model != "second-model" {
		t.Fatalf("paged history lost records or billed an already recorded vendor id: %+v", rows)
	}
	for _, row := range rows {
		if row.ProviderAccount != "alice" || row.Provider != provider.ExtensionProviderID(scope.Plugin, scope.Provider) {
			t.Fatalf("outside page changed the selected account: %+v", row)
		}
	}
	marks := readOutsideMarks()
	if len(marks) != 1 {
		t.Fatal("a complete paged window did not advance its watermark", marks)
	}
	for _, mark := range marks {
		if !mark.Equal(now.Add(-outsideOverlap)) {
			t.Fatal("paged window advanced to a different read window", mark)
		}
	}
	since = now.Add(-outsideOverlap).UnixMilli()
	setOutsideUsageFixture(t, manager, scope, pages, &since)
	ReadOutsideCalls(t.Context())
	if rows := Load(time.Time{}); len(rows) != 3 {
		t.Fatalf("paged overlap billed the same history again: %+v", rows)
	}
}

func TestSDKOutsideHistoryRetriesFailedWatermarkWriteWithoutObservingOrDuplicating(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	observer := &fakeObserver{}
	remove := Observe(observer)
	defer remove()
	setOutsideUsageFixture(t, manager, scope, map[string]any{"": outsideUsageParams()}, nil)
	if err := os.MkdirAll(outsidePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	ReadOutsideCalls(t.Context())
	if len(Load(time.Time{})) != 1 || len(readOutsideMarks()) != 0 {
		t.Fatal("watermark write failure lost the ingestion or invented a durable cursor")
	}
	if err := os.Remove(outsidePath()); err != nil {
		t.Fatal(err)
	}
	forgetReported()
	ReadOutsideCalls(t.Context())
	if len(Load(time.Time{})) != 1 || len(readOutsideMarks()) != 1 {
		t.Fatal("watermark repair did not deduplicate the repeated history window")
	}
	if _, _, count := observer.counts(); count != 0 {
		t.Fatal("retried history reached live observers", count)
	}
}

func TestSDKOutsideHistoryCannotStreamAnUnfinishedPage(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	raw, err := json.Marshal(map[string]any{
		"pages": map[string]any{"": outsideUsageParams()}, "event": "usage",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Store().StateCAS(scope, "calls-fixture", 0, raw); err != nil {
		t.Fatal(err)
	}
	ReadOutsideCalls(t.Context())
	if len(Load(time.Time{})) != 0 || len(readOutsideMarks()) != 0 {
		t.Fatal("streamed history page acquired a terminal publication receipt")
	}
}

func TestSDKOutsideHistoryPreservesWatermarkOnPageCursorAndIngestFailures(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	now := time.Now()
	previous := outsideIO.now
	outsideIO.now = func() time.Time { return now }
	t.Cleanup(func() { outsideIO.now = previous })
	since := now.Add(-outsideBack).UnixMilli()
	history := outsideUsageParams()
	for _, scenario := range []struct {
		name  string
		pages map[string]any
	}{
		{"page error", map[string]any{"": map[string]any{"calls": history["calls"], "nextCursor": "page2"}, "page2": map[string]any{"error": "read failed"}}},
		{"cursor loop", map[string]any{"": map[string]any{"calls": history["calls"], "nextCursor": "page2"}, "page2": map[string]any{"calls": []any{}, "nextCursor": "page2"}}},
		{"invalid later page", map[string]any{"": map[string]any{"calls": history["calls"], "nextCursor": "page2"}, "page2": map[string]any{"calls": []any{
			map[string]any{"id": "bad", "time": 0, "model": "fixture-model"},
		}}}},
		{"wrong account", map[string]any{"": map[string]any{"calls": history["calls"], "account": "bob"}}},
		{"unknown history", map[string]any{"": map[string]any{"calls": nil}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			setOutsideUsageFixture(t, manager, scope, scenario.pages, &since)
			ReadOutsideCalls(t.Context())
			if len(Load(time.Time{})) != 0 || len(readOutsideMarks()) != 0 {
				t.Fatalf("failed history window published records or advanced its watermark: %+v", readOutsideMarks())
			}
		})
	}
	setOutsideUsageFixture(t, manager, scope, map[string]any{"": history}, &since)
	if err := os.MkdirAll(Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	ReadOutsideCalls(t.Context())
	if len(readOutsideMarks()) != 0 {
		t.Fatal("failed history ingestion advanced its watermark")
	}
	if err := os.Remove(Path()); err != nil {
		t.Fatal(err)
	}
	ReadOutsideCalls(t.Context())
	if len(Load(time.Time{})) != 1 || len(readOutsideMarks()) != 1 {
		t.Fatal("a repaired history reader did not retry from the retained watermark")
	}
}

func TestSDKUsageAppendRejectsChangedAccountAfterAcceptance(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	scope.Operation, scope.Attempt = "revoked-own", 1
	_, err := invokeUsageReport(t, manager, scope, "usagefixture.own", "usage.append", ownUsageParams(), true, 1,
		func(event extensionhost.Event) error {
			if event.Name == "accepted" {
				_, err := manager.Store().SaveAccount(extensionhost.Account{
					Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"token":"replaced"}`),
				}, scope.Generation)
				return err
			}
			return nil
		})
	if err == nil || len(Load(time.Time{})) != 0 {
		t.Fatalf("revoked accepted write appended usage: %v", err)
	}
}

func TestSDKUsageAppendRequiresNegotiatedVersion(t *testing.T) {
	manager, scope := usageExtension(t, 7)
	scope.Operation, scope.Attempt = "legacy-own", 1
	if _, err := invokeUsageReport(t, manager, scope, "usagefixture.own", "usage.append", ownUsageParams(), true, 1, nil); err == nil {
		t.Fatal("SDK 1.7 acquired the new accounting callback")
	}
	if rows := Load(time.Time{}); len(rows) != 0 {
		t.Fatalf("unnegotiated callback appended usage: %+v", rows)
	}
}

func TestSDKOwnCallCannotRebillAnOrdinaryProviderExecution(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	body, err := json.Marshal(map[string]any{"method": "usage.append", "params": ownUsageParams(), "accept": true})
	if err != nil {
		t.Fatal(err)
	}
	scope.Operation, scope.Attempt = "ordinary-execution", 1
	if _, err := manager.Call(t.Context(), scope.Plugin, sdk.MethodProviderExecute, scope,
		sdk.ExecuteRequest{Protocol: "chat", Model: "fixture-model", Body: body}, nil); err != nil {
		t.Fatal(err)
	}
	if rows := Load(time.Time{}); len(rows) != 0 {
		t.Fatalf("ordinary inference submitted duplicate own-call billing: %+v", rows)
	}
}

func usageWorkerBarrier(t *testing.T) (string, <-chan struct{}, func()) {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	var entered, finish sync.Once
	unblock := func() { finish.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/usage-barrier" {
			http.NotFound(writer, request)
			return
		}
		entered.Do(func() { close(started) })
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(func() {
		unblock()
		server.Close()
	})
	return server.URL + "/usage-barrier", started, unblock
}

func replaceUsageModule(t *testing.T, manager *extensionhost.Manager, scope sdk.Scope) error {
	t.Helper()
	entries, err := manager.List()
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Manifest.ID != scope.Plugin {
		t.Fatal("unexpected fixture catalog", entries)
	}
	entry := entries[0]
	replacement := t.TempDir()
	worker, err := os.ReadFile(filepath.Join(filepath.Dir(Path()), "extensions", entry.Directory, entry.Manifest.Entry))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(replacement, entry.Manifest.Entry), worker, 0o700); err != nil {
		return err
	}
	manifest := entry.Manifest.Clone()
	manifest.Version = "1.0.1"
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(replacement, sdk.ManifestFile), raw, 0o600); err != nil {
		return err
	}
	_, err = manager.Install(t.Context(), replacement)
	return err
}

func TestSDKOwnCallRejectsCanceledAndReplacedAcceptedWriters(t *testing.T) {
	for _, change := range []string{"account", "config", "module", "canceled"} {
		t.Run(change, func(t *testing.T) {
			manager, scope := usageExtension(t, 8)
			scope.Operation, scope.Attempt = "held-own", 1
			endpoint, started, release := usageWorkerBarrier(t)
			input, err := json.Marshal(map[string]any{
				"method": "usage.append", "params": ownUsageParams(), "accept": true, "wait": endpoint,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				defer close(done)
				done <- manager.Invoke(ctx, scope.Plugin, sdk.MethodModuleCall, scope,
					sdk.ModuleCallRequest{Action: "usagefixture.own", Input: input}, nil, nil)
			}()
			t.Cleanup(func() {
				cancel()
				release()
				<-done
			})
			select {
			case <-started:
			case err := <-done:
				t.Fatal("accepted writer ended before reaching its held callback", err)
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			switch change {
			case "account":
				_, err = manager.Store().SaveAccount(extensionhost.Account{
					Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"token":"replaced"}`),
				}, scope.Generation)
			case "config":
				_, err = manager.CompareAndSwapModuleConfig(t.Context(), scope.Plugin, "0", json.RawMessage(`{"changed":true}`))
			case "module":
				err = replaceUsageModule(t, manager, scope)
			case "canceled":
				cancel()
			}
			release()
			reportErr := <-done
			if err != nil {
				t.Fatal(err)
			}
			if reportErr == nil || len(Load(time.Time{})) != 0 {
				t.Fatalf("%s accepted writer published under revoked authority: %v", change, reportErr)
			}
		})
	}
}

func TestSDKOutsideHistoryRejectsCanceledAndReplacedReaders(t *testing.T) {
	for _, change := range []string{"account", "config", "module", "disabled", "canceled"} {
		t.Run(change, func(t *testing.T) {
			manager, scope := usageExtension(t, 8)
			endpoint, started, release := usageWorkerBarrier(t)
			raw, err := json.Marshal(map[string]any{
				"pages": map[string]any{"": outsideUsageParams()}, "wait": endpoint,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Store().StateCAS(scope, "calls-fixture", 0, raw); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				ReadOutsideCalls(ctx)
			}()
			t.Cleanup(func() {
				cancel()
				release()
				<-done
			})
			select {
			case <-started:
			case <-done:
				t.Fatal("ordinary outside reader ended before reaching its held page")
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			switch change {
			case "account":
				_, err = manager.Store().SaveAccount(extensionhost.Account{
					Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"token":"replaced"}`),
				}, scope.Generation)
			case "config":
				_, err = manager.CompareAndSwapModuleConfig(t.Context(), scope.Plugin, "0", json.RawMessage(`{"changed":true}`))
			case "module":
				err = replaceUsageModule(t, manager, scope)
			case "disabled":
				err = manager.SetEnabled(t.Context(), scope.Plugin, false)
			case "canceled":
				cancel()
			}
			release()
			<-done
			if err != nil {
				t.Fatal(err)
			}
			if len(Load(time.Time{})) != 0 || len(readOutsideMarks()) != 0 {
				t.Fatalf("%s reader published under revoked authority", change)
			}
		})
	}
}
