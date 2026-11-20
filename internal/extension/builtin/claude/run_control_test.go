package claude

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestRunControlRevokesPendingCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ended := errors.New("the selected subprocess ended")
	var control sdk.AgentRunControl
	var writes int
	control = NewRunControl(sdk.AgentRunControlIO{
		NewID: func() string { return "call" },
		Ended: func(string) error { return ended },
		Send: func(context.Context, []byte) error {
			writes++
			if writes == 1 {
				cancel()
			} else {
				control.Close()
			}
			return nil
		},
	})
	if err := control.Initialize(ctx, nil, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("initialize after cancellation: %v", err)
	}
	control.Receive(json.RawMessage(`{"request_id":"call","subtype":"success"}`))
	if err := control.ApplyEnvironment(t.Context(), nil, time.Hour); !errors.Is(err, ended) {
		t.Fatalf("update after shutdown: %v", err)
	}
	if err := control.Initialize(t.Context(), nil, time.Hour); !errors.Is(err, ended) || writes != 2 {
		t.Fatalf("closed run accepted another request: writes=%d err=%v", writes, err)
	}
}

func TestRunControlTimeoutLeavesOneWayEffortAvailable(t *testing.T) {
	var frames []string
	control := NewRunControl(sdk.AgentRunControlIO{
		NewID: func() string { return "call" },
		Send: func(_ context.Context, frame []byte) error {
			frames = append(frames, string(frame))
			return nil
		},
	})
	if err := control.ApplyEnvironment(t.Context(), nil, 20*time.Millisecond); !errors.Is(err, sdk.ErrAgentRunControlTimeout) {
		t.Fatalf("the complete frame got no reply: %v", err)
	}
	if err := control.SetEffort("high", time.Second); err != nil {
		t.Fatalf("one-way effort: %v", err)
	}
	if len(frames) != 2 || frames[1] != `{"type":"control_request","request_id":"effort-call","request":{"subtype":"apply_flag_settings","settings":{"effortLevel":"high"}}}` {
		t.Fatalf("control frames: %q", frames)
	}
}
