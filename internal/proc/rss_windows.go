package proc

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	processMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")
	memoryStatus      = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
)

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// rss adds up the working sets of the job's processes, under t.mu: the
// job's handle is closed by sweep. A command not in a job is read alone.
func (t *Tree) rss(cmd *exec.Cmd) (int64, bool) {
	defer t.mu.Unlock()
	pids := []uint32{uint32(cmd.Process.Pid)}
	if t.job != 0 {
		// JOBOBJECT_BASIC_PROCESS_ID_LIST: two counts, then the ids
		var list struct {
			assigned, listed uint32
			ids              [256]uintptr
		}
		if err := windows.QueryInformationJobObject(t.job, windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&list)), uint32(unsafe.Sizeof(list)), nil); err == nil && list.listed > 0 {
			pids = pids[:0]
			for _, id := range list.ids[:min(int(list.listed), len(list.ids))] {
				pids = append(pids, uint32(id))
			}
		}
	}
	var total int64
	read := false
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			continue
		}
		size, ok := workingSet(h)
		_ = windows.CloseHandle(h)
		if ok {
			total += size
			read = true
		}
	}
	return total, read
}

// SelfRSS returns this process's working set in bytes.
func SelfRSS() (int64, bool) { return workingSet(windows.CurrentProcess()) }

func workingSet(process windows.Handle) (int64, bool) {
	var counters processMemoryCounters
	counters.cb = uint32(unsafe.Sizeof(counters))
	ok, _, _ := processMemoryInfo.Call(uintptr(process), uintptr(unsafe.Pointer(&counters)), uintptr(counters.cb))
	return int64(counters.workingSetSize), ok != 0
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

func memTotal() int64 {
	s := memoryStatusEx{}
	s.length = uint32(unsafe.Sizeof(s))
	if ok, _, _ := memoryStatus.Call(uintptr(unsafe.Pointer(&s))); ok == 0 {
		return 0
	}
	return int64(s.totalPhys)
}
