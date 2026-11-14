package extension

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestOwnCallAndOutsideReaderRequireDistinctNegotiatedContracts(t *testing.T) {
	manifest := completeManifest()
	manifest.Contract.Version.Minor = 8
	manifest.Providers[0].Calls = true
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(RequiredMethods(manifest), MethodProviderCalls) {
		t.Fatal("declared outside reader has no ordinary method")
	}
	valid := OwnCall{Kind: "cache_keepalive", Model: "model", Status: 200, Usage: Usage{InputTokens: 3}}
	if err := ValidateOwnCall(manifest, valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*OwnCall){
		func(call *OwnCall) { call.Kind = "" },
		func(call *OwnCall) { call.Kind = "arbitrary kind" },
		func(call *OwnCall) { call.Model = "bad model" },
		func(call *OwnCall) { call.Status = 600 },
		func(call *OwnCall) { call.Millis = -1 },
		func(call *OwnCall) { call.Error = strings.Repeat("x", 1025) },
		func(call *OwnCall) { call.Usage.InputTokens = -1 },
	} {
		invalid := valid
		mutate(&invalid)
		if ValidateOwnCall(manifest, invalid) == nil {
			t.Fatalf("invalid own call passed: %+v", invalid)
		}
	}
	for _, minor := range []int{6, 7} {
		older := manifest.Clone()
		older.Contract.Version.Minor = minor
		if older.Validate() == nil || ValidateOwnCall(older, valid) == nil {
			t.Fatal("older module acquired outside reader or own-call authority", minor)
		}
	}
}

func TestOutsideCallPagesKeepTheSelectedWindowAndAccount(t *testing.T) {
	manifest := completeManifest()
	manifest.Contract.Version.Minor = 8
	manifest.Providers[0].Calls = true
	scope := Scope{Plugin: manifest.ID, Provider: manifest.Providers[0].ID, Account: "alice", Generation: 2}
	raw := json.RawMessage(`{"since":1700000000000,"until":1700000060000,"cursor":"page-two"}`)
	request, err := DecodeOutsideCallsRequest(manifest, scope, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{"since":1700000000000,"until":1700000060000,"account":"bob"}`),
		json.RawMessage(`{"since":1700000060000,"until":1700000000000}`),
		json.RawMessage(`{"since":0,"until":1700000000000}`),
		json.RawMessage(`{"since":1700000000000,"until":9007199254740992}`),
	} {
		if _, err := DecodeOutsideCallsRequest(manifest, scope, invalid); err == nil {
			t.Fatal("outside reader accepted a selector or invalid window", string(invalid))
		}
	}
	valid := json.RawMessage(`{"calls":[{"id":"vendor-id","time":1700000000001,"model":"model","usage":{"facts":{"inputTokens":0}}}],"nextCursor":"last"}`)
	if _, err := DecodeOutsideCallsResult(manifest, request, valid); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{}`), json.RawMessage(`{"calls":null}`),
		json.RawMessage(`{"calls":[],"account":"bob"}`),
		json.RawMessage(`{"calls":[{"id":"x","time":1700000000001,"model":"m","account":"bob"}]}`),
		json.RawMessage(`{"calls":[{"id":"x","time":1700000000001,"model":"m","usage":{"facts":{"callerKeyId":"forged"}}}]}`),
		json.RawMessage(`{"calls":[{"id":"x","time":1700000060001,"model":"m"}]}`),
	} {
		if _, err := DecodeOutsideCallsResult(manifest, request, invalid); err == nil {
			t.Fatal("invalid outside page acquired publication authority", string(invalid))
		}
	}
	scope.Operation = "inference"
	if _, err := DecodeOutsideCallsRequest(manifest, scope, raw); err == nil {
		t.Fatal("outside history borrowed inference authority")
	}
}

func TestOwnCallAggregatedCountsStayRepresentable(t *testing.T) {
	manifest := completeManifest()
	manifest.Contract.Version.Minor = 8
	maximum := min(int64(^uint(0)>>1), int64(1<<53-1))
	one, zero := int64(1), int64(0)
	call := OwnCall{Kind: "cache_keepalive", Model: "model", Usage: Usage{Facts: &ExecutionUsageFacts{
		ExecutionCounts: ExecutionCounts{InputTokens: &maximum},
		Deferred:        &ExecutionCounts{InputTokens: &zero},
	}}}
	if err := ValidateOwnCall(manifest, call); err != nil {
		t.Fatal("representable aggregate refused", err)
	}
	call.Usage.Facts.Deferred.InputTokens = &one
	if err := ValidateOwnCall(manifest, call); err == nil {
		t.Fatal("overflowing current and deferred counts reached the ledger")
	}
}

func TestOwnCallCallbackCarriesScopeWithoutCredentials(t *testing.T) {
	module := operationModule(func(ctx context.Context, call Call, stream *Stream) (any, error) {
		return stream.AppendOwnCall(ctx, OwnCall{Kind: "cache_keepalive", Model: "model"})
	})
	module.Manifest.Contract.Version.Minor = 8
	module.Manifest.Contract.Capabilities = append(module.Manifest.Contract.Capabilities, UsageReporter, ModelDiscovery)
	module.Manifest.Providers = []ProviderDescriptor{providerDescriptor()}
	module.Handlers[MethodProviderModels] = func(context.Context, Call, *Stream) (any, error) { return ModelsResult{}, nil }
	module.Handlers[MethodProviderUsage] = func(context.Context, Call, *Stream) (any, error) { return UsageResult{}, nil }
	peer := startPeer(t, module)
	configurePeer(t, peer, module.Manifest, "1")
	call := callFrame("own", MethodOperationStatus, nil)
	call.Scope.Provider, call.Scope.Account, call.Scope.Generation = "fixture", "alice", 2
	call.Auth = json.RawMessage(`{"token":"private-token"}`)
	peer.send(call)
	callback := peer.read()
	if callback.Kind != KindHostCall || callback.Method != MethodUsageAppend || callback.Scope != call.Scope ||
		len(callback.Auth) != 0 || strings.Contains(string(callback.Params), "private-token") {
		t.Fatalf("own-call frame widened authority or disclosed credentials: %+v", callback)
	}
	peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: callback.Parent, Result: json.RawMessage(`{"accepted":true}`)})
	result := peer.read()
	if result.Error != nil || string(result.Result) != `{"accepted":true}` {
		t.Fatal("own-call acknowledgment was lost", result)
	}
}
