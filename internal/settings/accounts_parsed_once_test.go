package settings

import (
	"errors"
	"testing"
	"time"
)

// AccountDiscoveryOff parses settings.json once for each version of the
// file: it is asked for each agent and each plugin every time the
// providers are built, and parsed the whole file each time (#2502). A
// change saved is seen on the next ask.
func TestAccountDiscoveryOffParsedOnce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	s := Load()
	s.NoAccountDiscovery = true
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	if !AccountDiscoveryOff() {
		t.Fatal("saved off, read on")
	}
	n := Parses()
	for range 100 {
		AccountDiscoveryOff()
	}
	if got := Parses() - n; got != 0 {
		t.Fatalf("an unchanged settings.json was parsed %d more times", got)
	}
	s = Load()
	s.NoAccountDiscovery = false
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	if AccountDiscoveryOff() {
		t.Fatal("saved on, still read off")
	}
}

func TestCurrentAccountDiscoveryDoesNotWaitForSettingsWriter(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	if err := Save(Settings{NoAccountDiscovery: true}); err != nil {
		t.Fatal(err)
	}
	unlock := lockFile()
	result := make(chan error, 1)
	go func() {
		off, err := AccountDiscoveryOffNow()
		if err == nil && !off {
			err = errors.New("saved discovery setting was lost")
		}
		result <- err
	}()
	select {
	case err := <-result:
		unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		unlock()
		<-result
		t.Fatal("publication policy reads must not invert the settings writer's lock order")
	}
}
