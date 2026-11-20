package extension

import (
	"context"
	"time"
)

// MemorySemanticService binds the linked ranker and its management operations
// to one backend using the host's Memory index source.
type MemorySemanticService struct {
	Ranker MemorySemanticRanker
	On     func() bool
	Status func() MemorySemanticStatus
	Set    func(bool) error
	Keep   func(context.Context, time.Duration)
}

type MemorySemanticStatus struct {
	On          bool   `json:"on"`
	Installed   bool   `json:"installed"`
	Size        int64  `json:"size"`
	Downloading bool   `json:"downloading"`
	Done        int64  `json:"done"`
	Error       string `json:"error,omitempty"`
	Read        int    `json:"read"`
	Of          int    `json:"of"`
	Reading     bool   `json:"reading"`
	Failed      string `json:"failed,omitempty"`
}

// MemorySemanticRanker is a linked Go module for semantic Memory search.
// Ready reports availability before the host builds a ranking snapshot.
// Rank returns cosines by record ID, or nil for word-only ranking.
type MemorySemanticRanker struct {
	Ready func() bool
	Rank  func(MemorySemanticRequest) map[string]float64
}

// MemorySemanticRequest contains the query and the host-selected records.
// The host keeps scope filtering, rank fusion, usage and persistence.
type MemorySemanticRequest struct {
	Query   string
	Records []MemorySemanticRecord
}

// MemorySemanticRecord contains only the fields needed for similarity.
type MemorySemanticRecord struct {
	ID   string
	Text string
	Why  string
}

// MemorySemanticIndexSource supplies the host's active and candidate records
// to a linked indexer. An error means the snapshot is unknown and must not
// be used to prune the index.
type MemorySemanticIndexSource func() ([]MemorySemanticRecord, error)
