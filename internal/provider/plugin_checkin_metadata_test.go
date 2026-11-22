package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

func TestLoginCheckinsReadsLegacyMetadataWhileCheckinRuns(t *testing.T) {
	_, keys := pluginCheckinTeams(t, "A/u1", "B/u2")
	now := time.Now()
	legacy, err := json.Marshal(map[string]WorkBuddyCheckin{
		"fakeco|A@fake": {Day: wbCheckinDay(now), At: now.Add(-time.Minute), Outcome: CheckinClaimed, Credit: 400},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginCheckinPath(), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	s := settings.Load()
	s.CheckinOff = []string{"fakeco|B@fake"}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	savedSettings, err := os.ReadFile(settings.Path())
	if err != nil {
		t.Fatal(err)
	}

	started, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	background := checkiner{
		path: filepath.Join(t.TempDir(), "scheduled-checkin.json"), now: func() time.Time { return now },
		mu: &pluginCheckinMu, by: "plugin:scheduler", label: "scheduler",
		accounts: func() []checkinAcct {
			return []checkinAcct{{User: "scheduled", On: true, key: "scheduler|account", do: func(ctx context.Context) WorkBuddyCheckin {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
				}
				return WorkBuddyCheckin{Outcome: CheckinClaimed}
			}}}
		},
	}
	go func() {
		defer close(joined)
		background.checkinNow(t.Context(), false)
	}()
	defer func() {
		unblock()
		<-joined
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduled check-in did not reach its vendor fixture")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	read := make(chan map[string]SubscriptionQuota, 1)
	go func() {
		read <- LoginCheckinsContext(ctx, PluginID("fakeco"), map[string]SubscriptionQuota{
			"A@fake": {Plan: "Credits"},
			"B@fake": {Plan: "Credits"},
		})
	}()
	var got map[string]SubscriptionQuota
	select {
	case got = <-read:
		if err := ctx.Err(); err != nil {
			t.Errorf("read of local check-in metadata exhausted its deadline: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Error("reading login check-in metadata waited for the unrelated vendor")
		unblock()
		got = <-read
	}
	account := got["A@fake"]
	if account.CheckinKey != pluginCheckinKey("fakeco", keys["A@fake/u1"]) ||
		account.Checkin == nil || account.Checkin.Outcome != CheckinClaimed || account.Checkin.Credit != 400 || account.Plan != "Credits" {
		t.Errorf("legacy check-in display = %+v, result=%+v", account, account.Checkin)
	}
	off := got["B@fake"]
	if off.CheckinKey != pluginCheckinKey("fakeco", keys["B@fake/u2"]) || !off.CheckinOff {
		t.Errorf("legacy Off display = %+v", off)
	}
	if got, err := os.ReadFile(pluginCheckinPath()); err != nil || !bytes.Equal(got, legacy) {
		t.Errorf("metadata lookup changed check-in history: %s, %v", got, err)
	}
	if got, err := os.ReadFile(settings.Path()); err != nil || !bytes.Equal(got, savedSettings) {
		t.Errorf("metadata lookup changed check-in settings: %v", err)
	}
}

func TestEnablingLegacyCheckinAccountKeepsOtherSignInOff(t *testing.T) {
	_, keys := pluginCheckinTeams(t, "A/u1", "A/u2")
	s := settings.Load()
	s.CheckinOff = []string{"workbuddy|123", "fakeco|A@fake"}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	first := pluginCheckinKey("fakeco", keys["A@fake/u1"])
	second := pluginCheckinKey("fakeco", keys["A@fake/u2"])
	if err := SetCheckinAccountOn(t.Context(), first, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"workbuddy|123", second}
	if got := settings.Load().CheckinOff; !slices.Equal(got, want) {
		t.Fatalf("CheckinOff = %q, want %q", got, want)
	}
	var enabled, disabled bool
	for _, account := range CheckinAccounts() {
		enabled = enabled || account.Key == first && account.On
		disabled = disabled || account.Key == second && !account.On
	}
	if !enabled || !disabled {
		t.Fatalf("wanted only %s enabled among its sign-ins", first)
	}
}
