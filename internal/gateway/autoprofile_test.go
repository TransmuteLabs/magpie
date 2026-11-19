package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutoProfileKeepsHistoryAndWritesHeapProfiles(t *testing.T) {
	recorder := &autoProfileRecorder{dir: t.TempDir(), version: "test"}
	at := time.Now()
	recorder.takeBaseline(at)
	for range profileHistory + 2 {
		if err := recorder.startCPU(); err != nil {
			t.Fatal(err)
		}
		recorder.stopCPU()
	}
	if len(recorder.history) != profileHistory {
		t.Fatalf("history contains %d slices", len(recorder.history))
	}
	trigger := profileSample{At: at.Add(time.Second), CPUKnown: true, CPUPercent: profileCPUPercent}
	if recorder.observe(trigger) || recorder.incident == nil {
		t.Fatal("CPU threshold did not start a capture")
	}
	destination := recorder.incident.dir
	t.Cleanup(func() {
		if recorder.incident != nil {
			recorder.incident.unlock()
		}
	})
	for _, slice := range recorder.history {
		raw := readAutoProfile(t, filepath.Join(destination, slice.name))
		if !bytes.Contains(raw, []byte("nanoseconds")) {
			t.Fatalf("%s is not a CPU profile", slice.name)
		}
	}
	if recorder.observe(profileSample{At: trigger.At.Add(profileFollow - time.Second)}) {
		t.Fatal("capture ended before the follow-up window")
	}
	peak := profileSample{At: trigger.At.Add(profileFollow), Resident: profileMemory + profilePeakStep}
	if recorder.observe(peak) {
		t.Fatal("capture ended while memory was still high")
	}
	higher := profileSample{At: peak.At.Add(time.Second), Resident: peak.Resident + profilePeakStep/2}
	peakPath := filepath.Join(destination, "heap-peak.pb.gz")
	heapAtPeak, err := os.ReadFile(peakPath)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.observe(higher) {
		t.Fatal("capture ended at a higher memory sample")
	}
	heapAtHigher, err := os.ReadFile(peakPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(heapAtPeak, heapAtHigher) {
		t.Fatal("a small memory increase rewrote the heap profile")
	}
	if err := recorder.startCPU(); err != nil {
		t.Fatal(err)
	}
	recorder.stopCPU()
	recovery := profileSample{At: higher.At.Add(time.Second)}
	if !recorder.observe(recovery) {
		t.Fatal("capture did not finish after recovery")
	}
	recorder.finish(recovery, true)
	for _, phase := range []string{"before", "peak", "after"} {
		raw := readAutoProfile(t, filepath.Join(destination, "heap-"+phase+".pb.gz"))
		for _, metric := range []string{"inuse_space", "alloc_space"} {
			if !bytes.Contains(raw, []byte(metric)) {
				t.Errorf("%s is missing %s", phase, metric)
			}
		}
	}
	profiles, err := filepath.Glob(filepath.Join(destination, "cpu-*.pb.gz"))
	if err != nil || len(profiles) != profileHistory+1 {
		t.Fatalf("recorded %d CPU slices, error %v", len(profiles), err)
	}
	metadata, err := os.ReadFile(filepath.Join(destination, "incident.json"))
	if err != nil {
		t.Fatal(err)
	}
	var incident profileIncident
	if err := json.Unmarshal(metadata, &incident); err != nil {
		t.Fatal(err)
	}
	if incident.PID != os.Getpid() || incident.Version != "test" || incident.BaselineAt == nil ||
		!incident.BaselineAt.Equal(at) || !incident.Recovered || incident.End == nil || !incident.End.At.Equal(recovery.At) ||
		incident.MemoryPeak.Resident != higher.Resident || !incident.MemoryPeak.At.Equal(higher.At) ||
		incident.CPUPeak.CPUPercent != trigger.CPUPercent {
		t.Fatalf("wrong capture metadata: %s", metadata)
	}
	recorder.observe(profileSample{At: recovery.At.Add(time.Second), HeapObjects: profileMemory})
	if recorder.incident != nil {
		recorder.incident.unlock()
		t.Fatal("started a capture during the cooldown")
	}
}

func TestAutoProfileDeadlineAndRetention(t *testing.T) {
	root := t.TempDir()
	unrelated := filepath.Join(root, "magpie-incident-other")
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	for index := range profileKeep + 2 {
		recorder := &autoProfileRecorder{dir: root}
		trigger := profileSample{At: at.Add(time.Duration(index) * time.Minute), HeapObjects: profileMemory}
		recorder.observe(trigger)
		if recorder.incident == nil {
			t.Fatal("Go heap threshold did not start a capture")
		}
		end := trigger
		end.At = end.At.Add(profileDeadline)
		finished := recorder.observe(end)
		recorder.finish(end, false)
		if !finished {
			t.Fatal("sustained spike exceeded the capture deadline")
		}
	}
	oldest, err := filepath.Glob(filepath.Join(root, profilePrefix+at.UTC().Format(profileTimeFormat)+"-*"))
	if err != nil || len(oldest) != 0 {
		t.Fatalf("old capture survived: %v, %v", oldest, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != profileKeep+2 {
		t.Fatalf("unexpected retained captures: %v, %v", entries, err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal(err)
	}
}

func TestAutoProfileKeepsForeignFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeProfileIncident(dir); err == nil {
		t.Fatal("removed a directory containing a foreign file")
	}
	if body, err := os.ReadFile(path); string(body) != "keep" || err != nil {
		t.Fatalf("foreign file changed: %q, %v", body, err)
	}
}

func TestAutoProfileReservesCPUAndReleasesItOnStop(t *testing.T) {
	if stop, err := StartAutoProfile("relative", "test"); err == nil {
		stop()
		t.Fatal("accepted a relative profile directory")
	}
	stop, err := StartAutoProfile(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	request := httptest.NewRequest(http.MethodGet, "http://localhost"+CPUPath, nil)
	request.RemoteAddr = "127.0.0.1:50123"
	response := httptest.NewRecorder()
	(&Server{}).cpuProfile(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "MAGPIE_PROFILE_DIR") {
		t.Fatalf("concurrent manual profile returned %d %s", response.Code, response.Body.String())
	}
	stop()
	stop()
	if cpuRecording.Load() {
		t.Fatal("CPU profiler still reserved after stop")
	}
}

func TestAutoProfileDropsOversizedProfiles(t *testing.T) {
	buffer := &profileBuffer{limit: 8}
	for _, chunk := range []string{"one", "twothree", "four"} {
		if size, err := buffer.Write([]byte(chunk)); size != len(chunk) || err != nil {
			t.Fatalf("write returned %d, %v", size, err)
		}
	}
	if !buffer.exceeded || buffer.Len() != 0 {
		t.Fatal("oversize profile was retained or truncated")
	}
}

func readAutoProfile(t *testing.T, path string) []byte {
	t.Helper()
	profile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Close()
	reader, err := gzip.NewReader(profile)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
