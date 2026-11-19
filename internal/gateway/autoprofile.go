package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
)

const (
	profileTick       = time.Second
	profileSlice      = 15 * time.Second
	profileHistory    = 8
	profileBaseline   = time.Minute
	profileFollow     = 30 * time.Second
	profileDeadline   = 90 * time.Second
	profileCooldown   = 2 * time.Minute
	profileCPUPercent = 85
	profileMemory     = 1536 << 20
	profilePeakStep   = 128 << 20
	profileCPUBytes   = 1 << 20
	profileHeapBytes  = 4 << 20
	profileKeep       = 3
	profilePrefix     = "magpie-incident-"
	profileTimeFormat = "20060102T150405.000000000Z"
)

var autoProfiling atomic.Bool

type profileBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buf *profileBuffer) Write(data []byte) (int, error) {
	if buf.exceeded || len(data) > buf.limit-buf.Len() {
		buf.exceeded = true
		buf.Buffer = bytes.Buffer{}
		return len(data), nil
	}
	return buf.Buffer.Write(data)
}

type profileSample struct {
	At          time.Time `json:"at"`
	CPUPercent  float64   `json:"cpuPercent"`
	CPUKnown    bool      `json:"cpuKnown"`
	Resident    int64     `json:"residentBytes"`
	HeapObjects uint64    `json:"heapObjectBytes"`
}

func (sample profileSample) memory() uint64 {
	return max(uint64(max(sample.Resident, 0)), sample.HeapObjects)
}

func (sample profileSample) high() bool {
	return sample.CPUPercent >= profileCPUPercent || sample.memory() >= profileMemory
}

type profileSliceData struct {
	name string
	body []byte
}

type profileIncident struct {
	PID        int            `json:"pid"`
	Version    string         `json:"version"`
	BaselineAt *time.Time     `json:"baselineAt,omitempty"`
	Trigger    profileSample  `json:"trigger"`
	MemoryPeak profileSample  `json:"memoryPeak"`
	CPUPeak    profileSample  `json:"cpuPeak"`
	End        *profileSample `json:"end,omitempty"`
	Recovered  bool           `json:"recovered"`

	dir            string
	unlock         func()
	heapPeakMemory uint64
}

type autoProfileRecorder struct {
	dir          string
	version      string
	current      *profileBuffer
	currentSince time.Time
	history      []profileSliceData
	baseline     []byte
	baselineAt   time.Time
	nextCapture  time.Time
	incident     *profileIncident
}

// StartAutoProfile records rolling profiles until the returned function is called.
func StartAutoProfile(dir, version string) (func(), error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("MAGPIE_PROFILE_DIR must be an absolute directory path")
	}
	if !cpuRecording.CompareAndSwap(false, true) {
		return nil, errors.New("magpie is recording a CPU profile already")
	}
	recorder := autoProfileRecorder{dir: dir, version: version}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		cpuRecording.Store(false)
		return nil, err
	}
	if err := recorder.startCPU(); err != nil {
		cpuRecording.Store(false)
		return nil, err
	}
	autoProfiling.Store(true)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer cpuRecording.Store(false)
		defer autoProfiling.Store(false)
		recorder.run(stop)
	}()
	return sync.OnceFunc(func() {
		close(stop)
		<-done
	}), nil
}

func (recorder *autoProfileRecorder) startCPU() error {
	buffer := &profileBuffer{limit: profileCPUBytes}
	if err := pprof.StartCPUProfile(buffer); err != nil {
		return err
	}
	recorder.current = buffer
	recorder.currentSince = time.Now()
	return nil
}

func (recorder *autoProfileRecorder) stopCPU() {
	pprof.StopCPUProfile()
	buffer := recorder.current
	recorder.current = nil
	if buffer.exceeded {
		log.Printf("auto profile: CPU slice exceeded %d bytes", profileCPUBytes)
		return
	}
	slice := profileSliceData{
		name: "cpu-" + recorder.currentSince.UTC().Format(profileTimeFormat) + ".pb.gz",
		body: buffer.Bytes(),
	}
	if len(recorder.history) == profileHistory {
		copy(recorder.history, recorder.history[1:])
		recorder.history = recorder.history[:profileHistory-1]
	}
	recorder.history = append(recorder.history, slice)
	if recorder.incident != nil {
		recorder.incident.write(slice.name, slice.body)
	}
}

func (recorder *autoProfileRecorder) run(stop <-chan struct{}) {
	clock := time.NewTicker(profileTick)
	defer clock.Stop()
	lastAt := time.Now()
	lastCPU, lastKnown := processCPU()
	gauges := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	recorder.takeBaseline(lastAt)
	var sample profileSample
	for {
		select {
		case <-stop:
			recorder.stopCPU()
			if recorder.incident != nil {
				recorder.finish(sample, false)
			}
			return
		case <-clock.C:
		}

		at := time.Now()
		cpu, known := processCPU()
		metrics.Read(gauges)
		resident, _ := proc.SelfRSS()
		sample = profileSample{At: at, Resident: resident, HeapObjects: gauges[0].Value.Uint64()}
		if known && lastKnown && cpu >= lastCPU {
			sample.CPUKnown = true
			sample.CPUPercent = 100 * float64(cpu-lastCPU) / float64(at.Sub(lastAt))
		}
		lastAt, lastCPU, lastKnown = at, cpu, known

		wasCapturing := recorder.incident != nil
		finish := recorder.observe(sample)
		begun := !wasCapturing && recorder.incident != nil
		if begun || finish || time.Since(recorder.currentSince) >= profileSlice {
			recorder.stopCPU()
			if finish {
				runtime.GC()
				recorder.finish(sample, !sample.high())
				lastAt = time.Now()
				lastCPU, lastKnown = processCPU()
			}
			if err := recorder.startCPU(); err != nil {
				log.Printf("auto profile: %v", err)
				if recorder.incident != nil {
					recorder.finish(sample, false)
				}
				return
			}
		}
	}
}

func (recorder *autoProfileRecorder) observe(sample profileSample) bool {
	if incident := recorder.incident; incident != nil {
		memory := sample.memory()
		if memory > incident.MemoryPeak.memory() {
			incident.MemoryPeak = sample
		}
		if memory >= incident.heapPeakMemory+profilePeakStep {
			incident.heapPeakMemory = memory
			incident.write("heap-peak.pb.gz", autoHeap())
			incident.save()
		}
		if sample.CPUPercent > incident.CPUPeak.CPUPercent {
			incident.CPUPeak = sample
		}
		age := sample.At.Sub(incident.Trigger.At)
		return age >= profileDeadline || (age >= profileFollow && !sample.high())
	}
	if !sample.high() {
		if sample.At.Sub(recorder.baselineAt) >= profileBaseline {
			recorder.takeBaseline(sample.At)
		}
		return false
	}
	if sample.At.Before(recorder.nextCapture) {
		return false
	}
	incident, err := openProfileIncident(recorder.dir, recorder.version, sample)
	if err != nil {
		log.Printf("auto profile: %v", err)
		recorder.nextCapture = sample.At.Add(profileCooldown)
		return false
	}
	recorder.incident = incident
	if !recorder.baselineAt.IsZero() {
		stamp := recorder.baselineAt
		incident.BaselineAt = &stamp
	}
	incident.save()
	for _, slice := range recorder.history {
		incident.write(slice.name, slice.body)
	}
	incident.write("heap-before.pb.gz", recorder.baseline)
	incident.write("heap-peak.pb.gz", autoHeap())
	log.Printf("auto profile: capture %s", incident.dir)
	return false
}

func (recorder *autoProfileRecorder) takeBaseline(at time.Time) {
	if baseline := autoHeap(); baseline != nil {
		recorder.baseline, recorder.baselineAt = baseline, at
	}
}

func (recorder *autoProfileRecorder) finish(sample profileSample, recovered bool) {
	incident := recorder.incident
	defer incident.unlock()
	incident.End, incident.Recovered = &sample, recovered
	incident.write("heap-after.pb.gz", autoHeap())
	incident.save()
	recorder.incident = nil
	recorder.nextCapture = sample.At.Add(profileCooldown)
}

func autoHeap() []byte {
	buffer := &profileBuffer{limit: profileHeapBytes}
	if err := pprof.WriteHeapProfile(buffer); err != nil {
		log.Printf("auto profile: %v", err)
		return nil
	}
	if buffer.exceeded {
		log.Printf("auto profile: heap snapshot exceeded %d bytes", profileHeapBytes)
		return nil
	}
	return buffer.Bytes()
}

func openProfileIncident(root, version string, sample profileSample) (*profileIncident, error) {
	unlock, locked := filelock.Lock(filepath.Join(root, ".magpie-profile.lock"), 0)
	if !locked {
		return nil, errors.New("couldn't lock the profile directory")
	}
	if err := pruneProfileIncidents(root); err != nil {
		unlock()
		return nil, err
	}
	dir, err := os.MkdirTemp(root, profilePrefix+sample.At.UTC().Format(profileTimeFormat)+"-")
	if err != nil {
		unlock()
		return nil, err
	}
	return &profileIncident{
		PID: os.Getpid(), Version: version, Trigger: sample, MemoryPeak: sample, CPUPeak: sample,
		dir: dir, unlock: unlock, heapPeakMemory: sample.memory(),
	}, nil
}

func pruneProfileIncidents(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var owned []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, profilePrefix) {
			continue
		}
		stamp, suffix, ok := strings.Cut(strings.TrimPrefix(name, profilePrefix), "-")
		if _, err := time.Parse(profileTimeFormat, stamp); err != nil || !ok {
			continue
		}
		if _, err := strconv.ParseUint(suffix, 10, 32); err != nil {
			continue
		}
		owned = append(owned, name)
	}
	for _, name := range owned[:max(0, len(owned)-profileKeep+1)] {
		if err := removeProfileIncident(filepath.Join(root, name)); err != nil {
			return err
		}
	}
	return nil
}

func removeProfileIncident(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".part")
		if !entry.Type().IsRegular() || !profileArtifact(name) {
			return errors.New("unknown file in profile directory: " + filepath.Join(dir, entry.Name()))
		}
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(dir)
}

func profileArtifact(name string) bool {
	switch name {
	case "incident.json", "heap-before.pb.gz", "heap-peak.pb.gz", "heap-after.pb.gz":
		return true
	}
	stamp, ok := strings.CutPrefix(name, "cpu-")
	if !ok || !strings.HasSuffix(stamp, ".pb.gz") {
		return false
	}
	_, err := time.Parse(profileTimeFormat, strings.TrimSuffix(stamp, ".pb.gz"))
	return err == nil
}

func (incident *profileIncident) save() {
	data, err := json.MarshalIndent(incident, "", "  ")
	if err != nil {
		log.Printf("auto profile: %v", err)
		return
	}
	incident.write("incident.json", data)
}

func (incident *profileIncident) write(name string, data []byte) {
	if len(data) == 0 {
		return
	}
	path := filepath.Join(incident.dir, name)
	temp := path + ".part"
	err := os.WriteFile(temp, data, 0o600)
	if err == nil {
		err = os.Rename(temp, path)
	}
	if err == nil {
		return
	}
	log.Printf("auto profile: %v", err)
	if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("auto profile: %v", err)
	}
}
