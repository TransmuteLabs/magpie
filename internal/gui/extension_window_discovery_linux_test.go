package gui

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSDKWindowDiscoveryBoundsCatalogReads(t *testing.T) {
	pagePlugins(t)
	_, catalog := windowTestManager(t)
	installWindowDiscoveryFixtures(t, catalog, 10)
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := unix.InotifyAddWatch(fd, filepath.Join(catalog.Root, "catalog.json"), unix.IN_OPEN|unix.IN_CLOSE_NOWRITE); err != nil {
		t.Fatal(err)
	}

	mux := pageMux()
	checkWindowDiscoveryPages(t, mux, 10)
	if reads := catalogWindowReadEvents(t, fd); reads != 2 {
		t.Errorf("page discovery read catalog.json %d times; want 2", reads)
	}
	poll := do(mux, httptest.NewRequest(http.MethodGet, "/api/plugin-pages/changes", nil))
	if poll.Code != http.StatusOK {
		t.Fatalf("window change poll: %d %s", poll.Code, poll.Body)
	}
	if reads := catalogWindowReadEvents(t, fd); reads != 1 {
		t.Errorf("change poll read catalog.json %d times; want 1", reads)
	}
}

func catalogWindowReadEvents(t *testing.T, fd int) int {
	t.Helper()
	reads := 0
	var events [4096]byte
	for {
		size, err := unix.Read(fd, events[:])
		if err == unix.EAGAIN {
			break
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if size == 0 || size%unix.SizeofInotifyEvent != 0 {
			t.Fatalf("unexpected inotify read size: %d", size)
		}
		for offset := 0; offset < size; offset += unix.SizeofInotifyEvent {
			switch mask := binary.NativeEndian.Uint32(events[offset+4:]); mask {
			case unix.IN_CLOSE_NOWRITE:
				reads++
			case unix.IN_OPEN:
			default:
				t.Fatalf("unexpected catalog event: %#x", mask)
			}
		}
	}
	t.Logf("window discovery opened and closed catalog.json %d times", reads)
	return reads
}
