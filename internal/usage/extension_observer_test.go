package usage

import (
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

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKLedgerObserverOrdinaryAppendAndOutsideHistory(t *testing.T) {
	reportedHome(t)
	type received struct {
		row sdk.LedgerObservation
		raw string
	}
	rows := make(chan received, 8)
	collector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, err := io.ReadAll(request.Body)
		var row sdk.LedgerObservation
		if err != nil || json.Unmarshal(raw, &row) != nil {
			t.Error("invalid delivered ledger evidence", err)
			writer.WriteHeader(400)
			return
		}
		select {
		case rows <- received{row: row, raw: string(raw)}:
		case <-request.Context().Done():
			return
		}
		writer.WriteHeader(204)
	}))
	t.Cleanup(collector.Close)
	directory := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin/go"),
		"build", "-mod=readonly", "-o", filepath.Join(directory, entry), "./testdata/ledgerworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ledger fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: 1, ID: "ledgerfixture", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8}, Capabilities: []sdk.Capability{sdk.LedgerObserver}},
		Ledger:   &sdk.LedgerObservationDescriptor{TimeoutMs: 1000}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := extensionhost.NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	stopServing := ServeObservers()
	t.Cleanup(stopServing)
	firstContext, cancelFirst := context.WithCancel(t.Context())
	t.Cleanup(cancelFirst)
	first := ServeExtensionLedgerObservers(firstContext, manager)
	t.Cleanup(first)
	stop := ServeExtensionLedgerObservers(t.Context(), manager)
	t.Cleanup(stop)
	for _, registered := range Observers() {
		switch registered.(type) {
		case *extensionLedgerObserver, *extensionRequestObserver:
			t.Fatal("an empty catalog registered an SDK observer")
		}
	}
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]string{"sink": collector.URL})
	configured, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config)
	if err != nil {
		t.Fatal(err)
	}
	var observer *extensionLedgerObserver
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, registered := range Observers() {
			if selected, isSDK := registered.(*extensionLedgerObserver); isSDK && selected.manager == manager {
				observer = selected
			}
		}
		current := false
		if observer != nil {
			observer.mu.RLock()
			active := observer.active
			observer.mu.RUnlock()
			if active != nil {
				if targets := active.targets.Load(); targets != nil {
					for _, target := range *targets {
						current = current || target.Module == manifest.ID && target.ConfigVersion == configured.Version
					}
				}
			}
		}
		if current {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("installing a declared ledger module never registered its current configured observer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if observer.Bodies() != NoBodies || observer.Begin(Request{}) != nil {
		t.Fatal("ledger observation requested access to request bodies")
	}
	amount := 0.0
	Append(Record{Agent: "agent", Provider: "vendor", Model: "model", Status: 200, Input: 17,
		CacheWrite: 7, CacheWrite1h: 3, Credits: &amount, OwnCost: &amount,
		ReportedCost:    &ReportedCost{Amount: 0, Currency: "USD"},
		ProviderAccount: "private-account", ProviderKeyName: "private-key-label", CallerKeyID: "private-caller",
		Session: "private-session", RequestID: "private-request", ResponseID: "private-response",
		Error: "private-error", BodyIn: "private-request-body", BodyOut: "private-response-body"})
	take := func() received {
		t.Helper()
		select {
		case row := <-rows:
			return row
		case <-time.After(10 * time.Second):
			t.Fatal("ordinary Append never reached the declared SDK ledger observer")
			return received{}
		}
	}
	measured := take()
	if measured.row.Input != 17 || measured.row.CacheWrite1h != 3 || measured.row.Credits == nil ||
		*measured.row.Credits != 0 || measured.row.CostUSD == nil || *measured.row.CostUSD != 0 ||
		measured.row.ReportedCost == nil || measured.row.ReportedCost.Currency != "USD" ||
		strings.Contains(measured.raw, "private-") {
		t.Fatal("ledger sink lost evidence or exposed a private field", measured)
	}
	cancelFirst()
	first()
	if _, err := IngestReported([]Record{{Time: time.Now(), Agent: "outside", Provider: "vendor", Model: "outside",
		Status: 200, RequestID: "outside-rid", Input: 99}}); err != nil {
		t.Fatal(err)
	}
	Append(Record{Agent: "agent", Provider: "vendor", Model: "sentinel", Status: 200, Input: 1})
	if next := take(); next.row.Model != "sentinel" {
		t.Fatal("outside-account history or an ended listener changed the remaining observer", next)
	}
	client := AgentOf(strings.Repeat("a", 601) + "/1")
	Append(Record{Agent: client, Provider: "vendor", Model: "bounded", Status: 200, Input: 23,
		Requested: strings.Repeat("b", 601), Served: "vendor-model"})
	Append(Record{Agent: "agent", Provider: "vendor", Model: "after-bounded", Status: 200})
	bounded := take().row
	if bounded.Model != "bounded" || bounded.Agent != "other" || bounded.Requested != "" ||
		bounded.Provider != "vendor" || bounded.Served != "vendor-model" || bounded.Status != 200 || bounded.Input != 23 {
		t.Fatalf("ledger observer lost accounting for out-of-bounds metadata: %+v", bounded)
	}
	if next := take().row; next.Model != "after-bounded" || next.Agent != "agent" {
		t.Fatalf("ordinary observer metadata changed: %+v", next)
	}
	var retained bool
	for _, row := range Load(time.Time{}) {
		retained = retained || row.Model == "bounded" && row.Agent == client && len(row.Requested) == 601
	}
	if !retained {
		t.Fatal("SDK projection modified the saved ledger entry")
	}
	stop()
	for _, registered := range Observers() {
		if selected, isSDK := registered.(*extensionLedgerObserver); isSDK && selected.manager == manager {
			t.Fatal("listener shutdown retained its SDK observer")
		}
	}
	if observer.failed.Load() != 0 || observer.dropped.Load() != 0 {
		t.Fatal("ordinary ledger delivery failed", observer.failed.Load(), observer.dropped.Load())
	}
}

func TestSDKLedgerObserverQueueNeverBlocksAndCopiesSparseAmounts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serving := &extensionLedgerServing{ctx: ctx, cancel: cancel, queue: make(chan extensionLedgerDelivery, 1)}
	targets := []extensionhost.LedgerTarget{{Module: "fixture"}}
	serving.targets.Store(&targets)
	observer := &extensionLedgerObserver{active: serving}
	amount := 0.0
	record := Record{Time: time.Now(), Agent: "agent", Provider: "vendor", Model: "model", Status: 200,
		Credits: &amount, OwnCost: &amount, ReportedCost: &ReportedCost{Amount: 1, Currency: "EUR"}}
	observer.Ledger(record)
	amount = 99
	record.ReportedCost.Currency = "USD"
	observer.Ledger(record)
	if observer.dropped.Load() != 1 {
		t.Fatal("a full ledger queue failed to record its drop", observer.dropped.Load())
	}
	got := <-serving.queue
	if got.row.Credits == nil || *got.row.Credits != 0 || got.row.CostUSD == nil || *got.row.CostUSD != 0 ||
		got.row.ReportedCost == nil || got.row.ReportedCost.Currency != "EUR" {
		t.Fatal("queued ledger evidence retained mutable writer pointers", got.row)
	}
}
