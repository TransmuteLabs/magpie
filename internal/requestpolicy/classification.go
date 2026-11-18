// Package requestpolicy owns bounded retry classification, budgets and delays.
// Provider adapters supply facts and evidence, never a second scheduler.
package requestpolicy

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Class string

const (
	Unknown Class = "unknown"
	Rate    Class = "transient-rate"
	Quota   Class = "quota"
	Auth    Class = "auth-refusal"
	Request Class = "request-refusal"
	Content Class = "content-refusal"
	Outage  Class = "transient-outage"
)

// Facts are extracted from a bounded response. Code is an actual machine-code
// field, never a digit sequence found in a message or a request identifier.
type Facts struct {
	Status       int               `json:"status"`
	Code         string            `json:"code,omitempty"`
	Type         string            `json:"type,omitempty"`
	Message      string            `json:"message,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	RetryAfterMS *int64            `json:"retryAfterMs,omitempty"`
	ResetAt      string            `json:"resetAt,omitempty"`
	Operation    string            `json:"operation"`
	Phase        string            `json:"phase"`
	Complete     bool              `json:"complete"`
	bounds       *retryBounds
}

type Classification struct {
	Class Class  `json:"class"`
	Scope string `json:"scope"`
	Rule  string `json:"rule"`
}

// Rule predicates are ANDed. Within each policy the first matching rule wins;
// operator provider rules precede global rules, then the adapter's default.
type Rule struct {
	ID        string            `json:"id"`
	Status    []int             `json:"status,omitempty"`
	Code      string            `json:"code,omitempty"`
	Type      string            `json:"type,omitempty"`
	Contains  string            `json:"contains,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Operation string            `json:"operation,omitempty"`
	Phase     string            `json:"phase,omitempty"`
	Class     Class             `json:"class"`
	Scope     string            `json:"scope"`
}

func validClass(class Class) bool {
	switch class {
	case Unknown, Rate, Quota, Auth, Request, Content, Outage:
		return true
	}
	return false
}

func validScope(scope string) bool {
	return scope == "request" || scope == "model" || scope == "account" || scope == "unknown"
}

func (facts Facts) Validate() error {
	if facts.RetryAfterMS != nil && (*facts.RetryAfterMS < 0 || *facts.RetryAfterMS > int64(30*24*time.Hour/time.Millisecond)) {
		return fmt.Errorf("invalid retry duration")
	}
	if facts.ResetAt != "" {
		if len(facts.ResetAt) > 64 {
			return fmt.Errorf("invalid retry reset timestamp")
		}
		if _, err := time.Parse(time.RFC3339Nano, facts.ResetAt); err != nil {
			return fmt.Errorf("invalid retry reset timestamp")
		}
	}
	if facts.Status < 0 || facts.Status > 599 || facts.Status < 100 && facts.Phase == "response" || len(facts.Code) > 256 || len(facts.Type) > 128 || len(facts.Message) > 1024 ||
		len(facts.Operation) == 0 || len(facts.Operation) > 128 || len(facts.Phase) == 0 || len(facts.Phase) > 64 || len(facts.Headers) > 16 {
		return fmt.Errorf("invalid bounded request facts")
	}
	seen := map[string]bool{}
	for name, value := range facts.Headers {
		normalized := strings.ToLower(name)
		if name == "" || len(name) > 64 || len(value) > 300 || seen[normalized] || strings.ContainsAny(name, " :\t") || strings.ContainsFunc(name+value, unicode.IsControl) {
			return fmt.Errorf("invalid bounded response headers")
		}
		seen[normalized] = true
	}
	return nil
}

func header(headers map[string]string, key string) string {
	for name, value := range headers {
		if strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

func (rule Rule) matches(facts Facts) bool {
	if len(rule.Status) != 0 {
		found := false
		for _, status := range rule.Status {
			found = found || status == facts.Status
		}
		if !found {
			return false
		}
	}
	if rule.Code != "" && rule.Code != facts.Code || rule.Type != "" && rule.Type != facts.Type ||
		rule.Contains != "" && !strings.Contains(strings.ToLower(facts.Message), strings.ToLower(rule.Contains)) ||
		rule.Operation != "" && rule.Operation != facts.Operation || rule.Phase != "" && rule.Phase != facts.Phase {
		return false
	}
	for name, value := range rule.Headers {
		if header(facts.Headers, name) != value {
			return false
		}
	}
	return true
}

func (rule Rule) validate() error {
	if rule.ID == "" || len(rule.ID) > 64 || strings.ContainsFunc(rule.ID, unicode.IsControl) || !validClass(rule.Class) || !validScope(rule.Scope) || len(rule.Status) > 16 ||
		len(rule.Status) == 0 && rule.Code == "" && rule.Type == "" && rule.Contains == "" && len(rule.Headers) == 0 && rule.Operation == "" && rule.Phase == "" {
		return fmt.Errorf("invalid request classification rule")
	}
	for _, status := range rule.Status {
		if status < 100 || status > 599 {
			return fmt.Errorf("invalid rule HTTP status")
		}
	}
	facts := Facts{Status: 200, Code: rule.Code, Type: rule.Type, Message: rule.Contains, Headers: rule.Headers, Operation: rule.Operation, Phase: rule.Phase, Complete: true}
	if facts.Operation == "" {
		facts.Operation = "any"
	}
	if facts.Phase == "" {
		facts.Phase = "any"
	}
	return facts.Validate()
}

func (policy Policy) Classify(facts Facts, fallback Classification) Classification {
	unknown := Classification{Class: Unknown, Scope: "request", Rule: "unknown"}
	if facts.Validate() != nil || !facts.Complete {
		return unknown
	}
	for _, rule := range policy.Rules {
		if rule.matches(facts) {
			return Classification{rule.Class, rule.Scope, rule.ID}
		}
	}
	if !validClass(fallback.Class) || !validScope(fallback.Scope) || fallback.Rule == "" {
		return unknown
	}
	return fallback
}

// ClassifyVendor uses operator rules first, then stable vendor code/type rules,
// then vendor prose rules. The vendor controls order within those two groups;
// budget-specific prose must precede generic pace wording in its preset.
func (policy Policy) ClassifyVendor(facts Facts, rules []Rule) Classification {
	unknown := Classification{Class: Unknown, Scope: "request", Rule: "unknown"}
	if len(rules) > 64 || facts.Validate() != nil || !facts.Complete {
		return unknown
	}
	for _, rule := range rules {
		if rule.validate() != nil {
			return unknown
		}
	}
	for _, rule := range policy.Rules {
		if rule.matches(facts) {
			return Classification{rule.Class, rule.Scope, rule.ID}
		}
	}
	for _, stable := range []bool{true, false} {
		for _, rule := range rules {
			if (rule.Code != "" || rule.Type != "") == stable && rule.matches(facts) {
				return Classification{rule.Class, rule.Scope, rule.ID}
			}
		}
	}
	return unknown
}

// RetryAfter is a lower bound, not a cap. The decision rejects a delay which
// cannot fit the policy; it never shortens the server's requested wait.
func RetryAfter(headers map[string]string, now time.Time) time.Duration {
	return retryHeaderBounds(headers, now).minimum(now)
}

type retryBounds struct {
	elapsedUntil time.Time
	resetAt      time.Time
}

func (bounds retryBounds) empty() bool {
	return bounds.elapsedUntil.IsZero() && bounds.resetAt.IsZero()
}

func (bounds retryBounds) merge(other retryBounds) retryBounds {
	if other.elapsedUntil.After(bounds.elapsedUntil) {
		bounds.elapsedUntil = other.elapsedUntil
	}
	if other.resetAt.After(bounds.resetAt) {
		bounds.resetAt = other.resetAt
	}
	return bounds
}

func (bounds retryBounds) minimum(now time.Time) time.Duration {
	return max(0, bounds.elapsedUntil.Sub(now), bounds.resetAt.Sub(now.UTC()))
}

func (bounds retryBounds) nextAllowed(now time.Time) time.Time {
	if minimum := bounds.minimum(now); minimum > 0 {
		return now.Add(minimum).UTC()
	}
	return time.Time{}
}

func retryHeaderBounds(headers map[string]string, now time.Time) retryBounds {
	var bounds retryBounds
	var delay time.Duration
	value := strings.TrimSpace(header(headers, "Retry-After"))
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 && !math.IsNaN(seconds) {
		if seconds >= float64(math.MaxInt64)/float64(time.Second) {
			delay = time.Duration(math.MaxInt64)
		} else {
			delay = time.Duration(seconds * float64(time.Second))
		}
	}
	if at, err := http.ParseTime(value); err == nil {
		bounds.resetAt = at.UTC()
	}
	for name, value := range headers {
		name = strings.ToLower(name)
		if !strings.Contains(name, "ratelimit") || !strings.Contains(name, "reset") {
			continue
		}
		if at, err := time.Parse(time.RFC3339, value); err == nil {
			bounds = bounds.merge(retryBounds{resetAt: at.UTC()})
		} else if duration, err := time.ParseDuration(value); err == nil {
			delay = max(delay, duration)
		} else if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 1e9 {
			bounds = bounds.merge(retryBounds{resetAt: time.Unix(seconds, 0).UTC()})
		}
	}
	if delay > 0 {
		bounds.elapsedUntil = now.Add(delay)
	}
	return bounds
}

// RetryMinimum combines independent server hints. Adapters extract typed body
// hints; they never synthesize headers or ask a global prose parser for time.
func RetryMinimum(facts Facts, now time.Time) time.Duration {
	return retryBoundsFor(facts, now).minimum(now)
}

func retryBoundsFor(facts Facts, now time.Time) retryBounds {
	if facts.bounds != nil {
		return *facts.bounds
	}
	bounds := retryHeaderBounds(facts.Headers, now)
	if facts.RetryAfterMS != nil && *facts.RetryAfterMS > 0 {
		bounds = bounds.merge(retryBounds{elapsedUntil: now.Add(time.Duration(*facts.RetryAfterMS) * time.Millisecond)})
	}
	if at, err := time.Parse(time.RFC3339Nano, facts.ResetAt); err == nil {
		bounds = bounds.merge(retryBounds{resetAt: at.UTC()})
	}
	return bounds
}
