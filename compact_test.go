package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickyoung/ask/internal/event"
	"github.com/patrickyoung/ask/internal/provider"
)

// TestCompactLeavesThreeSessionsThatAllReplay is the whole contract: the
// source is untouched, the note was written somewhere inspectable, and the
// conversation continues from it. If any of the three stopped replaying,
// compaction would have become the one place the log cannot be trusted.
func TestCompactCarriesTheWorkIntoAFreshSession(t *testing.T) {
	dir, _, bodies := fake(t, 200, answerWire)
	if code, _, e := exec(t, "", "the original question"); code != 0 {
		t.Fatalf("seed: exit %d: %s", code, e)
	}
	src := filepath.Join(dir, sessions(t, dir)[0])
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := exec(t, "", "compact")
	if code != 0 {
		t.Fatalf("compact: exit %d: %s", code, stderr)
	}

	// stdout is the new session's path, so it can be handed to -f.
	out := strings.TrimSpace(stdout)
	if fi, err := os.Stat(out); err != nil || fi.IsDir() {
		t.Fatalf("stdout %q is not a session file: %v", out, err)
	}
	if out == src {
		t.Fatal("compact returned the session it was given")
	}
	if after, _ := os.ReadFile(src); string(after) != string(before) {
		t.Error("the source session was modified; it must be read-only to compact")
	}
	if n := len(sessions(t, dir)); n != 3 {
		t.Fatalf("have %d sessions, want 3 (source, summarizer, compacted)", n)
	}
	for _, s := range sessions(t, dir) {
		ev, err := event.ReadFile(filepath.Join(dir, s))
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(s), err)
		}
		if err := event.Check(ev); err != nil {
			t.Errorf("%s does not replay: %v", filepath.Base(s), err)
		}
	}

	// The summarizer was sent the conversation, not asked a question about it.
	last := (*bodies)[len(*bodies)-1]
	if !strings.Contains(last, "the original question") {
		t.Errorf("the summarizer never saw the transcript:\n%s", last)
	}
	if !strings.Contains(last, "handoff note") {
		t.Errorf("the summarizer did not get the handoff prompt:\n%s", last)
	}
	if !strings.Contains(stderr, "compacted") {
		t.Errorf("stderr does not say what happened:\n%s", stderr)
	}
}

// TestCompactStampsTheNote: model-written text entering a conversation is
// exactly what must never be indistinguishable from what somebody asked.
func TestCompactStampsTheNote(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	exec(t, "", "the original question")
	srcID := strings.TrimSuffix(sessions(t, dir)[0], ".jsonl")

	code, stdout, stderr := exec(t, "", "compact")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	ev, err := event.ReadFile(strings.TrimSpace(stdout))
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := event.As[event.Header](ev[0])
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Parent != srcID {
		t.Errorf("header parent = %q, want %q", hdr.Parent, srcID)
	}
	if hdr.Summary == "" {
		t.Error("header does not name the session the note was written in")
	}
	if _, err := os.Stat(filepath.Join(dir, hdr.Summary+".jsonl")); err != nil {
		t.Errorf("header names a summarizer session that does not exist: %v", err)
	}
	u, err := event.As[event.UserData](ev[1])
	if err != nil {
		t.Fatal(err)
	}
	if u.Source != "summary" {
		t.Errorf("the note is stamped %q, want %q: a reader must be able to tell "+
			"which message nobody actually said", u.Source, "summary")
	}
	if strings.TrimSpace(u.Text) == "" {
		t.Error("the note is empty")
	}
}

// TestCompactedSessionContinues: the note is a message, so the next turn is
// answered in light of it. Two user messages in a row is the shape this
// produces, and it has to survive the fold.
func TestCompactedSessionContinues(t *testing.T) {
	dir, _, bodies := fake(t, 200, answerWire)
	exec(t, "", "the original question")
	_, stdout, _ := exec(t, "", "compact")
	compacted := strings.TrimSpace(stdout)

	code, _, stderr := exec(t, "", "-f", compacted, "and now the next thing")
	if code != 0 {
		t.Fatalf("continuing a compacted session: exit %d: %s", code, stderr)
	}
	ev, err := event.ReadFile(compacted)
	if err != nil {
		t.Fatal(err)
	}
	if err := event.Check(ev); err != nil {
		t.Errorf("a continued compacted session does not replay: %v", err)
	}
	msgs, err := event.Fold(ev[:len(ev)-2]) // everything before the last request
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 2 || msgs[0].Role != provider.User || msgs[1].Role != provider.User {
		t.Fatalf("fold produced %d messages with roles %v; the note and the "+
			"question are both user messages and both must be sent", len(msgs), roles(msgs))
	}
	last := (*bodies)[len(*bodies)-1]
	if !strings.Contains(last, "and now the next thing") {
		t.Errorf("the new question never reached the provider:\n%s", last)
	}
	if strings.Contains(last, "the original question") {
		t.Error("the compacted session carried the old conversation; it must carry only the note")
	}
	_ = dir
}

// TestCompactMovesCurrentOnlyWhenItWasCurrent: compacting the conversation
// you are in moves you into the compact one, because that is the only
// reason to do it. Compacting some other file leaves you where you were.
func TestCompactMovesCurrentOnlyWhenItWasCurrent(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	exec(t, "", "the current conversation")

	_, stdout, _ := exec(t, "", "compact")
	compacted := strings.TrimSpace(stdout)
	cur, err := event.Latest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := real(t, cur), real(t, compacted); a != b {
		t.Errorf("current = %s, want the compacted session %s", filepath.Base(a), filepath.Base(compacted))
	}

	// A thread of the caller's own is not the current session, and
	// compacting it must not hijack one.
	thread := filepath.Join(t.TempDir(), "thread.jsonl")
	exec(t, "", "-f", thread, "a separate thread")
	if code, _, e := exec(t, "", "compact", thread); code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	after, _ := event.Latest(dir)
	if a, b := real(t, after), real(t, compacted); a != b {
		t.Errorf("compacting an unrelated file moved current to %s", filepath.Base(a))
	}
}

func TestCompactRefusesWhatItCannotDo(t *testing.T) {
	dir, calls, _ := fake(t, 200, answerWire)
	before := len(sessions(t, dir))

	if code, _, _ := exec(t, "", "compact", "no-such-session"); code != 1 {
		t.Error("compacting a session that does not exist should exit 1")
	}
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	os.WriteFile(empty, nil, 0o600)
	if code, _, stderr := exec(t, "", "compact", empty); code != 1 {
		t.Errorf("compacting an empty file exited %d: %s", code, stderr)
	}
	if n := len(sessions(t, dir)); n != before {
		t.Errorf("a refused compaction left %d new files behind", n-before)
	}
	if calls.Load() != 0 {
		t.Error("a refused compaction called the provider")
	}
}

func TestCompactVerifiesSourceBeforeCallingProvider(t *testing.T) {
	for _, damage := range []string{"unsealed", "digest", "sequence", "seal", "evidence"} {
		t.Run(damage, func(t *testing.T) {
			dir, calls, _ := fake(t, 200, answerWire)
			log, err := event.Create(dir)
			if err != nil {
				t.Fatal(err)
			}
			header(log, "anthropic/test-model", "")
			user := event.UserData{Text: "original question"}
			if damage == "evidence" {
				user.Evidence = &event.EvidenceData{Block: 0, SnapshotBytes: 1000}
			}
			log.Append(event.User, user)
			switch damage {
			case "unsealed":
				log.Append(event.Note, event.NoteData{Source: "ply", Kind: "ply.verifier/v1", Body: []byte(`{"status":"accepted"}`)})
			case "digest":
				log.Append(event.Request, provider.Request{Model: "test-model", Digest: "wrong"})
			case "seal":
				log.Append(event.Seal, event.SealData{Through: 2, SHA256: "wrong"})
			}
			log.Close()
			before, err := os.ReadFile(log.Path())
			if err != nil {
				t.Fatal(err)
			}
			if damage == "sequence" {
				before = []byte(strings.Replace(string(before), `"seq":2`, `"seq":3`, 1))
				if err := os.WriteFile(log.Path(), before, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			code, out, stderr := exec(t, "", "compact", "-q", log.Path())
			if code != 1 || out != "" || !strings.Contains(stderr, "verify session before compact") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
			}
			if calls.Load() != 0 || len(sessions(t, dir)) != 1 {
				t.Fatal("refused compaction called the provider or created a session")
			}
			after, _ := os.ReadFile(log.Path())
			if string(after) != string(before) {
				t.Fatal("refused compaction modified the source")
			}
		})
	}
}

func TestCompactRefusesAnIncompleteSummary(t *testing.T) {
	for _, mode := range []string{"max_tokens", "eof"} {
		t.Run(mode, func(t *testing.T) {
			wire := strings.ReplaceAll(answerWire, "end_turn", "max_tokens")
			if mode == "eof" {
				wire = strings.Split(answerWire, "event: message_delta")[0]
			}
			dir, _, _ := fake(t, 200, wire)
			log, err := event.Create(dir)
			if err != nil {
				t.Fatal(err)
			}
			header(log, "anthropic/test-model", "")
			log.Append(event.User, event.UserData{Text: "original question"})
			event.SetCurrent(dir, log)
			log.Close()
			code, out, stderr := exec(t, "", "compact", "-q")
			if code != 1 || out != "" || stderr == "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
			}
			cur, err := event.Current(dir)
			if err != nil || cur != log.Path() || len(sessions(t, dir)) != 2 {
				t.Fatal("incomplete summary created a handoff or moved current")
			}
		})
	}
}

func TestCompactAcceptsVerifiedStructuredNotes(t *testing.T) {
	dir, _, bodies := fake(t, 200, answerWire)
	for _, args := range [][]string{
		{"-q", "original question"},
		{"note", "-q", "-s", "ply", "-k", "ply.verifier/v1", "-json", `{"status":"accepted"}`, "-seal"},
		{"compact", "-q"},
	} {
		if code, _, stderr := exec(t, "", args...); code != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", args, code, stderr)
		}
	}
	if len(sessions(t, dir)) != 3 || len(*bodies) != 2 {
		t.Fatal("compaction did not create its summarizer and handoff sessions")
	}
	if !strings.Contains((*bodies)[1], "ply:ply.verifier/v1") || !strings.Contains((*bodies)[1], "accepted") {
		t.Fatal("verified note was not included in the summarizer input")
	}
	for _, name := range sessions(t, dir) {
		events, err := event.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := event.Check(events); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTranscriptCarriesWhatTransfers: reasoning is provider-opaque and
// addressed to a turn that is over, and leaving it out is most of why a
// transcript fits where the conversation it came from did not.
func TestTranscriptCarriesWhatTransfers(t *testing.T) {
	log, path := tmpLog(t)
	log.Append(event.Session, event.Header{ID: "x", Model: "m"})
	log.Append(event.User, event.UserData{Text: "what broke?"})
	log.Append(event.Assistant, event.Turn{Blocks: []provider.Block{
		{Type: provider.Reasoning, Text: "SECRET-THINKING"},
		{Type: provider.Text, Text: "the ring buffer wraps early"},
	}})
	log.Append(event.User, event.UserData{
		Blocks: []provider.Block{{Type: provider.Media, MediaType: "image/png", Data: []byte("PNGBYTES")}},
	})
	log.Append(event.Assistant, event.Turn{Partial: true, Blocks: []provider.Block{
		{Type: provider.Text, Text: "TORN-TURN"},
	}})
	log.Close()

	ev, err := event.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := transcript(ev)
	for _, want := range []string{"what broke?", "the ring buffer wraps early", "image/png"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript is missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"SECRET-THINKING", "PNGBYTES", "TORN-TURN"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("transcript carried %q, which does not transfer:\n%s", unwanted, got)
		}
	}
}

// TestOldLogsAreUnaffected: the new header and user fields are additive, so
// a session written before them folds to the same conversation and proves
// the same way. Migrate formats, never relax the check.
func TestOldLogsAreUnaffected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.jsonl")
	lines := []string{
		`{"seq":1,"type":"session","data":{"id":"old","ask":"0.0.1","model":"anthropic/m","system":"s"}}`,
		`{"seq":2,"type":"user","data":{"text":"hello"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ev, err := event.ReadFile(path)
	if err != nil {
		t.Fatalf("a log written before the new fields no longer reads: %v", err)
	}
	if err := event.Check(ev); err != nil {
		t.Errorf("an old log no longer replays: %v", err)
	}
	msgs, err := event.Fold(ev)
	if err != nil || len(msgs) != 1 || msgs[0].Blocks[0].Text != "hello" {
		t.Errorf("an old log folds differently now: %v %v", msgs, err)
	}
	hdr, err := event.As[event.Header](ev[0])
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Parent != "" || hdr.Summary != "" {
		t.Error("an old header invented provenance it never had")
	}
	// A message with no source is one somebody actually asked. That is
	// every message ask has ever written except the note.
	u, _ := event.As[event.UserData](ev[1])
	if u.Source != "" {
		t.Errorf("an old user message claims source %q", u.Source)
	}
	raw, _ := json.Marshal(u)
	if strings.Contains(string(raw), "source") {
		t.Errorf("an empty source is being written to disk: %s", raw)
	}
}

// real resolves a path the way macOS needs it before two of them can be
// compared: /var and /private/var are the same directory.
func real(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

func tmpLog(t *testing.T) (*event.Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	log, err := event.CreateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return log, path
}

func roles(msgs []provider.Message) []provider.Role {
	out := make([]provider.Role, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}
