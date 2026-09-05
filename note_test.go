package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestMutatingCommandsRequireCurrent(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "dangling"}[dangling], func(t *testing.T) {
			dir, calls, _ := fake(t, 200, answerWire)
			path := filepath.Join(dir, "named.jsonl")
			if code, _, stderr := exec(t, "", "-q", "-f", path, "hello"); code != 0 {
				t.Fatal(stderr)
			}
			if dangling {
				if err := os.Symlink("missing.jsonl", filepath.Join(dir, "current")); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			for _, args := range [][]string{
				{"note", "-s", "deploy", "released"},
				{"note", "-s", "verify", "-k", "result/v1", "-json", `{}`, "-seal"},
				{"compact"},
			} {
				code, out, stderr := exec(t, "", args...)
				if code != 1 || out != "" || !strings.Contains(stderr, "no current conversation") {
					t.Fatalf("%v: exit=%d stdout=%q stderr=%q", args, code, out, stderr)
				}
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) || calls.Load() != 1 || len(sessions(t, dir)) != 1 {
				t.Fatal("default mutation guessed a session")
			}
			// Explicit selection remains available even without current.
			if code, _, stderr := exec(t, "", "note", "-q", "-f", path, "-s", "deploy", "released"); code != 0 {
				t.Fatal(stderr)
			}
			if code, _, stderr := exec(t, "", "compact", "-q", path); code != 0 {
				t.Fatal(stderr)
			}
			if _, err := event.Current(dir); err == nil {
				t.Fatal("explicit mutation invented current")
			}
		})
	}
}

func TestStructuredNoteIsSealedAndReplayVerified(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	if code, _, _ := exec(t, "", "hello"); code != 0 {
		t.Fatal("seeding")
	}
	cur, _ := event.Latest(dir)

	body := `{"status":"accepted","exit_code":0}`
	code, stdout, stderr := exec(t, body, "note", "-s", "ply", "-k", "ply.verifier/v1", "-json", "-", "-seal")
	if code != 0 {
		t.Fatalf("structured note: exit %d, stderr %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	es := events(t, cur)
	if len(es) < 2 || es[len(es)-2].Type != event.Note || es[len(es)-1].Type != event.Seal {
		t.Fatalf("tail = %+v, want note then seal", es)
	}
	n, err := event.As[event.NoteData](es[len(es)-2])
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "ply.verifier/v1" || string(n.Body) != body || n.Text != "" {
		t.Fatalf("structured note = %+v", n)
	}
	if err := event.Check(es); err != nil {
		t.Fatalf("replay check: %v", err)
	}
}

func TestStructuredNoteFlagsAreAtomic(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	if code, _, _ := exec(t, "", "hello"); code != 0 {
		t.Fatal("seeding")
	}
	cur, _ := event.Latest(dir)
	before := len(events(t, cur))

	for _, argv := range [][]string{
		{"note", "-s", "ply", "-k", "ply.verifier/v1", "-json", `{}`},
		{"note", "-s", "ply", "-k", "ply.verifier/v1", "-seal"},
		{"note", "-s", "ply", "-json", `{}`, "-seal"},
		{"note", "-s", "ply", "-k", "ply.verifier/v1", "-json", `{bad`, "-seal"},
	} {
		if code, _, _ := exec(t, "", argv...); code == 0 {
			t.Fatalf("%v unexpectedly succeeded", argv)
		}
	}
	if after := len(events(t, cur)); after != before {
		t.Fatalf("invalid structured notes changed log: before %d, after %d", before, after)
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
	if code, _, se := exec(t, "", "-c", "second"); code != 0 {
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
