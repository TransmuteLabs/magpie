package redact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"regexp"
	"slices"
	"sort"
)

// DeclarativeRule is a checked supplemental rule from the installed SDK
// catalog. It emits an ordinary reversible placeholder for its whole match.
type DeclarativeRule struct {
	Kind    string
	Pattern *regexp.Regexp
}

// DeclarativeScope identifies the installed selection that issued a placeholder.
type DeclarativeScope struct {
	Selection, Revision, EnableGeneration string
}

type declarativeIssuance struct {
	scope DeclarativeScope
	at    uint64
}

const (
	byExtension              = "extension"
	maxDeclarativeMatches    = 4096
	maxDeclarativeValueBytes = 4096
)

// declarativeTag keeps the native wire grammar, with a domain and
// length-delimited identity independent of the plaintext boundaries.
func declarativeTag(kind, value string, scope DeclarativeScope) string {
	h := hmac.New(sha256.New, key())
	h.Write([]byte("\x00magpie-sdk-masking-v1\x00"))
	var length [8]byte
	for _, part := range [...]string{kind, scope.Selection, scope.Revision, scope.EnableGeneration, value} {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		h.Write(length[:])
		io.WriteString(h, part)
	}
	return formatTag(kind, h.Sum(nil))
}

func declarativePlaceholder(kind, value string, scope DeclarativeScope) string {
	p := declarativeTag(kind, value, scope)
	mu.Lock()
	defer mu.Unlock()
	if current, exists := values[p]; exists {
		if owner, sdk := declarativeValues[p]; !sdk || owner.scope != scope || current != value {
			panic("declarative placeholder identity collision")
		}
	} else {
		makeRoom()
		values[p] = value
		declarativeValues[p] = declarativeIssuance{scope: scope, at: clock + 1}
		kinds[kind] = true
	}
	touch(p)
	return p
}

// existingDeclarativePlaceholder never reissues a proof evicted between
// signed-span detection and application.
func existingDeclarativePlaceholder(kind, value string, scope DeclarativeScope, before uint64) string {
	p := declarativeTag(kind, value, scope)
	mu.Lock()
	defer mu.Unlock()
	if !declarativeIssued(p, value, scope, before) {
		panic("declarative placeholder proof expired during masking")
	}
	touch(p)
	return p
}

// declarativeIssued is called with mu held.
func declarativeIssued(p, value string, scope DeclarativeScope, before uint64) bool {
	owner, sdk := declarativeValues[p]
	return sdk && owner.scope == scope && owner.at <= before && values[p] == value
}

// extensionSpans leaves any value already classified by the native maskers
// alone. This preserves the user's actions for that value's original kind.
func extensionSpans(s string, rules []DeclarativeRule, native []span) []span {
	if len(rules) == 0 {
		return nil
	}
	protected := slices.Clone(native)
	sort.Slice(protected, func(i, j int) bool { return protected[i].start < protected[j].start })
	var out []span
	for _, rule := range rules {
		next, end := 0, 0
		matches := rule.Pattern.FindAllStringIndex(s, maxDeclarativeMatches+1)
		if len(matches) > maxDeclarativeMatches {
			panic("declarative masking match limit exceeded")
		}
		for _, hit := range matches {
			for next < len(protected) && protected[next].start < hit[1] {
				end = max(end, protected[next].end)
				next++
			}
			if end <= hit[0] {
				if hit[1]-hit[0] > maxDeclarativeValueBytes || len(out) == maxDeclarativeMatches {
					panic("declarative masking match limit exceeded")
				}
				out = append(out, span{start: hit[0], end: hit[1], kind: rule.Kind, by: byExtension})
			}
		}
	}
	return out
}

// knownExtensionSpans repairs a signed echo only when this exact rule
// already supplied the placeholder. New matches in vendor-signed text
// must remain as written.
func knownExtensionSpans(s string, rules []DeclarativeRule, native []span, scope DeclarativeScope, before uint64) []span {
	found := extensionSpans(s, rules, native)
	return slices.DeleteFunc(found, func(hit span) bool {
		value := s[hit.start:hit.end]
		p := declarativeTag(hit.kind, value, scope)
		mu.RLock()
		issued := declarativeIssued(p, value, scope, before)
		mu.RUnlock()
		return !issued
	})
}
