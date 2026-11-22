package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

func TestBuiltinCheckinKeepsConfirmedClaimWhenNextAccountCancels(t *testing.T) {
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	for _, vendor := range []string{"workbuddy", "minimax", "trae", "qoder"} {
		for _, phase := range []string{"status-request", "status-body", "before-claim", "claim-request", "claim-body", "vendor-timeout", "workbuddy-401"} {
			if phase == "workbuddy-401" && vendor != "workbuddy" {
				continue
			}
			t.Run(vendor+"/"+phase, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var statusBody string
				var calls []string
				send := http.DefaultClient.Do
				if vendor == "workbuddy" {
					send = wbPluginSigned
				}
				first := func(req *http.Request) (*http.Response, error) {
					res, err := send(req)
					if err != nil || statusBody != "" {
						return res, err
					}
					defer res.Body.Close()
					body, err := io.ReadAll(res.Body)
					if err != nil {
						return nil, err
					}
					statusBody = string(body)
					res.Body = io.NopCloser(strings.NewReader(statusBody))
					return res, nil
				}
				second := func(req *http.Request) (*http.Response, error) {
					calls = append(calls, req.URL.Path)
					switch {
					case phase == "vendor-timeout":
						return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: context.DeadlineExceeded}
					case phase == "workbuddy-401" && len(calls) == 1:
						return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(wbGateway401))}, nil
					case phase == "before-claim" && len(calls) == 1:
						cancel()
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(statusBody))}, nil
					case strings.HasPrefix(phase, "claim") && len(calls) == 1:
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(statusBody))}, nil
					}
					cancel()
					if strings.HasSuffix(phase, "-body") {
						body := io.MultiReader(strings.NewReader(`{"partial":`), iotest.ErrReader(req.Context().Err()))
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body)}, nil
					}
					return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: req.Context().Err()}
				}
				runner := builtinCheckinCancelFixture(t, vendor, now, first, second)
				accounts := runner.accounts()
				prior := WorkBuddyCheckin{Day: "2026-10-05", At: now.Add(-24 * time.Hour), Outcome: CheckinClaimed, Credit: 142, Streak: 3}
				writeFile(t, runner.path, map[string]WorkBuddyCheckin{accounts[1].key: prior})
				results := runner.checkinNow(ctx, true)
				stored, err := readCheckins(runner.path)
				if err != nil {
					t.Fatal(err)
				}
				credit := 100.0
				if vendor == "minimax" {
					credit = 800
				}
				confirmed := stored[accounts[0].key]
				if confirmed.Outcome != CheckinClaimed || confirmed.Credit != credit || !confirmed.Claimed.Equal(now) ||
					len(results) == 0 || !results[0].Asked || results[0].User != accounts[0].User {
					t.Fatalf("confirmed first claim was lost: %+v, results %+v", confirmed, results)
				}
				current := stored[accounts[1].key]
				if phase == "vendor-timeout" {
					if ctx.Err() != nil || current.Outcome != CheckinFailed || !current.Offline || len(results) != 2 {
						t.Fatalf("ordinary vendor timeout was discarded: %+v, results %+v", current, results)
					}
					return
				}
				wantCalls := 1
				if strings.HasPrefix(phase, "claim") || phase == "workbuddy-401" {
					wantCalls = 2
				}
				if ctx.Err() != context.Canceled || len(calls) != wantCalls {
					t.Fatalf("unexpected second account calls: %v (%v)", calls, ctx.Err())
				}
				if current != prior || len(results) != 1 {
					t.Fatalf("canceled second account replaced saved claim: %+v, results %+v", current, results)
				}
			})
		}
	}
}

func builtinCheckinCancelFixture(t *testing.T, vendor string, now time.Time, first, second func(*http.Request) (*http.Response, error)) checkiner {
	t.Helper()
	var runner checkiner
	switch vendor {
	case "workbuddy":
		fake := &fakeWBCheckin{active: true}
		fixture := wbCheckinFixture(t, fake.serve(t), &now)
		account := fixture.accounts()[0]
		account.via = first
		other := account
		other.User, other.creds.UID, other.via = "second", "u2", second
		fixture.accounts = func() []wbAccount { return []wbAccount{account, other} }
		runner = fixture.checkiner()
	case "minimax":
		(&fakeMiniMaxCheckin{today: mmDayClaimable}).serve(t)
		runner = newMiniMaxCheckiner(func() []miniMaxAccount {
			return []miniMaxAccount{{User: "first", On: true, uid: "u1", via: first}, {User: "second", On: true, uid: "u2", via: second}}
		})
	case "trae":
		(&fakeTraeCheckin{enable: true}).serve(t)
		runner = newTraeCheckiner(func() []traeAccount {
			return []traeAccount{{User: "first", On: true, uid: "u1", via: first}, {User: "second", On: true, uid: "u2", via: second}}
		})
	case "qoder":
		(&fakeQoderCampaigns{status: "CLAIMABLE", now: func() time.Time { return now }}).serve(t, "qoder")
		runner = newQoderCheckiner(func() []qoderCheckinAcct {
			return []qoderCheckinAcct{{User: "first", On: true, site: "qoder", uid: "u1", via: first}, {User: "second", On: true, site: "qoder", uid: "u2", via: second}}
		}, func() time.Time { return now })
	default:
		t.Fatalf("unknown vendor %q", vendor)
	}
	runner.path = filepath.Join(t.TempDir(), "checkin.json")
	runner.now = func() time.Time { return now }
	return runner
}

func TestCheckinDefersHistoryMigrationWhenNextAccountCancels(t *testing.T) {
	signIn(t)
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	prior := WorkBuddyCheckin{Day: wbCheckinDay(now), At: now, Outcome: CheckinClaimed, Credit: 41, Claimed: now}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := checkiner{
		path: filepath.Join(t.TempDir(), "checkin.json"), mu: &sync.Mutex{}, now: func() time.Time { return now },
		legacy: legacyCheckinKeys{"legacy-other": {"current-other"}},
		accounts: func() []checkinAcct {
			return []checkinAcct{
				{User: "first", key: "first", On: true, do: func(context.Context) WorkBuddyCheckin {
					return WorkBuddyCheckin{Outcome: CheckinClaimed, Credit: 6}
				}},
				{User: "second", key: "second", On: true, do: func(context.Context) WorkBuddyCheckin {
					cancel()
					return WorkBuddyCheckin{}
				}},
			}
		},
	}
	writeFile(t, runner.path, map[string]WorkBuddyCheckin{"legacy-other": prior})
	leaveOut(t, "legacy-other")
	runner.checkinNow(ctx, true)
	stored, err := readCheckins(runner.path)
	if err != nil || stored["first"].Outcome != CheckinClaimed || stored["first"].Credit != 6 {
		t.Fatalf("confirmed claim was lost: %+v (%v)", stored, err)
	}
	if stored["legacy-other"] != prior || len(stored) != 2 {
		t.Errorf("canceled pass published the unrelated history migration: %+v", stored)
	}
	if off := settings.Load().CheckinOff; !slices.Equal(off, []string{"legacy-other"}) {
		t.Errorf("canceled pass migrated the unrelated exclusion: %v", off)
	}
	runner.only = "current-other"
	runner.checkinNow(t.Context(), true)
	stored, err = readCheckins(runner.path)
	if err != nil || stored["current-other"] != prior || len(stored) != 2 {
		t.Fatalf("completed pass did not migrate the earlier history: %+v (%v)", stored, err)
	}
	if off := settings.Load().CheckinOff; !slices.Equal(off, []string{"current-other"}) {
		t.Fatalf("completed pass did not migrate the earlier exclusion: %v", off)
	}
}

func TestQoderCheckinRetainsPartialClaimOnCancel(t *testing.T) {
	var first qoderCampaign
	if err := json.Unmarshal([]byte(qoderDailyCampaignJSON), &first); err != nil {
		t.Fatal(err)
	}
	first.ClaimStatus = "CLAIMABLE"
	second, third := first, first
	second.ID, third.ID = "daily-2", "daily-3"
	now := time.Unix(int64(first.StartAt)+1, 0).UTC()
	list, err := json.Marshal(map[string]any{"campaigns": []qoderCampaign{first, second, third}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	claims := 0
	account := qoderCheckinAcct{User: "arno", On: true, site: "qoder", uid: "u1", via: func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(list)))}, nil
		}
		claims++
		if claims == 1 {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":{"status":"CLAIMED","benefit":{"amount":100}}}`))}, nil
		}
		cancel()
		return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: req.Context().Err()}
	}}
	runner := qoderCheckinFixture(t, &now, account)
	results := runner.checkinNow(ctx, true)
	stored, err := readCheckins(runner.path)
	claim := stored[qoderCheckinKey(account)]
	if err != nil || len(results) != 1 || results[0].Outcome != CheckinClaimed ||
		claim.Outcome != CheckinClaimed || claim.Credit != 100 || !claim.Claimed.Equal(now) || claims != 2 {
		t.Fatalf("canceled second campaign lost the first claim or started the third: %+v, results %+v, claims %d (%v)", claim, results, claims, err)
	}
}
