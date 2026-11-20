package extension

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// MemoryBundle is the portable state used by backup and sync.
type MemoryBundle struct {
	Memories  []*MemoryRecord           `json:"memories"`
	Projects  map[string]*MemoryProject `json:"projects,omitempty"`
	Dismissed []string                  `json:"dismissed,omitempty"`
	Removed   map[string]time.Time      `json:"removed,omitempty"`
	Declined  []string                  `json:"declined,omitempty"`
}

// Hash identifies sync changes. Search-use counters do not affect it.
func (b *MemoryBundle) Hash() string {
	if b == nil {
		return ""
	}
	snapshot := *b
	snapshot.Memories = make([]*MemoryRecord, len(b.Memories))
	for index, record := range b.Memories {
		copy := *record
		copy.Used, copy.Uses = time.Time{}, 0
		snapshot.Memories[index] = &copy
	}
	body, _ := json.Marshal(snapshot)
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
