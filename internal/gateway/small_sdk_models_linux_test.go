package gateway

import (
	"encoding/binary"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"golang.org/x/sys/unix"
)

func TestSmallCandidatesSDKReadsOneModelProjection(t *testing.T) {
	for _, api := range []struct {
		name  string
		minor int
	}{{"1.0", 0}, {"1.8", 8}} {
		t.Run(api.name, func(t *testing.T) {
			models := unpricedSmallSDKModels()
			p := smallSDKProvider(t, api.minor, models...)
			if got := len(p.Exposed()); got != len(models) {
				t.Fatalf("exposed models = %d, want %d", got, len(models))
			}
			fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			if _, err := unix.InotifyAddWatch(fd, extension.CatalogPath(), unix.IN_OPEN|unix.IN_CLOSE_NOWRITE); err != nil {
				t.Fatal(err)
			}
			if got := len(smallCands(p, seesAt(p.ID))); got != len(models) {
				t.Fatalf("candidate models = %d, want %d", got, len(models))
			}

			var reads int
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
					t.Fatalf("unexpected catalog event bytes: %d", size)
				}
				for offset := 0; offset < size; offset += unix.SizeofInotifyEvent {
					mask := binary.NativeEndian.Uint32(events[offset+4:])
					switch mask {
					case unix.IN_OPEN:
					case unix.IN_CLOSE_NOWRITE:
						reads++
					default:
						t.Fatalf("unexpected catalog event: %#x", mask)
					}
				}
			}
			t.Logf("SDK model candidates opened and closed catalog.json %d times", reads)
			if reads != 4 {
				t.Fatalf("SDK model candidates read catalog.json %d times, want 4 (two checks for each account)", reads)
			}
		})
	}
}
