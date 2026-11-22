package sessions

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStorageSourcesListReadsCatalogOncePerSelection(t *testing.T) {
	manager, owner := windowStorageFixture(t)
	events, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(events)
	if _, err := unix.InotifyAddWatch(events, filepath.Join(manager.Namespace(), "catalog.json"), unix.IN_OPEN|unix.IN_CLOSE_NOWRITE); err != nil {
		t.Fatal(err)
	}

	roots, consents, err := InspectStorageSources(t.Context())
	if err != nil || len(roots) != 2 || len(consents) != 0 {
		t.Fatalf("ordinary storage sources: %v, %v, %v", roots, consents, err)
	}
	for index, slot := range []string{"first", "second"} {
		if root := roots[index]; root.Plugin != owner.Module || root.RootSlot != slot || root.RootRef == "" ||
			root.Revision != owner.PackageRevision || root.Consented {
			t.Fatalf("wrong root for %s: %+v", slot, root)
		}
	}

	reads := 0
	var buffer [4096]byte
	for {
		size, err := unix.Read(events, buffer[:])
		if err == unix.EAGAIN {
			break
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil || size == 0 || size%unix.SizeofInotifyEvent != 0 {
			t.Fatalf("catalog read events: %d bytes, %v", size, err)
		}
		for offset := 0; offset < size; offset += unix.SizeofInotifyEvent {
			if binary.NativeEndian.Uint32(buffer[offset+4:]) == unix.IN_CLOSE_NOWRITE {
				reads++
			}
		}
	}
	if want := 1 + len(roots); reads != want {
		t.Fatalf("storage sources list read the catalog %d times, want %d (listing and one per root)", reads, want)
	}
}
