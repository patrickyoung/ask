package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickyoung/ask/internal/event"
)

// events reads a session file back as the log it is.
func events(t *testing.T, path string) []event.Event {
	t.Helper()
	es, err := event.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return es
}

func notes(t *testing.T, path string) []event.NoteData {
	t.Helper()
	var out []event.NoteData
	for _, e := range events(t, path) {
		if e.Type != event.Note {
			continue
		}
		n, err := event.As[event.NoteData](e)
		if err != nil {
			t.Fatalf("decoding note at seq %d: %v", e.Seq, err)
		}
		out = append(out, n)
	}
	return out
}

func TestNoteAppendsStampedRecord(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	if code, _, _ := exec(t, "", "hello"); code != 0 {
		t.Fatalf("seeding the session: exit %d", code)
	}
	cur, err := event.Latest(dir)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := exec(t, "", "note", "-s", "ply", "check passed: go test ./...")
	if code != 0 {
		t.Fatalf("note: exit %d, stderr %q", code, stderr)
	}
	if stdout != "" {
		t.Errorf("note wrote %q to stdout; a note is a record, not an answer", stdout)
	}
	if !strings.Contains(stderr, "ply") {
		t.Errorf("stderr does not name the source: %q", stderr)
	}

	got := notes(t, cur)
	if len(got) != 1 {
		t.Fatalf("got %d notes, want 1", len(got))
	}
	if got[0].Source != "ply" {
		t.Errorf("source = %q, want %q", got[0].Source, "ply")
	}
	if got[0].Text != "check passed: go test ./..." {
		t.Errorf("text = %q", got[0].Text)
	}
}

// The invariant. A note is a record, not a message: it must not change the
// conversation a provider sees, and it must not disturb a digest already
// written. Both halves are asserted, because either one alone would pass
// while the other broke.
func TestNoteIsNotFolded(t *testing.T) {
	dir, _, bodies := fake(t, 200, answerWire)
	if code, _, _ := exec(t, "", "first"); code != 0 {
		t.Fatal("seeding")
	}
	cur, err := event.Latest(dir)
	if err != nil {
		t.Fatal(err)
	}

	before, err := event.Fold(events(t, cur))
	if err != nil {
		t.Fatal(err)
	}
	if code, _, se := exec(t, "", "note", "-s", "ply", "check passed"); code != 0 {
		t.Fatalf("note: exit %d, %s", code, se)
	}
	after, err := event.Fold(events(t, cur))
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(before)
	gotJSON, _ := json.Marshal(after)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("a note changed the fold:\nbefore %s\nafter  %s", wantJSON, gotJSON)
	}

	// And the log still proves itself, before and after another turn is
	// taken on top of the note.
	if err := event.Check(events(t, cur)); err != nil {
		t.Fatalf("replay check after note: %v", err)
	}
	if code, _, se := exec(t, "", "second"); code != 0 {
		t.Fatalf("continuing: exit %d, %s", code, se)
	}
	if err := event.Check(events(t, cur)); err != nil {
		t.Fatalf("replay check after continuing past a note: %v", err)
	}
	// The note's text never reached the provider on that turn either.
	for i, b := range *bodies {
		if strings.Contains(b, "check passed") {
			t.Fatalf("request %d carried the note to the provider: %s", i, b)
		}
	}
}

func TestNoteRefusesUnattributed(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	if code, _, _ := exec(t, "", "hello"); code != 0 {
		t.Fatal("seeding")
	}
	cur, _ := event.Latest(dir)

	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"no source", []string{"note", "the check passed"}, "-s"},
		{"empty source", []string{"note", "-s", "  ", "x"}, "-s"},
		{"source is a sentence", []string{"note", "-s", "the check", "x"}, "one word"},
		{"nothing to note", []string{"note", "-s", "ply"}, "nothing to note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := exec(t, "", tc.argv...)
			if code == 0 {
				t.Fatalf("exit 0; a note that cannot be attributed must be refused")
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not mention %q", stderr, tc.want)
			}
			if n := notes(t, cur); len(n) != 0 {
				t.Fatalf("a refused note was written anyway: %+v", n)
			}
		})
	}
}

func TestNoteFromStdinAndFile(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	if code, _, _ := exec(t, "", "hello"); code != 0 {
		t.Fatal("seeding")
	}
	cur, _ := event.Latest(dir)

	typescript := "$ go test ./...\nok  \tx\t0.25s\n"
	if code, _, se := exec(t, typescript, "note", "-s", "ply", "-f", cur); code != 0 {
		t.Fatalf("note from stdin: exit %d, %s", code, se)
	}
	got := notes(t, cur)
	if len(got) != 1 || got[0].Text != typescript {
		t.Fatalf("stdin was not recorded verbatim: %+v", got)
	}
	_ = dir
}

// A note carries into a handoff. It was never folded, so a model continuing
// the conversation never saw it — but somebody picking the work up needs to
// know the check passed, and compaction is written for exactly that reader.
func TestNoteReachesTheTranscript(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	if code, _, _ := exec(t, "", "hello"); code != 0 {
		t.Fatal("seeding")
	}
	cur, _ := event.Latest(dir)
	if code, _, se := exec(t, "", "note", "-s", "ply", "check passed: go test ./..."); code != 0 {
		t.Fatalf("note: exit %d, %s", code, se)
	}
	tr := transcript(events(t, cur))
	if !strings.Contains(tr, "[ply]") || !strings.Contains(tr, "check passed") {
		t.Errorf("transcript drops the note:\n%s", tr)
	}
}

func TestNoteRendersInReplay(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	if code, _, _ := exec(t, "", "hello"); code != 0 {
		t.Fatal("seeding")
	}
	if code, _, se := exec(t, "", "note", "-s", "ply", "check passed"); code != 0 {
		t.Fatalf("note: exit %d, %s", code, se)
	}
	code, stdout, _ := exec(t, "", "replay")
	if code != 0 {
		t.Fatalf("replay: exit %d", code)
	}
	if !strings.Contains(stdout, "[ply]") || !strings.Contains(stdout, "check passed") {
		t.Errorf("replay does not show the note:\n%s", stdout)
	}
	cur, _ := event.Latest(dir)
	if code, _, se := exec(t, "", "replay", "-check", cur); code != 0 {
		t.Fatalf("replay -check on a session with a note: exit %d, %s", code, se)
	}
}
