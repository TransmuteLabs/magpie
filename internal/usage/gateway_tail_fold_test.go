package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestListedGatewayTailFoldDoesNotCopySealedMap(t *testing.T) {
	pageHome(t)
	settleLogClock(t)
	now := holdClock(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local))
	const sessionCount = 8 * logBlockRows
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	encode := json.NewEncoder(file)
	for index := range sessionCount + logBlockRows - 3 {
		record := tailFoldRecord(now, index)
		if index >= sessionCount {
			record.Session = ""
		}
		if err := encode.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	alias := provider.UsageAlias()
	checkList := func(wantSize int) *logSnapshot {
		t.Helper()
		listed := ListedGateway(nil, 3)
		snapshot, entry := logIndex.snapshot, gatewayCache.entry
		if snapshot == nil || !snapshot.complete() || !snapshot.settled ||
			entry == nil || entry.version != snapshot.version || len(entry.sessions) != wantSize {
			t.Fatalf("no current settled projection of %d sessions", wantSize)
		}
		want, _ := buildGatewaySessions(snapshot, time.Time{}, nil, alias)
		if !reflect.DeepEqual(entry.sessions, want) {
			t.Fatal("projection differs from the fresh ordered sessions, models and days")
		}
		if len(listed) != 3 {
			t.Fatalf("listed %d sessions, want 3", len(listed))
		}
		for index, row := range listed {
			if !reflect.DeepEqual(row, want[index].External()) {
				t.Fatalf("listed row %d differs from the fresh projection", index)
			}
		}
		return snapshot
	}
	emptyTail := checkList(sessionCount)
	prefix, original := gatewayCache.prefix, gatewayCache.entry
	if prefix.n != 8 || len(prefix.built) != sessionCount || len(emptyTail.blocks) != prefix.n+1 {
		t.Fatal("fixture must have eight sealed blocks and a filtered tail")
	}
	saved := make(map[gatewayKey]GatewaySession, len(prefix.built))
	for key, session := range prefix.built {
		saved[key] = cloneGatewaySession(session)
	}
	checkPrefix := func() {
		t.Helper()
		if !reflect.DeepEqual(prefix.built, saved) {
			t.Fatal("the previously published sealed sessions changed")
		}
		for _, session := range original.sessions {
			if !reflect.DeepEqual(session, saved[gatewayKey{agent: session.Agent, id: session.ID}]) {
				t.Fatal("a previously published projection row changed")
			}
		}
	}

	update := tailFoldRecord(now, 0)
	update.Time = now.AddDate(0, 0, 1)
	Append(update)
	activeTail := checkList(sessionCount)
	if gatewayCache.prefix != prefix || len(activeTail.blocks) != prefix.n+1 ||
		activeTail.blocks[prefix.n-1] != prefix.last ||
		activeTail.blocks[prefix.n].Count != emptyTail.blocks[prefix.n].Count+1 {
		t.Fatal("the two allocation arms must share the same sealed prefix")
	}
	if row, ok := GatewaySessionByID(update.Agent, update.Session, nil); !ok || row.Input != 2*update.Input || len(row.Daily) != 2 {
		t.Fatal("the tail update did not replace the sealed row with both days")
	}
	checkPrefix()

	fold := func(snapshot *logSnapshot) {
		rows, next := foldGateway(snapshot, prefix.meta, alias, prefix.agents, prefix)
		if len(rows) != sessionCount || cap(rows) != sessionCount || next != prefix {
			t.Fatal("measured fold changed output capacity or sealed prefix")
		}
		runtime.KeepAlive(rows)
	}
	fold(emptyTail)
	fold(activeTail)
	control := tailFoldBytes(func() { fold(emptyTail) })
	active := tailFoldBytes(func() { fold(activeTail) })

	newSession := tailFoldRecord(now, sessionCount)
	Append(newSession)
	checkList(sessionCount + 1)
	if gatewayCache.prefix != prefix {
		t.Fatal("the new tail session published a sealed prefix early")
	}
	Append(update)
	checkList(sessionCount + 1)
	if gatewayCache.prefix != prefix {
		t.Fatal("the final block must stay reopenable at its exact boundary")
	}
	Append(update)
	checkList(sessionCount + 1)
	sealed := gatewayCache.prefix
	if sealed == prefix || sealed.n != prefix.n+1 || len(sealed.built) != sessionCount+1 {
		t.Fatal("crossing the block boundary did not publish a new sealed map")
	}
	checkPrefix()

	limit := control + control/4
	t.Logf("tail fold bytes per rebuild: filtered=%d active=%d limit=%d", control, active, limit)
	if active > limit {
		t.Fatalf("tail fold allocated %d bytes with one updated session, budget %d over filtered control %d", active, limit, control)
	}
}

func tailFoldRecord(now time.Time, index int) Record {
	return Record{
		Time: now.Add(-time.Duration(index+1) * time.Second), Agent: "codex",
		Session: fmt.Sprintf("fold-%04d", index), Provider: "openai", Model: "m",
		Input: 10, Output: 3, Status: 200,
	}
}

func tailFoldBytes(fold func()) uint64 {
	const repeats = 6
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range repeats {
		fold()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / repeats
}
