package provider

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// Check in now for one account (xiaowu_95660: 手动签到一次) asks that
// account alone, once, even one left out of the daily check-in, and
// changes no switch: the other account is neither asked nor said, a
// second press the same day is answered from the claim, and the left-out
// account stays left out.
func TestCheckinOneAccountNow(t *testing.T) {
	signIn(t)
	f := &fakeWBCheckin{active: true}
	srv := f.serve(t)
	now := time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)
	w := wbCheckinFixture(t, srv, &now)
	one := w.accounts()[0]
	two := one
	two.User, two.creds.UID = "second", "u2"
	w.accounts = func() []wbAccount { return []wbAccount{one, two} }
	ctx := context.Background()
	leaveOut(t, "workbuddy|u2")

	c := w.checkiner()
	c.only = "workbuddy|u2"
	rs := c.checkinNow(ctx, true)
	if len(rs) != 1 || rs[0].User != "second" || rs[0].Outcome != CheckinClaimed || !rs[0].Asked {
		t.Fatalf("pressed for the left-out account: %+v", rs)
	}
	s0, cl0 := f.counts()
	if cl0 != 1 {
		t.Fatalf("claims %d, want 1 (the other account asked too?)", cl0)
	}
	if off := settings.Load().CheckinOff; !slices.Equal(off, []string{"workbuddy|u2"}) {
		t.Fatalf("the press changed the switch: %v", off)
	}

	// pressed again the same day: the claim stands, the vendor not asked
	rs = c.checkinNow(ctx, true)
	if len(rs) != 1 || rs[0].Asked || !rs[0].Checked() {
		t.Fatalf("second press: %+v", rs)
	}
	if s, cl := f.counts(); s != s0 || cl != cl0 {
		t.Fatalf("second press asked: status %d, claims %d", s, cl)
	}

	// the daily loop still leaves it out and checks the other in
	f.mu.Lock()
	f.checked = false
	f.mu.Unlock()
	rs = w.checkinNow(ctx, false)
	if len(rs) != 1 || rs[0].User != "旅行者" || !rs[0].Asked {
		t.Fatalf("the loop after the press: %+v", rs)
	}
}

// A press for an account no check-in reaches answers nothing.
func TestCheckinAccountNowUnknown(t *testing.T) {
	signIn(t)
	if rs := CheckInAccount(context.Background(), "", "workbuddy|nobody"); len(rs) != 0 {
		t.Fatalf("unknown account: %+v", rs)
	}
	if rs := CheckInAccount(context.Background(), "", ""); len(rs) != 0 {
		t.Fatalf("no key: %+v", rs)
	}
}

type checkinConfirmedHost struct {
	*fixtureExtensionProviderHost
	cancel context.CancelFunc
	calls  int
}

func (host *checkinConfirmedHost) CheckinAccountWithAdmission(ctx context.Context, scope sdk.Scope, _, _ string, _ sdk.AccountCheckinRequest, admit func(context.Context) error) (extensionhost.CheckinExecution, error) {
	if err := admit(ctx); err != nil {
		return extensionhost.CheckinExecution{}, err
	}
	host.calls++
	scope.Operation, scope.Attempt = "fixture-claim", 1
	credit := 3.0
	execution := extensionhost.CheckinExecution{
		Result:  &sdk.AccountCheckinResult{Outcome: CheckinClaimed, Credit: &credit},
		Receipt: extensionhost.JournalEntry{Scope: scope, State: "completed"},
	}
	host.cancel()
	return execution, nil
}

type checkinSettingsHost struct {
	*checkinConfirmedHost
	lists int
}

func (host *checkinSettingsHost) ListContext(ctx context.Context) ([]extensionhost.Installed, error) {
	host.lists++
	return host.fixtureExtensionProviderHost.ListContext(ctx)
}

func TestCheckinSettingsSharesProviderDiscovery(t *testing.T) {
	fixture := fixtureExtensionProvider(t, true)
	fixture.manifest.Contract.Version.Minor = 8
	fixture.manifest.Contract.Capabilities = append(fixture.manifest.Contract.Capabilities, sdk.ModuleRPC)
	fixture.manifest.Providers[0].Checkin = &sdk.AccountCheckinDescriptor{Action: "daily.claim"}
	fixture.manifest.Actions = []sdk.ActionDescriptor{{ID: "daily.claim", Effect: "write", InputSchema: []byte(`{"type":"object"}`)}}
	host := &checkinSettingsHost{checkinConfirmedHost: &checkinConfirmedHost{fixtureExtensionProviderHost: fixture}}
	providerExtensionHost = func() extensionProviderHost { return host }
	plugins, accounts := CheckinSettingsContext(t.Context())
	if host.lists != 1 || len(plugins) != 1 || len(accounts) != 2 {
		t.Fatalf("settings discovered the catalog %d times: plugins %+v, accounts %+v", host.lists, plugins, accounts)
	}
	for _, account := range accounts {
		if account.By != pluginCheckinBy(plugins[0].ID) || !account.On {
			t.Fatalf("account did not come from the shared discovery: %+v", account)
		}
	}
}

func TestCheckinAccountKeepsConfirmedReplyAfterCancel(t *testing.T) {
	fixture := fixtureExtensionProvider(t, true)
	fixture.manifest.Contract.Version.Minor = 8
	fixture.manifest.Contract.Capabilities = append(fixture.manifest.Contract.Capabilities, sdk.ModuleRPC)
	fixture.manifest.Providers[0].Checkin = &sdk.AccountCheckinDescriptor{Action: "daily.claim"}
	fixture.manifest.Actions = []sdk.ActionDescriptor{{ID: "daily.claim", Effect: "write", InputSchema: []byte(`{"type":"object"}`)}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	host := &checkinConfirmedHost{fixtureExtensionProviderHost: fixture, cancel: cancel}
	providerExtensionHost = func() extensionProviderHost { return host }
	accounts := CheckinAccounts()
	if len(accounts) != 2 {
		t.Fatalf("ordinary SDK check-in accounts: %+v", accounts)
	}
	account := accounts[0]
	results := CheckInAccount(ctx, account.By, account.Key)
	if !errors.Is(ctx.Err(), context.Canceled) || len(results) != 1 || results[0].Outcome != CheckinClaimed || !results[0].Asked {
		t.Fatalf("canceled press lost the confirmed result: %+v (%v)", results, ctx.Err())
	}
	stored, err := readCheckins(pluginCheckinPath())
	result := stored[account.Key]
	if err != nil || result.Outcome != CheckinClaimed || result.Credit != 3 || result.Operation == nil ||
		result.Operation.State != "completed" || result.Operation.Scope.Operation != "fixture-claim" {
		t.Fatalf("confirmed reply was not saved: %+v (%v)", result, err)
	}
	if results := CheckInAccount(t.Context(), account.By, account.Key); len(results) != 1 || results[0].Asked ||
		results[0].Outcome != CheckinClaimed || host.calls != 1 {
		t.Fatalf("the next press did not reuse the confirmed reply: %+v, calls %d", results, host.calls)
	}
}
