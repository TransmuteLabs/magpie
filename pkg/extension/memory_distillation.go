package extension

import (
	"context"
	"time"
)

// MemoryCompletion sends model work through the host's selected gateway.
type MemoryCompletion func(ctx context.Context, model, effort, system, user string) (string, error)

// MemoryDistillationService is the linked owner of conversation review and
// nightly cleanup. Manual and scheduled jobs use the same host model callback.
// ReadNow registers an admitted run with Status and Stop before returning.
type MemoryDistillationService struct {
	Status    func() (MemoryDistillationStatus, error)
	Configure func(MemoryDistillationSettings) (readNow bool, err error)
	ReadNow   func(context.Context)
	Stop      func()
	SetDream  func(bool) error
	DreamNow  func(context.Context)
	Undo      func() error
	Keep      func(context.Context, time.Duration, time.Duration)
}

type MemoryDistillationSettings struct {
	On     bool    `json:"on"`
	Model  string  `json:"model"`
	Effort *string `json:"effort"`
	Days   *int    `json:"days"`
}

type MemoryDistillationStatus struct {
	MemoryDistillationState
	Running  bool  `json:"running"`
	Dreaming bool  `json:"dreaming"`
	Ranges   []int `json:"ranges"`
	Doing    int   `json:"doing,omitempty"`
	Of       int   `json:"of,omitempty"`
}

type MemoryDistillationState struct {
	On         bool                 `json:"on,omitempty"`
	Model      string               `json:"model,omitempty"`
	Days       int                  `json:"days,omitempty"`
	Effort     string               `json:"effort,omitempty"`
	Since      time.Time            `json:"since,omitzero"`
	Done       map[string]time.Time `json:"done,omitempty"`
	Last       time.Time            `json:"last,omitzero"`
	LastModel  string               `json:"lastModel,omitempty"`
	Read       int                  `json:"read,omitempty"`
	Added      int                  `json:"added,omitempty"`
	Error      string               `json:"error,omitempty"`
	Left       int                  `json:"left,omitempty"`
	Reads      int                  `json:"reads,omitempty"`
	Dream      bool                 `json:"dream,omitempty"`
	DreamSince time.Time            `json:"dreamSince,omitzero"`
	Dreamt     time.Time            `json:"dreamt,omitzero"`
	DreamModel string               `json:"dreamModel,omitempty"`
	DreamError string               `json:"dreamError,omitempty"`
	Changes    []MemoryTidyChange   `json:"changes,omitempty"`
	Undone     bool                 `json:"undone,omitempty"`
}

// MemoryDistillationCodec is a linked Go module for formatting model
// requests and decoding proposals. The host selects their inputs, sends
// the request, validates replies and owns every store mutation.
type MemoryDistillationCodec struct {
	Source             string
	DistillationPrompt func(MemoryDistillationRequest) MemoryModelPrompt
	DistillationReply  func(string) []MemoryDistillationProposal
	DreamPrompt        func(MemoryDreamRequest) MemoryModelPrompt
	DreamReply         func(string) MemoryDreamSuggestions
}

// MemoryDistillationRequest is a host-selected conversation and the
// memories already known for its project or globally.
type MemoryDistillationRequest struct {
	ProjectName  string
	Known        []string
	Conversation string
}

// MemoryModelPrompt contains the two messages sent by the host's gateway
// client using the selected model, effort and request context.
type MemoryModelPrompt struct {
	System string
	User   string
}

// MemoryDistillationProposal is a model suggestion. Only the host can
// verify the quote, assign the source and scope, and save it as a candidate.
type MemoryDistillationProposal struct {
	Text  string `json:"text"`
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
	Why   string `json:"why"`
	Quote string `json:"quote"`
}

// MemoryDreamRequest contains the host-selected notes for one scope.
// Each label is local to this request.
type MemoryDreamRequest struct {
	Today time.Time
	Notes []MemoryDreamNote
}

// MemoryDreamNote is a projection of an active or candidate memory.
// Label takes the place of the store ID, and Fixed is decided by the host.
type MemoryDreamNote struct {
	Label   string
	Kind    string
	Created time.Time
	Uses    int
	Fixed   bool
	Text    string
	Why     string
}

// MemoryDreamSuggestions are untrusted model suggestions. The host
// resolves labels from the original request and checks changes at the store.
type MemoryDreamSuggestions struct {
	Merge []MemoryDreamMerge `json:"merge"`
	Stale []MemoryDreamStale `json:"stale"`
}

type MemoryDreamMerge struct {
	Labels []string `json:"ids"`
	Text   string   `json:"text"`
	Kind   string   `json:"kind"`
}

type MemoryDreamStale struct {
	Label string `json:"id"`
	Why   string `json:"why"`
}
