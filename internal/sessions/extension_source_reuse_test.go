package sessions

import (
	"os"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSourceRecordReuseKeepsFirstRootMatch(t *testing.T) {
	root := t.TempDir()
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	selected, err := opened.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	same, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	grant := sourceGrant{reference: "scope", enableGeneration: "current", info: selected}
	record := func(grant sourceGrant, root os.FileInfo, path string) *sessionSourceRecord {
		grant.info = root
		return &sessionSourceRecord{grant: grant, request: sdk.SessionSourceReadRequest{
			Locator: sdk.SessionSourceLocator{Path: path},
		}}
	}
	old := grant
	old.enableGeneration = "previous"
	first := record(grant, same, "match.jsonl")
	unique := record(grant, same, "unique.jsonl")
	reuse := sourceRecordReuse{records: []*sessionSourceRecord{
		record(old, same, "match.jsonl"),
		record(grant, other, "match.jsonl"),
		first,
		record(grant, same, "match.jsonl"),
		unique,
		record(grant, other, "other.jsonl"),
	}}
	if got := reuse.find(grant, "match.jsonl"); got != first {
		t.Fatal("duplicate locator displaced the first record from the selected root")
	}
	if got := reuse.find(grant, "unique.jsonl"); got != unique {
		t.Fatal("matching locator did not reuse its record")
	}
	for _, path := range []string{"other.jsonl", "absent.jsonl"} {
		if got := reuse.find(grant, path); got != nil {
			t.Errorf("%s reused an unrelated record", path)
		}
	}
}
