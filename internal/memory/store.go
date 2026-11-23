package memory

// Memory is kept as a folder, Dir, beside where memory.json was:
//
//   - <id>.md, one a memory: its fields as YAML front matter, its text as
//     the body. The file is the memory. One the user edits by hand is read
//     as the user's change (Origin user, Edited: Fixed, so nothing a
//     machine does writes over it), and one they add is a memory of
//     theirs, its file name its id.
//   - meta.json: what isn't one memory's. The projects, the published
//     digests, the texts dismissed, the memories removed, the suggestions
//     declined, when each memory was last used (so a search rewrites this
//     file, not the memory's), and each memory file's hash as magpie last
//     wrote it, which tells a hand edit from magpie's own.
//   - journal.json, while a change to more than one file is written:
//     every file the change writes, put down first, so a change cut off
//     halfway is finished by the next read, never left half done.
//
// memory.json, from before, is moved into the folder on the first read
// (migrate) and kept as memory.json.migrated.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/steady"
)

// Path is memory.json, where memory was kept before Dir; a variable for
// tests, which moves Dir with it.
var Path = func() string { return appdir.Join(appdir.Config(), "memory.json") }

// Dir is the folder memory is kept in.
func Dir() string { return filepath.Join(filepath.Dir(Path()), "memory") }

const (
	metaName    = "meta.json"
	indexName   = "index.json"
	journalName = "journal.json"
)

func in(name string) string { return filepath.Join(Dir(), name) }

var mu = filelock.Mutex{Path: func() string { return Dir() + ".lock" }}

var errStoreLock = errors.New("cannot lock the Memory store")

// meta is meta.json.
type meta struct {
	Version   int                  `json:"version"`
	Projects  map[string]*Project  `json:"projects,omitempty"`
	Digest    map[string]string    `json:"digest,omitempty"`
	Published time.Time            `json:"published,omitzero"`
	Dismissed []string             `json:"dismissed,omitempty"`
	Removed   map[string]time.Time `json:"removed,omitempty"`
	Declined  []string             `json:"declined,omitempty"`
	Used      map[string]use       `json:"used,omitempty"`
	Written   map[string]string    `json:"written,omitempty"`
}

// use is when a search last returned a memory, and how often.
type use struct {
	At   time.Time `json:"at"`
	Uses int       `json:"uses,omitempty"`
}

// front is a memory file's front matter.
type front struct {
	Kind         string   `yaml:"kind,omitempty"`
	Scope        string   `yaml:"scope,omitempty"`
	Status       string   `yaml:"status,omitempty"`
	Origin       string   `yaml:"origin,omitempty"`
	Pinned       bool     `yaml:"pinned,omitempty"`
	Tags         []string `yaml:"tags,omitempty,flow"`
	Why          string   `yaml:"why,omitempty"`
	SupersededBy string   `yaml:"supersededBy,omitempty"`
	Skill        string   `yaml:"skill,omitempty"`
	Created      string   `yaml:"created,omitempty"`
	Updated      string   `yaml:"updated,omitempty"`
	Edited       string   `yaml:"edited,omitempty"`
	Sources      []source `yaml:"sources,omitempty"`
}

type source struct {
	Agent   string `yaml:"agent,omitempty"`
	Session string `yaml:"session,omitempty"`
	Dir     string `yaml:"dir,omitempty"`
	File    string `yaml:"file,omitempty"`
	Quote   string `yaml:"quote,omitempty"`
	At      string `yaml:"at,omitempty"`
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// unstamp reads a time as magpie writes it, or as a person may: a date, or
// a date and time with no zone (local).
func unstamp(s string, or time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return or, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	for _, l := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q isn't a time", s)
}

// encode is m's file.
func encode(m *Memory) ([]byte, error) {
	fr := front{Kind: m.Kind, Scope: m.Scope, Status: m.Status, Origin: m.Origin, Pinned: m.Pinned, Tags: m.Tags, Why: m.Why,
		SupersededBy: m.SupersededBy, Skill: m.Skill, Created: stamp(m.Created), Updated: stamp(m.Updated), Edited: stamp(m.Edited)}
	for _, s := range m.Sources {
		fr.Sources = append(fr.Sources, source{Agent: s.Agent, Session: s.Session, Dir: s.Dir, File: s.File, Quote: s.Quote, At: stamp(s.At)})
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(fr); err != nil {
		return nil, err
	}
	e.Close()
	b.WriteString("---\n\n")
	b.WriteString(m.Text)
	b.WriteString("\n")
	return b.Bytes(), nil
}

// decode reads the file of the memory with id, changed at mod. What the
// front matter leaves out is what a memory the user adds is: a fact that
// holds everywhere, in use, theirs, made at mod.
func decode(id string, b []byte, mod time.Time) (*Memory, error) {
	s := strings.TrimPrefix(strings.ReplaceAll(string(b), "\r\n", "\n"), "\ufeff")
	var fr front
	body := s
	if rest, ok := strings.CutPrefix(s, "---\n"); ok {
		var head string
		switch i := strings.Index(rest, "\n---\n"); {
		case strings.HasPrefix(rest, "---\n"):
			head, body = "", rest[4:]
		case i >= 0:
			head, body = rest[:i], rest[i+5:]
		case strings.HasSuffix(rest, "\n---"):
			head, body = rest[:len(rest)-4], ""
		default:
			return nil, errors.New("its front matter has no closing ---")
		}
		if err := yaml.Unmarshal([]byte(head), &fr); err != nil {
			return nil, fmt.Errorf("its front matter: %w", err)
		}
	}
	m := &Memory{ID: id, Text: strings.TrimSpace(body), Kind: cmpOr(fr.Kind, Fact), Scope: scopeOf(fr.Scope),
		Status: cmpOr(fr.Status, Active), Origin: cmpOr(fr.Origin, FromUser), Pinned: fr.Pinned, Tags: CleanTags(fr.Tags),
		Why: strings.TrimSpace(fr.Why), SupersededBy: fr.SupersededBy, Skill: fr.Skill}
	if len(m.Tags) == 0 {
		m.Tags = nil
	}
	var err error
	if m.Created, err = unstamp(fr.Created, mod); err != nil {
		return nil, err
	}
	if m.Updated, err = unstamp(fr.Updated, mod); err != nil {
		return nil, err
	}
	if m.Edited, err = unstamp(fr.Edited, time.Time{}); err != nil {
		return nil, err
	}
	for _, src := range fr.Sources {
		at, err := unstamp(src.At, time.Time{})
		if err != nil {
			return nil, err
		}
		m.Sources = append(m.Sources, Source{Agent: src.Agent, Session: src.Session, Dir: src.Dir, File: src.File, Quote: src.Quote, At: at})
	}
	if err := check(m); err != nil {
		return nil, err
	}
	return m, nil
}

func cmpOr(s, or string) string {
	if s = strings.TrimSpace(s); s != "" {
		return strings.ToLower(s)
	}
	return or
}

// validID is an id that can name a file in Dir: one from another computer
// that can't is never written.
func validID(id string) bool {
	return id != "" && len(id) <= 100 && !strings.HasPrefix(id, ".") && !strings.ContainsAny(id, `/\:*?"<>|`) &&
		!strings.ContainsFunc(id, func(r rune) bool { return r < ' ' })
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type entry struct {
	mod    time.Time
	size   int64
	hash   string
	memory *Memory
}

const (
	parsedMemoryBytes = 4 << 20
	parsedMemoryFiles = 4096
)

var parsedMemories struct {
	dir     string
	entries map[string]entry
	bytes   int64
}

const notedMemoryFiles = 4096

type malformedNotes struct {
	dir      string
	info     fs.FileInfo
	byID     map[string]string
	overflow bool
}

var noted malformedNotes

type noteRead struct {
	dir     string
	checked bool
	current bool
	skipped bool
}

func memoryNoteRead(dir string) noteRead {
	scan := noteRead{dir: dir}
	if len(noted.byID) != 0 || noted.overflow {
		scan.check()
	}
	return scan
}

func (scan *noteRead) check() bool {
	if scan.checked {
		return scan.current
	}
	scan.checked = true
	info, err := memoryDirectoryInfo(scan.dir)
	if errors.Is(err, fs.ErrNotExist) {
		noted = malformedNotes{}
	}
	if err != nil || !info.IsDir() {
		return false
	}
	if noted.dir != scan.dir || !os.SameFile(noted.info, info) {
		noted = malformedNotes{dir: scan.dir, info: info}
	}
	scan.current = true
	return true
}

func memoryDirectoryInfo(dir string) (fs.FileInfo, error) {
	folder, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer folder.Close()
	return folder.Stat()
}

func (scan *noteRead) warn(id, hash string, err error) {
	if !scan.check() {
		return
	}
	if previous, tracked := noted.byID[id]; tracked {
		if previous == hash {
			return
		}
	} else if len(noted.byID) == notedMemoryFiles {
		scan.skipped = true
		return
	}
	if noted.byID == nil {
		noted.byID = make(map[string]string)
	}
	noted.byID[id] = hash
	log.Printf("memory: %s.md can't be read, left as it is: %v", id, err)
}

func (scan *noteRead) forget(id string) {
	if scan.current {
		delete(noted.byID, id)
	}
}

func (scan *noteRead) finish(broken map[string]bool) {
	if !scan.current {
		return
	}
	if broken != nil {
		maps.DeleteFunc(noted.byID, func(id, _ string) bool { return !broken[id] })
		if !scan.skipped && len(noted.byID) < notedMemoryFiles {
			noted.overflow = false
		}
	}
	if scan.skipped && len(noted.byID) == notedMemoryFiles && !noted.overflow {
		log.Printf("memory: too many malformed memory files for individual warnings (limit %d)", notedMemoryFiles)
		noted.overflow = true
	}
}

// read reads memory from Dir, under mu: a change a journal holds is
// finished first, memory.json moved in when Dir has none yet. A memory
// file that can't be read is unknown: left out, and never written over
// nor removed (f.broken). meta.json that can't be read is an error.
func read() (*file, error) {
	if err := replay(); err != nil {
		return nil, err
	}
	f := &file{Version: 1, seen: map[string]string{}, broken: map[string]bool{}, written: map[string]string{}}
	mt, data, err := readMeta()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if _, err := os.Stat(Path()); !errors.Is(err, fs.ErrNotExist) {
			return migrate()
		}
	case err != nil:
		return nil, err
	default:
		f.metaB = data
		f.Projects, f.Digest, f.Published, f.Dismissed, f.Removed, f.Declined = mt.Projects, mt.Digest, mt.Published, mt.Dismissed, mt.Removed, mt.Declined
		if mt.Written != nil {
			f.written = mt.Written
		}
	}
	dir := Dir()
	notes := memoryNoteRead(dir)
	es, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if parsedMemories.dir != dir {
		parsedMemories.dir = dir
		parsedMemories.entries = make(map[string]entry)
		parsedMemories.bytes = 0
	}
	var scratch []byte
	for _, e := range es {
		name := e.Name()
		id, ok := strings.CutSuffix(name, ".md")
		if e.IsDir() || !ok || !validID(id) {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			f.broken[id] = true
			continue
		}
		cached := parsedMemories.entries[id]
		current, err := readMemoryFile(&notes, id, info.ModTime(), &scratch, cached)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				f.broken[id], f.seen[id] = true, current.hash
			}
			continue
		}
		if current.memory != cached.memory {
			dropParsedMemory(id)
			if len(parsedMemories.entries) < parsedMemoryFiles && parsedMemories.bytes+current.size <= parsedMemoryBytes {
				parsedMemories.entries[id] = current
				parsedMemories.bytes += current.size
			}
		}
		m := new(*current.memory)
		m.Tags, m.Sources = slices.Clone(m.Tags), slices.Clone(m.Sources)
		h := current.hash
		f.seen[id] = h
		if f.metaB != nil && f.written[id] != h {
			// changed, or added, by hand: the user's
			t := info.ModTime()
			if t.Before(m.Updated) {
				t = m.Updated
			}
			m.Origin, m.Updated, m.Edited = FromUser, t, t
		}
		if u, ok := mt.Used[id]; ok {
			m.Used, m.Uses = u.At, u.Uses
		}
		f.Memories = append(f.Memories, m)
	}
	if err == nil {
		notes.finish(f.broken)
	}
	for id := range parsedMemories.entries {
		if _, seen := f.seen[id]; !seen || f.broken[id] {
			dropParsedMemory(id)
		}
	}
	slices.SortFunc(f.Memories, func(a, b *Memory) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return f, nil
}

func dropParsedMemory(id string) {
	if cached, ok := parsedMemories.entries[id]; ok {
		parsedMemories.bytes -= cached.size
		delete(parsedMemories.entries, id)
	}
}

func readMemoryFile(notes *noteRead, id string, mod time.Time, scratch *[]byte, cached entry) (entry, error) {
	data, err := steady.ReadThereInto(filepath.Join(notes.dir, id+".md"), *scratch)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			notes.forget(id)
		}
		return entry{}, err
	}
	*scratch = data[:0]
	hash := hashOf(data)
	if cached.memory != nil && hash == cached.hash && mod.Equal(cached.mod) {
		notes.forget(id)
		return cached, nil
	}
	record, err := decode(id, data, mod)
	if err != nil {
		notes.warn(id, hash, err)
	} else {
		notes.forget(id)
	}
	return entry{mod: mod, size: int64(len(data)), hash: hash, memory: record}, err
}

func readMeta() (meta, []byte, error) {
	var parsed meta
	data, err := lastgood.Read(in(metaName), func(data []byte) bool {
		var current meta
		if err := json.Unmarshal(data, &current); err != nil {
			return false
		}
		parsed = current
		return true
	})
	return parsed, data, err
}

// op is one file a write puts down (Data) or removes (Del).
type op struct {
	Name string `json:"name"`
	Data string `json:"data,omitempty"`
	Del  bool   `json:"del,omitempty"`
}

type journal struct {
	Ops []op `json:"ops"`
}

// write writes what changed in f since read: each memory file whose text
// is new, each removed, and meta.json. Changing more than one, it puts
// every file down in journal.json first. A memory file that changed on
// disk since read (a hand edit made meanwhile) is left as it is: the
// user's edit stays, and the next read takes it.
func write(f *file) error {
	written := maps.Clone(f.written)
	if written == nil {
		written = map[string]string{}
	}
	mt := meta{Version: 1, Projects: f.Projects, Digest: f.Digest, Published: f.Published, Dismissed: f.Dismissed,
		Removed: f.Removed, Declined: f.Declined, Written: written}
	var ops []op
	kept := map[string]bool{}
	for _, m := range f.Memories {
		if !validID(m.ID) || f.broken[m.ID] || kept[m.ID] {
			continue
		}
		kept[m.ID] = true
		if !m.Used.IsZero() || m.Uses > 0 {
			if mt.Used == nil {
				mt.Used = map[string]use{}
			}
			mt.Used[m.ID] = use{At: m.Used, Uses: m.Uses}
		}
		b, err := encode(m)
		if err != nil {
			return err
		}
		h := hashOf(b)
		if h == f.seen[m.ID] {
			written[m.ID] = h
			continue
		}
		if cur, err := os.ReadFile(in(m.ID + ".md")); err == nil && hashOf(cur) != f.seen[m.ID] ||
			err != nil && !errors.Is(err, fs.ErrNotExist) || err != nil && f.seen[m.ID] != "" {
			continue // changed (or gone, or unreadable) since read: not ours to write over
		}
		ops = append(ops, op{Name: m.ID + ".md", Data: string(b)})
		written[m.ID] = h
	}
	for _, id := range slices.Sorted(maps.Keys(f.seen)) {
		if kept[id] || f.broken[id] {
			continue
		}
		if cur, err := os.ReadFile(in(id + ".md")); err == nil && hashOf(cur) != f.seen[id] ||
			err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue // edited by hand since read: the user's, kept
		}
		ops = append(ops, op{Name: id + ".md", Del: true})
	}
	for id := range written {
		if !kept[id] && !f.broken[id] {
			delete(written, id)
		}
	}
	if len(written) == 0 {
		mt.Written = nil
	}
	metaWrite, err := metaChange(mt, f.metaB)
	if err != nil {
		return err
	}
	if metaWrite != nil {
		ops = append(ops, *metaWrite)
	}
	if len(ops) == 0 {
		return nil
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	if len(ops) > 1 {
		jb, err := json.Marshal(journal{Ops: ops})
		if err != nil {
			return err
		}
		if err := edit.WriteAtomic(in(journalName), jb); err != nil {
			return err
		}
	}
	if err := putDown(ops); err != nil {
		return err // the journal stays: the next read finishes it
	}
	if len(ops) > 1 {
		return os.Remove(in(journalName))
	}
	return nil
}

func metaChange(current meta, previous []byte) (*op, error) {
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return nil, err
	}
	if bytes.Equal(data, previous) {
		return nil, nil
	}
	return &op{Name: metaName, Data: string(data)}, nil
}

func touch(ids []string, at time.Time) {
	if len(ids) == 0 || !mu.LockChecked() {
		return
	}
	defer mu.Unlock()
	if err := replay(); err != nil {
		return
	}
	current, data, err := readMeta()
	if errors.Is(err, fs.ErrNotExist) {
		current = meta{Version: 1}
	} else if err != nil {
		return
	}
	dir := Dir()
	notes := memoryNoteRead(dir)
	var scratch []byte
	changed := false
	for _, id := range ids {
		useOf := current.Used[id]
		if !validID(id) || at.Sub(useOf.At) <= time.Minute {
			continue
		}
		info, err := os.Lstat(filepath.Join(dir, id+".md"))
		if err != nil || info.IsDir() {
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				notes.forget(id)
			}
			continue
		}
		cached := entry{}
		if parsedMemories.dir == dir {
			cached = parsedMemories.entries[id]
		}
		if _, err := readMemoryFile(&notes, id, info.ModTime(), &scratch, cached); err != nil {
			continue
		}
		if current.Used == nil {
			current.Used = map[string]use{}
		}
		current.Used[id] = use{At: at, Uses: useOf.Uses + 1}
		changed = true
	}
	notes.finish(nil)
	if changed {
		if write, err := metaChange(current, data); err == nil && write != nil {
			_ = putDown([]op{*write})
		}
	}
}

// putDown puts ops down, each file whole (written through a temp file).
func putDown(ops []op) error {
	for _, o := range ops {
		id, md := strings.CutSuffix(o.Name, ".md")
		if !(md && validID(id) || o.Name == metaName) {
			return fmt.Errorf("memory: the journal names %q, not a memory file", o.Name)
		}
		p := in(o.Name)
		if o.Del {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if o.Name == metaName {
			if err := lastgood.Keep(p, lastgood.Into[meta], 0o600); err != nil {
				return err
			}
		}
		if err := edit.WriteAtomic(p, []byte(o.Data)); err != nil {
			return err
		}
		if err := cut(); err != nil {
			return err
		}
	}
	return nil
}

// cut is called after each file putDown writes; a test's makes a write
// stop there, as a crash would.
var cut = func() error { return nil }

// replay finishes the change a journal holds, if one was cut off. A
// journal that can't be read is an error: what was written of it is
// unknown.
func replay() error {
	b, err := os.ReadFile(in(journalName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var j journal
	if err := json.Unmarshal(b, &j); err != nil {
		return fmt.Errorf("memory: %s can't be read: %w", in(journalName), err)
	}
	if err := putDown(j.Ops); err != nil {
		return err
	}
	return os.Remove(in(journalName))
}

// migrate moves memory.json into Dir, all at once (a journal), keeps it as
// memory.json.migrated, and reads Dir. A memory.json that can't be read
// is an error, and nothing is written.
func migrate() (*file, error) {
	b, err := lastgood.Read(Path(), lastgood.Into[file])
	if err != nil {
		return nil, err
	}
	old := &file{}
	if err := json.Unmarshal(b, old); err != nil {
		return nil, err
	}
	old.Memories = slices.DeleteFunc(old.Memories, func(m *Memory) bool { return m == nil || !validID(m.ID) })
	if err := write(old); err != nil {
		return nil, err
	}
	if err := os.Rename(Path(), Path()+".migrated"); err != nil {
		log.Printf("memory: moved into %s, but memory.json stays: %v", Dir(), err)
	}
	return read()
}

// change reads memory, lets fn change it and writes what changed, one
// magpie at a time. Memory that can't be read is never written over.
func change(fn func(f *file) error) error {
	return changeContext(context.Background(), fn)
}

func changeContext(ctx context.Context, fn func(f *file) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !mu.LockChecked() {
		return errStoreLock
	}
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := read()
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return write(f)
}

// view reads memory under the lock: consistent with the last write, and
// with no journal left half done.
func view() (*file, error) {
	if !mu.LockChecked() {
		return nil, errStoreLock
	}
	defer mu.Unlock()
	return read()
}

// Modified is when memory last changed, zero when there is none: for a
// reader to tell whether it changed.
func Modified() time.Time {
	var t time.Time
	es, _ := os.ReadDir(Dir())
	for _, e := range es {
		if e.Name() == indexName {
			continue
		}
		if i, err := e.Info(); err == nil && i.ModTime().After(t) {
			t = i.ModTime()
		}
	}
	return t
}

// scopeOf is a front matter's scope: as written, since a project's holds
// a path, but "global" in any case is Global, and none is Global too.
func scopeOf(s string) string {
	if s = strings.TrimSpace(s); s == "" || strings.EqualFold(s, Global) {
		return Global
	}
	return s
}
