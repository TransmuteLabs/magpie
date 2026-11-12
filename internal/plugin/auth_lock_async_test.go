package plugin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This file uses only EditAuth and the on-disk store, so it can also be
// copied to a previous checkout to reproduce that version's lost update.
func authPublicProbeHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
}

func authPublicProbeChild(t *testing.T) bool {
	t.Helper()
	if os.Getenv("MAGPIE_AUTH_PUBLIC_PROBE") != "holder" {
		return false
	}
	err := EditAuth(func(m map[string]json.RawMessage) (bool, error) {
		fmt.Fprintln(os.Stdout, "holder-ready")
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			return false, err
		}
		m["parked"] = json.RawMessage(`{"type":"api","key":"first"}`)
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func authPublicHolder(t *testing.T) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAuthLockPublicLiveHolder$")
	cmd.Env = append(os.Environ(), "MAGPIE_AUTH_PUBLIC_PROBE=holder")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "holder-ready\n" {
			t.Fatalf("holder did not enter EditAuth: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("holder did not enter EditAuth")
	}
	return cmd, in
}

// The child is parked inside the public callback after reading its snapshot.
// Aging the lock cannot make it safe to steal: on the old implementation the
// contender publishes, then the resumed child's old snapshot overwrites it.
func TestAuthLockPublicLiveHolder(t *testing.T) {
	if authPublicProbeChild(t) {
		return
	}
	authPublicProbeHome(t)
	cmd, in := authPublicHolder(t)
	old := time.Now().Add(-time.Hour)
	for _, suffix := range []string{".lock", ".flock"} {
		if _, err := os.Stat(AuthPath() + suffix); err == nil {
			if err := os.Chtimes(AuthPath()+suffix, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	entered := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- EditAuth(func(m map[string]json.RawMessage) (bool, error) {
			close(entered)
			m["contender"] = json.RawMessage(`{"type":"api","key":"second"}`)
			return true, nil
		})
	}()
	// If a contender entered, wait for its publication before the holder
	// resumes; this makes the old implementation's lost update observable.
	var earlyErr error
	early := false
	select {
	case <-entered:
		early = true
		select {
		case earlyErr = <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("entered contender did not finish")
		}
	case <-time.After(750 * time.Millisecond):
	}
	if _, err := io.WriteString(in, "release\n"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if early {
		if earlyErr != nil {
			t.Fatal(earlyErr)
		}
	} else {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("contender did not finish after release")
		}
	}
	all, err := AuthEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(all["parked"]) == 0 || len(all["contender"]) == 0 {
		t.Fatalf("a live writer was robbed and an update lost: %v", all)
	}
	if early {
		t.Fatal("a contender entered while the aged live holder was parked")
	}
}

func TestAuthLockPublicProcessDeathReleases(t *testing.T) {
	authPublicProbeHome(t)
	cmd, _ := authPublicHolder(t)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	finished := make(chan error, 1)
	go func() {
		finished <- EditAuth(func(m map[string]json.RawMessage) (bool, error) {
			m["after"] = json.RawMessage(`{"type":"api","key":"after"}`)
			return true, nil
		})
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		// Let a previous sentinel implementation's goroutine finish before
		// failing; the age change is not part of the successful assertion.
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(AuthPath()+".lock", old, old)
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
		}
		t.Fatal("dead holder's lock was not released promptly")
	}
}
