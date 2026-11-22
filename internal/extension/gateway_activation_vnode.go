//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package extension

import (
	"io/fs"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type kqueueActivation struct {
	queue  int
	reader *os.File
	writer *os.File
	wake   chan struct{}
	done   chan struct{}
	files  map[string]activationVnode

	eventMu  sync.Mutex
	pending  map[int]uint32
	checking map[int]uint32
	overflow bool
}

type activationVnode struct {
	file     *os.File
	fd       int
	info     fs.FileInfo
	children bool
}

func newActivationVnodeWatch() activationVnodeWatch {
	queue, err := unix.Kqueue()
	if err != nil {
		return nil
	}
	unix.CloseOnExec(queue)
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = unix.Close(queue)
		return nil
	}
	var change unix.Kevent_t
	unix.SetKevent(&change, int(reader.Fd()), unix.EVFILT_READ, unix.EV_ADD|unix.EV_ONESHOT)
	if _, err := activationKevent(queue, []unix.Kevent_t{change}, nil); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		_ = unix.Close(queue)
		return nil
	}
	watch := &kqueueActivation{
		queue: queue, reader: reader, writer: writer, wake: make(chan struct{}, 1),
		done: make(chan struct{}), files: make(map[string]activationVnode),
		pending: make(map[int]uint32), checking: make(map[int]uint32),
	}
	go watch.receive()
	return watch
}

func (watch *kqueueActivation) changes() <-chan struct{} { return watch.wake }

func (watch *kqueueActivation) receive() {
	defer close(watch.done)
	defer close(watch.wake)
	var events [32]unix.Kevent_t
	for {
		count, err := activationKevent(watch.queue, nil, events[:])
		if err != nil {
			return
		}
		for _, event := range events[:count] {
			if event.Filter == unix.EVFILT_READ || event.Flags&unix.EV_ERROR != 0 {
				return
			}
		}
		watch.eventMu.Lock()
		for _, event := range events[:count] {
			fd := int(event.Ident)
			watch.pending[fd] |= event.Fflags
			if len(watch.pending) > activationWatchLimit {
				watch.overflow = true
				clear(watch.pending)
				break
			}
		}
		watch.eventMu.Unlock()
		select {
		case watch.wake <- struct{}{}:
		default:
		}
	}
}

func (watch *kqueueActivation) relevant(missingParents map[string][]activationMissingPath) bool {
	watch.eventMu.Lock()
	watch.checking, watch.pending = watch.pending, watch.checking
	overflow := watch.overflow
	watch.overflow = false
	watch.eventMu.Unlock()
	defer clear(watch.checking)
	if overflow {
		return true
	}
	for name, entry := range watch.files {
		notes, found := watch.checking[entry.fd]
		if !found {
			continue
		}
		delete(watch.checking, entry.fd)
		if !entry.info.IsDir() || notes != unix.NOTE_WRITE {
			return true
		}
		for _, path := range missingParents[name] {
			info, err := os.Lstat(path.name)
			if err == nil && (path.directory && info.IsDir() || !path.directory && info.Mode().IsRegular()) {
				return true
			}
		}
	}
	return len(watch.checking) != 0
}

func (watch *kqueueActivation) close() {
	_ = watch.writer.Close()
	<-watch.done
	_ = watch.reader.Close()
	for _, entry := range watch.files {
		_ = entry.file.Close()
	}
	_ = unix.Close(watch.queue)
}

func (watch *kqueueActivation) update(targets map[string]fs.FileInfo, missingParents map[string][]activationMissingPath) bool {
	for name, entry := range watch.files {
		if info := targets[name]; info == nil || !os.SameFile(entry.info, info) {
			_ = entry.file.Close()
			delete(watch.files, name)
		}
	}
	armed := false
	for name, info := range targets {
		children := len(missingParents[name]) > 0
		entry, existing := watch.files[name]
		if existing && entry.children == children {
			continue
		}
		if !existing {
			file, err := openActivationVnode(name, info)
			if err != nil {
				continue
			}
			entry = activationVnode{file: file, fd: int(file.Fd()), info: info}
		}
		if err := watch.register(entry.fd, info.IsDir(), children); err != nil {
			_ = entry.file.Close()
			delete(watch.files, name)
			continue
		}
		entry.children = children
		watch.files[name] = entry
		armed = armed || !existing || children
	}
	return armed
}

func openActivationVnode(name string, expected fs.FileInfo) (*os.File, error) {
	flags := unix.O_CLOEXEC | unix.O_NONBLOCK
	if expected.IsDir() {
		flags |= unix.O_DIRECTORY
	} else {
		flags |= unix.O_NOFOLLOW
	}
	fd, err := unix.Open(name, flags, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || !os.SameFile(expected, info) {
		_ = file.Close()
		return nil, fs.ErrNotExist
	}
	return file, nil
}

func (watch *kqueueActivation) register(fd int, directory, children bool) error {
	var change unix.Kevent_t
	unix.SetKevent(&change, fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR)
	change.Fflags = unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_REVOKE | unix.NOTE_ATTRIB
	if !directory || children {
		change.Fflags |= unix.NOTE_WRITE
	}
	_, err := activationKevent(watch.queue, []unix.Kevent_t{change}, nil)
	return err
}

func activationKevent(queue int, changes, events []unix.Kevent_t) (int, error) {
	for {
		count, err := unix.Kevent(queue, changes, events, nil)
		if err != unix.EINTR {
			return count, err
		}
	}
}
