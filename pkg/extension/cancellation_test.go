package extension

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestCanceledStreamAcceptsLateAckAndKeepsWorkerHealthy(t *testing.T) {
	peer := startPeer(t, operationModule(func(ctx context.Context, call Call, stream *Stream) (any, error) {
		if call.ID == "other" {
			return "healthy", nil
		}
		return nil, stream.Emit(ctx, "data", []byte("awaiting acknowledgement"))
	}))
	peer.send(callFrame("canceled", MethodOperationStatus, nil))
	event := peer.read()
	if event.Kind != KindEvent || event.Seq != 1 {
		t.Fatal("stream did not acquire credit", event)
	}
	peer.send(Frame{Kind: KindCancel, ID: event.ID})
	terminal := peer.read()
	if terminal.Kind != KindResult || terminal.ID != event.ID || terminal.Error == nil || terminal.Error.Code != "canceled" {
		t.Fatal("cancel waited for acknowledgement", terminal)
	}
	peer.send(Frame{Kind: KindAck, ID: event.ID, Seq: event.Seq})
	peer.send(callFrame("other", MethodOperationStatus, nil))
	if next := peer.read(); next.ID != "other" || next.Error != nil {
		t.Fatal("late acknowledgement killed a healthy worker", next)
	}
}

func TestCanceledHostCallAcceptsLateReplyAndKeepsWorkerHealthy(t *testing.T) {
	peer := startPeer(t, operationModule(func(ctx context.Context, call Call, stream *Stream) (any, error) {
		if call.ID == "other" {
			return "healthy", nil
		}
		return nil, stream.HostCall(ctx, MethodStateGet, StateGetRequest{Key: "state"}, nil)
	}))
	peer.send(callFrame("canceled", MethodOperationStatus, nil))
	callback := peer.read()
	if callback.Kind != KindHostCall || callback.Parent != "canceled" {
		t.Fatal("host callback did not start", callback)
	}
	peer.send(Frame{Kind: KindCancel, ID: callback.Parent})
	terminal := peer.read()
	if terminal.Kind != KindResult || terminal.ID != callback.Parent || terminal.Error == nil || terminal.Error.Code != "canceled" {
		t.Fatal("cancel waited for the host service", terminal)
	}
	peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: callback.Parent, Result: json.RawMessage(`null`)})
	peer.send(callFrame("other", MethodOperationStatus, nil))
	if next := peer.read(); next.ID != "other" || next.Error != nil {
		t.Fatal("late host result killed a healthy worker", next)
	}
}

func TestUnknownHostReplyIsProtocolError(t *testing.T) {
	server := &worker{host: make(map[string]*hostWait)}
	err := server.dispatch(Frame{Kind: KindHostResult, ID: "unknown", Parent: "unknown-parent", Result: json.RawMessage(`null`)})
	var public *Error
	if !errors.As(err, &public) || public.Code != "protocol_error" {
		t.Fatal("unknown host result was ignored", err)
	}
}

func TestAbandonedStreamCreditForcesErrorEvenIfHandlerSwallowsCancellation(t *testing.T) {
	cancelEvent := make(chan context.CancelFunc, 1)
	peer := startPeer(t, operationModule(func(ctx context.Context, call Call, stream *Stream) (any, error) {
		if call.ID == "other" {
			return "healthy", nil
		}
		child, cancel := context.WithCancel(ctx)
		defer cancel()
		cancelEvent <- cancel
		stream.Emit(child, "data", []byte("pending"))
		return "must not succeed with unacknowledged credit", nil
	}))
	peer.send(callFrame("abandoned", MethodOperationStatus, nil))
	event := peer.read()
	select {
	case cancel := <-cancelEvent:
		cancel()
	default:
		t.Fatal("event context was not created")
	}
	terminal := peer.read()
	if terminal.Kind != KindResult || terminal.Error == nil || terminal.Error.Code != "canceled" {
		t.Fatal("unacknowledged credit was closed by a success terminal", terminal)
	}
	peer.send(Frame{Kind: KindAck, ID: event.ID, Seq: event.Seq})
	peer.send(callFrame("other", MethodOperationStatus, nil))
	if result := peer.read(); result.Error != nil || result.ID != "other" {
		t.Fatal("abandoned event credit stopped the worker", result)
	}
}

func TestClosedCancellationFramesStillRequireExactSequenceAndParent(t *testing.T) {
	for _, callback := range []bool{false, true} {
		t.Run(map[bool]string{false: "ack", true: "host result"}[callback], func(t *testing.T) {
			peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
				if callback {
					return nil, stream.HostCall(ctx, MethodStateGet, StateGetRequest{Key: "state"}, nil)
				}
				return nil, stream.Emit(ctx, "data", []byte("pending"))
			}))
			peer.send(callFrame("canceled", MethodOperationStatus, nil))
			waiting := peer.read()
			peer.send(Frame{Kind: KindCancel, ID: "canceled"})
			if result := peer.read(); result.Kind != KindResult || result.Error == nil {
				t.Fatal("call was not canceled", result)
			}
			if callback {
				peer.send(Frame{Kind: KindHostResult, ID: waiting.ID, Parent: "another-parent", Result: json.RawMessage(`null`)})
			} else {
				peer.send(Frame{Kind: KindAck, ID: waiting.ID, Seq: waiting.Seq + 1})
			}
			var public *Error
			if err := peer.wait(); !errors.As(err, &public) || public.Code != "protocol_error" {
				t.Fatal("closed credit accepted the wrong sequence or parent", err)
			}
		})
	}
}
