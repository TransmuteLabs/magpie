package gateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/usage"
)

func TestClassifierCanceledTurnDoesNotRestFreshSameAccount(t *testing.T) {
	fixture := newClassifierScopeFixture(t, true)
	firstCtx, cancelFirst := context.WithCancelCause(t.Context())
	finished := make(chan struct{})
	var first *httptest.ResponseRecorder
	go func() {
		defer close(finished)
		first = classifierScopeFront(firstCtx, fixture.server, fixture.secrets[0], "scope", "cancel-owner", "cancel this dispatched judge")
	}()
	t.Cleanup(func() {
		cancelFirst(context.Canceled)
		fixture.once.Do(func() { close(fixture.release) })
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("canceled classifier request did not join")
		}
	})
	select {
	case account := <-fixture.started:
		if account != "scope-a" {
			t.Fatalf("first judge selected account %q", account)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first judge did not dispatch")
	}
	cancelFirst(errors.New("the caller stopped the first turn"))
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("first judge did not honor live cancellation")
	}
	if first.Body.Len() != 0 || !slices.Equal(fixture.took(), []string{"scope-a"}) {
		t.Fatalf("canceled first turn replied or resent: response=%d %s sends=%v", first.Code, first.Body, fixture.took())
	}
	var judgeRecords []usage.Record
	for _, record := range usage.Load(time.Time{}) {
		if record.Provider == "scope-judge" {
			judgeRecords = append(judgeRecords, record)
		}
	}
	if len(judgeRecords) != 1 || judgeRecords[0].Failed() || !usage.CanceledBy(judgeRecords[0].Status, judgeRecords[0].Error) {
		t.Fatalf("first turn was not logged as caller cancellation: %+v", judgeRecords)
	}
	fixture.once.Do(func() { close(fixture.release) })
	nextCtx, cancelNext := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelNext()
	second := classifierScopeFront(nextCtx, fixture.server, fixture.secrets[0], "scope", "fresh-after-cancel", "a different fresh question after cancellation")
	route := lastRoute(fixture.server)
	detail := ""
	if route.Rule != nil && route.Rule.Classified != nil {
		detail = route.Rule.Classified.Error
	}
	sent := fixture.took()
	if !slices.Equal(sent, []string{"scope-a", "scope-a"}) {
		t.Fatalf("fresh same-account turn did not dispatch its judge after joined caller cancellation: sends=%v status=%d classifier=%q body=%s", sent, second.Code, detail, second.Body)
	}
	if second.Code != 200 || !strings.Contains(second.Body.String(), "default") || strings.Contains(detail, "not asked again") {
		t.Fatalf("fresh judge refusal did not follow ordinary fallback: status=%d classifier=%q body=%s", second.Code, detail, second.Body)
	}
}
