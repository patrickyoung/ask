package event

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Log is a single-writer append-only session file. Writes are unbuffered
// and O_APPEND, so every line lands at the true end of the file and no
// stale offset can leave a hole of zeroes mid-session.
type Log struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	seq    int
	obs    func(Event)
	events []Event
}

// Create starts a new session under a minted id in dir.
func Create(dir string) (*Log, error) {
	return CreateFile(filepath.Join(dir, newID()+".jsonl"))
}

// CreateFile starts a new session at an exact path — what `ask -f` uses to
// keep a thread where the caller wants it. O_EXCL means an existing file is
// never silently overwritten: a conversation is appended to or refused.
func CreateFile(path string) (*Log, error) {
	// 0700, matching the 0600 on the files: a session log holds every
	// question, every answer, and the bytes of every attachment verbatim.
	// The directory should not be more readable than what is in it.
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(f, path); err != nil {
		f.Close()
		os.Remove(path) // O_EXCL means this file is ours and is empty
		return nil, err
	}
	return &Log{f: f, path: path}, nil
}

// Open appends to an existing session, returning its events so the caller
// can continue it. It refuses if another process is writing the session.
//
// The lock is what keeps the replay invariant true under concurrency: two
// processes appending to one log would interleave two conversations, and
// every request digest written after the first interleaving would fail to
// match its fold. Refusing is the only honest answer.
func Open(path string) (*Log, []Event, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := lock(f, path); err != nil {
		f.Close()
		return nil, nil, err
	}
	// Read under the lock, never before taking it. Events read first can be
	// a turn out of date by the time the lock is held — the previous writer
	// may still have been finishing — and a fold over a stale prefix writes
	// a digest the file itself will not reproduce. That is precisely the
	// divergence the lock exists to prevent, arrived at from the other side.
	events, err := ReadFile(path)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if err := truncateTornTail(f, path); err != nil {
		f.Close()
		return nil, nil, err
	}
	// A new seal is meaningful only if the prefix it commits was already
	// valid. Verify under the same writer lock used for the read so a note
	// cannot bless an older divergence and defer discovery until replay.
	if err := Check(events); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("verify session before append: %w", err)
	}
	seq := 0
	if n := len(events); n > 0 {
		seq = events[n-1].Seq
	}
	return &Log{f: f, path: path, seq: seq, events: append([]Event(nil), events...)}, events, nil
}

// lock takes an exclusive advisory lock on the session file itself. The
// kernel owns it: it belongs to this open file description and is released
// when the descriptor closes, including when the process dies, however it
// dies. So there is no lock file, no recorded pid, and no staleness.
//
// A pid file cannot do this honestly, which is why it is not one. Judging a
// lock stale means reading a pid, asking whether it is alive, and removing
// a file — three steps that are not one operation, so two processes
// starting together can each remove the other's lock and both proceed,
// which is the interleaving the lock exists to prevent. And pids are
// recycled: an unrelated process inheriting a dead writer's pid makes the
// session look busy forever, with nothing telling the user what to delete.
func lock(f *os.File, path string) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return fmt.Errorf("session %s is in use by another ask process", filepath.Base(path))
	}
	return fmt.Errorf("locking %s: %w", path, err)
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
	return l.appendLocked(t, data)
}

func (l *Log) appendLocked(t Type, data any) (Event, error) {
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
	l.events = append(l.events, e)
	if l.obs != nil {
		l.obs(e)
	}
	return e, nil
}

// AppendSealed writes a structured record and a digest of the exact log prefix
// containing it. Both writes are fsynced before success is returned. A crash
// between them leaves an explicitly unverifiable record rather than a record
// replay could mistake for durable evidence.
func (l *Log) AppendSealed(t Type, data any) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	record, err := l.appendLocked(t, data)
	if err != nil {
		return Event{}, err
	}
	if err := l.f.Sync(); err != nil {
		return Event{}, fmt.Errorf("sync record in %s: %w", l.path, err)
	}
	digest, err := PrefixDigest(l.events)
	if err != nil {
		return Event{}, err
	}
	if _, err := l.appendLocked(Seal, SealData{Through: record.Seq, SHA256: digest}); err != nil {
		return Event{}, err
	}
	if err := l.f.Sync(); err != nil {
		return Event{}, fmt.Errorf("sync seal in %s: %w", l.path, err)
	}
	return record, nil
}

// PrefixDigest hashes events exactly as they appear in a session JSONL file.
func PrefixDigest(events []Event) (string, error) {
	h := sha256.New()
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			return "", err
		}
		h.Write(line)
		h.Write([]byte{'\n'})
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

// Sync fsyncs the file.
func (l *Log) Sync() error { return l.f.Sync() }

// Close syncs and closes the file, which releases the lock.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.f.Sync()
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
		// An Ask event is one newline-terminated JSON value. Even if a final
		// fragment happens to be valid JSON, without its newline the writer did
		// not complete the append and it is a torn event, not history.
		if atEOF && len(line) > 0 {
			return events, nil
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var e Event
			if jerr := json.Unmarshal(line, &e); jerr != nil {
				if atEOF {
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

// truncateTornTail discards bytes after the final newline before a locked log
// is continued. Those bytes were never a complete event; leaving them in place
// would splice the next append onto corruption and destroy replay permanently.
func truncateTornTail(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	r, err := os.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	var last [1]byte
	if _, err := r.ReadAt(last[:], info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	const chunkSize = 4096
	buf := make([]byte, chunkSize)
	for end := info.Size(); end > 0; {
		start := end - chunkSize
		if start < 0 {
			start = 0
		}
		n, err := r.ReadAt(buf[:end-start], start)
		if err != nil && err != io.EOF {
			return err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			if err := f.Truncate(start + int64(i) + 1); err != nil {
				return fmt.Errorf("repair torn tail in %s: %w", path, err)
			}
			return nil
		}
		end = start
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("repair torn tail in %s: %w", path, err)
	}
	return nil
}

func lastSeq(events []Event) int {
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Seq
}

// Current returns exactly the session named by dir/current. A missing or
// dangling pointer is an error: explicit continuation must not guess.
func Current(dir string) (string, error) {
	target, err := os.Readlink(filepath.Join(dir, "current"))
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, target)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

// Latest returns Current when it exists, falling back to the most recently
// written session file. It returns os.ErrNotExist if there are no sessions.
func Latest(dir string) (string, error) {
	if p, err := Current(dir); err == nil {
		return p, nil
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

// SetCurrent atomically repoints dir/current at the given session file.
// This is what `ask -c` follows to continue a conversation, via Current.
//
// It declines when the session does not live in dir. `current` names the
// one session `ask -c` continues, and -c looks in the conversation directory.
func SetCurrent(dir string, l *Log) error {
	if filepath.Clean(dir) != filepath.Dir(l.path) {
		return nil
	}
	// Named per process: a shared temporary name lets two concurrent runs
	// delete each other's half-made symlink, and the rename that follows
	// then fails for no reason worth having.
	tmp := filepath.Join(dir, fmt.Sprintf(".current.%d.tmp", os.Getpid()))
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(filepath.Base(l.path), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "current")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
