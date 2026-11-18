package usage

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"
)

func TestExecutionHashIncludesEvidenceAndIgnoresStorage(t *testing.T) {
	at := time.Unix(1800000000, 0)
	base := Record{Time: at, Agent: "codex", Model: "model", ResponseID: "response", UpstreamResponseID: "upstream",
		ReportedCost: &ReportedCost{Amount: 0.12, Currency: "EUR"}}
	chunk := func(record Record) *rowChunk {
		c := &rowChunk{}
		c.add(Row{Record: record}, "", 1, false)
		return c
	}
	c := chunk(base)
	want := hashedRows(c)
	for _, mutate := range []func(*Record){
		func(r *Record) { r.ResponseID = "other-response" },
		func(r *Record) { r.UpstreamResponseID = "other-upstream" },
		func(r *Record) { r.ReportedCost = &ReportedCost{Amount: 0.13, Currency: "EUR"} },
		func(r *Record) { r.ReportedCost = &ReportedCost{Amount: 0.12} },
		func(r *Record) { r.ReportedCost = nil },
	} {
		record := base
		mutate(&record)
		if hashedRows(chunk(record)) == want {
			t.Fatal("execution evidence did not change its digest", record)
		}
	}
	// Packing re-interns strings; selecting a row can also renumber its tuple.
	larger := chunk(Record{Time: at.Add(-time.Hour), ResponseID: "earlier", UpstreamResponseID: "earlier-upstream"})
	larger.add(Row{Record: base}, "", 1, false)
	for _, other := range []*rowChunk{c.pack().unpack(), larger.within(at, at.Add(time.Hour))} {
		if other == nil || hashedRows(other) != want {
			t.Fatal("storage representation changed the execution digest")
		}
	}
	base.UpstreamResponseID, base.ReportedCost = "", nil
	plain := chunk(base)
	tagged := chunk(base)
	tagged.executionText = append(tagged.executionText, [3]uint32{tagged.Rows[0].Varied[rowResponseAt], 0, 0})
	tagged.Rows[0].Varied[rowResponseAt] = rowExecutionTag
	if hashedRows(plain) != hashedRows(tagged) {
		t.Fatal("equivalent tagged and direct response identities differ")
	}
}

func TestExecutionHashInvalidatesOldSourceDigests(t *testing.T) {
	data := encodeSourceDigests("same-row-code", 1, nil,
		map[uint64][sha256.Size]byte{1: {1}}, map[uint64]struct{}{1: {}})
	if decodeSourceDigests(data, "same-row-code") == nil {
		t.Fatal("current digest fixture is invalid")
	}
	old := bytes.Replace(data, []byte(sourceDigestsMagic), []byte("magpie-heat-digests-2\n"), 1)
	if decodeSourceDigests(old, "same-row-code") != nil {
		t.Fatal("accepted a digest that omitted execution evidence")
	}
}
