package gateway

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"
	"weak"
)

// TestRecentRingKeepsNoDroppedBodies: a call that has left the Recent-calls
// ring is no longer kept alive by it, bodies and all. Re-slicing the oldest
// off left the dropped calls in the ring's backing array, up to about as
// many again as the ring holds, each with up to 2×256 KiB of bodies.
func TestRecentRingKeepsNoDroppedBodies(t *testing.T) {
	s := &Server{}
	const calls = 3 * recentMost
	bodies := make([]weak.Pointer[byte], calls)
	for i := range calls {
		body := strings.Repeat("x", 4<<10) // its own allocation, for the weak pointer
		bodies[i] = weak.Make(unsafe.StringData(body))
		s.record(Call{Model: "m" + strconv.Itoa(i), RequestBody: body})
	}
	runtime.GC()
	runtime.GC()
	var kept []int
	for i := range calls - recentMost {
		if bodies[i].Value() != nil {
			kept = append(kept, i)
		}
	}
	if len(kept) > 0 {
		t.Fatalf("%d calls dropped from the ring are still alive: %v", len(kept), kept)
	}
	for i := calls - recentMost; i < calls; i++ {
		if bodies[i].Value() == nil {
			t.Fatalf("call %d, still in the ring, lost its body", i)
		}
	}
	got := s.Recent()
	if len(got) != recentMost {
		t.Fatalf("ring holds %d calls, want %d", len(got), recentMost)
	}
	for i, c := range got {
		if want := "m" + strconv.Itoa(calls-1-i); c.Model != want {
			t.Fatalf("Recent()[%d] = %s, want %s (newest first)", i, c.Model, want)
		}
		if c.ID != uint64(calls-i) {
			t.Fatalf("Recent()[%d].ID = %d, want %d", i, c.ID, calls-i)
		}
	}
	runtime.KeepAlive(s)
}

func TestRecentRingBoundsPreviewBytes(t *testing.T) {
	server := &Server{}
	const budget = 8 << 20
	const withPreviews = budget / (2 * callBodyLimit)
	for index := range recentMost + 1 {
		server.record(Call{
			Route:             int64(index + 1),
			Status:            200,
			RequestBody:       strings.Repeat("q", callBodyLimit),
			ResponseBody:      strings.Repeat("a", callBodyLimit),
			RequestTruncated:  true,
			ResponseTruncated: true,
		})
	}
	recent := server.Recent()
	if len(recent) != recentMost {
		t.Fatalf("got %d recent calls, want %d", len(recent), recentMost)
	}
	var keptBytes int
	for _, call := range recent {
		keptBytes += len(call.RequestBody) + len(call.ResponseBody)
	}
	if keptBytes != budget {
		t.Fatalf("ring keeps %d preview bytes, want %d", keptBytes, budget)
	}
	for index, call := range recent {
		wantID := uint64(recentMost + 1 - index)
		byRoute, found := server.RouteCall(int64(wantID))
		if !found || byRoute.ID != wantID || call.ID != wantID || call.Status != 200 {
			t.Fatalf("metadata or route lookup lost for call %d", wantID)
		}
		wantSize := 0
		if index < withPreviews {
			wantSize = callBodyLimit
		}
		if len(byRoute.RequestBody) != wantSize || len(byRoute.ResponseBody) != wantSize {
			t.Fatalf("wrong previews kept for call %d", wantID)
		}
		if want := wantSize != 0; byRoute.RequestTruncated != want || byRoute.ResponseTruncated != want {
			t.Fatalf("wrong truncation flags for call %d", wantID)
		}
	}
}

func TestRecentRingDropsObserverInput(t *testing.T) {
	server := &Server{}
	observer := &laterMasked{plain: make([]byte, 2*callBodyLimit)}
	call := Call{Status: 200, RequestBody: "preview", maskIn: observer}
	server.record(call)

	got, found := server.RecentCall(1)
	if !found || got.RequestBody != call.RequestBody {
		t.Fatal("recent call lost its request preview")
	}
	if got.maskIn != nil {
		t.Fatal("recent call retains the observer's full request and context")
	}
	if call.maskIn != observer || len(observer.plain) != 2*callBodyLimit {
		t.Fatal("record changed the observer's original request")
	}
}
