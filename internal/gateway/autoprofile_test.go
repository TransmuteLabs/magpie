package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
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
	incident := readAutoProfileIncident(t, destination)
	if incident.PID != os.Getpid() || incident.Version != "test" || incident.BaselineAt == nil ||
		!incident.BaselineAt.Equal(at) || !incident.Recovered || incident.End == nil || !incident.End.At.Equal(recovery.At) ||
		incident.MemoryPeak.Resident != higher.Resident || !incident.MemoryPeak.At.Equal(higher.At) ||
		incident.CPUPeak.CPUPercent != trigger.CPUPercent {
		t.Fatalf("wrong capture metadata: %+v", incident)
	}
	for name, wantAt := range map[string]time.Time{profileBeforeFile: at, profilePeakFile: peak.At, profileAfterFile: recovery.At} {
		status := incident.HeapProfiles[name]
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if status.Attempt == nil || status.Attempt.Error != "" || !status.Attempt.At.Equal(wantAt) ||
			status.Attempt.Bytes != info.Size() || status.File == nil || !status.File.At.Equal(wantAt) ||
			status.File.Bytes != info.Size() || status.CopyError != "" {
			t.Fatalf("wrong heap status for %s: %+v", name, status)
		}
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
	dir := t.TempDir()
	stale := filepath.Join(dir, profileBaselineFile+".part")
	if err := os.WriteFile(stale, []byte("interrupted baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop, err := StartAutoProfile(dir, "test")
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
	assertAutoProfileAbsent(t, stale)
	assertAutoProfileAbsent(t, filepath.Join(dir, profileBaselineFile))
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

func TestAutoProfileRetainsLargeHeapProfiles(t *testing.T) {
	recorder := &autoProfileRecorder{dir: t.TempDir(), writeHeap: largeAutoHeapProfile(t)}
	at := time.Now()
	recorder.takeBaseline(at)
	trigger := profileSample{At: at.Add(time.Second), Resident: profileMemory}
	recorder.observe(trigger)
	if recorder.incident == nil {
		t.Fatal("memory threshold did not start a capture")
	}
	defer func() {
		if recorder.incident != nil {
			recorder.incident.unlock()
		}
	}()
	dir := recorder.incident.dir
	recorder.finish(profileSample{At: trigger.At.Add(profileFollow)}, true)
	for _, name := range []string{"heap-before.pb.gz", "heap-peak.pb.gz", "heap-after.pb.gz"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("missing large profile %s: %v", name, err)
		}
		if info.Size() < 9<<20 {
			t.Fatalf("%s has only %d compressed bytes", name, info.Size())
		}
		raw := readAutoProfile(t, path)
		if !bytes.Contains(raw, []byte("inuse_space")) || !bytes.Contains(raw, []byte("alloc_space")) {
			t.Fatalf("%s is missing heap profile sample types", name)
		}
	}
}

func TestAutoProfileReportsFailedPeakReplacement(t *testing.T) {
	recorder := &autoProfileRecorder{dir: t.TempDir()}
	trigger := profileSample{At: time.Now(), Resident: profileMemory}
	recorder.observe(trigger)
	if recorder.incident == nil {
		t.Fatal("memory threshold did not start a capture")
	}
	defer recorder.incident.unlock()
	dir := recorder.incident.dir
	path := filepath.Join(dir, profilePeakFile)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recorder.writeHeap = func(dst io.Writer) error {
		if err := fillAutoHeapProfile(dst); err != nil {
			return err
		}
		_, _ = io.WriteString(dst, "over the limit")
		return nil
	}
	peak := profileSample{At: trigger.At.Add(time.Second), Resident: trigger.Resident + profilePeakStep}
	recorder.observe(peak)
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatalf("failed replacement changed the saved peak: %v", err)
	}
	incident := readAutoProfileIncident(t, dir)
	status := incident.HeapProfiles[profilePeakFile]
	if !incident.MemoryPeak.At.Equal(peak.At) || status.Attempt == nil || !status.Attempt.At.Equal(peak.At) ||
		status.Attempt.Bytes != profileHeapBytes || status.Attempt.Error != errProfileHeapLimit.Error() {
		t.Fatalf("wrong failed peak attempt: %+v, sampled peak %+v", status.Attempt, incident.MemoryPeak)
	}
	if status.File == nil || !status.File.At.Equal(trigger.At) || status.File.Bytes != int64(len(original)) {
		t.Fatalf("earlier peak capture lost its identity: %+v", status.File)
	}
	assertAutoProfileAbsent(t, path+".part")
}

func TestAutoProfileKeepsPreviousBaselineAfterAnError(t *testing.T) {
	recorder := &autoProfileRecorder{dir: t.TempDir()}
	at := time.Now()
	recorder.takeBaseline(at)
	baseline := filepath.Join(recorder.dir, profileBaselineFile)
	original, err := os.ReadFile(baseline)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("heap sampler failed")
	attempts := 0
	recorder.writeHeap = func(io.Writer) error {
		attempts++
		return failure
	}
	failedAt := at.Add(profileBaseline)
	recorder.observe(profileSample{At: failedAt})
	recorder.observe(profileSample{At: failedAt.Add(profileTick)})
	if attempts != 1 {
		t.Fatalf("baseline retried before the next minute: %d attempts", attempts)
	}
	assertAutoProfileAbsent(t, baseline+".part")
	recorder.writeHeap = nil
	recorder.observe(profileSample{At: failedAt.Add(2 * profileTick), Resident: profileMemory})
	if recorder.incident == nil {
		t.Fatal("memory threshold did not start a capture")
	}
	defer recorder.incident.unlock()
	dir := recorder.incident.dir
	copied, err := os.ReadFile(filepath.Join(dir, profileBeforeFile))
	if err != nil || !bytes.Equal(copied, original) {
		t.Fatalf("older baseline was not saved: %v", err)
	}
	incident := readAutoProfileIncident(t, dir)
	status := incident.HeapProfiles[profileBeforeFile]
	if incident.BaselineAt == nil || !incident.BaselineAt.Equal(at) ||
		status.File == nil || !status.File.At.Equal(at) || status.File.Bytes != int64(len(original)) ||
		status.Attempt == nil || !status.Attempt.At.Equal(failedAt) || status.Attempt.Error != failure.Error() {
		t.Fatalf("wrong baseline status: file %+v, attempt %+v", status.File, status.Attempt)
	}
}

func TestAutoProfileDoesNotCopyOrRemoveAnotherRecordersBaseline(t *testing.T) {
	dir := t.TempDir()
	first := &autoProfileRecorder{dir: dir}
	second := &autoProfileRecorder{dir: dir}
	at := time.Now()
	first.takeBaseline(at)
	second.takeBaseline(at.Add(profileTick))
	first.clearBaseline()
	baseline := filepath.Join(dir, profileBaselineFile)
	if _, err := os.Stat(baseline); err != nil {
		t.Fatalf("other recorder's baseline was removed: %v", err)
	}
	first.observe(profileSample{At: at.Add(2 * profileTick), Resident: profileMemory})
	if first.incident == nil {
		t.Fatal("memory threshold did not start a capture")
	}
	defer first.incident.unlock()
	assertAutoProfileAbsent(t, filepath.Join(first.incident.dir, profileBeforeFile))
	incident := readAutoProfileIncident(t, first.incident.dir)
	status := incident.HeapProfiles[profileBeforeFile]
	if status.CopyError == "" || status.File != nil || incident.BaselineAt != nil {
		t.Fatalf("a replaced baseline was attributed to the first recorder: %+v", status)
	}
	second.clearBaseline()
	assertAutoProfileAbsent(t, baseline)
}

func TestAutoProfileHeapFileLimitAndAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), profilePeakFile)
	original := []byte("previous snapshot")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, size, err := writeHeapProfileFile(path, func(dst io.Writer) error {
		if err := fillAutoHeapProfile(dst); err != nil {
			return err
		}
		staged, err := os.Stat(path + ".part")
		if err != nil {
			return err
		}
		if staged.Size() != profileHeapBytes {
			return errors.New("profile bytes did not reach the staging file during capture")
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(saved, original) {
			return errors.New("profile changed before the replacement completed")
		}
		return nil
	})
	if err != nil || size != profileHeapBytes || info.Size() != profileHeapBytes {
		t.Fatalf("exact size limit was refused: %d bytes, %v", size, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("profile has mode %v", info.Mode())
	}
	assertAutoProfileAbsent(t, path+".part")
}

func TestAutoProfileHeapFileChecksWriterErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), profilePeakFile)
	original := []byte("previous snapshot")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, size, err := writeHeapProfileFile(path, func(dst io.Writer) error {
		if _, err := io.WriteString(dst, "part"); err != nil {
			return err
		}
		if err := dst.(*profileHeapWriter).file.(*os.File).Close(); err != nil {
			return err
		}
		_, _ = io.WriteString(dst, "after close")
		return nil
	})
	if !errors.Is(err, os.ErrClosed) || size != 4 {
		t.Fatalf("expected a write failure after 4 bytes, got %d bytes, %v", size, err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(saved, original) {
		t.Fatalf("failed write changed the saved profile: %v", err)
	}
	assertAutoProfileAbsent(t, path+".part")
}

func fillAutoHeapProfile(dst io.Writer) error {
	chunk := strings.Repeat("a", 32<<10)
	for range profileHeapBytes / len(chunk) {
		if _, err := io.WriteString(dst, chunk); err != nil {
			return err
		}
	}
	return nil
}

func largeAutoHeapProfile(t *testing.T) func(io.Writer) error {
	t.Helper()
	var captured bytes.Buffer
	if err := pprof.WriteHeapProfile(&captured); err != nil {
		t.Fatal(err)
	}
	heap, err := gzip.NewReader(&captured)
	if err != nil {
		t.Fatal(err)
	}
	defer heap.Close()
	profile, err := io.ReadAll(heap)
	if err != nil {
		t.Fatal(err)
	}
	const stringTableField = 6<<3 | 2
	const extra = 9 << 20
	padding := strings.Repeat("a", 32<<10)
	return func(dst io.Writer) error {
		gzipper, err := gzip.NewWriterLevel(dst, gzip.NoCompression)
		if err != nil {
			return err
		}
		defer gzipper.Close()
		if _, err := gzipper.Write(profile); err != nil {
			return err
		}
		if _, err := gzipper.Write(binary.AppendUvarint([]byte{stringTableField}, extra)); err != nil {
			return err
		}
		for range extra / len(padding) {
			if _, err := io.WriteString(gzipper, padding); err != nil {
				return err
			}
		}
		return gzipper.Close()
	}
}

func readAutoProfileIncident(t *testing.T, dir string) profileIncident {
	t.Helper()
	metadata, err := os.ReadFile(filepath.Join(dir, "incident.json"))
	if err != nil {
		t.Fatal(err)
	}
	var incident profileIncident
	if err := json.Unmarshal(metadata, &incident); err != nil {
		t.Fatal(err)
	}
	return incident
}

func assertAutoProfileAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent, got %v", path, err)
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
