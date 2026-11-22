// Package filelock keeps a change to a file one at a time across the
// magpies on a computer: the gateway that runs all day, and each magpie
// run from a terminal or a script meanwhile.
package filelock

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func LockMutex(ctx context.Context, mu *sync.Mutex) error {
	if ctx.Done() == nil {
		mu.Lock()
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Wait is how long Mutex waits for another magpie to let go, before it
// goes on without the lock, as it did before there was one: a magpie
// stuck holding it (or two magpies each waiting on a lock the other
// holds) never hangs every other.
const Wait = 10 * time.Second

// mkdirAll makes a lock file's folder; a test counts the calls.
var mkdirAll = os.MkdirAll

// Lock takes the lock on the file at path, made if it isn't there, waiting
// up to wait for another process to let it go. The system holds it on the
// file and lets it go when the process holding it ends, however it ends,
// so none is ever left behind; the file stays, since taken away one
// process could hold the lock on it while another took one on a new file.
// held is false when it went on without the lock: on a file system
// without locks, or after wait.
func Lock(path string, wait time.Duration) (unlock func(), held bool) {
	return lockPath(context.Background(), path, wait)
}

func lockPath(ctx context.Context, path string, wait time.Duration) (unlock func(), held bool) {
	if ctx.Err() != nil {
		return func() {}, false
	}
	// the folder is made only when the file can't be opened for want of
	// it: made first, each lock asked the system for the folder too, on
	// every take (#1665: ~190 times in a Windows start's first 25s, each
	// an antivirus filter driver's work)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if errors.Is(err, fs.ErrNotExist) {
		mkdirAll(filepath.Dir(path), 0o700)
		f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	}
	if err != nil {
		return func() {}, false // no lock to be had here: no file to change either
	}
	start := time.Now()
	for {
		got, err := TryLock(f)
		if err != nil { // a file system without locks, as an NFS home with no lockd
			f.Close()
			return func() {}, false
		}
		if got {
			return func() { Unlock(f); f.Close() }, true
		}
		if time.Since(start) > wait {
			f.Close()
			log.Printf("%s: another magpie held it for %v; going on without it", path, wait)
			return func() {}, false
		}
		select {
		case <-ctx.Done():
			f.Close()
			return func() {}, false
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Mutex is a sync.Mutex that also holds the lock on the file Path names
// while it is held, so a read, change and write of a file under it is
// one at a time across magpies too, not only within this one. Every
// read of the file a change is made from is to be made after Lock.
type Mutex struct {
	mu sync.Mutex
	// Path is the lock file: the file guarded, with ".lock".
	Path func() string
	// Locked, when set, is called once both locks are held: to have the
	// file guarded read afresh, not from what was parsed of it before
	// another magpie wrote it.
	Locked func()
	unlock func()
}

func (m *Mutex) Lock() {
	m.mu.Lock()
	m.lockFile(context.Background())
}

func (m *Mutex) LockChecked() bool {
	return m.LockCheckedContext(context.Background())
}

func (m *Mutex) LockCheckedContext(ctx context.Context) bool {
	if LockMutex(ctx, &m.mu) != nil {
		return false
	}
	if m.lockFile(ctx) && ctx.Err() == nil {
		return true
	}
	m.Unlock()
	return false
}

func (m *Mutex) lockFile(ctx context.Context) bool {
	var held bool
	m.unlock, held = lockPath(ctx, m.Path(), Wait)
	if m.Locked != nil && ctx.Err() == nil {
		m.Locked()
	}
	return held
}

func (m *Mutex) Unlock() {
	unlock := m.unlock
	m.unlock = nil
	if unlock != nil {
		unlock()
	}
	m.mu.Unlock()
}
