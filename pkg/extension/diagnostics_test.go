package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestStructuredDiagnosticsUseOnlyBoundedFactsAndNegotiatedVersion(t *testing.T) {
	manifest := Manifest{Contract: FullContract()}
	valid := Diagnostic{Level: "warn", Code: "vendor_timeout", Phase: "provider.execute", Status: 504, Millis: 1200, Count: 2}
	if err := ValidateDiagnostic(manifest, valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Diagnostic){
		func(value *Diagnostic) { value.Level = "fatal" },
		func(value *Diagnostic) { value.Code = "forged\nentry" },
		func(value *Diagnostic) { value.Code = strings.Repeat("x", 65) },
		func(value *Diagnostic) { value.Phase = "https://vendor.invalid?token=secret" },
		func(value *Diagnostic) { value.Status = 600 },
		func(value *Diagnostic) { value.Millis = -1 },
		func(value *Diagnostic) { value.Millis = 24*60*60*1000 + 1 },
		func(value *Diagnostic) { value.Count = -1 },
	} {
		invalid := valid
		mutate(&invalid)
		if err := ValidateDiagnostic(manifest, invalid); err == nil {
			t.Fatalf("invalid diagnostic was admitted: %+v", invalid)
		}
	}
	manifest.Contract.Version.Minor = 6
	if err := ValidateDiagnostic(manifest, valid); err == nil {
		t.Fatal("old module acquired structured diagnostics without negotiation")
	}
}

func TestStructuredDiagnosticHostCallInheritsScopeWithoutAuth(t *testing.T) {
	module := operationModule(func(ctx context.Context, call Call, stream *Stream) (any, error) {
		var result DiagnosticResult
		err := stream.HostCall(ctx, MethodDiagnosticEmit, Diagnostic{Level: "warn", Code: "vendor_timeout", Status: 504}, &result)
		return result, err
	})
	module.Manifest.Contract.Version.Minor = 7
	peer := startPeer(t, module)
	configurePeer(t, peer, module.Manifest, "1")
	call := callFrame("diagnostic", MethodOperationStatus, nil)
	call.Scope.Provider, call.Scope.Account, call.Scope.Generation = "vendor", "selected", 5
	call.Auth = json.RawMessage(`{"token":"private-credential"}`)
	peer.send(call)
	callback := peer.read()
	if callback.Kind != KindHostCall || callback.Method != MethodDiagnosticEmit || callback.Scope != call.Scope ||
		len(callback.Auth) != 0 || strings.Contains(string(callback.Params), "private-credential") {
		t.Fatalf("diagnostic callback lost authority or copied credentials: %+v", callback)
	}
	peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: callback.Parent, Result: json.RawMessage(`{"recorded":true}`)})
	result := peer.read()
	if result.Error != nil || string(result.Result) != `{"recorded":true}` {
		t.Fatal("diagnostic receipt was lost", result)
	}
}
