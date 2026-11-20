package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type runControl struct {
	io      sdk.AgentRunControlIO
	mu      sync.Mutex
	pending map[string]chan controlReply
	closed  bool
}

type controlRequest struct {
	Subtype            string           `json:"subtype"`
	Schema             json.RawMessage  `json:"jsonSchema,omitempty"`
	Settings           *controlSettings `json:"settings,omitempty"`
	Turn               *string          `json:"target_message_uuid,omitempty"`
	InterruptIfRunning bool             `json:"interrupt_if_running,omitempty"`
}

type controlSettings struct {
	Env    map[string]string `json:"env,omitempty"`
	Effort *string           `json:"effortLevel,omitempty"`
}

type controlReply struct {
	Subtype   string `json:"subtype"`
	RequestID string `json:"request_id"`
	Error     string `json:"error"`
	Response  struct {
		Rewound bool `json:"rewound"`
	} `json:"response"`
}

type controlUnanswered struct{ what string }

func (e *controlUnanswered) Error() string { return "Claude Code has not " + e.what }
func (e *controlUnanswered) Unwrap() error { return sdk.ErrAgentRunControlTimeout }

var errRunEnded = errors.New("the run ended")
var errControlTimeout = errors.New("control reply timed out")

func NewRunControl(io sdk.AgentRunControlIO) sdk.AgentRunControl {
	return &runControl{io: io}
}

func (c *runControl) Initialize(ctx context.Context, schema json.RawMessage, wait time.Duration) error {
	return c.apply(ctx, controlRequest{Subtype: "initialize", Schema: schema}, wait)
}

func (c *runControl) ApplyEnvironment(ctx context.Context, env map[string]string, wait time.Duration) error {
	return c.apply(ctx, controlRequest{Subtype: "apply_flag_settings", Settings: &controlSettings{Env: env}}, wait)
}

func (c *runControl) SetEffort(effort string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return c.send(ctx, c.shortID("effort-"), controlRequest{
		Subtype: "apply_flag_settings", Settings: &controlSettings{Effort: &effort},
	})
}

func (c *runControl) Rewind(turn string, wait time.Duration) error {
	request := controlRequest{Subtype: "rewind_conversation", Turn: &turn, InterruptIfRunning: true}
	reply, err := c.exchange(context.Background(), c.shortID("rewind-"), request, wait)
	switch {
	case errors.Is(err, errControlTimeout):
		return errors.New("Claude Code did not answer in " + wait.String())
	case err != nil:
		return err
	case reply.Subtype != "success":
		return fmt.Errorf("Claude Code refused it: %s", reply.Error)
	case !reply.Response.Rewound:
		return errors.New("Claude Code did not rewind")
	}
	return nil
}

func (c *runControl) shortID(prefix string) string {
	id := c.io.NewID()
	return prefix + id[:min(len(id), 12)]
}

func (c *runControl) apply(ctx context.Context, request controlRequest, wait time.Duration) error {
	reply, err := c.exchange(ctx, c.io.NewID(), request, wait)
	what := "answered magpie's " + request.Subtype + " request"
	switch {
	case errors.Is(err, errRunEnded):
		return c.io.Ended(what)
	case errors.Is(err, errControlTimeout):
		return &controlUnanswered{fmt.Sprintf("%s in %s", what, wait)}
	case err != nil:
		return err
	case reply.Subtype != "success":
		return fmt.Errorf("Claude settings: %s", reply.Error)
	}
	return nil
}

func (c *runControl) send(ctx context.Context, id string, request controlRequest) error {
	frame, err := json.Marshal(struct {
		Type    string         `json:"type"`
		ID      string         `json:"request_id"`
		Request controlRequest `json:"request"`
	}{"control_request", id, request})
	if err != nil {
		return err
	}
	return c.io.Send(ctx, frame)
}

func (c *runControl) exchange(ctx context.Context, id string, request controlRequest, wait time.Duration) (controlReply, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, wait, errControlTimeout)
	defer cancel()
	reply := make(chan controlReply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return controlReply{}, errRunEnded
	}
	if c.pending == nil {
		c.pending = make(map[string]chan controlReply)
	}
	c.pending[id] = reply
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.send(ctx, id, request); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && context.Cause(ctx) == errControlTimeout {
			err = errControlTimeout
		}
		return controlReply{}, err
	}
	select {
	case response, ok := <-reply:
		if !ok {
			return controlReply{}, errRunEnded
		}
		return response, nil
	case <-ctx.Done():
		if context.Cause(ctx) == errControlTimeout {
			return controlReply{}, errControlTimeout
		}
		return controlReply{}, ctx.Err()
	}
}

func (c *runControl) Receive(raw json.RawMessage) {
	var reply controlReply
	if json.Unmarshal(raw, &reply) != nil {
		return
	}
	c.mu.Lock()
	result := c.pending[reply.RequestID]
	delete(c.pending, reply.RequestID)
	c.mu.Unlock()
	if result != nil {
		result <- reply
	}
}

func (c *runControl) Close() {
	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, waiter := range pending {
		close(waiter)
	}
}
