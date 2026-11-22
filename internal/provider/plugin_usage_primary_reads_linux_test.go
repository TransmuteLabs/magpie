package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/plugin"
	"golang.org/x/sys/unix"
)

func TestCommunityUsagePrimaryReadPhases(t *testing.T) {
	login, requests := startPluginUsageFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	read := func() SubscriptionQuota {
		return LoginUsage(ctx, login.Agent)[login.User]
	}
	poll := func(t *testing.T, body string) SubscriptionQuota {
		t.Helper()
		StaleAllowance(login.Agent, login.User)
		reply, result := startPluginUsageCall(t, ctx, requests, read)
		reply <- pluginUsageReply{http.StatusOK, body}
		return readPluginUsageResult(t, result)
	}
	planBody := func(plan string) string {
		return fmt.Sprintf(`{"plan":%q,"windows":[{"name":"day","used":31,"resetSecs":3600}]}`, plan)
	}
	if quota := poll(t, planBody(login.Plan)); quota.Error != "" || quota.Plan != login.Plan || len(quota.Windows) != 1 {
		t.Fatalf("seed Usage: %+v", quota)
	}
	plugin.Refreshed()
	plugin.Told()

	path := loginsPath()
	probe := watchCommunityLoginReads(t, path)
	boundaries := make(chan error, 32)
	beforeWrite := beforeLoginsWrite
	beforeLoginsWrite = func() {
		beforeWrite()
		_, err := probe.drain()
		boundaries <- err
	}
	t.Cleanup(func() { beforeLoginsWrite = beforeWrite })

	for _, phase := range []struct {
		name  string
		polls int
		write bool
	}{
		{name: "warm", polls: 2},
		{name: "kept", polls: 2},
		{name: "identical", polls: 5, write: true},
		{name: "changed", polls: 5, write: true},
		{name: "resized", polls: 1, write: true},
	} {
		t.Run(phase.name, func(t *testing.T) {
			sameSize := 0
			for attempt := range phase.polls {
				before := readCommunityLoginGeneration(t, path)
				backupBefore := readCommunityLoginGeneration(t, lastgood.Bak(path))
				stamp := StampNow()
				if _, err := probe.drain(); err != nil {
					t.Fatal(err)
				}

				plan, body := login.Plan, planBody(login.Plan)
				var quota SubscriptionQuota
				switch phase.name {
				case "warm":
					quota = read()
					select {
					case <-requests:
						t.Fatal("warm Usage fetched from its vendor")
					default:
					}
				case "kept":
					body = `{"signIn":"kept","windows":[{"name":"day","used":31,"resetSecs":3600}]}`
				case "changed":
					plan = fmt.Sprintf("New plan %d", attempt)
					body = planBody(plan)
				case "resized":
					plan = "A substantially longer saved plan"
					body = planBody(plan)
				}
				if phase.name != "warm" {
					quota = poll(t, body)
				}
				if quota.Error != "" || quota.ReadDeferred || quota.Plan != plan ||
					len(quota.Windows) != 1 || quota.Windows[0].Used != 31 {
					t.Fatalf("Usage: %+v; want plan %q and 31%% used", quota, plan)
				}

				crossed := false
				select {
				case err := <-boundaries:
					if err != nil {
						t.Fatal(err)
					}
					crossed = true
				default:
				}
				if crossed != phase.write || len(boundaries) != 0 {
					t.Fatalf("writer hook reached=%v, extra hooks=%d, want one hook iff publication=%v", crossed, len(boundaries), phase.write)
				}
				reads, err := probe.drain()
				if err != nil {
					t.Fatal(err)
				}
				primary := readCommunityLoginGeneration(t, path)
				backup := readCommunityLoginGeneration(t, lastgood.Bak(path))

				if changed := StampNow() != stamp; changed != (phase.name == "changed" || phase.name == "resized") {
					t.Fatalf("catalog stamp changed=%v in %s", changed, phase.name)
				}
				if !phase.write {
					if reads.primaryMoves != 0 || reads.backupMoves != 0 ||
						!before.sameGeneration(primary) || !backupBefore.sameGeneration(backup) {
						t.Fatalf("%s changed primary or backup: %+v", phase.name, reads)
					}
					t.Logf("poll=%d full invocation reads=%+v", attempt+1, reads)
					continue
				}
				if reads.primaryMoves != 1 || reads.backupMoves != 1 || os.SameFile(primary.info, before.info) ||
					os.SameFile(backup.info, backupBefore.info) || os.SameFile(backup.info, before.info) ||
					!bytes.Equal(backup.data, before.data) {
					t.Fatalf("bad primary or previous-generation backup publication: %+v", reads)
				}
				if before.info.Mode().Perm() != 0o600 || primary.info.Mode().Perm() != 0o600 {
					t.Fatal("primary comparison requires private file modes")
				}
				oldOrder := before.order(t, login)
				newOrder := primary.order(t, login)
				if newOrder.Revision != oldOrder.Revision+1 ||
					!newOrder.SignIn.after(oldOrder.SignIn) || !newOrder.Plan.after(oldOrder.Plan) {
					t.Fatalf("saved observation order did not advance: %+v → %+v", oldOrder, newOrder)
				}
				t.Logf("poll=%d bytes=%d→%d post-hook reads=%+v", attempt+1, len(before.data), len(primary.data), reads)
				wantBefore, wantAfter := 1, 0
				if len(before.data) == len(primary.data) {
					sameSize++
					wantAfter = 1
				}
				if reads.primaryBeforeBackup != wantBefore || reads.primaryAfterBackup != wantAfter || reads.backup != 1 {
					t.Fatalf("post-hook opens: %+v; want primary %d + %d and backup 1", reads, wantBefore, wantAfter)
				}
			}
			if (phase.name == "identical" || phase.name == "changed") && sameSize == 0 {
				t.Fatal("no same-length publication observed")
			}
			if phase.name == "resized" && sameSize != 0 {
				t.Fatal("longer plan did not change the size of the primary")
			}
		})
	}
}

type communityLoginGeneration struct {
	data []byte
	info os.FileInfo
}

func readCommunityLoginGeneration(t *testing.T, path string) communityLoginGeneration {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return communityLoginGeneration{data, info}
}

func (file communityLoginGeneration) sameGeneration(other communityLoginGeneration) bool {
	return os.SameFile(file.info, other.info) && bytes.Equal(file.data, other.data)
}

func (file communityLoginGeneration) order(t *testing.T, login Login) pluginLoginObservations {
	t.Helper()
	logins, err := parseLogins(file.data)
	if err != nil {
		t.Fatal(err)
	}
	return loginOf(t, logins, login.Agent, login.User).PluginObservations
}

type communityLoginReads struct {
	primaryBeforeBackup int
	primaryAfterBackup  int
	backup              int
	primaryMoves        int
	backupMoves         int
}

type communityLoginProbe struct {
	fd      int
	primary string
	backup  string
}

func watchCommunityLoginReads(t *testing.T, path string) communityLoginProbe {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	_, err = unix.InotifyAddWatch(fd, filepath.Dir(path), unix.IN_OPEN|unix.IN_CLOSE_NOWRITE|unix.IN_MOVED_TO)
	if err != nil {
		t.Fatal(err)
	}
	return communityLoginProbe{fd, filepath.Base(path), filepath.Base(lastgood.Bak(path))}
}

func (probe communityLoginProbe) drain() (communityLoginReads, error) {
	var reads communityLoginReads
	var buffer [4096]byte
	for {
		size, err := unix.Read(probe.fd, buffer[:])
		switch err {
		case unix.EAGAIN:
			return reads, nil
		case unix.EINTR:
			continue
		case nil:
		default:
			return reads, err
		}
		events := buffer[:size]
		for len(events) >= unix.SizeofInotifyEvent {
			mask := binary.NativeEndian.Uint32(events[4:8])
			nameLength := int(binary.NativeEndian.Uint32(events[12:16]))
			if nameLength < 0 || nameLength > len(events)-unix.SizeofInotifyEvent {
				return reads, fmt.Errorf("invalid inotify name length %d", nameLength)
			}
			name, _, _ := bytes.Cut(events[unix.SizeofInotifyEvent:unix.SizeofInotifyEvent+nameLength], []byte{0})
			events = events[unix.SizeofInotifyEvent+nameLength:]
			switch {
			case mask&unix.IN_Q_OVERFLOW != 0:
				return reads, fmt.Errorf("inotify event queue overflow")
			case mask&unix.IN_MOVED_TO != 0 && string(name) == probe.primary:
				reads.primaryMoves++
			case mask&unix.IN_MOVED_TO != 0 && string(name) == probe.backup:
				reads.backupMoves++
			case reads.primaryMoves != 0 || mask&unix.IN_CLOSE_NOWRITE == 0:
			case string(name) == probe.backup:
				reads.backup++
			case string(name) == probe.primary && reads.backupMoves == 0:
				reads.primaryBeforeBackup++
			case string(name) == probe.primary:
				reads.primaryAfterBackup++
			}
		}
		if len(events) != 0 || size == 0 {
			return reads, fmt.Errorf("truncated inotify event buffer (%d bytes)", size)
		}
	}
}
