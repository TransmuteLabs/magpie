package extension

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const MethodMemorySearch = "memory.search"

const (
	memorySearchMinor    = 14
	MaxMemorySearchQuery = 1024
	MaxMemorySearchHits  = 10
	MaxMemorySearchBytes = 16 << 10
)

func toolAllowsMemorySearch(manifest Manifest, toolID string) bool {
	if manifest.Contract.Version.Minor < memorySearchMinor || toolID == "" {
		return false
	}
	for _, tool := range manifest.Tools {
		if tool.Name == toolID {
			return tool.MemorySearch && tool.Exposure == "local-owner"
		}
	}
	return false
}

type MemorySearchRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

type MemorySearchHit struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type MemorySearchResult struct {
	Hits []MemorySearchHit `json:"hits"`
}

func ValidateMemorySearch(request MemorySearchRequest) error {
	if strings.TrimSpace(request.Query) == "" || len(request.Query) > MaxMemorySearchQuery ||
		!utf8.ValidString(request.Query) || request.Limit < 0 || request.Limit > MaxMemorySearchHits {
		return errors.New("invalid scoped Memory search")
	}
	return nil
}

func DecodeMemorySearch(raw json.RawMessage) (MemorySearchRequest, error) {
	var request MemorySearchRequest
	var fields map[string]json.RawMessage
	invalid := errors.New("invalid scoped Memory search")
	if len(raw) > 8<<10 || !utf8.Valid(raw) || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return request, invalid
	}
	for name, value := range fields {
		if (name != "query" && name != "limit") || string(value) == "null" {
			return request, invalid
		}
	}
	if json.Unmarshal(raw, &request) != nil || ValidateMemorySearch(request) != nil {
		return MemorySearchRequest{}, invalid
	}
	return request, nil
}
