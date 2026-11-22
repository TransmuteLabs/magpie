package memory

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLexicalMembershipScoresAndOrder(t *testing.T) {
	pool := []*Memory{
		{ID: "older", Text: "other topic", Why: "echo", Tags: []string{"世界"}, Kind: Reference, Scope: Global, Updated: time.Unix(2, 0)},
		{ID: "literal", Text: "ECHO echo 世界。", Scope: Global, Updated: time.Unix(1, 0)},
		{ID: "fresh", Text: "unrelated text", Tags: []string{"echo", "世界", "reference"}, Scope: Global, Updated: time.Unix(3, 0)},
		{ID: "rare", Text: "café", Scope: Global, Updated: time.Unix(4, 0)},
	}
	type result struct {
		id    string
		score float64
	}
	for _, fixture := range []struct {
		name string
		text string
		want []result
	}{
		{
			name: "repeat_phrase_and_cjk",
			text: "echo echo 世界",
			want: []result{{"literal", 8.12}, {"fresh", 6.12}, {"older", 6.12}},
		},
		{
			name: "rare_casefolded_term_with_other_hits",
			text: "CAFÉ echo",
			want: []result{{"rare", .96}, {"fresh", .61}, {"older", .61}, {"literal", .61}},
		},
		{
			name: "tag_and_kind",
			text: "reference",
			want: []result{{"fresh", 1.51}, {"older", 1.51}},
		},
		{name: "no_words", text: "!?"},
		{name: "no_hits", text: "missing"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var got []result
			for _, hit := range wordHits(pool, fixture.text) {
				got = append(got, result{hit.ID, hit.Score})
			}
			if !slices.Equal(got, fixture.want) {
				t.Fatalf("got %+v, want %+v", got, fixture.want)
			}
		})
	}
}

func TestLexicalMembershipManyQueryTerms(t *testing.T) {
	var query []string
	for term := range 67 {
		query = append(query, fmt.Sprintf("q%02d", term))
	}
	first := &Memory{ID: "first", Tags: []string{"q00", "q66"}, Pinned: true, Scope: "project:fixture"}
	second := &Memory{ID: "second", Why: "q66 q66", Scope: Global}
	got := wordHits([]*Memory{first, second}, strings.Join(query, " "))
	want := []Hit{{MemoryRecord: first, Score: .09}, {MemoryRecord: second, Score: .01}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLexicalMembershipAllocationBudget(t *testing.T) {
	var irrelevant []string
	for term := range 64 {
		irrelevant = append(irrelevant, fmt.Sprintf("irrelevant%02d", term))
	}
	text := strings.Join(irrelevant, " ")
	pool := make([]*Memory, 64)
	for index := range pool {
		record := &Memory{Text: text, Scope: Global}
		if index%8 == 0 {
			record.Tags = []string{"match"}
		}
		pool[index] = record
	}
	tokenizer := testing.AllocsPerRun(5, func() {
		for _, record := range pool {
			if len(Words(record.Text+" "+record.Why+" "+strings.Join(record.Tags, " ")+" "+record.Kind)) < len(irrelevant) {
				t.Fatal("fixture lost document words")
			}
		}
	})
	search := testing.AllocsPerRun(5, func() {
		if got := len(wordHits(pool, "match")); got != len(pool)/8 {
			t.Fatalf("got %d matches, want %d", got, len(pool)/8)
		}
	})
	if limit := tokenizer + float64(len(pool)); search > limit {
		t.Fatalf("search allocated %.0f times, want at most %.0f (tokenizer %.0f)", search, limit, tokenizer)
	}
}

func BenchmarkLexicalMembership(b *testing.B) {
	for _, count := range []int{192, 768} {
		b.Run(fmt.Sprintf("records=%d", count), func(b *testing.B) {
			pool := make([]*Memory, count)
			for index := range pool {
				text := strings.Repeat("Release validation and local routing before gateway changes. ", 12)
				if index%8 == 0 {
					text += "ECHO echo 世界"
				}
				pool[index] = &Memory{ID: fmt.Sprintf("fixture-%d", index), Text: text, Scope: Global}
			}
			b.ReportAllocs()
			for b.Loop() {
				if hits := wordHits(pool, "echo echo 世界"); len(hits) != count/8 {
					b.Fatalf("got %d matches, want %d", len(hits), count/8)
				}
			}
		})
	}
}
