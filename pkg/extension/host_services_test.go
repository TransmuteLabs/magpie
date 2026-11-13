package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestNetworkProxyHostCallUsesOnlyItsParentScope(t *testing.T) {
	const target = "https://provider.invalid/native"
	const proxy = "http://selected:proxy-secret@127.0.0.1:1080"
	peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
		var result NetworkProxyResult
		err := stream.HostCall(ctx, MethodNetworkProxy, NetworkProxyRequest{URL: target}, &result)
		return result, err
	}))
	call := callFrame("proxy", MethodOperationStatus, nil)
	call.Scope.Provider, call.Scope.Account, call.Scope.Generation = "provider", "selected-account", 4
	call.Auth = json.RawMessage(`{"token":"selected-auth"}`)
	peer.send(call)
	callback := peer.read()
	var request NetworkProxyRequest
	if callback.Kind != KindHostCall || callback.Method != MethodNetworkProxy || callback.Parent != call.ID ||
		callback.Scope != call.Scope || callback.Auth != nil || json.Unmarshal(callback.Params, &request) != nil || request.URL != target {
		t.Fatalf("proxy callback lost parent authority: %+v", callback)
	}
	result, _ := json.Marshal(NetworkProxyResult{URL: proxy})
	peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: callback.Parent, Result: result})
	terminal := peer.read()
	var resolved NetworkProxyResult
	if terminal.Error != nil || terminal.ID != call.ID || json.Unmarshal(terminal.Result, &resolved) != nil || resolved.URL != proxy {
		t.Fatal("scoped proxy reply was not delivered", terminal)
	}
	body, err := json.Marshal(NetworkProxyResult{})
	if err != nil || string(body) != "{}" {
		t.Fatal("direct/bypassed proxy response changed", string(body), err)
	}
	if _, found := MethodCapability(MethodNetworkProxy); found || len(FullContract().Capabilities) != 10 {
		t.Fatal("host proxy service acquired a worker capability")
	}
}

func TestHostServicesAcceptOneMiBStateAndEnforceNewPayloadBounds(t *testing.T) {
	t.Run("typed state value", func(t *testing.T) {
		value := json.RawMessage(`"` + strings.Repeat("x", (1<<20)-2) + `"`)
		peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
			var applied StateCASResult
			if err := stream.HostCall(ctx, MethodStateCAS, StateCASRequest{Key: "model-cache", Version: "1", Value: value}, &applied); err != nil {
				return nil, err
			}
			var result StateValue
			err := stream.HostCall(ctx, MethodStateGet, StateGetRequest{Key: "model-cache"}, &result)
			return len(result.Value), err
		}))
		peer.send(callFrame("state", MethodOperationStatus, nil))
		callback := peer.read()
		var request StateCASRequest
		if callback.Kind != KindHostCall || json.Unmarshal(callback.Params, &request) != nil || len(request.Value) != 1<<20 {
			t.Fatal("one MiB state DTO was refused", callback.Kind, callback.Error)
		}
		peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: callback.Parent, Result: json.RawMessage(`{"swapped":true,"version":"2"}`)})
		callback = peer.read()
		if callback.Kind != KindHostCall || callback.Method != MethodStateGet {
			t.Fatal("state read did not follow CAS", callback.Kind, callback.Error)
		}
		response, err := json.Marshal(StateValue{Found: true, Version: "2", Value: value})
		if err != nil {
			t.Fatal(err)
		}
		peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: callback.Parent, Result: response})
		result := peer.read()
		var length int
		if result.Error != nil || json.Unmarshal(result.Result, &length) != nil || length != 1<<20 {
			t.Fatal("one MiB state value was truncated", result)
		}
	})
	for _, size := range []int{(1 << 20) + 128, MaxHostBytes, MaxHostBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			payload := json.RawMessage(`"` + strings.Repeat("x", size-2) + `"`)
			peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
				var reply string
				err := stream.HostCall(ctx, MethodStateCAS, payload, &reply)
				return len(reply), err
			}))
			peer.send(callFrame("state", MethodOperationStatus, nil))
			frame := peer.read()
			if size > MaxHostBytes {
				if frame.Kind != KindResult || frame.Error == nil || frame.Error.Code != "resource_exhausted" {
					t.Fatal("oversized host parameters escaped", frame)
				}
				return
			}
			if frame.Kind != KindHostCall || len(frame.Params) != size {
				t.Fatalf("large state was refused: kind=%s size=%d error=%v", frame.Kind, len(frame.Params), frame.Error)
			}
			peer.send(Frame{Kind: KindHostResult, ID: frame.ID, Parent: frame.Parent, Result: payload})
			result := peer.read()
			var length int
			if result.Error != nil || json.Unmarshal(result.Result, &length) != nil || length != size-2 {
				t.Fatal("large host result was refused", result)
			}
		})
	}
	t.Run("oversized result", func(t *testing.T) {
		peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
			return nil, stream.HostCall(ctx, MethodStateGet, StateGetRequest{Key: "cache"}, nil)
		}))
		peer.send(callFrame("state", MethodOperationStatus, nil))
		callback := peer.read()
		if callback.Kind != KindHostCall {
			t.Fatal("state callback did not start", callback)
		}
		peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: callback.Parent,
			Result: json.RawMessage(`"` + strings.Repeat("x", MaxHostBytes-1) + `"`)})
		if result := peer.read(); result.Kind != KindResult || result.Error == nil || result.Error.Code != "resource_exhausted" {
			t.Fatal("oversized host result escaped", result)
		}
	})
	if MaxChunkBytes != 64<<10 || MaxFrameBytes != 24<<20 || MaxHostBytes != (1<<20)+(64<<10) {
		t.Fatal("host services changed event or envelope limits")
	}
}
