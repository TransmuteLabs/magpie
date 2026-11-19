package requestpolicy

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Window applicability is exact model identity, as in SDK UsageWindow. A window
// ID is a label inside one selected account, never authority over an egress IP
// or unrelated accounts. Multiple applicable windows constrain time together.
type Window struct {
	ID           string   `json:"id"`
	Models       []string `json:"models,omitempty"`
	NotModels    []string `json:"notModels,omitempty"`
	RetryAfterMS *int64   `json:"retryAfterMs,omitempty"`
	ResetAt      string   `json:"resetAt,omitempty"`
}

func (window Window) Applies(model string) bool {
	return (len(window.Models) == 0 || slices.Contains(window.Models, model)) && !slices.Contains(window.NotModels, model)
}
func (window Window) Validate() error {
	if window.ID == "" || len(window.ID) > 128 || strings.ContainsFunc(window.ID, unicode.IsControl) || len(window.Models) > 1024 || len(window.NotModels) > 1024 || len(window.Models) > 0 && len(window.NotModels) > 0 {
		return errors.New("invalid refusal window")
	}
	for _, models := range [][]string{window.Models, window.NotModels} {
		seen := map[string]bool{}
		for _, model := range models {
			if model == "" || len(model) > 300 || seen[model] || strings.ContainsFunc(model, func(character rune) bool { return unicode.IsSpace(character) || unicode.IsControl(character) }) {
				return errors.New("invalid refusal model selector")
			}
			seen[model] = true
		}
	}
	return (Facts{Status: 200, Operation: "provider.execute", Phase: "response", RetryAfterMS: window.RetryAfterMS, ResetAt: window.ResetAt}).Validate()
}

// Refusal is a final host classification, not an intermediate callback result.
// The provider bridge constructs it only after not-started publication checks.
type Refusal struct {
	Classification
	Step                                 string
	Module, Provider, Account, Operation string
	Generation, Attempt                  uint64
	ResponseNotBefore                    time.Time
	Status                               int
	Observation                          Observation
	Model                                string
	NoEffect                             bool
	Handoff                              bool
	ObservedAt                           time.Time
	NextAllowed                          time.Time
	Windows                              []Window
	bounds                               retryBounds
	responseBounds                       retryBounds
}

func (refusal *Refusal) SetTiming(facts Facts) {
	refusal.responseBounds = retryBounds{}
	if facts.Validate() == nil && facts.Complete {
		refusal.responseBounds = retryBoundsFor(facts, refusal.ObservedAt)
	}
	refusal.bounds = refusal.responseBounds
	for _, window := range refusal.Windows {
		refusal.bounds = refusal.bounds.merge(retryBoundsFor(Facts{RetryAfterMS: window.RetryAfterMS, ResetAt: window.ResetAt}, refusal.ObservedAt))
	}
	refusal.ResponseNotBefore = refusal.responseBounds.nextAllowed(refusal.ObservedAt)
	refusal.NextAllowed = refusal.bounds.nextAllowed(refusal.ObservedAt)
}

func (refusal Refusal) Facts(now time.Time) Facts {
	bounds := refusal.bounds
	if bounds.empty() && !refusal.NextAllowed.IsZero() {
		bounds.resetAt = refusal.NextAllowed.UTC()
	}
	facts := Facts{Status: refusal.Status, Operation: "provider.execute", Phase: "response", Complete: true, bounds: &bounds}
	if next := bounds.nextAllowed(now); !next.IsZero() {
		facts.ResetAt = next.Format(time.RFC3339Nano)
	}
	return facts
}

type availabilityWindow struct {
	window  Window
	refusal Refusal
	bounds  retryBounds
}

// Availability is a bounded host cache of proved times. Unknown resets are not
// persisted, guessed or copied from a different quota window.
type Availability struct {
	mu      sync.Mutex
	entries map[string]map[string]availabilityWindow
}

func (availability *Availability) Remember(identity string, refusal Refusal) {
	if !refusal.NoEffect || refusal.Scope == "request" || refusal.Scope == "unknown" {
		return
	}
	var observations []availabilityWindow
	for _, window := range refusal.Windows {
		observations = append(observations, availabilityWindow{
			window: window,
			bounds: retryBoundsFor(Facts{RetryAfterMS: window.RetryAfterMS, ResetAt: window.ResetAt}, refusal.ObservedAt),
		})
	}
	response := refusal.responseBounds
	if response.empty() && !refusal.ResponseNotBefore.IsZero() {
		response.resetAt = refusal.ResponseNotBefore.UTC()
	}
	if len(observations) > 0 && !response.empty() {
		observations = append(observations, availabilityWindow{
			window: Window{ID: "response." + refusal.Rule, Models: []string{refusal.Model}, ResetAt: refusal.ResponseNotBefore.UTC().Format(time.RFC3339Nano)},
			bounds: response,
		})
	}
	if len(observations) == 0 {
		window := Window{ID: refusal.Rule}
		if refusal.Scope == "model" {
			window.Models = []string{refusal.Model}
		}
		bounds := refusal.bounds
		if bounds.empty() {
			bounds = response
		}
		if !refusal.NextAllowed.IsZero() {
			window.ResetAt = refusal.NextAllowed.UTC().Format(time.RFC3339Nano)
			if bounds.empty() {
				bounds.resetAt = refusal.NextAllowed.UTC()
			}
		}
		observations = []availabilityWindow{{window: window, bounds: bounds}}
	}
	availability.mu.Lock()
	defer availability.mu.Unlock()
	if availability.entries == nil {
		availability.entries = map[string]map[string]availabilityWindow{}
	}
	// Remove expired observations before admitting a new identity. No guessed TTL
	// turns a still-exhausted window into a claim that it has reset.
	for key, entries := range availability.entries {
		for id, entry := range entries {
			if entry.bounds.minimum(refusal.ObservedAt) <= 0 {
				delete(entries, id)
			}
		}
		if len(entries) == 0 {
			delete(availability.entries, key)
		}
	}
	if len(availability.entries) >= 4096 && availability.entries[identity] == nil {
		return
	}
	for _, observation := range observations {
		window, bounds := observation.window, observation.bounds
		if bounds.minimum(refusal.ObservedAt) <= 0 {
			continue
		}
		if availability.entries[identity] == nil {
			availability.entries[identity] = map[string]availabilityWindow{}
		}
		previous, exists := availability.entries[identity][window.ID]
		if !exists && len(availability.entries[identity]) >= 64 {
			continue
		}
		if exists {
			bounds = previous.bounds.merge(bounds)
			if bounds == previous.bounds {
				continue
			}
		}
		window.Models = slices.Clone(window.Models)
		window.NotModels = slices.Clone(window.NotModels)
		snapshot := refusal
		snapshot.Windows = nil
		availability.entries[identity][window.ID] = availabilityWindow{window: window, refusal: snapshot, bounds: bounds}
	}
}
func (availability *Availability) Blocked(identity, model string, now time.Time) (Refusal, bool) {
	availability.mu.Lock()
	defer availability.mu.Unlock()
	var result Refusal
	var windows []Window
	var bounds retryBounds
	var longest time.Duration
	blocked := false
	for id, entry := range availability.entries[identity] {
		minimum := entry.bounds.minimum(now)
		if minimum <= 0 {
			delete(availability.entries[identity], id)
			continue
		}
		if !entry.window.Applies(model) {
			continue
		}
		windows = append(windows, entry.window)
		bounds = bounds.merge(entry.bounds)
		if !blocked || minimum > longest {
			result = entry.refusal
			longest = minimum
		}
		blocked = true
	}
	result.Model = model
	result.Windows = windows
	result.bounds = bounds
	result.NextAllowed = bounds.nextAllowed(now)
	return result, blocked
}
