package sessions

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNativeTranscriptReadsClaimsAtAdmissionAndPublication(t *testing.T) {
	for _, fixture := range []struct {
		name, needle string
		want         int
	}{
		{name: "transcript", want: 2},
		{name: "needle", needle: "needle", want: 3},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			setup(t)
			path := filepath.Join(t.TempDir(), "session.jsonl")
			writeLines(t, path, `{"type":"user","message":{"role":"user","content":"find needle"}}`)
			session := Session{Agent: "claude", ID: "session", Path: path}
			writeHistoryAuthority(t, sourceBindingsPath(), []byte("[]"))
			writeHistoryAuthority(t, storageConsentsPath(), []byte("[]"))

			fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			watches := map[int32]string{}
			for _, path := range []string{sourceBindingsPath(), storageConsentsPath()} {
				watch, err := unix.InotifyAddWatch(fd, path, unix.IN_OPEN|unix.IN_CLOSE_NOWRITE)
				if err != nil {
					t.Fatal(err)
				}
				watches[int32(watch)] = filepath.Base(path)
			}
			transcript, err := TranscriptAtContext(t.Context(), session, fixture.needle)
			if err != nil || len(transcript.Parts) != 1 || transcript.Parts[0].Text != "find needle" {
				t.Fatalf("transcript: %+v, %v", transcript, err)
			}
			counts := map[int32]int{}
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
					t.Fatalf("inotify event bytes: %d", size)
				}
				for offset := 0; offset < size; offset += unix.SizeofInotifyEvent {
					watch := int32(binary.NativeEndian.Uint32(events[offset:]))
					mask := binary.NativeEndian.Uint32(events[offset+4:])
					if mask == unix.IN_CLOSE_NOWRITE {
						counts[watch]++
					}
				}
			}
			for watch, name := range watches {
				if counts[watch] != fixture.want {
					t.Errorf("%s read %d times, want %d", name, counts[watch], fixture.want)
				}
			}
		})
	}
}
