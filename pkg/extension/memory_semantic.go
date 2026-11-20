package extension

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
