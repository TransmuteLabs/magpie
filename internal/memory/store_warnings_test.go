package memory

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const malformedNoteA = "---\nkind: [fact\n---\ndamaged"
const malformedNoteB = "---\nkind: [rule\n---\ndamaged"

func memoryWarningLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	fresh(t)
	if err := os.Mkdir(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	output := new(bytes.Buffer)
	previous := log.Writer()
	log.SetOutput(output)
	t.Cleanup(func() { log.SetOutput(previous) })
	return output
}

func readForWarnings(t *testing.T, want int) {
	t.Helper()
	records, _, err := All()
	if err != nil || len(records) != want {
		t.Fatalf("Memory listing returned %d records, want %d: %v", len(records), want, err)
	}
}

func expectMemoryWarnings(t *testing.T, output *bytes.Buffer, individual, overflow int) string {
	t.Helper()
	contents := output.String()
	gotIndividual := strings.Count(contents, "can't be read, left as it is")
	gotOverflow := strings.Count(contents, "memory: too many malformed memory files")
	if gotIndividual != individual || gotOverflow != overflow {
		t.Errorf("warnings = %d individual, %d overflow; want %d, %d",
			gotIndividual, gotOverflow, individual, overflow)
	}
	output.Reset()
	return contents
}

func removeWarningFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryMalformedWarnings(t *testing.T) {
	t.Run("latest_revision_repair_and_absence", func(t *testing.T) {
		output := memoryWarningLog(t)
		path := mdOf("noted-revision")
		for _, revision := range []string{malformedNoteA, malformedNoteB, malformedNoteA} {
			handWrite(t, path, revision)
			readForWarnings(t, 0)
			expectMemoryWarnings(t, output, 1, 0)
			readForWarnings(t, 0)
			expectMemoryWarnings(t, output, 0, 0)
		}
		handWrite(t, path, "repaired text")
		readForWarnings(t, 1)
		expectMemoryWarnings(t, output, 0, 0)
		handWrite(t, path, malformedNoteA)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 1, 0)
		removeWarningFixture(t, path)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 0, 0)
		handWrite(t, path, malformedNoteA)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 1, 0)
	})
	t.Run("unknown_read", func(t *testing.T) {
		output := memoryWarningLog(t)
		path := mdOf("noted-unknown")
		handWrite(t, path, malformedNoteA)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 1, 0)
		removeWarningFixture(t, path)
		if err := os.Symlink(t.TempDir(), path); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("directory symlink unavailable: %v", err)
			}
			t.Fatal(err)
		}
		if _, err := os.ReadFile(path); err == nil || os.IsNotExist(err) {
			t.Fatalf("fixture requires an unknown read result: %v", err)
		}
		touch([]string{"noted-unknown"}, now())
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 0, 0)
		removeWarningFixture(t, path)
		handWrite(t, path, malformedNoteA)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 0, 0)
	})
	t.Run("directory_identity", func(t *testing.T) {
		output := memoryWarningLog(t)
		handWrite(t, mdOf("noted-directory"), malformedNoteA)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 1, 0)

		dir := Dir()
		if err := os.Rename(dir, filepath.Join(filepath.Dir(dir), "old-memory")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		handWrite(t, mdOf("noted-directory"), malformedNoteA)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 1, 0)

		fresh(t)
		if err := os.Mkdir(Dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		handWrite(t, mdOf("noted-directory"), malformedNoteA)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 1, 0)
	})
	t.Run("touch_settles_only_its_record", func(t *testing.T) {
		output := memoryWarningLog(t)
		record, sibling := "noted-touch", "noted-sibling"
		handWrite(t, mdOf(record), malformedNoteA)
		handWrite(t, mdOf(sibling), malformedNoteA)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 2, 0)

		handWrite(t, mdOf(record), malformedNoteB)
		touch([]string{record}, now())
		expectMemoryWarnings(t, output, 1, 0)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 0, 0)

		handWrite(t, mdOf(record), "repaired before usage recheck")
		touch([]string{record}, now())
		expectMemoryWarnings(t, output, 0, 0)
		metadata, _, err := readMeta()
		if err != nil {
			t.Fatal(err)
		}
		if use := metadata.Used[record]; use.Uses != 1 || !use.At.Equal(now()) {
			t.Fatalf("usage recheck returned %+v", use)
		}

		handWrite(t, mdOf(record), malformedNoteA)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 1, 0)
		removeWarningFixture(t, mdOf(record))
		touch([]string{record}, now().Add(2*time.Minute))
		handWrite(t, mdOf(record), malformedNoteA)
		readForWarnings(t, 0)
		expectMemoryWarnings(t, output, 1, 0)
	})
}

func TestMemoryMalformedWarningSaturation(t *testing.T) {
	output := memoryWarningLog(t)
	const capacity = 4096
	path := func(index int) string { return mdOf(fmt.Sprintf("noted-cap-%04d", index)) }
	for index := range capacity + 2 {
		if err := os.WriteFile(path(index), []byte(malformedNoteA), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readForWarnings(t, 0)
	first := expectMemoryWarnings(t, output, capacity, 1)
	if strings.Contains(first, "noted-cap-4096.md") || strings.Contains(first, "noted-cap-4097.md") {
		t.Error("warned individually about an untracked file")
	}
	readForWarnings(t, 0)
	expectMemoryWarnings(t, output, 0, 0)

	handWrite(t, path(0), malformedNoteB)
	readForWarnings(t, 0)
	if update := expectMemoryWarnings(t, output, 1, 0); !strings.Contains(update, "noted-cap-0000.md") {
		t.Error("lost a tracked file's updated warning while full")
	}

	removeWarningFixture(t, path(0))
	readForWarnings(t, 0)
	expectMemoryWarnings(t, output, 0, 0)
	readForWarnings(t, 0)
	if replacement := expectMemoryWarnings(t, output, 1, 0); !strings.Contains(replacement, "noted-cap-4096.md") {
		t.Error("did not use space recovered by the settled absence")
	}

	removeWarningFixture(t, path(1))
	removeWarningFixture(t, path(capacity+1))
	readForWarnings(t, 0)
	expectMemoryWarnings(t, output, 0, 0)
	handWrite(t, path(0), malformedNoteA)
	handWrite(t, path(1), malformedNoteA)
	readForWarnings(t, 0)
	expectMemoryWarnings(t, output, 1, 1)
	readForWarnings(t, 0)
	expectMemoryWarnings(t, output, 0, 0)
}
