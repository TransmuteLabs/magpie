package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	profileHeapBytes  = 16 << 20
	profileKeep       = 3
	profilePrefix     = "magpie-incident-"
	profileTimeFormat = "20060102T150405.000000000Z"

	profileBaselineFile = ".magpie-heap-baseline.pb.gz"
	profileBeforeFile   = "heap-before.pb.gz"
	profilePeakFile     = "heap-peak.pb.gz"
	profileAfterFile    = "heap-after.pb.gz"
)

var autoProfiling atomic.Bool
var errProfileHeapLimit = fmt.Errorf("heap profile exceeds %d bytes", profileHeapBytes)

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

type profileHeapWriter struct {
	file  io.Writer
	bytes int64
	err   error
}

func (writer *profileHeapWriter) Write(data []byte) (int, error) {
	if writer.err != nil {
		return 0, writer.err
	}
	if int64(len(data)) > profileHeapBytes-writer.bytes {
		writer.err = errProfileHeapLimit
		return 0, writer.err
	}
	size, err := writer.file.Write(data)
	writer.bytes += int64(size)
	if err == nil && size != len(data) {
		err = io.ErrShortWrite
	}
	writer.err = err
	return size, err
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

type profileHeapFile struct {
	At    time.Time `json:"at"`
	Bytes int64     `json:"bytes"`
}

type profileHeapAttempt struct {
	At    time.Time `json:"at"`
	Bytes int64     `json:"bytes"`
	Error string    `json:"error,omitempty"`
}

type profileHeapStatus struct {
	Attempt   *profileHeapAttempt `json:"attempt,omitempty"`
	File      *profileHeapFile    `json:"file,omitempty"`
	CopyError string              `json:"copyError,omitempty"`
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

	HeapProfiles map[string]profileHeapStatus `json:"heapProfiles,omitempty"`

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
	baseline     profileHeapStatus
	baselineInfo os.FileInfo
	writeHeap    func(io.Writer) error
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
	defer recorder.clearBaseline()
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
		if sample.CPUPercent > incident.CPUPeak.CPUPercent {
			incident.CPUPeak = sample
		}
		if memory >= incident.heapPeakMemory+profilePeakStep {
			incident.heapPeakMemory = memory
			recorder.captureIncidentHeap(profilePeakFile, sample.At)
			incident.save()
		}
		age := sample.At.Sub(incident.Trigger.At)
		return age >= profileDeadline || (age >= profileFollow && !sample.high())
	}
	if !sample.high() {
		if recorder.baseline.Attempt == nil || sample.At.Sub(recorder.baseline.Attempt.At) >= profileBaseline {
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
	incident.save()
	for _, slice := range recorder.history {
		incident.write(slice.name, slice.body)
	}
	recorder.copyBaseline()
	recorder.captureIncidentHeap(profilePeakFile, sample.At)
	incident.save()
	log.Printf("auto profile: capture %s", incident.dir)
	return false
}

func (recorder *autoProfileRecorder) takeBaseline(at time.Time) {
	path := filepath.Join(recorder.dir, profileBaselineFile)
	unlock, locked := filelock.Lock(path+".lock", 0)
	if !locked {
		recorder.baseline.Attempt = heapProfileAttempt(path, at, 0, errors.New("couldn't lock the heap baseline"))
		return
	}
	defer unlock()
	if info := recorder.captureHeap(path, at, &recorder.baseline); info != nil {
		recorder.baselineInfo = info
		if closed, err := regularProfileFile(path); err == nil && os.SameFile(info, closed) {
			recorder.baselineInfo = closed
		}
	}
}

func (recorder *autoProfileRecorder) clearBaseline() {
	if recorder.baselineInfo == nil {
		return
	}
	path := filepath.Join(recorder.dir, profileBaselineFile)
	unlock, locked := filelock.Lock(path+".lock", 0)
	if !locked {
		return
	}
	defer unlock()
	if info, err := regularProfileFile(path); err == nil && recorder.ownsBaseline(info) {
		if err := os.Remove(path); err != nil {
			log.Printf("auto profile: %v", err)
		}
	}
}

func (recorder *autoProfileRecorder) ownsBaseline(info os.FileInfo) bool {
	expected := recorder.baselineInfo
	return expected != nil && os.SameFile(expected, info) &&
		info.Size() == expected.Size() && info.ModTime().Equal(expected.ModTime())
}

func (recorder *autoProfileRecorder) copyBaseline() {
	status := profileHeapStatus{Attempt: recorder.baseline.Attempt}
	if recorder.baseline.File != nil {
		dest := filepath.Join(recorder.incident.dir, profileBeforeFile)
		_, _, err := writeHeapProfileFile(dest, recorder.readBaseline)
		if err != nil {
			status.CopyError = err.Error()
			log.Printf("auto profile: %s: %v", dest, err)
		} else {
			status.File = recorder.baseline.File
			recorder.incident.BaselineAt = &status.File.At
		}
	}
	if status.Attempt != nil {
		recorder.incident.HeapProfiles[profileBeforeFile] = status
	}
}

func (recorder *autoProfileRecorder) readBaseline(dst io.Writer) error {
	path := filepath.Join(recorder.dir, profileBaselineFile)
	info, err := regularProfileFile(path)
	if err != nil {
		return err
	}
	if !recorder.ownsBaseline(info) {
		return errors.New("the heap baseline was replaced")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return err
	}
	if !recorder.ownsBaseline(info) {
		return errors.New("the heap baseline was replaced")
	}
	size, err := io.Copy(dst, file)
	if err == nil && size != info.Size() {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func (recorder *autoProfileRecorder) finish(sample profileSample, recovered bool) {
	incident := recorder.incident
	defer incident.unlock()
	incident.End, incident.Recovered = &sample, recovered
	recorder.captureIncidentHeap(profileAfterFile, sample.At)
	incident.save()
	recorder.incident = nil
	recorder.nextCapture = sample.At.Add(profileCooldown)
}

func (recorder *autoProfileRecorder) captureIncidentHeap(name string, at time.Time) {
	incident := recorder.incident
	status := incident.HeapProfiles[name]
	recorder.captureHeap(filepath.Join(incident.dir, name), at, &status)
	incident.HeapProfiles[name] = status
}

func (recorder *autoProfileRecorder) captureHeap(path string, at time.Time, status *profileHeapStatus) os.FileInfo {
	writeHeap := recorder.writeHeap
	if writeHeap == nil {
		writeHeap = pprof.WriteHeapProfile
	}
	info, size, err := writeHeapProfileFile(path, writeHeap)
	status.Attempt = heapProfileAttempt(path, at, size, err)
	if err == nil {
		status.File = &profileHeapFile{At: at, Bytes: size}
	}
	return info
}

func heapProfileAttempt(path string, at time.Time, size int64, err error) *profileHeapAttempt {
	attempt := &profileHeapAttempt{At: at, Bytes: size}
	if err != nil {
		attempt.Error = err.Error()
		log.Printf("auto profile: %s: %v", path, err)
	}
	return attempt
}

func regularProfileFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return info, err
}

func writeHeapProfileFile(path string, write func(io.Writer) error) (os.FileInfo, int64, error) {
	if _, err := regularProfileFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	temp := path + ".part"
	if _, err := regularProfileFile(temp); err == nil {
		if err := os.Remove(temp); err != nil {
			return nil, 0, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, 0, err
	}
	writer := &profileHeapWriter{file: file}
	err = write(writer)
	if writer.err != nil {
		err = writer.err
	}
	if err == nil && writer.bytes == 0 {
		err = errors.New("empty heap profile")
	}
	var info os.FileInfo
	if err == nil {
		info, err = file.Stat()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp, path)
	}
	if err != nil {
		if removeErr := os.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
		return nil, writer.bytes, err
	}
	return info, writer.bytes, nil
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
		HeapProfiles: make(map[string]profileHeapStatus, 3),
		dir:          dir, unlock: unlock, heapPeakMemory: sample.memory(),
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
	case "incident.json", profileBeforeFile, profilePeakFile, profileAfterFile:
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
