package extension

import (
	"context"
	"math"
	"slices"
	"sync/atomic"
	"testing"
)

func ledgerManifest() Manifest {
	manifest := testManifest(LedgerObserver)
	manifest.Contract.Version.Minor = 8
	manifest.Ledger = &LedgerObservationDescriptor{}
	return manifest
}

func ledgerSample() LedgerObservation {
	return LedgerObservation{Time: 1, Agent: "agent", Provider: "vendor", Model: "model",
		Input: 17, Output: 3, CacheRead: 4, CacheWrite: 7, CacheWrite1h: 3, Status: 200}
}

func TestLedgerObserverIsExplicitPrimaryAndVersioned(t *testing.T) {
	manifest := ledgerManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(RequiredMethods(manifest), []string{MethodLedgerObserve}) {
		t.Fatal("ledger observation acquired unrelated methods")
	}
	if clone := manifest.Clone(); clone.Ledger == manifest.Ledger {
		t.Fatal("ledger permission shares a mutable descriptor")
	}
	for _, mutate := range []func(*Manifest){
		func(value *Manifest) { value.Contract.Version.Minor = 7 },
		func(value *Manifest) { value.Ledger = nil },
		func(value *Manifest) { value.Contract.Capabilities = []Capability{OperationControl} },
		func(value *Manifest) { value.Ledger.TimeoutMs = 9 },
		func(value *Manifest) { value.Ledger.TimeoutMs = 1001 },
		func(value *Manifest) { value.Component = &ComponentIdentity{ID: "backend"} },
	} {
		value := manifest.Clone()
		mutate(&value)
		if value.Validate() == nil {
			t.Fatal("undeclared ledger authority was admitted", value)
		}
	}
}

func TestLedgerObserverValidatesNumericAndPrivacyEnvelope(t *testing.T) {
	for _, mutate := range []func(*LedgerObservation){
		func(value *LedgerObservation) { value.Time = 0 },
		func(value *LedgerObservation) { value.Time = 1 << 53 },
		func(value *LedgerObservation) { value.Input = -1 },
		func(value *LedgerObservation) { value.Input = 1 << 53 },
		func(value *LedgerObservation) { value.CacheWrite1h = 8 },
		func(value *LedgerObservation) { value.Status = 600 },
		func(value *LedgerObservation) { value.Provider = "vendor\nsecret" },
		func(value *LedgerObservation) { value.CostUSD = executionFact(math.NaN()) },
		func(value *LedgerObservation) { value.ReportedCost = &ExecutionCost{Amount: -1} },
		func(value *LedgerObservation) { value.ReportedCost = &ExecutionCost{Amount: math.Inf(1)} },
		func(value *LedgerObservation) { value.ReportedCost = &ExecutionCost{Amount: 1, Currency: "EUR\n"} },
	} {
		row := ledgerSample()
		mutate(&row)
		if ValidateLedgerObservation(row) == nil {
			t.Fatal("invalid ledger evidence was admitted", row)
		}
	}
}

func TestLedgerObserverRetainsUnspecifiedCurrency(t *testing.T) {
	row := ledgerSample()
	row.ReportedCost = &ExecutionCost{Amount: 0.12}
	if err := ValidateLedgerObservation(row); err != nil {
		t.Fatal(err)
	}
	if row.ReportedCost.Amount != 0.12 || row.ReportedCost.Currency != "" || row.CostUSD != nil {
		t.Fatal("unspecified currency was converted or lost", row)
	}
}

func TestLedgerObserverRPCRefusesCredentialsWritesAndStreams(t *testing.T) {
	for _, mode := range []string{"ordinary", "auth", "account", "session", "operation", "extra fields", "accepted", "data", "result mutation"} {
		t.Run(mode, func(t *testing.T) {
			var called atomic.Int32
			peer := startPeer(t, Module{Manifest: ledgerManifest(), Handlers: map[string]Handler{
				MethodLedgerObserve: func(ctx context.Context, call Call, stream *Stream) (any, error) {
					called.Add(1)
					if mode == "accepted" || mode == "data" {
						_ = stream.Emit(ctx, mode, AcceptedEvent{})
					}
					if mode == "result mutation" {
						return map[string]int{"input": 99}, nil
					}
					return struct{}{}, nil
				},
			}})
			configurePeer(t, peer, ledgerManifest(), "0")
			frame := callFrame("ledger", MethodLedgerObserve, ledgerSample())
			switch mode {
			case "auth":
				frame.Auth = []byte(`{"token":"private"}`)
			case "account":
				frame.Scope.Account, frame.Scope.Generation = "private", 1
			case "session":
				frame.Scope.Session = "private"
			case "operation":
				frame.Scope.Operation = "write"
			case "extra fields":
				frame.Params = []byte(`{"time":1,"privateBody":"private"}`)
			}
			peer.send(frame)
			result := peer.read()
			if result.Kind != KindResult || (result.Error == nil) != (mode == "ordinary") {
				t.Fatal("ledger RPC acquired unexpected behavior", mode, result)
			}
			if (mode == "auth" || mode == "account" || mode == "session" || mode == "operation" || mode == "extra fields") && called.Load() != 0 {
				t.Fatal("ledger handler saw private authority before validation")
			}
		})
	}
}
