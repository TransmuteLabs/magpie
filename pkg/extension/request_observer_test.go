package extension

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
)

func requestObserverManifest() Manifest {
	manifest := testManifest(RequestObserver)
	manifest.Contract.Version.Minor = 12
	manifest.RequestObserver = &RequestObservationDescriptor{}
	return manifest
}

func TestRequestObserverOptInAndCompatibility(t *testing.T) {
	manifest := requestObserverManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := RequiredMethods(manifest); !slices.Equal(got, []string{MethodRequestObserve}) {
		t.Fatal(got)
	}
	prior := FullContract()
	prior.Capabilities = slices.DeleteFunc(prior.Capabilities, func(capability Capability) bool { return capability == RequestObserver })
	if prior.Check(manifest.Contract) == nil {
		t.Fatal("older host silently admitted this capability")
	}
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.Contract.Version.Minor = 11 },
		func(m *Manifest) { m.RequestObserver = nil },
		func(m *Manifest) { m.Contract.Capabilities = []Capability{OperationControl} },
		func(m *Manifest) { m.RequestObserver.TimeoutMs = 9 },
		func(m *Manifest) { m.Component = &ComponentIdentity{} },
	} {
		other := manifest.Clone()
		mutate(&other)
		if err := other.Validate(); err == nil {
			t.Fatalf("admitted invalid declaration: %+v", other)
		}
	}
	if manifest.RequestObserver.TimeoutMs != 0 {
		t.Fatal("manifest clone changed original declaration")
	}
}

func TestRequestObservationRejectsMalformedPhaseAndFields(t *testing.T) {
	for _, raw := range []string{
		`{"phase":"attempt","requestId":"abc","time":1}`,
		`{"phase":"end","requestId":"abc","time":1,"provider":"unexpected"}`,
		`{"phase":"start","requestId":"abc","time":1,"status":200}`,
		`{"Phase":"start","requestId":"abc","time":1}`,
		`{"phase":"start","requestId":"abc","time":9007199254740992}`,
		`{"phase":"start","requestId":"abc\n","time":1}`,
	} {
		if _, err := DecodeRequestObservation(json.RawMessage(raw)); err == nil {
			t.Fatalf("admitted malformed request observation: %s", raw)
		}
	}
	for _, payload := range []string{`[]`, `false`, `{"status":200}`} {
		if err := ValidateRequestObservationResult(json.RawMessage(payload)); err == nil {
			t.Fatalf("admitted invalid acknowledgment: %s", payload)
		}
	}
	for _, payload := range []string{`{}`, `null`} {
		if err := ValidateRequestObservationResult(json.RawMessage(payload)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRequestObserverRPCIsUnaryAndReadOnly(t *testing.T) {
	for _, mode := range []string{"normal", "auth", "null auth", "scope", "body", "event", "host callback", "mutation"} {
		t.Run(mode, func(t *testing.T) {
			var called atomic.Int32
			manifest := requestObserverManifest()
			peer := startPeer(t, Module{Manifest: manifest, Handlers: map[string]Handler{
				MethodRequestObserve: func(ctx context.Context, _ Call, stream *Stream) (any, error) {
					called.Add(1)
					switch mode {
					case "event":
						_ = stream.Emit(ctx, "accepted", AcceptedEvent{})
					case "host callback":
						var deny *Error
						if err := stream.HostCall(ctx, MethodStateCAS, StateCASRequest{Key: "ledger"}, nil); !errors.As(err, &deny) || deny.Code != "permission_denied" {
							return nil, errors.New("host callback lacked a permission denial")
						}
					case "mutation":
						return map[string]int{"input": 99}, nil
					}
					return nil, nil
				},
			}})
			configurePeer(t, peer, manifest, "0")
			frame := callFrame("observe", MethodRequestObserve,
				RequestObservation{Phase: "start", RequestID: "abc", Time: 1})
			switch mode {
			case "auth":
				frame.Auth = json.RawMessage(`{"token":"synthetic"}`)
			case "null auth":
				frame.Auth = json.RawMessage(`null`)
			case "scope":
				frame.Scope.Session = "synthetic"
			case "body":
				frame.Params = json.RawMessage(`{"phase":"start","requestId":"abc","time":1,"body":"synthetic"}`)
			}
			peer.send(frame)
			result := peer.read()
			allowed := mode == "normal" || mode == "host callback"
			if result.Kind != KindResult || (result.Error == nil) != allowed {
				t.Fatalf("%s: unexpected RPC result: %+v", mode, result)
			}
			if slices.Contains([]string{"auth", "null auth", "scope", "body"}, mode) {
				if called.Load() != 0 {
					t.Fatal("private request material reached the worker handler")
				}
			} else if called.Load() != 1 {
				t.Fatal("observer handler was not called")
			}
		})
	}
}
