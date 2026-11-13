package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtensionAgentLockSubprocess(t *testing.T) {
	home := os.Getenv("MAGPIE_AGENT_LOCK_TEST_HOME")
	if home == "" {
		return
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata"))
	fmt.Println("WAITING")
	unlock, err := extensionBindingLock(t.Context(), home, "lock-fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	fmt.Println("ACQUIRED")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionAgentLockCoordinatesIndependentProcesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata"))
	unlock, err := extensionBindingLock(t.Context(), home, "lock-fixture", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestExtensionAgentLockSubprocess$")
	child.Env = append(os.Environ(), "MAGPIE_AGENT_LOCK_TEST_HOME="+home)
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		input.Close()
		if !waited {
			cancel()
			child.Wait()
		}
	}()
	reader := bufio.NewReader(output)
	line := func() string {
		raw, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal("lock subprocess stopped before its handshake", err)
		}
		return strings.TrimSpace(raw)
	}
	if line() != "WAITING" {
		t.Fatal("lock subprocess did not start")
	}
	nested, err := extensionBindingLock(t.Context(), home, "lock-fixture", true)
	if err != nil {
		t.Fatal("nested shared ordinary writer deadlocked behind a waiting profile", err)
	}
	nested()
	unlock()
	unlock = nil
	if line() != "ACQUIRED" {
		t.Fatal("profile did not acquire authority after ordinary writers finished")
	}
	path := filepath.Join(filepath.Dir(extensionBindingPath()), "sdk-agent-locks", extensionBindingKey(home, "", "lock-fixture")+".lock")
	probe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	held, err := extensionTryLock(probe, true)
	if held {
		extensionUnlock(probe)
	}
	probe.Close()
	if err != nil || held {
		t.Fatal("another process's exclusive profile claim admitted a shared ordinary writer", held, err)
	}
	if _, err := input.Write([]byte("release\n")); err != nil {
		t.Fatal(err)
	}
	input.Close()
	err = child.Wait()
	waited = true
	if err != nil {
		t.Fatal(err)
	}
	unlock, err = extensionBindingLock(t.Context(), home, "lock-fixture", true)
	if err != nil {
		t.Fatal("completed profile did not release its process lock", err)
	}
}
