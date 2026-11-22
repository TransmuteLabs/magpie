package provider

import (
	"encoding/binary"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
	"golang.org/x/sys/unix"
)

func TestExtensionDiscoveryReadsOneCatalog(t *testing.T) {
	manager := discoveryCatalogManager(t)
	source, entry := buildDispatchBindingWorker(t)
	for index := range 4 {
		minor := 8
		if index == 0 {
			minor = 0
		}
		installDiscoveryCatalogModule(t, manager, source, entry, "fixture.discovery-"+strconv.Itoa(index), minor)
	}
	if _, err := manager.Store().StateCASContext(t.Context(), sdk.Scope{Plugin: "fixture.discovery-0"}, "module.config", 0,
		json.RawMessage(`"legacy state"`)); err != nil {
		t.Fatal(err)
	}

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := unix.InotifyAddWatch(fd, filepath.Join(manager.Namespace(), "catalog.json"),
		unix.IN_OPEN|unix.IN_CLOSE_NOWRITE); err != nil {
		t.Fatal(err)
	}
	providers, err := ExtensionProviders()
	if err != nil || len(providers) != 4 {
		t.Fatalf("discover installed providers: got %d, error %v", len(providers), err)
	}

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
			t.Fatalf("unexpected inotify read: %d bytes", size)
		}
		for offset := 0; offset < size; offset += unix.SizeofInotifyEvent {
			mask := binary.NativeEndian.Uint32(events[offset+4:])
			switch mask {
			case unix.IN_CLOSE_NOWRITE:
				reads++
			case unix.IN_OPEN:
			default:
				t.Fatalf("unexpected catalog event: %#x", mask)
			}
		}
	}
	t.Logf("provider discovery opened and closed catalog.json %d times", reads)
	if reads != 1 {
		t.Fatalf("provider discovery read catalog.json %d times; want 1", reads)
	}
	for _, provider := range providers {
		if !provider.Account.Lists("m1") || provider.Account.Lists("absent") {
			t.Fatalf("provider %s gave wrong model eligibility", provider.ID)
		}
	}
}
