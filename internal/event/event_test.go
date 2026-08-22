package event

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrickyoung/ask/internal/provider"
)

func user(text string) Event {
	raw, _ := json.Marshal(UserData{Text: text})
	return Event{Type: User, Data: raw}
}

func assistant(text string, partial bool) Event {
	raw, _ := json.Marshal(Turn{
		Blocks:  []provider.Block{{Type: provider.Text, Text: text}},
		Partial: partial,
	})
	return Event{Type: Assistant, Data: raw}
}

func request(t *testing.T, seq int, msgs []provider.Message) Event {
	t.Helper()
	raw, err := json.Marshal(provider.Request{Model: "m"}.Logged())
	if err != nil {
		t.Fatal(err)
	}
	if msgs != nil {
		raw, err = json.Marshal(provider.Request{Model: "m", Messages: msgs}.Logged())
		if err != nil {
			t.Fatal(err)
		}
	}
	return Event{Seq: seq, Type: Request, Data: raw}
}

func TestFoldProjectsConversation(t *testing.T) {
	events := []Event{
		{Type: Session},
		user("hi"),
		assistant("hello", false),
		{Type: Retry},
		user("again"),
		assistant("torn", true), // partial: never part of context
		{Type: Abort},
		assistant("sure", false),
		{Type: Done},
	}
	msgs, err := Fold(events)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		role provider.Role
		text string
	}{
		{provider.User, "hi"},
		{provider.Assistant, "hello"},
		{provider.User, "again"},
		{provider.Assistant, "sure"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("folded %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i].Role != w.role || msgs[i].Blocks[0].Text != w.text {
			t.Errorf("message %d = %s %q, want %s %q", i, msgs[i].Role, msgs[i].Blocks[0].Text, w.role, w.text)
		}
	}
}

// TestCheckHoldsAcrossTurns is the headline property: what was sent is a
// function of what is logged, on every turn of a conversation.
func TestCheckHoldsAcrossTurns(t *testing.T) {
	var events []Event
	for turn := range 4 {
		events = append(events, user("q"))
		msgs, err := Fold(events)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, request(t, turn*3+2, msgs))
		events = append(events, assistant("a", false))
	}
	if err := Check(events); err != nil {
		t.Fatalf("Check on a well-formed log: %v", err)
	}
}

// TestCheckCatchesDivergence: an event edited after the fact must fail the
// check. Without this the log would be a story rather than a record.
func TestCheckCatchesDivergence(t *testing.T) {
	events := []Event{user("hi")}
	msgs, _ := Fold(events)
	events = append(events, request(t, 2, msgs), assistant("hello", false))

	// Rewrite history: the user asked something else.
	events[0] = user("hi there")
	err := Check(events)
	if err == nil {
		t.Fatal("Check passed on a log whose history was rewritten")
	}
	if !strings.Contains(err.Error(), "replay divergence") {
		t.Errorf("error = %v, want a replay divergence", err)
	}
}

// TestCheckRefusesEmptyRequest: a request recording neither messages nor a
// digest must be an error, never a pass. If a lost field could make Check
// vacuously succeed, the invariant would erode silently.
func TestCheckRefusesEmptyRequest(t *testing.T) {
	raw, _ := json.Marshal(provider.Request{Model: "m"})
	err := Check([]Event{user("hi"), {Seq: 2, Type: Request, Data: raw}})
	if err == nil || !strings.Contains(err.Error(), "neither messages nor a digest") {
		t.Fatalf("Check err = %v, want a refusal to verify nothing", err)
	}
}

// TestCheckVerifiesPreDigestLogs: logs written before the digest carry the
// messages themselves and must still verify, or old sessions stop being
// readable when the format moves on.
func TestCheckVerifiesPreDigestLogs(t *testing.T) {
	events := []Event{user("hi")}
	msgs, _ := Fold(events)
	raw, _ := json.Marshal(provider.Request{Model: "m", Messages: msgs}) // no Logged(): messages inline
	events = append(events, Event{Seq: 2, Type: Request, Data: raw})
	if err := Check(events); err != nil {
		t.Fatalf("Check on a pre-digest log: %v", err)
	}

	events[0] = user("something else")
	if err := Check(events); err == nil {
		t.Fatal("structural branch passed on a rewritten history")
	}
}

func TestLogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	log, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	var observed []Type
	log.Observe(func(e Event) { observed = append(observed, e.Type) })
	if _, err := log.Append(Session, Header{ID: log.ID(), Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(User, UserData{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := SetCurrent(dir, log); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []Type{Session, User}; len(observed) != 2 || observed[0] != want[0] || observed[1] != want[1] {
		t.Errorf("observed %v, want %v", observed, want)
	}

	events, err := ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("read back %+v", events)
	}

	// current points at it, and both exact and fallback lookup follow it.
	current, err := Current(dir)
	if err != nil {
		t.Fatal(err)
	}
	if current != log.Path() {
		t.Errorf("Current = %s, want %s", current, log.Path())
	}
	got, err := Latest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != log.Path() {
		t.Errorf("Latest = %s, want %s", got, log.Path())
	}

	// Reopening continues the sequence rather than restarting it.
	log2, prev, err := Open(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer log2.Close()
	if len(prev) != 2 {
		t.Fatalf("Open returned %d events, want 2", len(prev))
	}
	e, err := log2.Append(Done, DoneData{Reason: "end"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 3 {
		t.Errorf("seq after reopen = %d, want 3", e.Seq)
	}
}

func TestAppendSealedRoundTripAndTamperDetection(t *testing.T) {
	dir := t.TempDir()
	log, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(Session, Header{ID: log.ID(), Model: "m"}); err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(`{"status":"accepted","candidate_sha256":"sha256:abc"}`)
	if _, err := log.AppendSealed(Note, NoteData{Source: "ply", Kind: "ply.verifier/v1", Body: body}); err != nil {
		t.Fatal(err)
	}
	path := log.Path()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[1].Type != Note || events[2].Type != Seal {
		t.Fatalf("events = %+v, want session, note, seal", events)
	}
	if err := Check(events); err != nil {
		t.Fatalf("sealed log did not verify: %v", err)
	}

	var note NoteData
	if err := json.Unmarshal(events[1].Data, &note); err != nil {
		t.Fatal(err)
	}
	note.Body = json.RawMessage(`{"status":"rejected","candidate_sha256":"sha256:abc"}`)
	events[1].Data, _ = json.Marshal(note)
	if err := Check(events); err == nil || !strings.Contains(err.Error(), "seal divergence") {
		t.Fatalf("tampered record check = %v, want seal divergence", err)
	}
}

func TestCheckRefusesUnsealedStructuredRecordAndSequenceGap(t *testing.T) {
	raw, _ := json.Marshal(NoteData{Source: "ply", Kind: "ply.verifier/v1", Body: json.RawMessage(`{"status":"accepted"}`)})
	events := []Event{{Seq: 1, Type: Session, Data: json.RawMessage(`{}`)}, {Seq: 2, Type: Note, Data: raw}}
	if err := Check(events); err == nil || !strings.Contains(err.Error(), "not immediately sealed") {
		t.Fatalf("unsealed record check = %v", err)
	}
	events[1].Seq = 3
	if err := Check(events); err == nil || !strings.Contains(err.Error(), "sequence divergence") {
		t.Fatalf("sequence gap check = %v", err)
	}
}

// TestSetCurrentStaysHome: current names a session in the conversation
// directory. A session elsewhere cannot be named without dropping a symlink
// in a directory ask was only visiting.
func TestSetCurrentStaysHome(t *testing.T) {
	home, away := t.TempDir(), t.TempDir()

	outside, err := CreateFile(filepath.Join(away, "thread.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer outside.Close()
	if err := SetCurrent(home, outside); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(away, "current")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a session outside the conversation directory left a current symlink beside it")
	}
	if _, err := os.Lstat(filepath.Join(home, "current")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a session outside the conversation directory was made current")
	}

	// A session in the conversation directory can become current.
	inside, err := CreateFile(filepath.Join(home, "chosen.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer inside.Close()
	if err := SetCurrent(home, inside); err != nil {
		t.Fatal(err)
	}
	got, err := Latest(home)
	if err != nil {
		t.Fatal(err)
	}
	if got != inside.Path() {
		t.Errorf("Latest = %s, want %s", got, inside.Path())
	}
}

func TestCurrentDoesNotGuess(t *testing.T) {
	dir := t.TempDir()
	log, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := log.Path()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing.jsonl", filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := Current(dir); err == nil {
		t.Fatal("Current followed a fallback instead of refusing a dangling pointer")
	}
	if got, err := Latest(dir); err != nil || got != path {
		t.Fatalf("Latest = %q, %v; want fallback %q", got, err, path)
	}
}

// TestLockRefusesConcurrentWriters: two processes appending to one log
// would interleave two conversations, and every digest written after the
// interleaving would fail its fold. Refusing is the only honest answer.
func TestLockRefusesConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	log, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, _, err := Open(log.Path()); err == nil {
		t.Fatal("a second writer was allowed onto a live session")
	} else if !strings.Contains(err.Error(), "in use") {
		t.Errorf("error = %v, want it to name the holder", err)
	}
}

// TestKilledWriterStrandsNothing: the lock is held by the kernel on behalf
// of an open file description, so a writer that dies without cleaning up —
// SIGKILL, a panic, a pulled power cord — releases it on the way out. The
// pid file this replaced could not do that honestly: it had to guess
// liveness from a recorded pid, and a recycled pid made the session look
// busy forever with no documented way out. The child here is killed as
// violently as the operating system allows.
func TestKilledWriterStrandsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if os.Getenv("ASK_LOCK_CHILD") == "1" {
		log, err := CreateFile(os.Getenv("ASK_LOCK_PATH"))
		if err != nil {
			os.Exit(3)
		}
		log.Append(User, UserData{Text: "hello"})
		fmt.Println("held")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestKilledWriterStrandsNothing", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "ASK_LOCK_CHILD=1", "ASK_LOCK_PATH="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	held := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "held" {
				held <- "held"
				return
			}
		}
		close(held)
	}()
	select {
	case _, ok := <-held:
		if !ok {
			t.Fatal("child never took the lock")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the child to take the lock")
	}

	// While it lives, the session is genuinely refused.
	if log, _, err := Open(path); err == nil {
		log.Close()
		t.Fatal("a second writer was allowed onto a session another process holds")
	} else if !strings.Contains(err.Error(), "in use") {
		t.Errorf("error = %v, want it to say the session is in use", err)
	}

	if err := cmd.Process.Kill(); err != nil { // SIGKILL: no cleanup runs
		t.Fatal(err)
	}
	cmd.Wait()

	log, events, err := Open(path)
	if err != nil {
		t.Fatalf("a killed writer stranded the session: %v", err)
	}
	defer log.Close()
	if len(events) != 1 || events[0].Type != User {
		t.Errorf("read %d events from the dead writer's session, want its 1 user event", len(events))
	}
	if _, err := os.Stat(path + ".lock"); err == nil {
		t.Error("a lock file was left behind; the kernel holds the lock, not the filesystem")
	}
}

// TestCreateFileRefusesOverwrite: `ask -f` must append to a thread or
// refuse, never silently replace one.
func TestCreateFileRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread.jsonl")
	log, err := CreateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	log.Close()
	if _, err := CreateFile(path); err == nil {
		t.Fatal("CreateFile overwrote an existing session")
	}
}

// TestReadFileTornTail: a crash mid-write leaves a half line. Dropping it
// is right — that turn never completed — but only at the very end.
func TestReadFileTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	good := `{"seq":1,"type":"user","data":{"text":"hi"}}` + "\n"
	if err := os.WriteFile(path, []byte(good+`{"seq":2,"type":"assis`), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := ReadFile(path)
	if err != nil {
		t.Fatalf("torn tail should not be an error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("read %d events, want 1", len(events))
	}
}

func TestOpenRepairsTornTailBeforeContinuing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	good := `{"seq":1,"type":"session","data":{}}` + "\n"
	if err := os.WriteFile(path, []byte(good+`{"seq":2,"type":"assis`), 0o600); err != nil {
		t.Fatal(err)
	}
	log, previous, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(previous) != 1 {
		t.Fatalf("previous events = %d, want 1", len(previous))
	}
	if _, err := log.Append(Done, DoneData{Reason: "end"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := ReadFile(path)
	if err != nil || len(events) != 2 || events[1].Seq != 2 {
		t.Fatalf("continued events=%+v err=%v", events, err)
	}
}

func TestReadFileMalformedTerminatedTailIsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"seq":1,"type":"user","data":{"text":"hi"}}` + "\n" +
		`{"seq":2,"type":"broken"` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := ReadFile(path)
	if err == nil {
		t.Fatal("newline-terminated malformed final event was treated as a torn write")
	}
	if len(events) != 1 {
		t.Fatalf("salvaged %d events, want 1", len(events))
	}
}

// TestReadFileCorruptMiddle: damage anywhere but the tail is real
// corruption. The events before it come back with the error, so a reader
// can show what survives while a verifier still refuses.
func TestReadFileCorruptMiddle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"seq":1,"type":"user","data":{"text":"hi"}}` + "\n" +
		"\x00\x00 not json\n" +
		`{"seq":3,"type":"done","data":{"reason":"end"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := ReadFile(path)
	if err == nil {
		t.Fatal("corruption mid-file was not reported")
	}
	if len(events) != 1 {
		t.Errorf("salvaged %d events, want the 1 before the damage", len(events))
	}
}

func TestListIsChronological(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for range 3 {
		log, err := Create(dir)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, filepath.Base(log.Path()))
		log.Close()
	}
	names, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Fatalf("listed %d, want 3", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("ids are not sorted chronologically: %v", names)
		}
	}
}
