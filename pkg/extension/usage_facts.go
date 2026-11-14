package extension

import (
	"errors"
	"math"
	"strings"
	"unicode"
)

type ExecutionCounts struct {
	InputTokens        *int64 `json:"inputTokens,omitempty"`
	OutputTokens       *int64 `json:"outputTokens,omitempty"`
	CachedTokens       *int64 `json:"cachedTokens,omitempty"`
	CacheWriteTokens   *int64 `json:"cacheWriteTokens,omitempty"`
	CacheWrite1hTokens *int64 `json:"cacheWrite1hTokens,omitempty"`
	ReasoningTokens    *int64 `json:"reasoningTokens,omitempty"`
}

type ExecutionCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

type ExecutionUsageFacts struct {
	ExecutionCounts
	CacheUnknown     *bool            `json:"cacheUnknown,omitempty"`
	Deferred         *ExecutionCounts `json:"deferred,omitempty"`
	ServedModel      *string          `json:"servedModel,omitempty"`
	UpstreamProvider *string          `json:"upstreamProvider,omitempty"`
	RequestID        *string          `json:"requestId,omitempty"`
	ResponseID       *string          `json:"responseId,omitempty"`
	Stop             *string          `json:"stop,omitempty"`
	Tier             *string          `json:"tier,omitempty"`
	ErrorType        *string          `json:"errorType,omitempty"`
	Credits          *float64         `json:"credits,omitempty"`
	Cost             *ExecutionCost   `json:"cost,omitempty"`
}

func (totals *Usage) Clone() *Usage {
	if totals == nil {
		return nil
	}
	copy := *totals
	copy.Facts = totals.Facts.Clone()
	return &copy
}

func cloneFact[Value any](value *Value) *Value {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (counts ExecutionCounts) Clone() ExecutionCounts {
	counts.InputTokens = cloneFact(counts.InputTokens)
	counts.OutputTokens = cloneFact(counts.OutputTokens)
	counts.CachedTokens = cloneFact(counts.CachedTokens)
	counts.CacheWriteTokens = cloneFact(counts.CacheWriteTokens)
	counts.CacheWrite1hTokens = cloneFact(counts.CacheWrite1hTokens)
	counts.ReasoningTokens = cloneFact(counts.ReasoningTokens)
	return counts
}

func (facts *ExecutionUsageFacts) Clone() *ExecutionUsageFacts {
	if facts == nil {
		return nil
	}
	copy := *facts
	copy.ExecutionCounts = facts.ExecutionCounts.Clone()
	copy.CacheUnknown = cloneFact(facts.CacheUnknown)
	if facts.Deferred != nil {
		deferred := facts.Deferred.Clone()
		copy.Deferred = &deferred
	}
	copy.ServedModel, copy.UpstreamProvider = cloneFact(facts.ServedModel), cloneFact(facts.UpstreamProvider)
	copy.RequestID, copy.ResponseID = cloneFact(facts.RequestID), cloneFact(facts.ResponseID)
	copy.Stop, copy.Tier, copy.ErrorType = cloneFact(facts.Stop), cloneFact(facts.Tier), cloneFact(facts.ErrorType)
	copy.Credits, copy.Cost = cloneFact(facts.Credits), cloneFact(facts.Cost)
	return &copy
}

func (counts ExecutionCounts) valid() bool {
	for _, count := range []*int64{counts.InputTokens, counts.OutputTokens, counts.CachedTokens,
		counts.CacheWriteTokens, counts.CacheWrite1hTokens, counts.ReasoningTokens} {
		if count != nil && (*count < 0 || *count > 1<<53-1 || int64(int(*count)) != *count) {
			return false
		}
	}
	return counts.CacheWrite1hTokens == nil || counts.CacheWriteTokens != nil &&
		*counts.CacheWrite1hTokens <= *counts.CacheWriteTokens
}

func ValidateExecutionUsage(manifest Manifest, totals *Usage) error {
	if totals == nil {
		return nil
	}
	for _, count := range []int64{totals.InputTokens, totals.OutputTokens, totals.CachedTokens, totals.CacheWriteTokens} {
		if count < 0 || count > 1<<53-1 || int64(int(count)) != count {
			return errors.New("invalid execution usage totals")
		}
	}
	if !validExecutionAmount(totals.Cost) || !validExecutionText(totals.Currency, 16) {
		return errors.New("invalid execution usage cost")
	}
	facts := totals.Facts
	if facts == nil {
		return nil
	}
	if manifest.Contract.Version.Minor < 7 || totals.InputTokens != 0 || totals.OutputTokens != 0 ||
		totals.CachedTokens != 0 || totals.CacheWriteTokens != 0 || totals.Cost != 0 || totals.Currency != "" {
		return errors.New("sparse execution usage requires SDK API 1.7 and a distinct facts envelope")
	}
	if !facts.ExecutionCounts.valid() || facts.Deferred != nil && !facts.Deferred.valid() ||
		facts.Credits != nil && !validExecutionAmount(*facts.Credits) {
		return errors.New("invalid sparse execution usage totals")
	}
	for _, text := range []*string{facts.ServedModel, facts.UpstreamProvider, facts.RequestID, facts.ResponseID,
		facts.Stop, facts.Tier, facts.ErrorType} {
		if text != nil && !validExecutionText(*text, 300) {
			return errors.New("invalid execution usage metadata")
		}
	}
	if facts.Cost != nil && (!validExecutionAmount(facts.Cost.Amount) || facts.Cost.Currency == "" ||
		!validExecutionText(facts.Cost.Currency, 16)) {
		return errors.New("invalid execution usage monetary amount")
	}
	return nil
}

func validExecutionAmount(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func validExecutionText(value string, limit int) bool {
	return len(value) <= limit && !strings.ContainsFunc(value, unicode.IsControl)
}
