package extension

import (
	"context"
	"time"
)

// MemoryInput is a new record or a patch to one. An omitted field is left
// alone on edit.
type MemoryInput struct {
	Text   *string   `json:"text,omitempty"`
	Kind   *string   `json:"kind,omitempty"`
	Scope  *string   `json:"scope,omitempty"`
	Tags   *[]string `json:"tags,omitempty"`
	Why    *string   `json:"why,omitempty"`
	Status *string   `json:"status,omitempty"`
	Pinned *bool     `json:"pinned,omitempty"`
}

// MemorySource identifies where a memory was said or found. Dir is the
// working directory and File is an imported memory's original file.
type MemorySource struct {
	Agent   string    `json:"agent,omitempty"`
	Session string    `json:"session,omitempty"`
	Dir     string    `json:"dir,omitempty"`
	File    string    `json:"file,omitempty"`
	Quote   string    `json:"quote,omitempty"`
	At      time.Time `json:"at"`
}

// MemoryProject groups memories by repository or folder and lists the
// roots where that project was seen.
type MemoryProject struct {
	Key   string   `json:"key"`
	Name  string   `json:"name"`
	Roots []string `json:"roots,omitempty"`
}

// MemoryRecord is one thing known. Why gives the reason behind it.
// Edited tracks the user's last change, Used and Uses track searches,
// and Skill names the Library skill made from this procedure.
type MemoryRecord struct {
	ID           string         `json:"id"`
	Text         string         `json:"text"`
	Kind         string         `json:"kind"`
	Scope        string         `json:"scope"`
	Tags         []string       `json:"tags,omitempty"`
	Why          string         `json:"why,omitempty"`
	Origin       string         `json:"origin"`
	Status       string         `json:"status"`
	SupersededBy string         `json:"supersededBy,omitempty"`
	Pinned       bool           `json:"pinned,omitempty"`
	Sources      []MemorySource `json:"sources,omitempty"`
	Created      time.Time      `json:"created"`
	Updated      time.Time      `json:"updated"`
	Edited       time.Time      `json:"edited,omitzero"`
	Used         time.Time      `json:"used,omitzero"`
	Uses         int            `json:"uses,omitempty"`
	Skill        string         `json:"skill,omitempty"`
}

// MemoryQuery selects scopes, kinds and tags before ranking. Empty Scopes
// includes all scopes and empty Status selects active records.
type MemoryQuery struct {
	Text   string
	Scopes []string
	Kinds  []string
	Tags   []string
	Status string
	Limit  int
}

type MemoryHit struct {
	*MemoryRecord
	Score float64 `json:"score"`
}

// MemoryAgentWriter is a linked contract bound by the host to an admitted
// Memory caller. Its scope and provenance are selected before invocation.
// A record ID identifies a record within the grant. It conveys no authority.
type MemoryAgentWriter interface {
	Save(context.Context, MemoryInput) (*MemoryRecord, bool, error)
	Edit(context.Context, string, MemoryInput) (*MemoryRecord, error)
	Forget(context.Context, string) (*MemoryRecord, error)
}

// MemoryOwner is the linked app owner used after GUI admission and by local
// maintenance. It keeps validation, locking and persistence with Memory.
// Installable module handlers do not receive this contract.
type MemoryOwner struct {
	All        func() ([]*MemoryRecord, map[string]*MemoryProject, error)
	Get        func(string) (*MemoryRecord, error)
	Rank       func(MemoryQuery) ([]MemoryHit, error)
	Search     func(MemoryQuery) ([]MemoryHit, error)
	Save       func(MemoryInput, string, *MemorySource, *MemoryProject) (*MemoryRecord, bool, error)
	Edit       func(string, MemoryInput, bool) (*MemoryRecord, error)
	Remove     func(...string) error
	SetStatus  func(string, ...string) (int, error)
	Promote    func([]string) (*MemoryRecord, error)
	Suggest    func() ([]MemorySuggestion, error)
	Decline    func(string) error
	MadeSkill  func(string, string) error
	Tidy       func(MemoryTidyPlan) ([]MemoryTidyChange, error)
	Untidy     func([]MemoryTidyChange) (int, error)
	Prune      func() (MemoryPruned, error)
	Publish    func() (bool, error)
	PublishDue func() (bool, error)
	Digest     func(string) (string, error)
}

type MemorySuggestion struct {
	Key      string   `json:"key"`
	To       string   `json:"to"`
	IDs      []string `json:"ids"`
	Text     string   `json:"text"`
	Projects []string `json:"projects,omitempty"`
	Uses     int      `json:"uses,omitempty"`
}

type MemoryTidyMerge struct {
	IDs  []string
	Text string
	Kind string
}

type MemoryTidyStale struct {
	ID  string
	Why string
}

type MemoryTidyPlan struct {
	Merges []MemoryTidyMerge
	Stale  []MemoryTidyStale
	Before time.Time
}

type MemoryTidyChange struct {
	Action string   `json:"action"`
	Into   string   `json:"into,omitempty"`
	New    bool     `json:"new,omitempty"`
	IDs    []string `json:"ids"`
	Was    []string `json:"was"`
	Text   string   `json:"text"`
	Why    string   `json:"why,omitempty"`
}

type MemoryPruned struct {
	Archived int
	Removed  int
}
