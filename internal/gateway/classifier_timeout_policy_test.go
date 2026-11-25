package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/usage"
)

func TestClassifierLiveParentCancellationThroughFront(t *testing.T) {
	fixture := newClassifierScopeFixture(t, true)
	ctx, cancel := context.WithCancelCause(t.Context())
	finished := make(chan struct{})
	var response *httptest.ResponseRecorder
	go func() {
		defer close(finished)
		response = classifierScopeFront(ctx, fixture.server, fixture.secrets[0], "scope", "live-cancel", "cancel the running classifier")
	}()
	t.Cleanup(func() {
		cancel(context.Canceled)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("the canceled classifier request did not finish")
		}
	})
	select {
	case account := <-fixture.started:
		if account != "scope-a" {
			t.Fatalf("classifier selected account %q", account)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the classifier did not reach its selected account")
	}
	cancel(errors.New("the caller stopped the turn"))
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the classifier ignored its live caller cancellation")
	}
	if sent := fixture.took(); len(sent) != 1 || sent[0] != "scope-a" {
		t.Fatalf("canceled classifier sent more requests: %v", sent)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("the canceled request produced a reply: %d %s", response.Code, response.Body.String())
	}
	var records []usage.Record
	for _, record := range usage.Load(time.Time{}) {
		if record.Provider == "scope-judge" {
			records = append(records, record)
		}
	}
	if len(records) != 1 || !usage.CanceledBy(records[0].Status, records[0].Error) || records[0].Failed() {
		t.Fatalf("live caller cancellation was logged as a classifier failure: %+v", records)
	}
}

func TestClassifierTimeoutCauseRetainsParentAndPolicy(t *testing.T) {
	for _, ending := range []string{"classifier", "caller", "parent-deadline"} {
		t.Run(ending, func(t *testing.T) {
			fresh(t)
			started := make(chan struct{})
			var sends atomic.Int32
			upstream := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				io.Copy(io.Discard, request.Body)
				if sends.Add(1) == 1 {
					close(started)
				}
				<-request.Context().Done()
			})))
			t.Cleanup(upstream.Close)
			upstream.Client().Timeout = 5 * time.Second
			probeFixtureRoot(t, upstream)
			if err := provider.Save(provider.Provider{ID: "timeout-judge", Name: "Timeout judge",
				Key: "judge", Chat: upstream.URL + "/v1", Models: []string{"judge"}}); err != nil {
				t.Fatal(err)
			}
			server := New()
			t.Cleanup(server.idle.stop)
			parent, cancel := context.WithCancelCause(ownPolicyContext(t, 0))
			t.Cleanup(func() { cancel(context.Canceled) })
			ctx := parent
			wait := 2 * time.Second
			switch ending {
			case "classifier":
				wait = 500 * time.Millisecond
			case "parent-deadline":
				var leave context.CancelFunc
				ctx, leave = context.WithTimeoutCause(parent, 500*time.Millisecond, errors.New("the parent's deadline"))
				t.Cleanup(leave)
			}
			finished := make(chan struct{})
			var result error
			go func() {
				defer close(finished)
				_, result = server.askLeast(ctx, wait, false, "timeout-judge/judge", func(effort string) []byte {
					return classifyBody("timeout-judge/judge", effort, []string{"bug"}, before{}, "fix this")
				})
			}()
			t.Cleanup(func() {
				cancel(context.Canceled)
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("the classifier did not join after cancellation")
				}
			})
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("the classifier did not dispatch")
			}
			if ending == "caller" {
				cancel(errors.New("the caller stopped the classifier"))
			}
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("the classifier ignored its deadline")
			}
			if result == nil || sends.Load() != 1 {
				t.Fatalf("ended classifier returned %v after %d sends", result, sends.Load())
			}
			if root, selected := requestpolicy.FromContext(parent).Counts("timeout-judge"); root != 0 || selected != 0 {
				t.Fatalf("ending the classifier changed retry permits: %d/%d", root, selected)
			}
			records := usage.Load(time.Time{})
			if len(records) != 1 || records[0].Provider != "timeout-judge" || records[0].Status != usage.StatusCanceled {
				t.Fatalf("the classifier's canceled usage record: %+v", records)
			}
			record := records[0]
			own := ending == "classifier"
			if record.Failed() != own || usage.CanceledBy(record.Status, record.Error) == own {
				t.Fatalf("%s has the wrong cancellation owner: %d %q", ending, record.Status, record.Error)
			}
			if own && !strings.Contains(record.Error, "magpie stopped waiting: its decision model had no answer in "+wait.String()) {
				t.Fatalf("classifier deadline lost its cause: %q", record.Error)
			}
		})
	}
}
