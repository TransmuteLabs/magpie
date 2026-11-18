package sessions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestWindowStorageNativeSelectedWALAndMedia(t *testing.T) {
	fixture := installStorageNativeFixture(t)
	config, err := fixture.manager.ModuleConfig(t.Context(), fixture.entry.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner := WindowStorageOwner{Module: fixture.entry.Manifest.ID, PackageRevision: fixture.entry.Revision,
		ConfigVersion: config.Version, PageRef: "native-fixture-page", Component: "window:page"}
	inspection, err := InspectWindowStorage(t.Context(), owner, "opencode-history")
	if err != nil || !inspection.ReadAllowed || len(inspection.Roots) != 1 || !inspection.Roots[0].Consented {
		t.Fatal(inspection, err)
	}
	sourceView.Store(nil)
	page, err := ListWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, inspection.Roots[0].RootRef, "", 1)
	if err != nil || len(page.Entries) != 1 || page.NextCursor == "" {
		t.Fatal("scoped catalog did not issue a bounded cursor independently of the global view", page, err)
	}
	selected := ""
	for {
		for _, entry := range page.Entries {
			if entry.Title == "Committed WAL session" {
				selected = entry.SessionRef
			}
		}
		if page.NextCursor == "" {
			break
		}
		if _, err := ListWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, inspection.Roots[0].RootRef, page.NextCursor+"x", 1); err == nil {
			t.Fatal("forged selected catalog cursor accepted")
		}
		page, err = ListWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, inspection.Roots[0].RootRef, page.NextCursor, 1)
		if err != nil || len(page.Entries) > 1 {
			t.Fatal(page, err)
		}
	}
	if selected == "" {
		t.Fatal("committed WAL-only session was not selected")
	}
	read, err := ReadWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, selected)
	if err != nil || len(read.Media) == 0 || read.SessionRef != selected || read.ReadRef == "" {
		t.Fatal("selected native transcript/media failed", read, err)
	}
	var text strings.Builder
	for _, part := range read.Parts {
		text.WriteString(part.Text)
	}
	if !strings.Contains(text.String(), "current instruction") || strings.Contains(text.String(), "retired instruction") ||
		strings.Index(text.String(), "first split part") >= strings.Index(text.String(), "second split part") {
		t.Fatal("actual storage projection lost ordered parts or instruction epoch", text.String())
	}
	second, err := ReadWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, selected)
	if err != nil || second.ReadRef != read.ReadRef {
		t.Fatal("stable content changed revision across independent native grants", second, err)
	}
	media, err := MediaWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, selected, read.ReadRef, read.Media[0].MediaRef)
	if err != nil || media.MIME != "image/png" {
		t.Fatal("selected binary media failed", media, err)
	}
	digest := sha256.Sum256(media.Bytes)
	if "media:"+hex.EncodeToString(digest[:]) != fixture.initial.MediaID {
		t.Fatal("returned bytes were not the selected committed blob")
	}
	if _, err := MediaWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, selected, read.ReadRef, strings.Repeat("f", 64)); err == nil {
		t.Fatal("unissued media reference accepted")
	}
	after := fixture.ask(t, "state")
	if after.DB != fixture.initial.DB || after.WAL != fixture.initial.WAL || after.SHM != fixture.initial.SHM {
		t.Fatal("browser owner modified DB/WAL/SHM bytes")
	}
	fixture.ask(t, "append")
	if _, err := MediaWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, selected, read.ReadRef, read.Media[0].MediaRef); err == nil {
		t.Fatal("changed selected content retained an old read/media revision")
	}
	fresh, err := ReadWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, selected)
	if err != nil || fresh.ReadRef == read.ReadRef {
		t.Fatal("later WAL commit did not change normalized content revision", fresh, err)
	}
	if bytes.Equal(media.Bytes, nil) {
		t.Fatal("empty committed media")
	}
	old := sourceBeforePublish
	t.Cleanup(func() { sourceBeforePublish = old })
	sourceBeforePublish = func() {
		sourceBeforePublish = nil
		result := ReleaseWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, inspection.Roots[0].ConsentRef)
		if result.State != "released" {
			t.Fatal("held publication revocation failed", result)
		}
	}
	if _, err := ReadWindowStorage(t.Context(), owner, "opencode-history", inspection.ViewRef, selected); err == nil {
		t.Fatal("native held transcript published after exact consent release")
	}
}
