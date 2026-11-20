package memdistill

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sessions"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestReadAllKeepsSourceInterruptionPending(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, context.Canceled,
		&sdk.Error{Code: "deadline", Message: "source did not finish"}, &sdk.Error{Code: "canceled", Message: "source stopped"}} {
		t.Run(failure.Error(), func(t *testing.T) {
			sb := newSandbox(t)
			on(t, "")
			sb.session("pending", time.Hour, "we deploy on Tuesdays", "keep this preference")
			normal := read
			read = func(context.Context, sessions.Session) (sessions.Transcript, error) {
				return sessions.Transcript{}, failure
			}
			result, err := ReadAll(t.Context(), "a/m", sb.complete)
			if !errors.Is(err, failure) || result != (Result{}) || len(sb.asked) != 0 {
				t.Fatalf("interrupted transcript reached the model or hid the cause: %+v %v %v", result, err, sb.asked)
			}
			state, err := Load()
			if err != nil || len(state.Done) != 0 || state.Left != 1 || state.Error != failure.Error() {
				t.Fatalf("interrupted transcript was not left pending: %+v %v", state, err)
			}
			read = normal
			result, err = ReadAll(t.Context(), "a/m", sb.complete)
			if err != nil || result.Read != 1 || len(sb.asked) != 1 {
				t.Fatalf("unchanged transcript was not retried: %+v %v %v", result, err, sb.asked)
			}
		})
	}
}

func TestStoredGatewayTranscriptHonorsCanceledContext(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := sessions.SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	turn := sessions.GatewayTurn{Agent: "workbuddy", Session: "stored", Time: time.Now(), Status: 200,
		Input: []sessions.Part{{Role: "user", Kind: "text", Text: "Recorded user words."}}}
	if err := sessions.SaveGatewayTurn(turn); err != nil {
		t.Fatal(err)
	}
	session := sessions.Session{Agent: turn.Agent, ID: turn.Session, Gateway: true}
	if transcript, err := transcriptOf(t.Context(), session); err != nil || transcript.Captured != 1 {
		t.Fatalf("journal fixture is not readable: %+v %v", transcript, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	transcript, err := transcriptOf(ctx, session)
	if !errors.Is(err, context.Canceled) || transcript.Captured != 0 || len(transcript.Parts) != 0 {
		t.Fatalf("stopped reader consumed a stored gateway transcript: %+v %v", transcript, err)
	}
}
