package provider

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

func replaceProviderFixture(replacement file) error {
	current, err := read()
	if err != nil {
		return err
	}
	replacement.source = current.source
	return store(replacement)
}

func TestProviderSnapshotRejectsConcurrentSave(t *testing.T) {
	providerFileHome(t)
	writeProviderFile(t, originalProviderFile)
	stale, err := read()
	if err != nil {
		t.Fatal(err)
	}
	stale.Providers[0].Name = "Stale edit"
	if err := Save(Provider{ID: "unrelated", Chat: "https://new.invalid", Key: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(Path() + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if err := store(stale); err == nil || !strings.Contains(err.Error(), "changed since it was read") {
		t.Fatalf("stale provider snapshot was not rejected: %v", err)
	}
	if after, err := os.ReadFile(Path()); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("stale snapshot overwrote the unrelated Save: %v", err)
	}
	if after, err := os.ReadFile(Path() + ".bak"); err != nil || !bytes.Equal(backup, after) {
		t.Fatalf("stale snapshot changed the last-good backup: %v", err)
	}
	current, err := read()
	if err != nil || len(current.Providers) != 2 || len(current.Groups) != 1 {
		t.Fatalf("saved providers and groups not retained: %+v, %v", current, err)
	}
	if err := store(file{}); err == nil {
		t.Fatal("a source-less snapshot was accepted")
	}
}

func TestProviderMissingSnapshotRejectsConcurrentCreation(t *testing.T) {
	providerFileHome(t)
	missing, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(Provider{ID: "created", Chat: "https://created.invalid", Key: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := store(missing); err == nil {
		t.Fatal("missing-source snapshot overwrote a concurrent creation")
	}
	if after, err := os.ReadFile(Path()); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("concurrently created provider was overwritten: %v", err)
	}
}

func TestProviderPublicationSerializesSnapshots(t *testing.T) {
	providerFileHome(t)
	writeProviderFile(t, originalProviderFile)
	snapshots := make([]file, 6)
	for i := range snapshots {
		snapshot, err := read()
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Providers[0].Name = fmt.Sprintf("Writer %d", i)
		snapshots[i] = snapshot
	}
	start := make(chan struct{})
	results := make(chan error, len(snapshots))
	for _, snapshot := range snapshots {
		go func() {
			<-start
			results <- publishProviders(snapshot)
		}()
	}
	close(start)
	published := 0
	for range cap(results) {
		switch err := <-results; {
		case err == nil:
			published++
		case !errors.Is(err, errProvidersChanged):
			t.Errorf("unexpected publication error: %v", err)
		}
	}
	if published != 1 {
		t.Fatalf("%d snapshots were published from the same source, want one", published)
	}
	current, err := read()
	if err != nil || len(current.Providers) != 1 || len(current.Groups) != 1 {
		t.Fatalf("concurrent publication damaged the catalog: %+v, %v", current, err)
	}
}

func TestProviderPublicationReadErrorKeepsBackup(t *testing.T) {
	providerFileHome(t)
	writeProviderFile(t, originalProviderFile)
	snapshot, err := read()
	if err != nil {
		t.Fatal(err)
	}
	backup := []byte("backup must stay unchanged")
	if err := os.WriteFile(Path()+".bak", backup, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store(snapshot); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("unreadable current source was not rejected: %v", err)
	}
	if after, err := os.ReadFile(Path() + ".bak"); err != nil || !bytes.Equal(backup, after) {
		t.Fatalf("unreadable source changed the backup: %v", err)
	}
}

func TestProviderBaselineTracksRawAndFallbackSource(t *testing.T) {
	providerFileHome(t)
	valid := `{"providers":[{"id":"kept","chat":"https://kept.invalid"}]}`
	writeProviderFile(t, string(utf8BOM)+valid)
	bomSnapshot, err := read()
	if err != nil {
		t.Fatal(err)
	}
	writeProviderFile(t, valid)
	if err := store(bomSnapshot); err == nil {
		t.Fatal("BOM-only source change was not detected")
	}
	existing, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(Path()); err != nil {
		t.Fatal(err)
	}
	if err := store(existing); err == nil {
		t.Fatal("removed source was recreated from a stale snapshot")
	}
	if _, err := os.Stat(Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed source reappeared: %v", err)
	}
	writeProviderFile(t, `{"providers":`)
	if err := os.WriteFile(Path()+".bak", []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	fallback, err := read()
	if err != nil || len(fallback.Providers) != 1 {
		t.Fatalf("last-good fixture was not read: %+v, %v", fallback, err)
	}
	newBad := `{"providers":[`
	writeProviderFile(t, newBad)
	if err := store(fallback); err == nil {
		t.Fatal("fallback snapshot ignored the actual source change")
	}
	if after, err := os.ReadFile(Path()); err != nil || string(after) != newBad {
		t.Fatalf("newer invalid source was overwritten: %q, %v", after, err)
	}
	current, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if err := store(current); err != nil {
		t.Fatalf("unchanged fallback source could not be repaired: %v", err)
	}
}

func TestProviderRenameKeepsCatalogOnRejectedPublication(t *testing.T) {
	providerFileHome(t)
	writeProviderFile(t, originalProviderFile)
	for _, id := range []string{"original", decisionsID("original")} {
		if err := catalog.SaveLive(id, "https://original.invalid", []catalog.Model{{ID: "model"}}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(Path()+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Rename("original", "renamed"); err == nil {
		t.Fatal("rename succeeded without its provider lock")
	}
	if after, err := os.ReadFile(Path()); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed rename changed providers: %v", err)
	}
	for _, id := range []string{"original", decisionsID("original")} {
		if _, err := os.Stat(catalog.LivePath(id)); err != nil {
			t.Fatalf("failed rename moved %s's catalog: %v", id, err)
		}
	}
	for _, id := range []string{"renamed", decisionsID("renamed")} {
		if _, err := os.Stat(catalog.LivePath(id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed rename created %s's catalog: %v", id, err)
		}
	}
	if err := os.Remove(Path() + ".lock"); err != nil {
		t.Fatal(err)
	}
	wasChanged := catalog.Changed
	t.Cleanup(func() { catalog.Changed = wasChanged })
	notifications, sawMovedLists := 0, false
	catalog.Changed = func() {
		notifications++
		_, modelsErr := os.Stat(catalog.LivePath("renamed"))
		_, decisionsErr := os.Stat(catalog.LivePath(decisionsID("renamed")))
		sawMovedLists = modelsErr == nil && decisionsErr == nil
	}
	if err := Rename("original", "renamed"); err != nil {
		t.Fatal(err)
	}
	if notifications != 1 || !sawMovedLists {
		t.Fatalf("notification preceded renamed lists: count=%d, lists=%v", notifications, sawMovedLists)
	}
}
