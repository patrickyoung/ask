package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/patrickyoung/ask/internal/auth"
	"github.com/patrickyoung/ask/internal/event"
	"github.com/patrickyoung/ask/internal/provider"
)

// The tests below drive run() exactly as a shell does — argv in, streams
// and an exit code out — against a fake Anthropic endpoint. Nothing is
// stubbed between the flag parsing and the wire, so what they pin is the
// program, not a rehearsal of it.

// answerWire is a complete Anthropic turn: reasoning "thinking", answer
// "the answer", 10 in / 5 out.
const answerWire = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thinking"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the answer"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

const structuredWire = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"n\":7}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

// plainJSON re-encodes a request body with HTML escaping off. Go escapes
// <, > and & inside JSON strings by default, so a body read straight off
// the wire spells "<stdin>" as an escape sequence; round-tripping it keeps
// the assertions below about what was sent rather than how it was spelled.
func plainJSON(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	var out strings.Builder
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return string(b)
	}
	return out.String()
}

// fake stands the program up against a canned provider: it points ask at a
// temp conversation directory and an httptest endpoint, and returns the
// directory plus a count of calls actually made.
func fake(t *testing.T, status int, body string) (dir string, calls *atomic.Int32, bodies *[]string) {
	t.Helper()
	calls = &atomic.Int32{}
	sent := []string{}
	bodies = &sent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		sent = append(sent, plainJSON(b))
		if status == 200 {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	dir = t.TempDir()
	t.Setenv("ASK_DIR", dir)
	t.Setenv("ASK_MODEL", "anthropic/test-model")
	t.Setenv("ASK_SYSTEM", "be terse")
	t.Setenv("ANTHROPIC_API_KEY", "k")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("ASK_AUTH_URL", "")
	t.Setenv("ASK_AUTH_FILE", filepath.Join(t.TempDir(), "auth.json"))
	t.Setenv("NO_COLOR", "1")
	return dir, calls, bodies
}

// exec runs argv with stdin bound to text ("" means a closed terminal, the
// way an interactive shell invokes ask) and captures both streams
// separately, so which stream carried what is itself under test.
func exec(t *testing.T, stdin string, argv ...string) (code int, stdout, stderr string) {
	t.Helper()
	return execStreams(t, stdin, false, argv...)
}

func execStreams(t *testing.T, stdin string, merge bool, argv ...string) (code int, stdout, stderr string) {
	t.Helper()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	defer func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr }()

	if stdin == "" {
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer devnull.Close()
		os.Stdin = devnull
	} else {
		in := tmpFile(t, "stdin")
		if _, err := in.WriteString(stdin); err != nil {
			t.Fatal(err)
		}
		if _, err := in.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		os.Stdin = in
	}
	out := tmpFile(t, "stdout")
	os.Stdout = out
	errf := out
	if !merge {
		errf = tmpFile(t, "stderr")
	}
	os.Stderr = errf

	code = run(argv)

	stdout = readAll(t, out)
	if merge {
		return code, stdout, stdout
	}
	return code, stdout, readAll(t, errf)
}

func tmpFile(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func readAll(t *testing.T, f *os.File) string {
	t.Helper()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sessions(t *testing.T, dir string) []string {
	t.Helper()
	names, err := event.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// TestFilterContract is the whole promise in one test: the answer is
// stdout, progress is stderr, and the exit code is zero.
func TestFilterContract(t *testing.T) {
	dir, calls, _ := fake(t, 200, answerWire)
	code, stdout, stderr := exec(t, "", "what is the answer")

	if code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr)
	}
	if stdout != "the answer\n" {
		t.Errorf("stdout = %q, want exactly the answer", stdout)
	}
	if strings.Contains(stdout, "thinking") {
		t.Error("reasoning leaked onto stdout; stdout is the product alone")
	}
	if !strings.Contains(stderr, "thinking") {
		t.Errorf("reasoning did not stream to stderr:\n%s", stderr)
	}
	if calls.Load() != 1 {
		t.Errorf("made %d provider calls, want 1", calls.Load())
	}

	// One session, and it replays.
	names := sessions(t, dir)
	if len(names) != 1 {
		t.Fatalf("wrote %d sessions, want 1", len(names))
	}
	events, err := event.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if err := event.Check(events); err != nil {
		t.Errorf("session does not replay: %v", err)
	}
	if events[0].Type != event.Session {
		t.Errorf("first event is %s, want the header", events[0].Type)
	}
	if last := events[len(events)-1]; last.Type != event.Done {
		t.Errorf("last event is %s, want done", last.Type)
	}
}

func TestStructuredOutputIsNativeValidatedAndReplayable(t *testing.T) {
	dir, calls, bodies := fake(t, 200, structuredWire)
	path := writeSchema(t, numberSchema)
	code, stdout, stderr := exec(t, "", "-q", "-schema", path, "extract the number")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if stdout != `{"n":7}`+"\n" {
		t.Errorf("stdout = %q, want the JSON document alone", stdout)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	body := (*bodies)[0]
	for _, want := range []string{`"output_config"`, `"format"`, `"type":"json_schema"`, `"additionalProperties":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("native structured-output request missing %s:\n%s", want, body)
		}
	}
	events, err := event.ReadFile(filepath.Join(dir, sessions(t, dir)[0]))
	if err != nil {
		t.Fatal(err)
	}
	if err := event.Check(events); err != nil {
		t.Errorf("structured session does not replay: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Type != event.Request {
			continue
		}
		req, err := event.As[provider.Request](e)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(req.Schema, &schema); err != nil {
			t.Fatal(err)
		}
		found = schema["type"] == "object"
	}
	if !found {
		t.Error("request event did not record the output schema")
	}
}

func TestStructuredSchemaNumberSurvivesWireAndReplay(t *testing.T) {
	dir, _, bodies := fake(t, 200, structuredWire)
	const exact = "9007199254740993"
	schema := `{
  "type": "object",
  "properties": {
    "n": {"type": "integer"},
    "unused": {"const": ` + exact + `}
  },
  "required": ["n"]
}`
	code, _, stderr := exec(t, "", "-q", "-schema", writeSchema(t, schema), "extract the number")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains((*bodies)[0], exact) {
		t.Errorf("provider request changed exact schema number:\n%s", (*bodies)[0])
	}

	name := sessions(t, dir)[0]
	events, err := event.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	step := 0
	for _, e := range events {
		if e.Type == event.Request {
			step = e.Seq
			break
		}
	}
	code, stdout, stderr := exec(t, "", "replay", "-d", dir, "-step", fmt.Sprint(step), name)
	if code != 0 {
		t.Fatalf("replay exit = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, exact) {
		t.Errorf("replayed request changed exact schema number:\n%s", stdout)
	}
}

func TestStructuredOutputFailureLeavesStdoutEmpty(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	code, stdout, stderr := exec(t, "", "-schema", writeSchema(t, numberSchema), "extract the number")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("invalid structured output leaked to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "structured output is not valid JSON") {
		t.Errorf("stderr does not name the failed contract:\n%s", stderr)
	}
	if len(sessions(t, dir)) != 1 {
		t.Error("the failed provider turn should remain recorded")
	}
}

func TestStructuredSchemaMismatchLeavesStdoutEmpty(t *testing.T) {
	dir, _, _ := fake(t, 200, structuredWire)
	schema := strings.Replace(numberSchema, `"integer"`, `"string"`, 1)
	code, stdout, stderr := exec(t, "", "-schema", writeSchema(t, schema), "extract the number")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("schema mismatch leaked to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "structured output does not match schema") {
		t.Errorf("stderr does not name the failed contract:\n%s", stderr)
	}
	if len(sessions(t, dir)) != 1 {
		t.Error("the rejected provider turn should remain recorded")
	}
}

func TestStructuredFailureStillEmitsRawEvents(t *testing.T) {
	fake(t, 200, answerWire)
	code, stdout, stderr := exec(t, "", "-q", "-json", "-schema", writeSchema(t, numberSchema), "extract the number")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, `"type":"assistant"`) || !strings.Contains(stdout, `"type":"done"`) {
		t.Errorf("raw event stream did not record the rejected turn:\n%s", stdout)
	}
	if !strings.Contains(stderr, "structured output is not valid JSON") {
		t.Errorf("stderr does not name the failed contract:\n%s", stderr)
	}
}

func TestBadSchemaFailsBeforeCreatingASession(t *testing.T) {
	dir, calls, _ := fake(t, 200, structuredWire)
	code, stdout, stderr := exec(t, "", "-schema", writeSchema(t, `{`), "hi")
	if code != 1 || stdout != "" {
		t.Fatalf("exit/stdout = %d/%q, want 1/empty", code, stdout)
	}
	if !strings.Contains(stderr, "schema is not valid JSON") {
		t.Errorf("stderr = %q", stderr)
	}
	if calls.Load() != 0 || len(sessions(t, dir)) != 0 {
		t.Errorf("bad schema made %d calls and %d sessions", calls.Load(), len(sessions(t, dir)))
	}
}

func TestSchemaCanComeFromStdin(t *testing.T) {
	_, _, bodies := fake(t, 200, structuredWire)
	code, stdout, stderr := exec(t, numberSchema, "-q", "-schema", "-", "extract the number")
	if code != 0 || stdout != `{"n":7}`+"\n" {
		t.Fatalf("exit/stdout = %d/%q: %s", code, stdout, stderr)
	}
	if !strings.Contains((*bodies)[0], `"output_config"`) {
		t.Error("schema read from stdin did not reach the provider")
	}
}

func TestEmptySchemaStillMeansStructuredJSON(t *testing.T) {
	dir, _, bodies := fake(t, 200, structuredWire)
	code, stdout, stderr := exec(t, "", "-q", "-schema", writeSchema(t, `{}`), "return some JSON")
	if code != 0 || stdout != `{"n":7}`+"\n" {
		t.Fatalf("exit/stdout = %d/%q: %s", code, stdout, stderr)
	}
	if !strings.Contains((*bodies)[0], `"output_config"`) {
		t.Error("empty schema was mistaken for no structured-output request")
	}
	events, err := event.ReadFile(filepath.Join(dir, sessions(t, dir)[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == event.Request {
			req, _ := event.As[provider.Request](e)
			if string(req.Schema) != `{}` {
				t.Errorf("logged schema = %#v, want present empty object", req.Schema)
			}
			return
		}
	}
	t.Error("no request event")
}

// TestSameOutSuppressesTheEcho: when stdout and stderr are the same place,
// the answer already streamed — printing it again would double it.
func TestSameOutSuppressesTheEcho(t *testing.T) {
	fake(t, 200, answerWire)
	code, merged, _ := execStreams(t, "", true, "hi")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if n := strings.Count(merged, "the answer"); n != 1 {
		t.Errorf("answer appears %d times when both streams are one file, want 1:\n%s", n, merged)
	}
}

// TestContinuesByDefault: the second ask carries the first exchange, and
// says so on stderr — continuing must never be a silent surprise.
func TestContinuesByDefault(t *testing.T) {
	dir, _, bodies := fake(t, 200, answerWire)
	if code, _, e := exec(t, "", "first question"); code != 0 {
		t.Fatalf("exit = %d: %s", code, e)
	}
	code, _, stderr := exec(t, "", "second question")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if len(sessions(t, dir)) != 1 {
		t.Errorf("continuing created a second session: %v", sessions(t, dir))
	}
	if !strings.Contains(stderr, "1 turns so far") {
		t.Errorf("stderr does not announce the conversation being joined:\n%s", stderr)
	}
	if !strings.Contains(stderr, "-n starts fresh") {
		t.Errorf("stderr does not say how to start fresh:\n%s", stderr)
	}
	second := (*bodies)[1]
	for _, want := range []string{"first question", "the answer", "second question"} {
		if !strings.Contains(second, want) {
			t.Errorf("second request is missing %q:\n%s", want, second)
		}
	}
}

// TestNewStartsFresh: -n is the escape hatch, and the old conversation is
// left intact beside the new one.
func TestNewStartsFresh(t *testing.T) {
	dir, _, bodies := fake(t, 200, answerWire)
	exec(t, "", "first question")
	code, _, stderr := exec(t, "", "-n", "unrelated question")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if n := len(sessions(t, dir)); n != 2 {
		t.Fatalf("have %d sessions, want 2", n)
	}
	if strings.Contains((*bodies)[1], "first question") {
		t.Error("-n carried the previous conversation into the new one")
	}
}

// TestSessionFileKeepsItsOwnThread: -f is a conversation of the caller's
// own, appended to across runs.
func TestSessionFileKeepsItsOwnThread(t *testing.T) {
	dir, _, bodies := fake(t, 200, answerWire)
	thread := filepath.Join(t.TempDir(), "thread.jsonl")
	if code, _, e := exec(t, "", "-f", thread, "first question"); code != 0 {
		t.Fatalf("exit = %d: %s", code, e)
	}
	if code, _, e := exec(t, "", "-f", thread, "second question"); code != 0 {
		t.Fatalf("exit = %d: %s", code, e)
	}
	if !strings.Contains((*bodies)[1], "first question") {
		t.Error("-f did not continue its own thread")
	}
	if n := len(sessions(t, dir)); n != 0 {
		t.Errorf("-f wrote %d sessions into the default directory, want 0", n)
	}
	events, err := event.ReadFile(thread)
	if err != nil {
		t.Fatal(err)
	}
	if err := event.Check(events); err != nil {
		t.Errorf("thread does not replay: %v", err)
	}
}

// TestStdinComposes: piped input rides with the message, delimited, so the
// model can tell instruction from data.
func TestStdinComposes(t *testing.T) {
	_, _, bodies := fake(t, 200, answerWire)
	code, _, stderr := exec(t, "diff --git a/x b/x\n", "write a commit message")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	body := (*bodies)[0]
	for _, want := range []string{"write a commit message", "<stdin>", "diff --git"} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %q:\n%s", want, body)
		}
	}
}

// TestStdinAloneIsTheMessage: with nothing in argv, the pipe is the question.
func TestStdinAloneIsTheMessage(t *testing.T) {
	_, _, bodies := fake(t, 200, answerWire)
	if code, _, e := exec(t, "summarize this please"); code != 0 {
		t.Fatalf("exit = %d: %s", code, e)
	}
	body := (*bodies)[0]
	if !strings.Contains(body, "summarize this please") {
		t.Errorf("piped message did not reach the provider:\n%s", body)
	}
	if strings.Contains(body, "<stdin>") {
		t.Error("stdin alone should be the message, not wrapped as attached data")
	}
}

// TestNoMessageIsUsage: nothing to ask is misuse, not an empty model call.
func TestNoMessageIsUsage(t *testing.T) {
	dir, calls, _ := fake(t, 200, answerWire)
	code, stdout, stderr := exec(t, "")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if calls.Load() != 0 {
		t.Error("called the provider with no message")
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on misuse", stdout)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("stderr does not show usage:\n%s", stderr)
	}
	if n := len(sessions(t, dir)); n != 0 {
		t.Errorf("misuse left %d session files behind", n)
	}
}

// TestOverflowExitsTwo: a full context window is permanent, so it gets its
// own exit code — a retry loop that treated it as an error would grind
// against it forever.
func TestOverflowExitsTwo(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens > 200000 maximum"}}`
	_, calls, _ := fake(t, 400, body)
	code, stdout, stderr := exec(t, "", "hi")
	if code != 2 {
		t.Fatalf("exit = %d, want 2. stderr:\n%s", code, stderr)
	}
	if calls.Load() != 1 {
		t.Errorf("made %d calls for a permanent failure, want 1", calls.Load())
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "-n") {
		t.Errorf("stderr does not say how to recover:\n%s", stderr)
	}
}

// TestProviderErrorExitsOne, and the session records why.
func TestProviderErrorExitsOne(t *testing.T) {
	dir, _, _ := fake(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"no such model"}}`)
	code, _, stderr := exec(t, "", "hi")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no such model") {
		t.Errorf("stderr does not carry the provider's words:\n%s", stderr)
	}
	names := sessions(t, dir)
	events, err := event.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	last, err := event.As[event.DoneData](events[len(events)-1])
	if err != nil {
		t.Fatal(err)
	}
	if last.Reason != "error" {
		t.Errorf("done reason = %q, want error", last.Reason)
	}
}

// TestNoModelLeavesNoLitter: configuration errors must fail before a
// session file exists.
func TestNoModelLeavesNoLitter(t *testing.T) {
	dir, calls, _ := fake(t, 200, answerWire)
	t.Setenv("ASK_MODEL", "")
	code, stdout, stderr := exec(t, "", "hi")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no model") {
		t.Errorf("stderr = %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if calls.Load() != 0 {
		t.Error("called a provider without a model")
	}
	if n := len(sessions(t, dir)); n != 0 {
		t.Errorf("a bad invocation left %d session files behind", n)
	}
}

// TestReplayCheckProvesTheSession: the headline claim, exercised the way a
// user would.
func TestReplayCheckProvesTheSession(t *testing.T) {
	fake(t, 200, answerWire)
	exec(t, "", "first")
	exec(t, "", "second")
	exec(t, "", "third")

	code, stdout, stderr := exec(t, "", "replay", "-check")
	if code != 0 {
		t.Fatalf("replay -check exit = %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "replays exactly") {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestReplayCheckCatchesAnEditedLog: a log that was tampered with must
// fail, or the check would be decoration.
func TestReplayCheckCatchesAnEditedLog(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	exec(t, "", "the original question")

	path := filepath.Join(dir, sessions(t, dir)[0])
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(body), "the original question", "a different question", 1)
	if edited == string(body) {
		t.Fatal("test did not edit anything")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := exec(t, "", "replay", "-check")
	if code != 1 {
		t.Fatalf("exit = %d on a rewritten log, want 1", code)
	}
	if !strings.Contains(stderr, "replay divergence") {
		t.Errorf("stderr = %q, want a divergence report", stderr)
	}
}

// TestReplayStepPrintsTheExactRequest: the log stores a digest, so -step
// has to reconstitute the conversation from the fold and show what really
// went out.
func TestReplayStepPrintsTheExactRequest(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	exec(t, "", "the question")

	events, err := event.ReadFile(filepath.Join(dir, sessions(t, dir)[0]))
	if err != nil {
		t.Fatal(err)
	}
	seq := 0
	for _, e := range events {
		if e.Type == event.Request {
			seq = e.Seq
		}
	}
	code, stdout, stderr := exec(t, "", "replay", "-step", itoa(seq))
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	var req provider.Request
	if err := json.Unmarshal([]byte(stdout), &req); err != nil {
		t.Fatalf("step output is not a request: %v\n%s", err, stdout)
	}
	if req.Digest != "" {
		t.Error("step printed a digest instead of the messages it stands for")
	}
	if len(req.Messages) != 1 || req.Messages[0].Blocks[0].Text != "the question" {
		t.Errorf("reconstituted messages = %+v", req.Messages)
	}
	if req.System != "be terse" {
		t.Errorf("system = %q, want the one actually sent", req.System)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestSystemPromptIsAValue: `ask system` prints the default, which is what
// makes extending it ordinary shell rather than a config format.
func TestSystemPromptIsAValue(t *testing.T) {
	fake(t, 200, answerWire)
	code, stdout, _ := exec(t, "", "system")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.TrimSpace(stdout) != strings.TrimSpace(defaultSystem) {
		t.Error("ask system did not print the default system prompt verbatim")
	}
	if !strings.Contains(stdout, "cannot ask a question") {
		t.Error("the default prompt lost the rule that makes it a filter's prompt")
	}
}

// TestSystemPromptPrecedence: -S beats $ASK_SYSTEM beats the default, and
// -S "" means none at all. Whatever wins is recorded in the header, so a
// session says what shaped it.
func TestSystemPromptPrecedence(t *testing.T) {
	for _, c := range []struct {
		name string
		env  string
		argv []string
		want string
	}{
		{"default", "", []string{"hi"}, defaultSystem},
		{"env", "from env", []string{"hi"}, "from env"},
		{"flag beats env", "from env", []string{"-S", "from flag", "hi"}, "from flag"},
		{"explicitly none", "from env", []string{"-S", "", "hi"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, _, bodies := fake(t, 200, answerWire)
			t.Setenv("ASK_SYSTEM", c.env)
			if c.env == "" {
				os.Unsetenv("ASK_SYSTEM")
			}
			if code, _, e := exec(t, "", c.argv...); code != 0 {
				t.Fatalf("exit = %d: %s", code, e)
			}

			events, err := event.ReadFile(filepath.Join(dir, sessions(t, dir)[0]))
			if err != nil {
				t.Fatal(err)
			}
			h, err := event.As[event.Header](events[0])
			if err != nil {
				t.Fatal(err)
			}
			if h.System != c.want {
				t.Errorf("header system = %q, want %q", h.System, c.want)
			}
			body := (*bodies)[0]
			if c.want == "" {
				if strings.Contains(body, `"system"`) {
					t.Errorf("sent a system prompt when none was wanted:\n%s", body)
				}
			} else if !strings.Contains(body, c.want[:min(len(c.want), 20)]) {
				t.Errorf("system prompt did not reach the provider:\n%s", body)
			}
		})
	}
}

// TestJSONEmitsEventsNotProse: -json puts the machine view on stdout, and
// the answer does not double up there.
func TestJSONEmitsEventsNotProse(t *testing.T) {
	fake(t, 200, answerWire)
	code, stdout, _ := exec(t, "", "-json", "hi")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var types []string
	for line := range strings.Lines(stdout) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e event.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("stdout line is not an event: %v\n%s", err, line)
		}
		types = append(types, string(e.Type))
	}
	want := []string{"session", "user", "request", "assistant", "done"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("event types = %v, want %v", types, want)
	}
}

// TestQuietSilencesStderr: -q leaves only the answer.
func TestQuietSilencesStderr(t *testing.T) {
	fake(t, 200, answerWire)
	code, stdout, stderr := exec(t, "", "-q", "hi")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if stdout != "the answer\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want silence under -q", stderr)
	}
}

// TestTypoGuard: a word one edit from a command is a typo, not a paid
// model call. Anything further away is a message, because asking is the
// default action and has to stay that way.
func TestTypoGuard(t *testing.T) {
	for _, c := range []struct {
		word    string
		refused bool
	}{
		{"replya", true},  // transposition
		{"replay", false}, // the command itself
		{"sytsem", true},  // transposition
		{"helpp", true},   // insertion
		{"summarize", false},
		{"why", false},
		{"versions", true}, // one character too many
	} {
		t.Run(c.word, func(t *testing.T) {
			_, calls, _ := fake(t, 200, answerWire)
			code, _, stderr := exec(t, "", c.word)
			switch {
			case c.refused:
				if code != 1 || !strings.Contains(stderr, "did you mean") {
					t.Errorf("%q: exit %d, stderr %q — want a refusal", c.word, code, stderr)
				}
				if calls.Load() != 0 {
					t.Errorf("%q reached the provider anyway", c.word)
				}
			case c.word == "replay":
				// A real command, handled elsewhere; just not a message.
				if calls.Load() != 0 {
					t.Errorf("%q was sent as a message", c.word)
				}
			default:
				if code != 0 {
					t.Errorf("%q: exit %d, want it treated as a message: %s", c.word, code, stderr)
				}
				if calls.Load() != 1 {
					t.Errorf("%q was not sent as a message", c.word)
				}
			}
		})
	}
}

// TestDashDashSendsAVerbAsAMessage: the escape hatch has to work, or a
// question about a command becomes unaskable.
func TestDashDashSendsAVerbAsAMessage(t *testing.T) {
	_, calls, bodies := fake(t, 200, answerWire)
	code, _, stderr := exec(t, "", "--", "replay")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if calls.Load() != 1 {
		t.Fatal("-- did not send the word as a message")
	}
	if !strings.Contains((*bodies)[0], "replay") {
		t.Error("the message did not reach the provider")
	}
}

// TestHelpStreams: help that was asked for is the output of a successful
// command and belongs on stdout, so `ask -h | less` works. A misuse is a
// diagnostic and belongs on stderr, leaving stdout clean for whoever is
// parsing it.
func TestHelpStreams(t *testing.T) {
	fake(t, 200, answerWire)

	code, stdout, stderr := exec(t, "", "help")
	if code != 0 || !strings.Contains(stdout, "ask —") || stderr != "" {
		t.Errorf("ask help: exit %d, stdout %d bytes, stderr %q", code, len(stdout), stderr)
	}

	code, stdout, stderr = exec(t, "", "replay", "-h")
	if code != 0 || !strings.Contains(stdout, "usage: ask replay") || stderr != "" {
		t.Errorf("ask replay -h: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	code, stdout, stderr = exec(t, "", "-nonesuch", "hi")
	if code != 1 {
		t.Errorf("bad flag: exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("bad flag wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "nonesuch") || !strings.Contains(stderr, "usage:") {
		t.Errorf("bad flag stderr = %q", stderr)
	}
}

// TestVersionAndEffortValidation covers the two remaining front-door checks.
func TestVersionAndEffortValidation(t *testing.T) {
	fake(t, 200, answerWire)

	if code, stdout, _ := exec(t, "", "version"); code != 0 || !strings.HasPrefix(stdout, "ask ") {
		t.Errorf("version: exit %d, stdout %q", code, stdout)
	}
	code, _, stderr := exec(t, "", "-effort", "sideways", "hi")
	if code != 1 || !strings.Contains(stderr, "-effort must be") {
		t.Errorf("bad effort: exit %d, stderr %q", code, stderr)
	}
}

// TestHeaderRecordsProvenance: a session says which binary and which model
// produced it, so a log read later can be trusted to its own account.
func TestHeaderRecordsProvenance(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	exec(t, "", "hi")
	events, err := event.ReadFile(filepath.Join(dir, sessions(t, dir)[0]))
	if err != nil {
		t.Fatal(err)
	}
	h, err := event.As[event.Header](events[0])
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != version {
		t.Errorf("header version = %q, want %q", h.Version, version)
	}
	if h.Model != "anthropic/test-model" {
		t.Errorf("header model = %q, want the full spec", h.Model)
	}
	if h.ID == "" || h.Go == "" {
		t.Errorf("header is missing provenance: %+v", h)
	}
}

// TestContinuingInheritsTheModel: `ask` twice in a row should not need -m
// the second time.
func TestContinuingInheritsTheModel(t *testing.T) {
	fake(t, 200, answerWire)
	if code, _, e := exec(t, "", "first"); code != 0 {
		t.Fatalf("exit = %d: %s", code, e)
	}
	t.Setenv("ASK_MODEL", "") // nothing on the command line either
	code, stdout, stderr := exec(t, "", "second")
	if code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr)
	}
	if stdout != "the answer\n" {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestUsageTextFitsEightyColumns: help is read in a terminal.
func TestUsageTextFitsEightyColumns(t *testing.T) {
	for i, line := range strings.Split(usageText, "\n") {
		if n := len([]rune(line)); n > 80 {
			t.Errorf("usage line %d is %d columns:\n%s", i+1, n, line)
		}
	}
}

// TestDocsCoverEveryCommand guards the whole, not just the parts: a verb
// that exists in the binary but in no documentation is exactly the drift
// that hides for months.
func TestDocsCoverEveryCommand(t *testing.T) {
	man, err := os.ReadFile("ask.1")
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range verbs {
		if !strings.Contains(usageText, "ask "+v) {
			t.Errorf("verb %q is missing from ask help", v)
		}
		if !strings.Contains(string(man), ".B ask "+v) {
			t.Errorf("verb %q is missing from the ask.1 SYNOPSIS", v)
		}
		if !strings.Contains(string(readme), "ask "+v) {
			t.Errorf("verb %q is missing from README.md", v)
		}
	}
}

// TestDocsCoverEveryFlag: same guard one level down. The man page escapes
// hyphens as \- so they render as minus signs, so the comparison is made
// against the text with roff's backslashes removed rather than against one
// particular spelling of it.
func TestDocsCoverEveryFlag(t *testing.T) {
	raw, err := os.ReadFile("ask.1")
	if err != nil {
		t.Fatal(err)
	}
	man := strings.ReplaceAll(string(raw), `\`, "")
	for _, f := range []string{"-m", "-S", "-n", "-a", "-f", "-d", "-effort", "-max-tokens", "-schema", "-json", "-q", "-check", "-step"} {
		if !strings.Contains(usageText, f+" ") && !strings.Contains(usageText, f+"\n") {
			t.Errorf("flag %s is missing from ask help", f)
		}
		if !strings.Contains(man, f+" ") && !strings.Contains(man, f+"\n") {
			t.Errorf("flag %s is missing from ask.1", f)
		}
	}
}

// TestManPageLints holds the reference to the bar a man page is actually
// held to: mandoc -Tlint -Wall must say nothing at all, and the file must
// be pure ASCII, because literal UTF-8 renders as mojibake in a
// non-UTF-8 locale. An em dash is \(em, an arrow is \(->.
func TestManPageLints(t *testing.T) {
	body, err := os.ReadFile("ask.1")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(body), "\n") {
		for _, r := range line {
			if r > 127 {
				t.Errorf("ask.1:%d is not ASCII (%q):\n%s", i+1, r, line)
				break
			}
		}
	}
	if _, err := osexec.LookPath("mandoc"); err != nil {
		t.Skip("mandoc not installed")
	}
	out, err := osexec.Command("mandoc", "-Tlint", "-Wall", "ask.1").CombinedOutput()
	if len(out) > 0 || err != nil {
		t.Errorf("mandoc -Tlint -Wall ask.1 (err %v):\n%s", err, out)
	}
}

// TestVersionIsOneNumber: the version lives in the code and the man page
// header repeats it. Two spellings of one fact drift, and the one that
// drifts is the one nobody runs — a man page claiming a release the binary
// does not report is the first thing a packager notices and the last thing
// an author does.
func TestVersionIsOneNumber(t *testing.T) {
	man, err := os.ReadFile("ask.1")
	if err != nil {
		t.Fatal(err)
	}
	if want := `"ask ` + version + `"`; !strings.Contains(string(man), want) {
		t.Errorf("the ask.1 .TH line does not carry %s (version = %q)", want, version)
	}
}

// TestProvidersAreDocumented: a provider New accepts that no document names
// is a provider nobody can find, and one the documents name that New
// rejects is worse. provider.Providers is the single list; both files quote
// from it.
func TestProvidersAreDocumented(t *testing.T) {
	for _, doc := range []string{"ask.1", "README.md"} {
		body, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		// roff spells a literal hyphen \-, so compare what renders.
		text := strings.ReplaceAll(string(body), `\-`, "-")
		for _, p := range provider.Providers {
			if !strings.Contains(text, p) {
				t.Errorf("provider %q is not named in %s", p, doc)
			}
		}
	}
}

// TestEnvVarsAreDocumented: an environment variable the code reads and the
// man page does not mention is undiscoverable.
func TestEnvVarsAreDocumented(t *testing.T) {
	man, err := os.ReadFile("ask.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{
		"ASK_MODEL", "ASK_SYSTEM", "ASK_DIR", "ASK_AUTH_FILE", "ASK_AUTH_URL",
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY",
		"ANTHROPIC_VERTEX_PROJECT_ID", "CLOUD_ML_REGION", "NO_COLOR",
	} {
		if !strings.Contains(string(man), v) {
			t.Errorf("%s is not documented in ask.1", v)
		}
		if !strings.Contains(usageText, v) {
			t.Errorf("%s is not documented in ask help", v)
		}
	}
}

// TestQuietStillPrintsTheAnswer: -q installs no renderer, so nothing
// streams to stderr. The stdout echo may only be suppressed when the
// renderer actually showed the answer — otherwise `ask -q "..."` in a
// terminal, where both streams are the tty, prints nothing at all and the
// run is silently lost. Found by pointing ask at its own source.
func TestQuietStillPrintsTheAnswer(t *testing.T) {
	for _, merge := range []bool{true, false} {
		fake(t, 200, answerWire)
		code, stdout, _ := execStreams(t, "", merge, "-q", "hi")
		if code != 0 {
			t.Fatalf("merge=%v: exit = %d", merge, code)
		}
		if stdout != "the answer\n" {
			t.Errorf("merge=%v: ask -q produced %q, want the answer exactly once", merge, stdout)
		}
	}
}

// TestAttachReachesTheProvider is the whole feature end to end: -a puts an
// image on the wire, in the right shape, and the session that records it
// still replays exactly.
func TestAttachReachesTheProvider(t *testing.T) {
	dir, _, bodies := fake(t, 200, answerWire)
	shot := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(shot, png, 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := exec(t, "", "-a", shot, "what is this?")
	if code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr)
	}
	if stdout != "the answer\n" {
		t.Errorf("stdout = %q", stdout)
	}

	body := (*bodies)[0]
	for _, want := range []string{`"type":"image"`, `"media_type":"image/png"`, `"what is this?"`} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %s:\n%s", want, body)
		}
	}

	events, err := event.ReadFile(filepath.Join(dir, sessions(t, dir)[0]))
	if err != nil {
		t.Fatal(err)
	}
	if err := event.Check(events); err != nil {
		t.Errorf("a session carrying an image does not replay: %v", err)
	}
	// The bytes are in the log, so the fold stays a function of the log.
	u, err := event.As[event.UserData](events[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Blocks) != 2 {
		t.Fatalf("user event has %d blocks, want image then text: %+v", len(u.Blocks), u.Blocks)
	}
	if u.Blocks[0].Type != provider.Media || !bytes.Equal(u.Blocks[0].Data, png) {
		t.Errorf("image bytes not recorded verbatim: %+v", u.Blocks[0])
	}
	if u.Blocks[1].Type != provider.Text {
		t.Errorf("text block is not last: %+v", u.Blocks[1])
	}
}

// TestBinaryStdinBecomesAnAttachment: screencapture -x -t png - | ask "..."
// should need no new flag. Text stdin keeps its old meaning.
func TestBinaryStdinBecomesAnAttachment(t *testing.T) {
	_, _, bodies := fake(t, 200, answerWire)
	if code, _, e := exec(t, string(png), "what is this?"); code != 0 {
		t.Fatalf("exit = %d: %s", code, e)
	}
	if body := (*bodies)[0]; !strings.Contains(body, `"media_type":"image/png"`) {
		t.Errorf("piped PNG did not become an attachment:\n%s", body)
	}

	_, _, bodies2 := fake(t, 200, answerWire)
	if code, _, e := exec(t, "some plain text", "summarise"); code != 0 {
		t.Fatalf("exit = %d: %s", code, e)
	}
	body := (*bodies2)[0]
	if !strings.Contains(body, "<stdin>") || strings.Contains(body, "media_type") {
		t.Errorf("text stdin changed meaning:\n%s", body)
	}
}

// TestTextAttachmentStaysReadable: attaching a source file must not turn
// the log into base64. grep and jq over sessions are load-bearing.
func TestTextAttachmentStaysReadable(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	src := filepath.Join(t.TempDir(), "hello.go")
	if err := os.WriteFile(src, []byte("package main // hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, e := exec(t, "", "-a", src, "review this"); code != 0 {
		t.Fatalf("exit = %d: %s", code, e)
	}
	raw, err := os.ReadFile(filepath.Join(dir, sessions(t, dir)[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "package main // hello") {
		t.Error("a text attachment was not readable in the log")
	}
	if !strings.Contains(string(raw), "hello.go") {
		t.Error("the log does not say which file it was")
	}
}

// TestUnsendableAttachmentLeavesNoSession: validation happens before the
// log exists, so a refused attachment cannot poison every later fold.
func TestUnsendableAttachmentLeavesNoSession(t *testing.T) {
	dir, calls, _ := fake(t, 200, answerWire)
	clip := filepath.Join(t.TempDir(), "clip.wav")
	if err := os.WriteFile(clip, wav, 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := exec(t, "", "-a", clip, "transcribe this")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "does not accept audio/wav") {
		t.Errorf("stderr = %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if calls.Load() != 0 {
		t.Error("sent a request the provider cannot answer")
	}
	if n := len(sessions(t, dir)); n != 0 {
		t.Errorf("a refused attachment left %d session files behind", n)
	}
}

// TestOversizedStdinIsRefusedNotTruncated. A filter that quietly drops the
// tail of its input is worse than one that fails: the answer looks like an
// answer to the whole file, and nothing downstream can tell that it is not.
// The bound is the same one attachments get, and crossing it is an error
// with an empty stdout, before any session exists.
func TestOversizedStdinIsRefusedNotTruncated(t *testing.T) {
	dir, calls, _ := fake(t, 200, answerWire)
	code, stdout, stderr := exec(t, strings.Repeat("a", maxAttachment+1), "summarize")
	if code != 1 {
		t.Fatalf("exit = %d, want 1. stderr:\n%s", code, stderr)
	}
	if calls.Load() != 0 {
		t.Error("sent a truncated pipe to the provider")
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "larger than") {
		t.Errorf("stderr does not say the input was too large:\n%s", stderr)
	}
	if n := len(sessions(t, dir)); n != 0 {
		t.Errorf("a refused pipe left %d session files behind", n)
	}
}

// TestLargeStdinStillPasses guards the fix from becoming an off-by-one that
// refuses input that fits.
func TestLargeStdinStillPasses(t *testing.T) {
	_, _, bodies := fake(t, 200, answerWire)
	text := strings.Repeat("z", maxAttachment)
	if code, _, stderr := exec(t, text, "summarize"); code != 0 {
		t.Fatalf("exit = %d for input exactly at the limit: %s", code, stderr)
	}
	if body := (*bodies)[0]; !strings.Contains(body, text) {
		t.Errorf("the pipe reached the provider short: %d bytes of body", len(body))
	}
}

// TestSecondInterruptKills. The first ^C cancels: the stream ends, the
// abort is logged, and the session stays continuable. The second must reach
// the operating system. signal.NotifyContext keeps the signal captured for
// the life of the context and swallows every one after the first, which
// leaves a filter that cannot be stopped from the terminal it is running in
// if anything below it fails to honor cancellation. This runs in a child
// process, because a test that proves a SIGINT is fatal cannot survive it.
func TestSecondInterruptKills(t *testing.T) {
	if os.Getenv("ASK_SIGNAL_CHILD") == "1" {
		ctx, stop := sigCtx()
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("canceled")
		time.Sleep(30 * time.Second) // the stream that will not stop
		return
	}

	cmd := osexec.Command(os.Args[0], "-test.run=TestSecondInterruptKills", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "ASK_SIGNAL_CHILD=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	await := func(want string) {
		t.Helper()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("child exited before %q", want)
				}
				if strings.TrimSpace(line) == want {
					return
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("timed out waiting for %q", want)
			}
		}
	}

	await("ready")
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	await("canceled") // the first one was handled, not fatal

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *osexec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("child exited %v, want death by signal", err)
		}
		st, ok := ee.Sys().(syscall.WaitStatus)
		if !ok || !st.Signaled() || st.Signal() != syscall.SIGINT {
			t.Fatalf("child exit status = %v, want signaled with SIGINT", ee)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the second interrupt was swallowed; the child is still running")
	}
}

// TestLoginRefusesCleartextTokenEndpoint: refusing at login is the whole
// point of checking here as well as at use. The operator finds out while
// they are looking at the command that was wrong, and nothing is written —
// a credential file that exists is a credential file someone will trust.
func TestLoginRefusesCleartextTokenEndpoint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("ASK_AUTH_FILE", p)
	code, stdout, stderr := exec(t, "", "login", "openai-codex",
		"-refresh-token", "r-0", "-token-url", "http://idp.corp/oauth/token")
	if code != 1 {
		t.Fatalf("exit = %d, want 1. stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "in the clear") {
		t.Errorf("stderr does not say why:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on refusal", stdout)
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("a refused login wrote a credential file anyway")
	}
}

// TestLoginStoresAndLogsOut walks the credential commands the way a person
// does — in, listed, out — and checks the file mode on the way past. A
// token given in argv is visible in ps, which is why help offers "-";
// that path is exercised here too.
func TestLoginStoresAndLogsOut(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("ASK_AUTH_FILE", p)

	if code, _, stderr := exec(t, "tok-from-stdin\n", "login", "openai-codex", "-access-token", "-"); code != 0 {
		t.Fatalf("login exit = %d: %s", code, stderr)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("auth file mode is %04o, want 0600", perm)
	}
	cred, ok, err := auth.Get("openai-codex")
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if cred.AccessToken != "tok-from-stdin" {
		t.Errorf("stored access token = %q; the trailing newline should be trimmed", cred.AccessToken)
	}

	code, stdout, _ := exec(t, "", "auth")
	if code != 0 || !strings.Contains(stdout, "openai-codex") {
		t.Errorf("ask auth exit = %d, stdout = %q", code, stdout)
	}
	if strings.Contains(stdout, "tok-from-stdin") {
		t.Error("ask auth printed the token itself; it lists providers, not secrets")
	}

	if code, _, stderr := exec(t, "", "logout", "openai-codex"); code != 0 {
		t.Fatalf("logout exit = %d: %s", code, stderr)
	}
	if _, ok, _ := auth.Get("openai-codex"); ok {
		t.Error("logout left the credential behind")
	}
	if _, stdout, _ := exec(t, "", "auth"); !strings.Contains(stdout, "no stored credentials") {
		t.Errorf("after logout, ask auth says %q", stdout)
	}
}

// TestSessionFilesArePrivate: logs now hold photographs and documents.
func TestSessionFilesArePrivate(t *testing.T) {
	dir, _, _ := fake(t, 200, answerWire)
	exec(t, "", "hi")
	fi, err := os.Stat(filepath.Join(dir, sessions(t, dir)[0]))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("session file mode is %04o, want 0600", perm)
	}
}

// TestSessionDirIsPrivate: the 0600 on the files is worth little if the
// directory holding them is world-readable. Every directory ask makes on
// the way to a session — ~/.ask as much as ~/.ask/sessions — is its own.
func TestSessionDirIsPrivate(t *testing.T) {
	fake(t, 200, answerWire)
	dir := filepath.Join(t.TempDir(), "dot-ask", "sessions")
	t.Setenv("ASK_DIR", dir)
	if code, _, stderr := exec(t, "", "hi"); code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	for _, p := range []string{dir, filepath.Dir(dir)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode is %04o, want 0700", p, perm)
		}
	}
}
