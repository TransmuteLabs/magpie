package memory

import (
	"errors"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/memembed"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// Query is what a search asks for. Scopes are the scopes searched (none:
// every scope), Kinds and Tags narrow it (a memory has one of the kinds
// and every tag), Status is the state looked in ("": active).
type Query = sdk.MemoryQuery

// Hit is a memory a search found, with how well it matched.
type Hit = sdk.MemoryHit

var linkedSemantic = memembed.Bind(SemanticIndexSnapshot)

func Semantic() sdk.MemorySemanticService { return linkedSemantic }

// SemanticIndexSnapshot returns the Memory records eligible for background
// indexing, newest first.
func SemanticIndexSnapshot() ([]sdk.MemorySemanticRecord, error) {
	snapshot, err := view()
	if err != nil {
		return nil, err
	}
	if len(snapshot.broken) != 0 {
		return nil, errors.New("Memory semantic index snapshot has unreadable records")
	}
	all := snapshot.Memories
	slices.SortStableFunc(all, func(a, b *Memory) int { return b.Updated.Compare(a.Updated) })
	var records []sdk.MemorySemanticRecord
	for _, item := range all {
		if item.Status == Active || item.Status == Candidate {
			records = append(records, semanticRecord(item))
		}
	}
	return records, nil
}

func semanticRecord(item *Memory) sdk.MemorySemanticRecord {
	return sdk.MemorySemanticRecord{ID: item.ID, Text: item.Text, Why: item.Why}
}

// A memory counts as meaning what was searched for at a cosine of Near
// or more, and Within of the nearest. Over 97 real memories and 16
// searches (half in Chinese, for memories in English), the memory sought
// was at a median of 0.47, the rest at a median of 0.16 and a 95th
// percentile of 0.36. Near alone let ~11 of the rest in a search; with
// Within, 12 of the 16 were found and ~4 of the rest came along, behind
// them, and the word ranking finds most of the four it misses.
var (
	Near   = 0.30
	Within = 0.15
)

// rrfK is reciprocal-rank fusion's constant: the word ranking and the
// meaning ranking each give a hit 1/(rrfK+rank).
const rrfK = 20

// Search finds memories matching q, best first: every word of the text
// counted by how rare it is among the memories, a memory holding the whole
// text ahead, pinned ones and a project's or agent's own a little ahead of
// the rest; with search by meaning on, the ones that mean the
// same in other words too, the two rankings fused. With no text, it lists
// them by kind and newest change. The hits are noted as used.
func Search(q Query) ([]Hit, error) {
	hits, err := Rank(q)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(q.Text) != "" {
		at := now()
		var ids []string
		for _, hit := range hits {
			if at.Sub(hit.Used) > time.Minute {
				ids = append(ids, hit.ID)
			}
		}
		touch(ids, at)
	}
	return hits, nil
}

// Rank is Search without noting the hits as used: for the Memory tab's
// search box, which the user types into.
func Rank(q Query) ([]Hit, error) {
	f, err := view()
	if err != nil {
		return nil, err
	}
	return search(f.Memories, q), nil
}

func search(all []*Memory, q Query) []Hit {
	status := q.Status
	if status == "" {
		status = Active
	}
	tags := CleanTags(q.Tags)
	pool := []*Memory{}
	for _, m := range all {
		if m.Status != status ||
			(len(q.Scopes) > 0 && !slices.Contains(q.Scopes, m.Scope)) ||
			(len(q.Kinds) > 0 && !slices.Contains(q.Kinds, m.Kind)) ||
			slices.ContainsFunc(tags, func(t string) bool { return !slices.Contains(m.Tags, t) }) {
			continue
		}
		pool = append(pool, m)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	text := strings.TrimSpace(q.Text)
	if text == "" {
		slices.SortStableFunc(pool, func(a, b *Memory) int {
			if a.Pinned != b.Pinned {
				if a.Pinned {
					return -1
				}
				return 1
			}
			if d := kindRank(a.Kind) - kindRank(b.Kind); d != 0 {
				return d
			}
			return b.Updated.Compare(a.Updated)
		})
		out := []Hit{}
		for _, m := range pool[:min(limit, len(pool))] {
			out = append(out, Hit{MemoryRecord: m})
		}
		return out
	}
	out := wordHits(pool, text)
	if ranker := linkedSemantic.Ranker; len(pool) != 0 && ranker.Ready() {
		records := make([]sdk.MemorySemanticRecord, len(pool))
		for index, item := range pool {
			records[index] = semanticRecord(item)
		}
		if scores := ranker.Rank(sdk.MemorySemanticRequest{Query: text, Records: records}); scores != nil {
			out = fuse(pool, out, scores)
		}
	}
	return out[:min(limit, len(out))]
}

// wordHits are pool's memories holding words of text, best first.
func wordHits(pool []*Memory, text string) []Hit {
	words := Words(text)
	if len(words) == 0 {
		return []Hit{}
	}
	type term struct {
		frequency int
		lastDoc   int
		weight    float64
	}
	byWord := make(map[string]int, len(words))
	order := make([]int, len(words))
	terms := make([]term, 0, len(words))
	for i, word := range words {
		slot, exists := byWord[word]
		if !exists {
			slot = len(terms)
			byWord[word] = slot
			terms = append(terms, term{})
		}
		order[i] = slot
	}

	offsets := make([]int, len(pool)+1)
	var matches []int
	for i, m := range pool {
		doc := i + 1
		visitWordBytes(m.Text+" "+m.Why+" "+strings.Join(m.Tags, " ")+" "+m.Kind, func(word []byte) {
			if slot, exists := byWord[string(word)]; exists && terms[slot].lastDoc != doc {
				terms[slot].lastDoc = doc
				terms[slot].frequency++
				matches = append(matches, slot)
			}
		})
		offsets[doc] = len(matches)
	}
	for i := range terms {
		terms[i].lastDoc = 0
		terms[i].weight = 1 + math.Log(float64(len(pool)+1)/float64(terms[i].frequency+1))
	}

	whole := norm(text)
	out := []Hit{}
	for i, m := range pool {
		doc := i + 1
		if offsets[i] == offsets[doc] {
			continue
		}
		for _, slot := range matches[offsets[i]:offsets[doc]] {
			terms[slot].lastDoc = doc
		}
		score, matched := 0.0, 0
		for _, slot := range order {
			if terms[slot].lastDoc == doc {
				matched++
				score += terms[slot].weight
			}
		}
		// most of the words, not one common one, make a match
		score *= float64(matched) / float64(len(words))
		if strings.Contains(norm(m.Text), whole) {
			score += 2
		}
		if m.Pinned {
			score *= 1.2
		}
		if m.Scope != Global {
			score *= 1.1
		}
		out = append(out, Hit{MemoryRecord: m, Score: math.Round(score*100) / 100})
	}
	slices.SortStableFunc(out, func(a, b Hit) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return b.Updated.Compare(a.Updated)
	})
	return out
}

// fuse is the word hits and the memories near text in meaning (sims, at
// Near or more and Within of the nearest) as one ranking: each list gives
// a hit 1/(rrfK+rank), and a pinned or a project's or agent's own memory
// is a little ahead, as in wordHits. A memory found both ways is ahead of
// one found either way, and of two that score the same, the nearer in
// meaning: the first word hit may hold one common word of the search.
func fuse(pool []*Memory, words []Hit, sims map[string]float64) []Hit {
	bar := Near
	for _, m := range pool {
		if score, ok := sims[m.ID]; ok {
			bar = max(bar, score-Within)
		}
	}
	near := []*Memory{}
	for _, m := range pool {
		if s, ok := sims[m.ID]; ok && s >= bar {
			near = append(near, m)
		}
	}
	slices.SortStableFunc(near, func(a, b *Memory) int {
		if sims[a.ID] != sims[b.ID] {
			if sims[a.ID] > sims[b.ID] {
				return -1
			}
			return 1
		}
		return b.Updated.Compare(a.Updated)
	})
	score := map[string]float64{}
	byID := map[string]*Memory{}
	for i, h := range words {
		score[h.ID] += 1 / float64(rrfK+i+1)
		byID[h.ID] = h.MemoryRecord
	}
	for i, m := range near {
		score[m.ID] += 1 / float64(rrfK+i+1)
		byID[m.ID] = m
	}
	out := make([]Hit, 0, len(score))
	for id, s := range score {
		m := byID[id]
		if m.Pinned {
			s *= 1.2
		}
		if m.Scope != Global {
			s *= 1.1
		}
		out = append(out, Hit{MemoryRecord: m, Score: math.Round(s*rrfK*100) / 100})
	}
	slices.SortStableFunc(out, func(a, b Hit) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		if sa, sb := sims[a.ID], sims[b.ID]; sa != sb {
			if sa > sb {
				return -1
			}
			return 1
		}
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func kindRank(k string) int {
	if i := slices.Index(Kinds, k); i >= 0 {
		return i
	}
	return len(Kinds)
}

// Words are the words of s as searched: letters and digits in lower case,
// and a run of Chinese, Japanese or Korean characters as each character
// and each pair of them, since those are written without spaces.
func Words(s string) []string {
	out := []string{}
	visitWordBytes(s, func(word []byte) {
		out = append(out, string(word))
	})
	return out
}

func visitWordBytes(s string, visit func([]byte)) {
	var word []byte
	var cjk [2 * utf8.UTFMax]byte
	var previousCJK rune
	flushWord := func() {
		if len(word) > 0 {
			visit(word)
			word = word[:0]
		}
	}
	for _, r := range s {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			flushWord()
			if previousCJK != 0 {
				prefix := utf8.AppendRune(cjk[:0], previousCJK)
				visit(utf8.AppendRune(prefix, r))
			}
			visit(utf8.AppendRune(cjk[:0], r))
			previousCJK = r
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			previousCJK = 0
			word = utf8.AppendRune(word, unicode.ToLower(r))
		default:
			flushWord()
			previousCJK = 0
		}
	}
	flushWord()
}
