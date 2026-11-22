// Package filememo keeps what was parsed from a file until the file
// changes, so a page that asks many times over reads it once.
package filememo

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/steady"
)

type entry struct {
	mod    time.Time
	size   int64
	digest [sha256.Size]byte
	v      any
	// the bytes parsed, while the file's time is too recent to tell a
	// second write by: Linux stamps a file from a clock a tick (a few
	// milliseconds) coarse, so two writes of one size inside a tick (two
	// accounts swapped) leave the same time and size
	raw []byte
	// again is the buffer the file is read into again while raw is kept,
	// to be compared with it
	again []byte
	// when the file was last found unchanged, while a request held them
	// (Hold), and in which of Forget's generations
	checked time.Time
	gen     uint64
	// touched says it was read since sweepIdle last looked
	touched bool
}

// memoKey is a parse's place in seen: kind and path apart, not joined
// into one string, which each read made anew, as Look did for every file
// kept (a read is asked for each model of each provider a poll lists:
// 33 MB of what twenty window polls allocated).
type memoKey struct{ kind, path string }

// settled is how old a file's time must be for any later write to stamp
// a later one, many ticks of the coarsest clock.
const settled = 2 * time.Second

const currentReadBufferLimit = 256 << 10

var (
	mu   sync.Mutex
	seen = map[memoKey]entry{}
	// holds is how many requests hold what was read (Hold); gen is moved
	// on by every write magpie makes (Forget)
	holds int
	gen   uint64
	// gone is the files found not there last time looked at, not waited
	// on again (steady.Stat): one there before and not now may be being
	// replaced (Windows), and is
	gone = map[string]bool{}
	// missing is the files found not there while a request held what was
	// read, when, and in which of Forget's generations: taken as still
	// not there as a file found unchanged is, not looked at again. A
	// provider with no list fetched has no file of one, and a request
	// asked after it for each of its models, each time a look at the disk
	// (a third of a request's CPU with 30 providers of 60 models).
	missing = map[string]missingFile{}
	// stats counts the looks at the disk, for tests
	stats int
	// sweptAt is when sweepIdle last ran
	sweptAt time.Time
)

// idleFor is how long a parse is kept unread, at least, before it is let
// go: a read idleFor after the last sweep drops what wasn't read since,
// so a parse goes between idleFor and twice that after its last read
// (later, while nothing reads at all). What is read once, at start up
// (RenameLegacy's look at every agent's config, ~2 MB of a tray-only
// magpie's 12.8 MB live heap), was kept for the life of the process;
// what a poll reads stays. Swept on a read, not a timer: a timer armed
// inside a test's synctest bubble is the bubble's (see endWaits).
const idleFor = 5 * time.Minute

// sweepIdle lets go of the parses not read since it last ran, when that
// was idleFor ago; mu is held.
func sweepIdle(now time.Time) {
	if now.Sub(sweptAt) < idleFor {
		return
	}
	sweptAt = now
	for k, e := range seen {
		if !e.touched {
			delete(seen, k)
			continue
		}
		e.touched = false
		seen[k] = e
	}
}

type missingFile struct {
	err     error
	checked time.Time
	gen     uint64
}

// heldFor is how long a file found unchanged is taken as unchanged, while
// a request holds what was read, before it is looked at again.
const heldFor = time.Second

// Hold has a file found unchanged not looked at again, for a request that
// only reads, until release is called or a second has gone by: the GUI's
// state looked at each agent's files hundreds of times over, a few
// thousand looks at the disk that a slow one (a Mac's antivirus looking
// at each) made seconds. A write meanwhile (Forget) has them looked at.
func Hold() (release func()) {
	mu.Lock()
	holds++
	mu.Unlock()
	return sync.OnceFunc(func() {
		mu.Lock()
		holds--
		mu.Unlock()
	})
}

// Forget has every file looked at again on its next read: magpie wrote
// one.
func Forget() {
	mu.Lock()
	gen++
	gens.Store(gen)
	endWaits()
	mu.Unlock()
}

// wake has every loop waiting on Written look at its files, with nothing
// read forgotten: another process wrote one (Watch), which a look at the
// disk tells.
func wake() {
	mu.Lock()
	endWaits()
	mu.Unlock()
}

// endWaits closes the channel Written gave since the last write, if it
// gave one, mu held. It makes none: a test that saves inside a synctest
// bubble writes through Forget too, a channel made there is the
// bubble's, and the next write from outside the bubble closing it is a
// fatal error, which took internal/provider's tests down (#3632).
func endWaits() {
	if written != nil {
		close(written)
		written = nil
	}
}

// written is what Written gave since the last write; nil until a loop
// waits again.
var written chan struct{}

// Written is closed by the next write magpie makes (Forget), or by
// another process's that Watch is told of: a loop that follows magpie's
// files waits on it, and looks at them only every so often (OthersEvery)
// for what another process wrote, rather than polling each second for its
// own writes too (#3185: those polls woke an idle magpie ~12 times a
// second). No loop may take it inside a synctest bubble: a channel made
// there is the bubble's and outlives it, and its next use from outside
// the bubble (a write's close, a loop's select) is fatal, in whichever
// test comes next.
func Written() <-chan struct{} {
	mu.Lock()
	defer mu.Unlock()
	if written == nil {
		written = make(chan struct{})
	}
	return written
}

// gens is gen, read without mu.
var gens atomic.Uint64

// Gen is moved on by every Forget: what is built from files read here, and
// kept past one read, is built again when it moved.
func Gen() uint64 { return gens.Load() }

// Look has the file at path looked at on its next read, even while a
// request holds what was read (Hold): another magpie may have written it,
// which Forget, moved on by this one's writes alone, can't know of.
func Look(path string) {
	mu.Lock()
	defer mu.Unlock()
	for k, e := range seen {
		if k.path == path && !e.checked.IsZero() {
			e.checked = time.Time{}
			seen[k] = e
		}
	}
	delete(missing, path)
}

// Read is parse of the file at path, from what it gave last time when the
// file has the same size and time as then (and, written just now, the same
// bytes). kind tells apart two parses of
// one file. What it returns is shared: the caller must not change it.
func Read[T any](kind, path string, parse func([]byte) (T, error)) (T, error) {
	return read(kind, path, parse, heldRead, nil)
}

// ReadNow checks the file even during Hold. It uses the same size, timestamp
// and near-write content checks as Read.
func ReadNow[T any](kind, path string, parse func([]byte) (T, error)) (T, error) {
	return read(kind, path, parse, unheldRead, nil)
}

// ReadCurrent reuses a parse only when the current file contents match,
// including after replacements that preserve size and modification time.
func ReadCurrent[T any](kind, path string, parse func([]byte) (T, error)) (T, error) {
	return read(kind, path, parse, verifiedRead, nil)
}

// ReadNowWithSnapshot checks contents like ReadCurrent and uses the supplied
// reader. The reader must verify that its bytes are from the file just statted.
func ReadNowWithSnapshot[T any](kind, path string, parse func([]byte) (T, error), snapshot func(string, os.FileInfo, []byte) ([]byte, error)) (T, error) {
	return read(kind, path, parse, verifiedRead, snapshot)
}

type readMode uint8

const (
	heldRead readMode = iota
	unheldRead
	verifiedRead
)

func read[T any](kind, path string, parse func([]byte) (T, error), mode readMode, snapshot func(string, os.FileInfo, []byte) ([]byte, error)) (T, error) {
	var zero T
	key := memoKey{kind, path}
	mu.Lock()
	sweepIdle(time.Now())
	e, ok := seen[key]
	if ok && !e.touched {
		e.touched = true
		seen[key] = e
	}
	if ok && mode == heldRead && holds > 0 && e.raw == nil && e.gen == gen && time.Since(e.checked) < heldFor {
		mu.Unlock()
		return e.v.(T), nil
	}
	if m, ok := missing[path]; ok && mode == heldRead && holds > 0 && m.gen == gen && time.Since(m.checked) < heldFor {
		mu.Unlock()
		return zero, m.err
	}
	g := gen
	mu.Unlock()
	var fi os.FileInfo
	var err error
	if snapshot == nil {
		fi, err = statFile(path)
	} else {
		fi, err = steady.Stat(path)
	}
	if err != nil {
		if snapshot == nil && errors.Is(err, fs.ErrNotExist) {
			mu.Lock()
			if holds > 0 && g == gen {
				missing[path] = missingFile{err, time.Now(), g}
			}
			mu.Unlock()
		}
		return zero, err
	}
	sameStamp := ok && e.mod.Equal(fi.ModTime()) && e.size == fi.Size()
	if mode != verifiedRead && sameStamp && e.raw == nil {
		mu.Lock()
		if holds > 0 && g == gen {
			e.checked, e.gen = time.Now(), g
			seen[key] = e
		}
		mu.Unlock()
		return e.v.(T), nil
	}
	if sameStamp {
		// The stamp alone is insufficient near a write or on ReadCurrent.
		// A read beside this one takes a buffer of its own.
		mu.Lock()
		buf := seen[key].again
		if cur, ok := seen[key]; ok {
			cur.again = nil
			seen[key] = cur
		}
		mu.Unlock()
		var b []byte
		if snapshot != nil {
			b, err = snapshot(path, fi, buf)
		} else {
			if int64(cap(buf)) <= fi.Size() {
				buf = make([]byte, 0, fi.Size()+512)
			}
			b, err = steady.ReadThereInto(path, buf)
		}
		if err != nil {
			return zero, err
		}
		if e.raw != nil && bytes.Equal(b, e.raw) || mode == verifiedRead && sha256.Sum256(b) == e.digest {
			mu.Lock()
			if cur, ok := seen[key]; ok && cur.again == nil && cur.mod.Equal(e.mod) &&
				cur.digest == e.digest && (mode != verifiedRead || cap(b) <= currentReadBufferLimit) {
				cur.again = b[:0]
				seen[key] = cur
			}
			mu.Unlock()
			if snapshot == nil {
				settle(key, e)
			}
			return e.v.(T), nil
		}
		// the buffer was this read's alone: it is what was parsed now
		return parsed(key, fi, b, parse, g, snapshot != nil)
	}
	var b []byte
	if snapshot != nil {
		b, err = snapshot(path, fi, nil)
	} else {
		b, err = steady.ReadThere(path)
	}
	if err != nil {
		return zero, err
	}
	if ok && e.raw != nil && e.mod.Equal(fi.ModTime()) && bytes.Equal(b, e.raw) {
		if snapshot == nil {
			settle(key, e)
		}
		return e.v.(T), nil
	}
	return parsed(key, fi, b, parse, g, snapshot != nil)
}

// settle drops e's bytes once its file's time is old enough to tell a
// later write by: from then on its time and size are looked at alone.
func settle(key memoKey, e entry) {
	if e.raw != nil && time.Since(e.mod) > settled {
		mu.Lock()
		if cur, ok := seen[key]; ok && cur.digest == e.digest && cur.mod.Equal(e.mod) && cur.size == e.size {
			cur.raw, cur.again = nil, nil
			seen[key] = cur
		}
		mu.Unlock()
	}
}

// parsed is parse of b, the file at key's bytes as fi found it, kept for
// the next read; g is Forget's generation when it was looked at.
func parsed[T any](key memoKey, fi os.FileInfo, b []byte, parse func([]byte) (T, error), g uint64, snapshot bool) (T, error) {
	v, err := parse(b)
	if err != nil {
		var zero T
		return zero, err
	}
	digest := sha256.Sum256(b)
	mu.Lock()
	e := entry{mod: fi.ModTime(), size: fi.Size(), digest: digest, v: v, touched: true}
	if snapshot && cap(b) <= currentReadBufferLimit || !snapshot && time.Since(e.mod) <= settled {
		e.raw = b
	} else if holds > 0 && g == gen {
		e.checked, e.gen = time.Now(), g
	}
	seen[key] = e
	mu.Unlock()
	return v, nil
}

// statFile is os.Stat of path, waiting out a moment of it not there
// (another process replacing it, on Windows) unless it wasn't there last
// time either: taken for gone, a file of accounts read as no accounts, and
// the next write left them all out (Packing1 on Discord, Windows 11:
// Antigravity's account gone after an update).
func statFile(path string) (os.FileInfo, error) {
	mu.Lock()
	wasGone := gone[path]
	stats++
	mu.Unlock()
	var fi os.FileInfo
	var err error
	if wasGone {
		fi, err = os.Stat(path)
	} else {
		fi, err = steady.Stat(path)
	}
	mu.Lock()
	if errors.Is(err, fs.ErrNotExist) {
		gone[path] = true
	} else {
		delete(gone, path)
		delete(missing, path)
	}
	mu.Unlock()
	return fi, err
}

// DropKind lets go of what was kept of every file read as kind: the next
// Read of one reads and parses it again.
func DropKind(kind string) {
	mu.Lock()
	defer mu.Unlock()
	for k := range seen {
		if k.kind == kind {
			delete(seen, k)
		}
	}
}
