package gateway

import (
	"net/http"
	"os"
	"runtime/metrics"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
)

const heapStatsPath = "/v1/magpie/heap/stats"
const heapStatsCall = "curl --noproxy '*' http://127.0.0.1:3425" + heapStatsPath

type heapSnapshot struct {
	PID               int       `json:"pid"`
	Version           string    `json:"version"`
	ProcessStartedAt  time.Time `json:"processStartedAt"`
	SampleStartedAt   time.Time `json:"sampleStartedAt"`
	SampleFinishedAt  time.Time `json:"sampleFinishedAt"`
	HeapObjectBytes   uint64    `json:"heapObjectBytes"`
	HeapInUseBytes    uint64    `json:"heapInUseBytes"`
	HeapIdleBytes     uint64    `json:"heapIdleBytes"`
	HeapReleasedBytes uint64    `json:"heapReleasedBytes"`
	HeapSysBytes      uint64    `json:"heapSysBytes"`
	RuntimeSysBytes   uint64    `json:"runtimeSysBytes"`
	LastGCLiveBytes   uint64    `json:"lastGCLiveBytes"`
	GCCycles          uint64    `json:"gcCycles"`
	ForcedGCCycles    uint64    `json:"forcedGCCycles"`
	Goroutines        uint64    `json:"goroutines"`
}

func heapStatsAllowed(r *http.Request) bool {
	return r.URL.EscapedPath() == heapStatsPath && !forwarded(r) &&
		access.Caller(r.Context()).KeyID == "" && callerKey(r) == "" &&
		profileRefused(r, "heap stats", heapStatsCall) == ""
}

func refuseHeapStats(w http.ResponseWriter, _ *http.Request) {
	writeError(w, provider.Chat, http.StatusForbidden, "heap stats require an unkeyed call directly to the loopback gateway: run "+heapStatsCall)
}

func writeHeapStats(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	pid := os.Getpid()
	started, err := proc.Started(pid)
	if err != nil {
		writeError(w, provider.Chat, http.StatusInternalServerError, "the process start time couldn't be read")
		return
	}
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/unused:bytes"},
		{Name: "/memory/classes/heap/free:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/heap/live:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/gc/cycles/forced:gc-cycles"},
		{Name: "/sched/goroutines:goroutines"},
	}
	sampleStart := time.Now().UTC()
	metrics.Read(samples)
	sampleEnd := time.Now().UTC()
	objects := samples[0].Value.Uint64()
	inUse := objects + samples[1].Value.Uint64()
	released := samples[3].Value.Uint64()
	idle := samples[2].Value.Uint64() + released
	writeJSON(w, http.StatusOK, heapSnapshot{
		PID:               pid,
		Version:           Version,
		ProcessStartedAt:  started.UTC(),
		SampleStartedAt:   sampleStart,
		SampleFinishedAt:  sampleEnd,
		HeapObjectBytes:   objects,
		HeapInUseBytes:    inUse,
		HeapIdleBytes:     idle,
		HeapReleasedBytes: released,
		HeapSysBytes:      inUse + idle,
		RuntimeSysBytes:   samples[4].Value.Uint64(),
		LastGCLiveBytes:   samples[5].Value.Uint64(),
		GCCycles:          samples[6].Value.Uint64(),
		ForcedGCCycles:    samples[7].Value.Uint64(),
		Goroutines:        samples[8].Value.Uint64(),
	})
}
