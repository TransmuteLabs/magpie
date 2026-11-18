package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func fixtureLedger(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var row sdk.LedgerObservation
	if err := json.Unmarshal(call.Params, &row); err != nil {
		return nil, err
	}
	if row.Kind == "write" {
		var result sdk.StateCASResult
		if stream.HostCall(ctx, sdk.MethodStateCAS, sdk.StateCASRequest{
			Key: "ledger.must-not-write", Version: "0", Value: json.RawMessage(`true`),
		}, &result) == nil {
			return nil, errors.New("ledger observer acquired persistent write authority")
		}
	}
	current, found := sdk.ModuleContextFrom(ctx)
	var config struct {
		Trace string `json:"trace"`
	}
	if !found || json.Unmarshal(current.Config.Values, &config) != nil || config.Trace == "" {
		return nil, errors.New("missing fixture ledger trace")
	}
	output, err := os.OpenFile(config.Trace, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer output.Close()
	return struct{}{}, json.NewEncoder(output).Encode(row)
}

func TestLedgerObserverRetainsGenerationAndCannotWrite(t *testing.T) {
	manager, _ := usageManagerVersion(t, 8, func(manifest *sdk.Manifest) {
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.LedgerObserver)
		manifest.Ledger = &sdk.LedgerObservationDescriptor{}
	})
	trace := filepath.Join(t.TempDir(), "first.jsonl")
	values, _ := json.Marshal(map[string]string{"trace": trace})
	config, err := manager.CompareAndSwapModuleConfig(t.Context(), "fixture", "0", values)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := manager.LedgerTargets()
	if err != nil || len(targets) != 1 {
		t.Fatal("ordinary discovery lost the declared ledger observer", targets, err)
	}
	row := sdk.LedgerObservation{Time: 1, Agent: "agent", Provider: "vendor", Model: "model", Status: 200, Input: 17, Kind: "write"}
	if err := manager.ObserveLedger(t.Context(), targets[0], row); err != nil {
		t.Fatal("ordinary ledger observation failed", err)
	}
	state, err := manager.Store().StateGet(sdk.Scope{Plugin: "fixture"}, "ledger.must-not-write")
	if err != nil || state.Version != 0 {
		t.Fatal("ledger observation wrote private host state", state, err)
	}
	raw, err := os.ReadFile(trace)
	var measured sdk.LedgerObservation
	if err != nil || json.Unmarshal(raw, &measured) != nil || measured.Input != 17 {
		t.Fatal("ordinary ledger evidence did not reach its worker", string(raw), err)
	}
	next := filepath.Join(t.TempDir(), "next.jsonl")
	values, _ = json.Marshal(map[string]string{"trace": next})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), "fixture", config.Version, values); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveLedger(t.Context(), targets[0], row); err == nil {
		t.Fatal("queued old ledger data was delivered to the replacement configuration")
	}
	if _, err := os.Stat(next); !os.IsNotExist(err) {
		t.Fatal("replacement configuration received an old queued row", err)
	}
	current, err := manager.LedgerTargets()
	if err != nil || len(current) != 1 {
		t.Fatal(current, err)
	}
	if err := manager.ObserveLedger(t.Context(), current[0], row); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetEnabled(t.Context(), "fixture", false); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveLedger(t.Context(), current[0], row); err == nil {
		t.Fatal("disabled ledger module retained new invocation authority")
	}
}
