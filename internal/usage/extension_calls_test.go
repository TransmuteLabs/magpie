package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

func writeUsageSubscriptionOwner(t *testing.T, manager *extensionhost.Manager, scope sdk.Scope, active bool) {
	t.Helper()
	saveUsageSubscriptionOwner(t, manager, scope, active)
	provider.Changed()
}

func saveUsageSubscriptionOwner(t *testing.T, manager *extensionhost.Manager, scope sdk.Scope, active bool) {
	t.Helper()
	owner := struct {
		extensionhost.SubscriptionOwner
		GrantRevision string            `json:"grantRevision"`
		Digests       map[string]string `json:"digests"`
		Tickets       []string          `json:"tickets"`
	}{
		SubscriptionOwner: extensionhost.SubscriptionOwner{
			Module: scope.Plugin, Provider: scope.Provider, PublicID: "saved-vendor",
			Revision: fmt.Sprintf("fixture-owner-%t", active), Active: active,
			Accounts: []extensionhost.SubscriptionAccount{
				{Key: "saved-key", ID: scope.Account, User: "saved-alice", LoginID: "saved-login"},
			},
		},
		GrantRevision: strings.Repeat("a", 64),
		Digests:       map[string]string{"saved-key": strings.Repeat("b", 64)},
		Tickets:       []string{"fixture-ticket"},
	}
	raw, err := json.Marshal([]any{owner})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(manager.Namespace(), "private", ".saved-signins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subscription-owners.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSDKOwnCallUsesOwnerAtDispatch(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	lease, err := manager.Acquire(t.Context(), scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	release := provider.Hold()
	defer release()
	heldProvider := provider.ExtensionProviderID(scope.Plugin, scope.Provider)
	saveUsageSubscriptionOwner(t, manager, scope, true)
	if got := provider.ExtensionProviderID(scope.Plugin, scope.Provider); got != heldProvider {
		t.Fatalf("the held subscription view changed before dispatch: %q", got)
	}
	scope.Operation, scope.Attempt = "own-after-transfer", 1
	if _, err := invokeUsageReport(t, manager, scope, "usagefixture.own", "usage.append", ownUsageParams(), true, 1, nil); err != nil {
		t.Fatal(err)
	}
	if rows := Load(time.Time{}); len(rows) != 1 || rows[0].Provider != "saved-vendor" || rows[0].ProviderAccount != "saved-alice" {
		t.Fatalf("new execution used the held owner from before transfer: %+v", rows)
	}
	if entry, err := manager.Store().Operation(scope.Plugin, scope.Operation); err != nil || entry.Scope != scope {
		t.Fatalf("execution changed the private operation scope: %+v %v", entry, err)
	}
}

func TestSDKOwnCallKeepsOwnerAfterAcceptance(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	selectedProvider := provider.ExtensionProviderID(scope.Plugin, scope.Provider)
	scope.Operation, scope.Attempt = "own-before-transfer", 1
	if _, err := invokeUsageReport(t, manager, scope, "usagefixture.own", "usage.append", ownUsageParams(), true, 1,
		func(event extensionhost.Event) error {
			if event.Name == "accepted" {
				saveUsageSubscriptionOwner(t, manager, scope, true)
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if rows := Load(time.Time{}); len(rows) != 1 || rows[0].Provider != selectedProvider || rows[0].ProviderAccount != scope.Account {
		t.Fatalf("accepted execution switched its accounting owner: %+v", rows)
	}
}

func TestSDKUsageKeepsTransferredAccountTogether(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	writeUsageSubscriptionOwner(t, manager, scope, true)
	providers, err := provider.ExtensionProviders()
	if err != nil || len(providers) != 1 || providers[0].ID != "saved-vendor" || providers[0].Account == nil ||
		providers[0].Account.User != "saved-alice" {
		t.Fatalf("SDK routing must resolve the saved subscription's public account: %+v, %v", providers, err)
	}
	routed := providers[0]
	now := time.Now().UTC()
	previous := outsideIO.now
	outsideIO.now = func() time.Time { return now }
	t.Cleanup(func() { outsideIO.now = previous })
	Append(Record{Time: now.Add(-10 * time.Minute), Agent: "codex", Provider: routed.ID, ProviderAccount: routed.Account.User,
		Model: "fixture-model", Status: 200, Input: 11, RequestID: "vendor-through"})

	scope.Operation, scope.Attempt = "transferred-own", 1
	if _, err := invokeUsageReport(t, manager, scope, "usagefixture.own", "usage.append", ownUsageParams(), true, 1, nil); err != nil {
		t.Fatal(err)
	}
	scope.Operation, scope.Attempt = "", 0

	key := "sdk:" + routed.ID + "\x00" + scope.Account
	watermark := now.Add(-time.Hour)
	if err := writeOutsideMarks(map[string]time.Time{key: watermark}); err != nil {
		t.Fatal(err)
	}
	window := watermark.UnixMilli()
	var calls []any
	for _, id := range []string{"vendor-through", "vendor-own", "vendor-history"} {
		call := outsideUsageParams()["calls"].([]any)[0].(map[string]any)
		call["id"], call["time"] = id, now.Add(-10*time.Minute).UnixMilli()
		calls = append(calls, call)
	}
	setOutsideUsageFixture(t, manager, scope, map[string]any{"": map[string]any{"calls": calls}}, &window)
	ReadOutsideCalls(t.Context())

	records := Load(time.Time{})
	var input, imported int
	for _, record := range records {
		if record.Provider != routed.ID || record.ProviderAccount != routed.Account.User {
			t.Errorf("Usage split the transferred account: %q / %q", record.Provider, record.ProviderAccount)
		}
		input += record.Input
		if record.IsReported() {
			imported++
			namespace := sha256.Sum256([]byte(scope.Plugin + "\x00" + scope.Provider + "\x00" + scope.Account))
			if want := fmt.Sprintf("sdk:%x:vendor-history", namespace); record.RequestID != want {
				t.Errorf("unexpected reported call %q, want %q", record.RequestID, want)
			}
		}
	}
	if imported != 1 || len(records) != 3 || input != 31 {
		t.Errorf("overlap affected Usage totals: %d imported of %d rows, %d input tokens", imported, len(records), input)
	}
	if marks := readOutsideMarks(); len(marks) != 1 || !marks[key].Equal(now.Add(-outsideOverlap)) {
		t.Errorf("transferred account lost its persisted watermark: %+v", marks)
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
	for _, change := range []string{"account", "config", "module", "disabled", "reenabled", "canceled", "owner"} {
		t.Run(change, func(t *testing.T) {
			manager, scope := usageExtension(t, 8)
			if change == "owner" {
				writeUsageSubscriptionOwner(t, manager, scope, true)
			}
			var before extensionhost.Installed
			var beforeConfig sdk.ConfigSnapshot
			if change == "reenabled" {
				var err error
				before, beforeConfig, err = manager.ActiveModuleConfig(t.Context(), scope.Plugin)
				if err != nil {
					t.Fatal(err)
				}
			}
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
			case "reenabled":
				if err = manager.SetEnabled(t.Context(), scope.Plugin, false); err != nil {
					t.Fatal(err)
				}
				err = manager.SetEnabled(t.Context(), scope.Plugin, true)
				after, afterConfig, readErr := manager.ActiveModuleConfig(t.Context(), scope.Plugin)
				if readErr != nil || before.Revision != after.Revision || beforeConfig.Version != afterConfig.Version {
					t.Fatalf("off/on changed the package or configuration: %s/%s → %s/%s (%v)",
						before.Revision, beforeConfig.Version, after.Revision, afterConfig.Version, readErr)
				}
			case "canceled":
				cancel()
			case "owner":
				writeUsageSubscriptionOwner(t, manager, scope, false)
			}
			release()
			<-done
			if err != nil {
				t.Fatal(err)
			}
			if len(Load(time.Time{})) != 0 || len(readOutsideMarks()) != 0 {
				t.Fatalf("%s reader published under revoked authority", change)
			}
			if change == "reenabled" {
				ReadOutsideCalls(t.Context())
				if rows, marks := Load(time.Time{}), readOutsideMarks(); len(rows) != 1 || !rows[0].IsReported() || len(marks) != 1 {
					t.Fatalf("current enablement did not publish a fresh history window: rows=%+v marks=%v", rows, marks)
				}
			}
		})
	}
}

func holdOutsideUsageIngest(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var unlock sync.Once
	reported.ingest.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ReadOutsideCalls(ctx)
	}()
	finish := func() {
		unlock.Do(reported.ingest.Unlock)
		<-done
	}
	t.Cleanup(func() {
		cancel()
		finish()
	})
	stacks := make([]byte, 256<<10)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		n := runtime.Stack(stacks, true)
		if strings.Contains(string(stacks[:n]), ".ingestReported(") {
			return finish
		}
		select {
		case <-done:
			t.Fatal("outside reader exited before reaching reported ingestion")
		case <-ctx.Done():
			t.Fatal("outside reader did not reach reported ingestion", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestSDKOutsideHistoryRechecksOwnerAfterWaitingToIngest(t *testing.T) {
	for _, transferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("transferred=%t", transferred), func(t *testing.T) {
			manager, scope := usageExtension(t, 8)
			if transferred {
				writeUsageSubscriptionOwner(t, manager, scope, true)
			}
			setOutsideUsageFixture(t, manager, scope, map[string]any{"": outsideUsageParams()}, nil)
			finish := holdOutsideUsageIngest(t)
			writeUsageSubscriptionOwner(t, manager, scope, !transferred)
			finish()
			if rows, marks := Load(time.Time{}), readOutsideMarks(); len(rows) != 0 || len(marks) != 0 {
				t.Fatalf("ownership changed before ingestion but the stale window published: rows=%+v marks=%v", rows, marks)
			}
			writeUsageSubscriptionOwner(t, manager, scope, true)
			ReadOutsideCalls(t.Context())
			if rows, marks := Load(time.Time{}), readOutsideMarks(); len(rows) != 1 || rows[0].Provider != "saved-vendor" ||
				rows[0].ProviderAccount != "saved-alice" || len(marks) != 1 {
				t.Fatalf("new owner could not import a clean window: rows=%+v marks=%v", rows, marks)
			}
		})
	}
}

func TestSDKOutsideHistorySeesGatewayAppendAfterVisit(t *testing.T) {
	manager, scope := usageExtension(t, 8)
	writeUsageSubscriptionOwner(t, manager, scope, true)
	const publicProvider, publicUser = "saved-vendor", "saved-alice"
	vendorIDs := []string{"same-call", "other-account", "other-provider"}
	var calls []any
	for _, id := range vendorIDs {
		call := outsideUsageParams()["calls"].([]any)[0].(map[string]any)
		call["id"] = id
		calls = append(calls, call)
	}
	setOutsideUsageFixture(t, manager, scope, map[string]any{"": map[string]any{"calls": calls}}, nil)
	Append(Record{Provider: publicProvider, ProviderAccount: publicUser, RequestID: "earlier-local", Input: 10, Status: 200})
	observer := &fakeObserver{}
	remove := Observe(observer)
	defer remove()
	finish := holdOutsideUsageIngest(t)
	Append(Record{Provider: publicProvider, ProviderAccount: publicUser, RequestID: vendorIDs[0], Input: 11, Status: 200})
	Append(Record{Provider: publicProvider, ProviderAccount: "other-user", RequestID: vendorIDs[1], Input: 21, Status: 200})
	Append(Record{Provider: "other-provider", ProviderAccount: publicUser, RequestID: vendorIDs[2], Input: 31, Status: 200})
	finish()
	namespace := sha256.Sum256([]byte(scope.Plugin + "\x00" + scope.Provider + "\x00" + scope.Account))
	var ids []string
	var input int
	for _, row := range Load(time.Time{}) {
		if row.Provider != publicProvider || row.ProviderAccount != publicUser {
			continue
		}
		input += row.Input
		if row.IsReported() {
			ids = append(ids, row.RequestID)
		}
	}
	if len(ids) != 2 || ids[0] != fmt.Sprintf("sdk:%x:%s", namespace, vendorIDs[1]) ||
		ids[1] != fmt.Sprintf("sdk:%x:%s", namespace, vendorIDs[2]) || input != 35 {
		t.Fatalf("late gateway write split the overlap or deduped another provider/account: ids=%v input=%d", ids, input)
	}
	if _, _, count := observer.counts(); count != 3 {
		t.Fatalf("only successful ordinary calls reach live observers, got %d", count)
	}
	if marks := readOutsideMarks(); len(marks) != 1 || marks["sdk:saved-vendor\x00alice"].IsZero() {
		t.Fatalf("a reconciled import did not advance its watermark: %v", marks)
	}
}
