package extension

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var ErrAgentRunControlTimeout = errors.New("agent did not answer its control request in time")

// AgentRunControl drives one linked agent process. The host owns its lifetime
// and decides when an operation is allowed. Replies come from that run alone.
type AgentRunControl interface {
	Initialize(context.Context, json.RawMessage, time.Duration) error
	ApplyEnvironment(context.Context, map[string]string, time.Duration) error
	SetEffort(string, time.Duration) error
	Rewind(string, time.Duration) error
	Receive(json.RawMessage)
	Close()
}

// AgentRunControlIO lends only the selected process's input and diagnostics.
// Send must stop writing when its context is canceled. The host closes the
// process input on shutdown and retires any input with a partial frame.
type AgentRunControlIO struct {
	Send  func(context.Context, []byte) error
	NewID func() string
	Ended func(string) error
}
