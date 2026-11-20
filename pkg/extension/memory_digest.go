package extension

import "time"

// MemoryDigestRenderer is a linked Go module that renders and assembles digests.
// Source identifies the registered implementation. The host selects the
// records and owns persistence and writes to agent instructions.
// Render runs synchronously under the store lock using only the request data.
type MemoryDigestRenderer struct {
	Source  string
	Render  func(MemoryDigestRequest) (MemoryDigestResult, error)
	Compose func(MemoryDigestDeliveryRequest) (string, error)
}

// MemoryDigestRequest contains active records in store order. Budgets count
// the characters in item lines and their line breaks. Section headings are
// additional. KindOrder specifies the digest's kind order.
type MemoryDigestRequest struct {
	Records       []MemoryDigestRecord
	KindOrder     []string
	GlobalBudget  int
	ProjectBudget int
	AgentBudget   int
}

// MemoryDigestRecord contains the fields needed to render and order an item.
// Scope identifies "global", "project:<key>" or "agent:<id>".
type MemoryDigestRecord struct {
	Text    string
	Why     string
	Kind    string
	Scope   string
	Pinned  bool
	Updated time.Time
}

// MemoryDigestResult transfers rendered Markdown for selected scopes only.
type MemoryDigestResult struct {
	ByScope map[string]string
}

// MemoryDigestDeliveryRequest is the published content selected for one agent.
// Budget is the combined section budget in characters.
type MemoryDigestDeliveryRequest struct {
	Global   string
	Agent    string
	Projects []MemoryDigestProject
	Budget   int
}

type MemoryDigestProject struct {
	Scope    string
	Location string
	Markdown string
	Updated  time.Time
}
