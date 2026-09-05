// Command ask puts a question through a language model and writes the
// answer to stdout.
//
// It is a filter. The answer is stdout, progress is stderr, and the exit
// code says what happened, so ask composes with everything else in the
// shell. Every model turn appends to an append-only JSONL session log that
// replays exactly: `ask replay -check` proves that the conversation sent
// to the model is a function of the log and nothing else.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"

	"github.com/patrickyoung/ask/internal/chat"
	"github.com/patrickyoung/ask/internal/event"
	"github.com/patrickyoung/ask/internal/provider"
)

const version = "0.2.0"

const usageText = `ask — put a question through a language model, get the answer on stdout

  ask [flags] [message ...]       ask; -a attaches files, stdin composes
  ask replay [flags] [session]    re-render a session (-check verifies replay)
  ask compact [flags] [session]   continue a full conversation in a fresh one
  ask note -s src [flags] [text]  record text or sealed structured JSON
  ask system                      print the built-in system prompt
  ask version                     print the version (-V, --version)
  ask help                        print this summary (-h, --help)

Anything that is not a command is a message; -- forces a command-like word.

streams: the answer is stdout. Progress and errors are stderr. Piped stdin
is the message, or data for an instruction:
  git diff | ask "write a commit message"
An unfinished or output-limited answer exits 1 with no normal stdout.
-json still emits its recorded events; replay can display the saved text.

conversation: each ask starts a fresh session. -c continues the current one;
-f keeps a named thread in a file of your own.
A session that fills the window is exit 2 and stays exit 2; ask compact
starts a fresh one from a model-written handoff note, so the work survives
the window. Branching verbatim needs no verb: cp the file.

attachments: -a takes a regular file and repeats. Content decides its type:
text is inlined; images, PDFs, audio and video are attached. Binary stdin is
an attachment too, so
screencapture -x -t png - | ask "what is this?" needs no flag. Providers
differ in what they carry, and ask says so before sending, not after.

flags:
  -m spec       provider/model. Default: $ASK_MODEL, then a continued
                session's model; e.g. anthropic/your-model
  -a file       attach a file; repeat for more (16 max, 16MB each, 32MB
                total). The bytes land in the session log, so it replays
  -S text       system prompt for this call, replacing $ASK_SYSTEM or the
                built-in default. -S "" sends none
  -c            continue the current conversation
  -f file       named session; continue it if it exists, create it otherwise
  -d dir        conversation directory ($ASK_DIR, or ~/.ask/sessions)
  -effort e     reasoning effort: off, low, medium, high, xhigh; provider
                mapping varies (default: the provider's own)
  -max-tokens n max output tokens (default 16384). openai-codex does not
                support this flag and refuses it
  -header-fd n  descriptor containing one HTTP Authorization header;
                use: oauth with PROFILE -- ask -header-fd 3 ...
  -schema file  constrain the answer with JSON Schema ("-" reads stdin)
  -json         emit this invocation's raw events instead of the answer
  -q            no progress on stderr; errors still print
compact only:
  -m spec       summarizer provider/model (default: the session's own)
  -d dir        conversation directory ($ASK_DIR)
  -header-fd n  descriptor containing one HTTP Authorization header
  -q            no progress on stderr; errors still print
                The note lands as the first message of a new session,
                stamped source=summary, with the parent and the
                summarizer's own session named in the header. The source
                is never touched. stdout is the new session's path.
replay only:
  -d dir        conversation directory ($ASK_DIR)
  -check        verify the replay invariant before producing output
  -step n       print the normalized request at this seq, rebuilding messages
  -json         emit raw events; combine with -check for a verified snapshot
note only:
  -s source     program writing the note (required, one word)
  -f file       session to append to (default: current)
  -d dir        conversation directory ($ASK_DIR)
  -q            no progress on stderr; errors still print
  -k kind       structured record kind; requires -json and -seal
  -json body    note JSON ("-" reads stdin); requires -k and -seal
  -seal         durably seal the structured note; requires -k and -json
keys: ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY, OPENROUTER_API_KEY
env: ASK_MODEL (-m) · ASK_SYSTEM (-S) · ASK_DIR (-d) · NO_COLOR
gateway: ANTHROPIC_BASE_URL · OPENAI_BASE_URL · OPENAI_CODEX_BASE_URL ·
  GEMINI_BASE_URL · OPENROUTER_BASE_URL replace provider endpoints. Supply
  OAuth through -header-fd; OPENAI_CODEX_ACCOUNT_ID supplies its non-secret
  account routing id when that provider requires one
vertex: ANTHROPIC_VERTEX_PROJECT_ID + CLOUD_ML_REGION route anthropic/ models
  through Google Vertex AI (ANTHROPIC_VERTEX_BASE_URL overrides the endpoint)
exit: 0 answered · 1 error · 2 context window full · 130 interrupted
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "replay":
			return cmdReplay(args[1:])
		case "system":
			return cmdSystem(args[1:])
		case "compact":
			return cmdCompact(args[1:])
		case "note":
			return cmdNote(args[1:])
		case "version", "-V", "--version":
			return printOutput("ask " + version + "\n")
		case "help", "-h", "--help":
			return printOutput(usageText)
		}
		if slices.Contains(retiredVerbs, args[0]) {
			fmt.Fprintf(os.Stderr, "ask: %s was removed; OAuth credentials belong to the oauth filter\n", args[0])
			fmt.Fprintln(os.Stderr, "ask: run oauth with PROFILE -- ask -header-fd 3 ...")
			return 1
		}
		if v := nearVerb(args[0]); v != "" {
			fmt.Fprintf(os.Stderr, "ask: unknown command %q — did you mean %q?\n", args[0], v)
			fmt.Fprintf(os.Stderr, "ask: to send it as a message instead: ask -- %s\n", args[0])
			return 1
		}
	}
	return cmdAsk(args)
}

// verbs is ask's command set, named once for the typo guard.
var verbs = []string{"replay", "compact", "note", "system", "version", "help"}
var retiredVerbs = []string{"login", "logout", "auth"}

// nearVerb returns the command a bare first word was probably meant to be,
// or "" if it was probably just a message. Asking is the default action and
// has to stay that way — ask why is this slow and ask summarize are both
// messages — so only a word one edit from a real verb is refused. Without
// the guard a typo is silently a paid model call: ask replya asks the model
// about "replya" and appends it to the live conversation, which is an
// expensive and confusing way to find out you mistyped replay.
func nearVerb(word string) string {
	// A flag, a phrase, or the -- escape: all messages.
	if word == "" || word[0] == '-' || strings.ContainsAny(word, " \t\n") {
		return ""
	}
	if slices.Contains(verbs, word) {
		return "" // a hit, not a near miss; the caller has already run it
	}
	for _, v := range verbs {
		if within1(word, v) {
			return v
		}
	}
	return ""
}

// within1 reports whether two words are at most one typo apart: an insert,
// a delete, a substitution, or a transposition of neighbours. Transposition
// earns its place — "replya" and "sytsem" are what a keyboard actually
// produces, and plain edit distance scores them as two.
func within1(a, b string) bool {
	switch n := len(a) - len(b); {
	case n == 0:
		for i := 0; i < len(a); i++ {
			if a[i] == b[i] {
				continue
			}
			// The first difference is free. A second is allowed only as a
			// swap of this pair, with everything after it identical.
			return a[i+1:] == b[i+1:] ||
				(i+1 < len(a) && a[i] == b[i+1] && a[i+1] == b[i] && a[i+2:] == b[i+2:])
		}
		return true // identical
	case n == 1 || n == -1:
		long, short := a, b
		if n < 0 {
			long, short = b, a
		}
		for i := 0; i < len(short); i++ {
			if long[i] != short[i] {
				return long[i+1:] == short[i:] // drop long[i] and they match
			}
		}
		return true // the extra character is at the end
	}
	return false
}

func cmdAsk(args []string) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	var (
		spec       = fs.String("m", os.Getenv("ASK_MODEL"), "provider/model")
		sys        = fs.String("S", "", "system prompt for this call")
		continuing = fs.Bool("c", false, "continue the current conversation")
		file       = fs.String("f", "", "session log file")
		dir        = fs.String("d", askDir(), "conversation directory")
		effort     = fs.String("effort", "", "reasoning effort: off, low, medium, high, xhigh")
		maxTokens  = fs.Int("max-tokens", 16384, "max output tokens")
		headerFD   = fs.Int("header-fd", -1, "descriptor containing an HTTP Authorization header")
		schemaFile = fs.String("schema", "", "JSON Schema for the answer ('-' reads stdin)")
		jsonOut    = fs.Bool("json", false, "emit raw events on stdout")
		quiet      = fs.Bool("q", false, "no progress on stderr; errors still print")
		attached   attachFlag
	)
	fs.Var(&attached, "a", "attach a file; repeat for more")
	usage(fs, `ask [flags] [message ...]`)
	if err := fs.Parse(args); err != nil {
		return usageCode(fs, err)
	}
	if *continuing && *file != "" {
		return fail(errors.New("-c and -f cannot be used together"))
	}
	sysSet, maxTokensSet := false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "S":
			sysSet = true
		case "max-tokens":
			maxTokensSet = true
		}
	})
	switch *effort {
	case "", "off", "low", "medium", "high", "xhigh":
	default:
		return fail(fmt.Errorf("-effort must be off, low, medium, high, or xhigh, got %q", *effort))
	}
	var outputSchema *structuredOutput
	if *schemaFile != "" {
		var err error
		outputSchema, err = loadSchema(*schemaFile)
		if err != nil {
			return fail(err)
		}
	}

	// Everything that can fail on configuration alone fails before a
	// session file exists, so a bad invocation leaves no litter behind.
	path, cont, err := session(*dir, *file, *continuing)
	if err != nil {
		return fail(err)
	}
	var log *event.Log
	var events []event.Event
	if cont {
		if log, events, err = event.Open(path); err != nil {
			return fail(err)
		}
		defer log.Close()
	}
	if *spec == "" {
		*spec = headerModel(events)
	}
	if *spec == "" {
		return fail(errors.New("no model: pass -m provider/model or set ASK_MODEL"))
	}
	if maxTokensSet && strings.HasPrefix(*spec, "openai-codex/") {
		return fail(errors.New("-max-tokens is not supported by openai-codex"))
	}
	authorization, err := readAuthorizationFD(*headerFD)
	if err != nil {
		return fail(err)
	}
	prov, model, err := provider.New(*spec, provider.Options{
		Authorization: authorization, CodexAccountID: os.Getenv("OPENAI_CODEX_ACCOUNT_ID"),
	})
	if err != nil {
		return fail(err)
	}
	// Attachments are read, typed, sized and checked against the provider
	// here — before any session file exists — so a run that cannot be sent
	// never becomes an event that every later fold would rebuild.
	files, err := attach(attached, *spec)
	if err != nil {
		return fail(err)
	}
	data, piped, err := stdinData()
	if err != nil {
		return fail(err)
	}
	content, evidence, err := message(strings.Join(fs.Args(), " "), data, files, *spec)
	if err != nil {
		return fail(err)
	}
	if len(content) == 0 {
		if piped {
			return fail(errors.New("stdin was empty and no message was given"))
		}
		fs.SetOutput(os.Stderr)
		fs.Usage()
		return 1
	}

	if !cont {
		if path == "" {
			log, err = event.Create(*dir)
		} else {
			log, err = event.CreateFile(path)
		}
		if err != nil {
			return fail(err)
		}
		defer log.Close()
	}
	c := &chat.Chat{
		Provider: prov, Model: model, System: system(*sys, sysSet),
		MaxTokens: *maxTokens, Effort: *effort, Schema: outputSchema.requestSchema(),
		Evidence: evidence, Log: log,
	}
	c.Load(events)

	// Two views over one event stream: progress on stderr for a human,
	// raw JSONL on stdout for a program. Neither is a second log.
	var views []func(event.Event)
	var outputErr error
	var progress *renderer
	if !*quiet {
		r := newRenderer(os.Stderr)
		progress = r
		c.OnDelta = r.delta
		views = append(views, r.event)
		if n := c.Turns(); n > 0 {
			fmt.Fprintln(os.Stderr, r.dim(fmt.Sprintf(
				"ask: continuing %s · %s · %d turns so far", log.ID(), *spec, n)))
		}
	}
	if *jsonOut {
		emit := jsonEvents(os.Stdout)
		views = append(views, func(e event.Event) {
			if outputErr == nil {
				outputErr = emit(e)
			}
		})
	}
	log.Observe(func(e event.Event) {
		for _, view := range views {
			view(e)
		}
	})

	// A header describes a file, not a turn: one per session, written
	// before anything else. A continued session keeps the one it was
	// born with.
	if !cont {
		if err := header(log, *spec, c.System); err != nil {
			return fail(err)
		}
		// A plain ask starts the conversation a later `ask -c` will
		// continue. An explicit -f owns its thread and never changes this
		// pointer.
		if *file == "" {
			if err := event.SetCurrent(*dir, log); err != nil {
				return fail(err)
			}
		}
	}

	ctx, stop := sigCtx()
	defer stop()
	answer, err := c.Say(ctx, content)
	if err == nil && answer == "" {
		// A turn that streamed nothing is a failed run, not a quiet
		// success: the next program in the pipe would read an empty
		// input and have no way to tell why.
		err = errNoText
	}
	if err == nil && outputSchema != nil {
		if err = outputSchema.validate(answer); err != nil {
			// Invalid structured data is not an answer. It remains in the
			// append-only log, but stdout stays empty so a pipeline cannot
			// mistake it for a successful document.
			answer = ""
		}
	}
	done(log, err)
	if outputErr == nil && progress != nil && sameOut() {
		outputErr = progress.err
	}
	if outputErr != nil {
		return fail(fmt.Errorf("writing stdout: %w", outputErr))
	}
	return finish(*jsonOut, !*quiet, answer, err)
}

var errNoText = errors.New("the model returned no text")

// session decides which log this run writes to. Plain ask starts fresh, -c
// continues exactly current, and -f continues or creates the named thread.
func session(dir, file string, continuing bool) (path string, cont bool, err error) {
	if file != "" {
		if _, err := os.Stat(file); err == nil {
			return file, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("session %s: %w", file, err)
		}
		return file, false, nil
	}
	if continuing {
		p, err := event.Current(dir)
		if err != nil {
			return "", false, fmt.Errorf("no current conversation in %s: %w", dir, err)
		}
		return p, true, nil
	}
	return "", false, nil // event.Create mints an id in dir
}

// askDir is where conversations live: $ASK_DIR, else ~/.ask/sessions. A
// filter should not litter the directory it is run from, and keeping the
// logs beside the credentials means one place holds everything ask owns.
func askDir() string {
	if d := os.Getenv("ASK_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ask/sessions"
	}
	return filepath.Join(home, ".ask", "sessions")
}

// headerModel reads the model a session was started with, so continuing it
// needs no -m. Switching providers mid-conversation is allowed and
// degrades gracefully: each adapter replays its own opaque reasoning state
// and ignores another's.
func headerModel(events []event.Event) string {
	for _, e := range events {
		if e.Type == event.Session {
			if h, err := event.As[event.Header](e); err == nil {
				return h.Model
			}
		}
	}
	return ""
}

func header(log *event.Log, spec, sys string) error {
	_, err := log.Append(event.Session, event.Header{
		ID: log.ID(), Version: version, Go: goVersion(), SDKs: sdkVersions(),
		Model: spec, System: sys,
	})
	return err
}

// done records how the turn ended, so a log read later says what happened
// rather than leaving the reader to infer it from what is missing.
func done(log *event.Log, err error) {
	switch {
	case err == nil:
		log.Append(event.Done, event.DoneData{Reason: "end"})
	case errors.Is(err, chat.ErrOverflow):
		log.Append(event.Done, event.DoneData{Reason: "overflow", Error: err.Error()})
	case errors.Is(err, context.Canceled):
		// The abort event is already logged, and the session is usable.
	default:
		log.Append(event.Done, event.DoneData{Reason: "error", Error: err.Error()})
	}
}

// finish writes the answer and maps the outcome to an exit code.
//
// streamed says whether a renderer already put the answer on stderr. Only
// then can printing it to a stdout that lands in the same place show it
// twice — under -q nothing has been shown, so the answer must be printed
// whatever the streams are pointed at.
func finish(jsonOut, streamed bool, answer string, err error) int {
	if answer != "" && !jsonOut && !(streamed && sameOut()) {
		if code := printOutput(answer + "\n"); code != 0 {
			return code
		}
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, chat.ErrOverflow):
		fmt.Fprintln(os.Stderr, "ask:", err)
		fmt.Fprintln(os.Stderr, "ask: start a new conversation without -c or -f")
		return 2
	case errors.Is(err, context.Canceled):
		return 130
	default:
		fmt.Fprintln(os.Stderr, "ask:", err)
		return 1
	}
}

func printOutput(text string) int {
	if _, err := io.WriteString(os.Stdout, text); err != nil {
		return fail(fmt.Errorf("writing stdout: %w", err))
	}
	return 0
}

// sameOut reports whether stdout and stderr land in the same place — the
// one case where printing the answer would show it twice, since it already
// streamed through the renderer. Same terminal, same file, same pipe: all
// suppress the echo; a redirected stderr (2>/dev/null) does not.
func sameOut() bool {
	so, err1 := os.Stdout.Stat()
	se, err2 := os.Stderr.Stat()
	return err1 == nil && err2 == nil && os.SameFile(so, se)
}

// stdinData reads piped or redirected stdin to EOF. A terminal (or any
// other character device, like /dev/null) contributes nothing. The bytes
// come back untouched: what stdin is holding is decided by looking at it,
// not by assuming.
//
// Too much input is an error, never a truncation. A filter that quietly
// dropped the tail of its input would answer a question about the first
// sixteen megabytes and present it as an answer about the file, and nothing
// downstream could tell the difference. Reading one byte past the limit is
// what makes "too much" detectable at all. A read that fails is reported as
// what it was, rather than becoming an indistinguishable empty pipe.
func stdinData() (data []byte, piped bool, err error) {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil, false, nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, maxAttachment+1))
	if err != nil {
		return nil, true, fmt.Errorf("reading stdin: %w", err)
	}
	if len(b) > maxAttachment {
		return nil, true, fmt.Errorf("piped input is larger than %d MB", maxAttachment>>20)
	}
	return b, true, nil
}

// message assembles one user message, in order: the attachments as they
// were named, then stdin if it turned out to be an attachment itself, then
// the text. Media before text is what every provider asks for, and it is
// the only order there is — a message is one argv string, so the sole
// ordering a caller can express is the order of the -a flags, and that is
// preserved exactly.
//
// stdin keeps its old meaning when it is text: it rides inside the message
// delimited as evidence, so `git diff | ask "write a commit message"` is
// unchanged. When stdin is a PNG, it becomes an attachment instead —
// `screencapture -x -t png - | ask "what is this?"` needs no new flag.
func message(text string, data []byte, files []provider.Block, spec string) ([]provider.Block, *event.EvidenceData, error) {
	blocks := files
	if len(data) > 0 && !isText(data) {
		// The size bound belongs to stdinData, which is the only thing that
		// can tell "exactly at the limit" from "more where that came from".
		b, err := classify("stdin", data)
		if err != nil {
			return nil, nil, fmt.Errorf("stdin: %w", err)
		}
		if err := provider.Accepts(spec, b.MediaType); err != nil {
			return nil, nil, fmt.Errorf("stdin: %w", err)
		}
		blocks = append(blocks, b)
		data = nil
	}
	trimmed := strings.TrimSpace(string(data))
	var evidence *event.EvidenceData
	if trimmed != "" {
		block := len(blocks)
		offset := 0
		if text != "" {
			offset = len(text + "\n\n<stdin>\n")
		}
		var claimed bool
		var err error
		evidence, claimed, err = event.ContextEvidence([]byte(trimmed), block, offset)
		if err != nil {
			return nil, nil, err
		}
		if !claimed {
			evidence, err = envelopedContextEvidence(trimmed, block)
			if err != nil {
				return nil, nil, err
			}
		}
	}
	if t := compose(text, trimmed); t != "" {
		blocks = append(blocks, provider.Block{Type: provider.Text, Text: t})
	}
	return blocks, evidence, nil
}

// envelopedContextEvidence recognizes the same stdin envelope Ask and Ply use
// when data rides with an instruction. Ply sends its composed first message on
// stdin, so from Ask's process boundary the envelope is already present. The
// envelope is unambiguous for Context JSONL: raw newlines inside JSON strings
// are escaped, and a normalized record is one physical line. Text may follow
// the envelope (for example Ply's initial-check transcript).
func envelopedContextEvidence(text string, block int) (*event.EvidenceData, error) {
	const open, close = "\n\n<stdin>\n", "\n</stdin>"
	for search := 0; search < len(text); {
		relStart := strings.Index(text[search:], open)
		if relStart < 0 {
			return nil, nil
		}
		start := search + relStart + len(open)
		relEnd := strings.Index(text[start:], close)
		if relEnd < 0 {
			return nil, nil
		}
		end := start + relEnd
		manifest, claimed, err := event.ContextEvidence([]byte(text[start:end]), block, start)
		if err != nil {
			return nil, err
		}
		if claimed {
			return manifest, nil
		}
		// Search after this opening, not after its closing tag: if arbitrary
		// goal text contained an opening marker, Ply's real envelope may be
		// nested inside the candidate we just rejected.
		search = start
	}
	return nil, nil
}

// compose merges an instruction and textual stdin, the way a shell user
// expects: git diff | ask "write a commit message". Stdin alone is the
// whole message. Unlike an agent, ask has no tools, so there is nowhere to
// put input except in the conversation: a pipe too large for the context
// window is reported as such rather than silently truncated.
func compose(text, data string) string {
	switch {
	case data == "":
		return text
	case text == "":
		return data
	}
	return text + "\n\n<stdin>\n" + data + "\n</stdin>"
}

func cmdSystem(args []string) int {
	fs := flag.NewFlagSet("system", flag.ContinueOnError)
	usage(fs, "ask system")
	if err := fs.Parse(args); err != nil {
		return usageCode(fs, err)
	}
	return printOutput(defaultSystem + "\n")
}

func cmdReplay(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	var (
		dir     = fs.String("d", askDir(), "conversation directory")
		jsonOut = fs.Bool("json", false, "emit raw events on stdout")
		check   = fs.Bool("check", false, "verify the replay invariant before producing output")
		step    = fs.Int("step", 0, "print the normalized request at this seq")
	)
	usage(fs, "ask replay [flags] [session]")
	if err := fs.Parse(args); err != nil {
		return usageCode(fs, err)
	}
	path, err := sessionPath(*dir, fs.Arg(0))
	if err != nil {
		return fail(err)
	}
	// A damaged log still renders as far as it reads: showing a session up
	// to the corruption beats showing nothing. Verifying one does not —
	// what cannot be read cannot be proved — so -check keeps failing.
	events, rerr := event.ReadFile(path)
	if rerr != nil && (len(events) == 0 || *check) {
		return fail(rerr)
	}
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "ask: %v\nask: rendering the %d events before it\n", rerr, len(events))
	}
	if len(events) == 0 {
		return fail(fmt.Errorf("%s is empty", path))
	}
	switch {
	case *check:
		if err := event.Check(events); err != nil {
			return fail(err)
		}
		if *jsonOut {
			emit := jsonEvents(os.Stdout)
			for _, e := range events {
				if err := emit(e); err != nil {
					return fail(fmt.Errorf("writing stdout: %w", err))
				}
			}
		} else {
			return printOutput(fmt.Sprintf("ok: %s replays exactly; sealed records verify (%d events)\n", filepath.Base(path), len(events)))
		}
	case *step > 0:
		for i, e := range events {
			if e.Seq != *step || e.Type != event.Request {
				continue
			}
			req, err := event.As[provider.Request](e)
			if err != nil {
				return fail(err)
			}
			// The log stores the conversation as a digest, so the exact
			// request is the recorded fields plus the fold that produced
			// it — the same fold -check proves.
			if req.Digest != "" {
				if req.Messages, err = event.Fold(events[:i]); err != nil {
					return fail(err)
				}
				req.Digest = ""
			}
			out, err := json.MarshalIndent(req, "", "  ")
			if err != nil {
				return fail(err)
			}
			return printOutput(string(out) + "\n")
		}
		return fail(fmt.Errorf("no request event at seq %d", *step))
	case *jsonOut:
		emit := jsonEvents(os.Stdout)
		for _, e := range events {
			if err := emit(e); err != nil {
				return fail(fmt.Errorf("writing stdout: %w", err))
			}
		}
	default:
		r := newRenderer(os.Stdout)
		for _, e := range events {
			switch e.Type {
			case event.Session:
				h, _ := event.As[event.Header](e)
				r.line(fmt.Sprintf("session %s · %s · %s", h.ID, h.Model, e.Time.Local().Format("2006-01-02 15:04")))
				continue
			case event.Assistant:
				t, _ := event.As[event.Turn](e)
				for _, b := range t.Blocks {
					switch b.Type {
					case provider.Reasoning:
						r.delta(provider.KindReasoning, b.Text+"\n")
					case provider.Text:
						r.delta(provider.KindText, b.Text+"\n")
					}
				}
			}
			r.event(e)
		}
		if r.err != nil {
			return fail(fmt.Errorf("writing stdout: %w", r.err))
		}
	}
	if rerr != nil {
		return 1
	}
	return 0
}

// sessionPath resolves a replay argument: a path, a bare session id in the
// conversation directory, or nothing at all for the current session.
func sessionPath(dir, arg string) (string, error) {
	if arg == "" {
		p, err := event.Latest(dir)
		if err != nil {
			return "", fmt.Errorf("no sessions in %s", dir)
		}
		return p, nil
	}
	if _, err := os.Stat(arg); err == nil {
		return arg, nil
	}
	p := filepath.Join(dir, strings.TrimSuffix(arg, ".jsonl")+".jsonl")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("no session %q in %s", arg, dir)
}

// jsonEvents renders events as raw JSONL — the machine view.
func jsonEvents(w io.Writer) func(event.Event) error {
	enc := json.NewEncoder(w)
	return func(e event.Event) error { return enc.Encode(e) }
}

// usage gives a flag set a synopsis line above its flag defaults, and takes
// the printing away from the flag package: Parse would put both the error
// and the usage on stderr, and the two belong on different streams.
// usageCode chooses.
func usage(fs *flag.FlagSet, synopsis string) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s\n", synopsis)
		fs.PrintDefaults()
	}
}

// usageCode maps a flag parse error to an exit code and prints the usage
// where it belongs. Help that was asked for is the output of a successful
// command, so it goes to stdout and `ask -h | less` works; a misuse is a
// diagnostic, so it goes to stderr with the offending flag named and leaves
// stdout empty for the caller parsing it. Exit 2 belongs to a full context
// window, never to typos.
func usageCode(fs *flag.FlagSet, err error) int {
	if errors.Is(err, flag.ErrHelp) {
		var out strings.Builder
		fs.SetOutput(&out)
		fs.Usage()
		return printOutput(out.String())
	}
	fs.SetOutput(os.Stderr)
	fmt.Fprintln(os.Stderr, "ask:", err)
	fs.Usage()
	return 1
}

// sigCtx cancels on interrupt, so ^C ends the stream, logs the abort, and
// leaves a session that can be continued.
//
// The handler unregisters itself the moment it fires, which signal.
// NotifyContext does not do: it keeps the signal captured for the life of
// the context, so every ^C after the first is swallowed. That is the wrong
// bargain for a terminal program. The first ^C asks politely and gets a
// clean session; if something below is not honoring cancellation, a second
// takes the default action and kills the process, the way it does for every
// other command in the shell. Nobody should have to reach for kill -9 on a
// filter.
func sigCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			signal.Stop(ch) // the next one is the operating system's
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { signal.Stop(ch); cancel() }
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "ask:", err)
	return 1
}

func goVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.GoVersion
	}
	return ""
}

func sdkVersions() map[string]string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	v := map[string]string{}
	for _, d := range bi.Deps {
		switch d.Path {
		case "github.com/anthropics/anthropic-sdk-go",
			"github.com/openai/openai-go/v3",
			"google.golang.org/genai":
			v[d.Path] = d.Version
		}
	}
	return v
}
