package extension

import "time"

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
