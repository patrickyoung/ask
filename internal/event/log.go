package event

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Log is a single-writer append-only session file. Writes are unbuffered
// and O_APPEND, so every line lands at the true end of the file and no
// stale offset can leave a hole of zeroes mid-session.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	path string
	lock string
	seq  int
	obs  func(Event)
}

// Create starts a new session under a minted id in dir.
func Create(dir string) (*Log, error) {
	return CreateFile(filepath.Join(dir, newID()+".jsonl"))
}

// CreateFile starts a new session at an exact path — what `ask -f` uses to
// keep a thread where the caller wants it. O_EXCL means an existing file is
// never silently overwritten: a conversation is appended to or refused.
func CreateFile(path string) (*Log, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	lock, err := acquire(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Remove(lock)
		return nil, err
	}
	return &Log{f: f, path: path, lock: lock}, nil
}

// Open appends to an existing session, returning its events so the caller
// can continue it. It refuses (with the holder's pid) if another live
// process holds the session; a lock left by a dead process is stolen.
//
// The lock is what keeps the replay invariant true under concurrency: two
// processes appending to one log would interleave two conversations, and
// every request digest written after the first interleaving would fail to
// match its fold. Refusing is the only honest answer.
func Open(path string) (*Log, []Event, error) {
	events, err := ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	lock, err := acquire(path)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Remove(lock)
		return nil, nil, err
	}
	seq := 0
	if n := len(events); n > 0 {
		seq = events[n-1].Seq
	}
	return &Log{f: f, path: path, lock: lock, seq: seq}, events, nil
}

// Path returns the session file path.
func (l *Log) Path() string { return l.path }

// ID returns the session id (the file basename without extension).
func (l *Log) ID() string {
	return strings.TrimSuffix(filepath.Base(l.path), ".jsonl")
}

// Observe registers a function called with every appended event, after it
// is written. Renderers hang off this; there is no second logging pipeline.
func (l *Log) Observe(fn func(Event)) { l.obs = fn }

// Append marshals data and writes one event line.
func (l *Log) Append(t Type, data any) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("marshal %s: %w", t, err)
	}
	l.seq++
	e := Event{Seq: l.seq, Time: time.Now().UTC(), Type: t, Data: raw}
	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, err
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return Event{}, fmt.Errorf("append %s: %w", l.path, err)
	}
	if l.obs != nil {
		l.obs(e)
	}
	return e, nil
}

// Sync fsyncs the file.
func (l *Log) Sync() error { return l.f.Sync() }

// Close syncs, releases the lock, and closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.f.Sync()
	os.Remove(l.lock)
	return errors.Join(err, l.f.Close())
}

// ReadFile parses a session file. A torn final line (a crash mid-write) is
// dropped; corruption anywhere else is an error — but the events read up
// to that point are returned with it, so a caller that would rather show a
// damaged session than nothing can. Callers that need the whole log check
// the error and stop; only rendering and inspection use the salvage.
func ReadFile(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []Event
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		atEOF := err == io.EOF
		if err != nil && !atEOF {
			return nil, err
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var e Event
			if jerr := json.Unmarshal(line, &e); jerr != nil {
				if atEOF || !more(r) {
					return events, nil // torn tail
				}
				return events, fmt.Errorf("%s: corrupt event after seq %d: %w", path, lastSeq(events), jerr)
			}
			events = append(events, e)
		}
		if atEOF {
			return events, nil
		}
	}
}

func more(r *bufio.Reader) bool {
	_, err := r.Peek(1)
	return err == nil
}

func lastSeq(events []Event) int {
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Seq
}

// Latest returns the path of dir's current session: the target of the
// current symlink, falling back to the most recently written session file
// (ids collide on same-second creation, mtime doesn't). It returns
// os.ErrNotExist if there are no sessions.
func Latest(dir string) (string, error) {
	if target, err := os.Readlink(filepath.Join(dir, "current")); err == nil {
		p := filepath.Join(dir, target)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	names, err := List(dir)
	if err != nil {
		return "", err
	}
	newest, best := "", time.Time{}
	for _, name := range names {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err == nil && (newest == "" || fi.ModTime().After(best)) {
			newest, best = name, fi.ModTime()
		}
	}
	if newest == "" {
		return "", os.ErrNotExist
	}
	return filepath.Join(dir, newest), nil
}

// List returns session file names in dir, oldest first. Session ids are
// time-prefixed, so lexical order is chronological.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// newID returns a sortable, human-readable session id:
// 20260801-153042-9f3a1c8b22d1e0ff. Unique per directory by O_EXCL; the
// 64 random bits keep same-second invocations from colliding.
func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return fmt.Sprintf("%s-%x", time.Now().UTC().Format("20060102-150405"), b)
}

// acquire takes path.lock via O_EXCL, stealing locks whose holder is dead.
func acquire(path string) (string, error) {
	lock := path + ".lock"
	for range 2 {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return lock, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		pid, perr := readPid(lock)
		if perr == nil && alive(pid) {
			return "", fmt.Errorf("session %s is in use by running process %d", filepath.Base(path), pid)
		}
		os.Remove(lock) // stale: holder is gone
	}
	return "", fmt.Errorf("could not lock %s", path)
}

func readPid(lock string) (int, error) {
	b, err := os.ReadFile(lock)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil || errors.Is(syscall.Kill(pid, 0), syscall.EPERM)
}

// SetCurrent atomically repoints dir/current at the given session file.
// This is what `ask -c` follows.
func SetCurrent(l *Log) {
	dir := filepath.Dir(l.path)
	tmp := filepath.Join(dir, ".current.tmp")
	os.Remove(tmp)
	if err := os.Symlink(filepath.Base(l.path), tmp); err != nil {
		return // best effort; Latest falls back to newest file
	}
	os.Rename(tmp, filepath.Join(dir, "current"))
}
