// Compaction is the one thing this program cannot do for you quietly.
//
// A conversation that outgrew its window fails identically forever — that
// is what exit 2 means and why it has its own status. The only way forward
// is to carry less of it, which means deciding what to drop, which is a
// judgment, which means a model writes the thing you continue from. So the
// note is written by an explicit verb, in its own session, and stamped
// where it lands: unattributed model text appearing in a conversation is
// the one kind of magic ask refuses.
//
// Three files, three roles. The source is never touched. The summarizer
// gets a session of its own, so the call that wrote the note is as
// replayable as any other. The compacted session holds the note as its
// first message, with its parent and its summarizer named in the header.
// Each of the three replays alone, and `ask replay -check` proves all
// three.
//
// Branching verbatim needs no verb: a session is a self-contained file and
// -f names one, so `cp` is fork.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/patrickyoung/ask/internal/chat"
	"github.com/patrickyoung/ask/internal/event"
	"github.com/patrickyoung/ask/internal/provider"
)

// summarySystem asks for a handoff, not a book report. The difference
// matters: "the parser rejects tabs" is worth carrying and "the user asked
// about the parser" is not, and a model told to summarize a conversation
// will write the second one.
const summarySystem = `You are writing a handoff note for someone who will continue this work with no memory of it beyond what you write down.

From the transcript, record: what was being worked on; what was established, decided, or ruled out, and briefly why; the state things are in now, with specifics — names, paths, numbers, exact wording where the wording matters; and what was about to happen next.

Write notes to a colleague, not a report about a conversation: "the parser rejects tabs" rather than "the user asked about the parser". Carry the facts, not the fact that they were discussed. If something was uncertain, say it is uncertain. Plain text, no preamble, no sign-off, no headings unless the material genuinely has parts.`

func cmdCompact(args []string) int {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	var (
		dir      = fs.String("d", askDir(), "conversation directory")
		mspec    = fs.String("m", "", "summarizer provider/model")
		headerFD = fs.Int("header-fd", -1, "descriptor containing an HTTP Authorization header")
		quiet    = fs.Bool("q", false, "no progress on stderr; errors still print")
	)
	usage(fs, "ask compact [flags] [session]")
	if err := fs.Parse(args); err != nil {
		return usageCode(fs, err)
	}

	src, err := sessionPath(*dir, fs.Arg(0))
	if err != nil {
		return fail(err)
	}
	events, err := event.ReadFile(src)
	if err != nil {
		return fail(err)
	}
	if err := event.Check(events); err != nil {
		return fail(fmt.Errorf("verify session before compact: %w", err))
	}
	var hdr event.Header
	if len(events) > 0 && events[0].Type == event.Session {
		hdr, _ = event.As[event.Header](events[0])
	}
	if *mspec == "" {
		*mspec = hdr.Model
	}
	if *mspec == "" {
		return fail(errors.New("session names no model: pass -m provider/model"))
	}
	authorization, err := readAuthorizationFD(*headerFD)
	if err != nil {
		return fail(err)
	}
	prov, model, err := provider.New(*mspec, provider.Options{
		Authorization: authorization, CodexAccountID: os.Getenv("OPENAI_CODEX_ACCOUNT_ID"),
	})
	if err != nil {
		return fail(err)
	}
	text := transcript(events)
	if strings.TrimSpace(text) == "" {
		return fail(fmt.Errorf("%s holds no conversation to compact", filepath.Base(src)))
	}

	// Everything that can fail has failed by here, so no file is created
	// for an invocation that was never going to run. The new sessions land
	// beside the source, wherever that is.
	home := filepath.Dir(src)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	note, sumID, code := summarize(ctx, home, prov, model, *mspec, text, *quiet)
	if code != 0 {
		return code
	}

	log, err := event.Create(home)
	if err != nil {
		return fail(err)
	}
	defer log.Close()
	if _, err := log.Append(event.Session, event.Header{
		ID: log.ID(), Version: version, Go: goVersion(), SDKs: sdkVersions(),
		Model: hdr.Model, System: hdr.System, Parent: idOf(hdr, src), Summary: sumID,
	}); err != nil {
		return fail(err)
	}
	// The note is a user message because that is what it is: the thing the
	// next turn is answered in light of. Source says a model wrote it.
	if _, err := log.Append(event.User, event.UserData{Text: note, Source: "summary"}); err != nil {
		return fail(err)
	}
	if err := log.Sync(); err != nil {
		return fail(err)
	}
	// Compacting the conversation you are in moves you into the compact
	// one, because that is the only reason to do it. Compacting some other
	// file leaves your current session where it was.
	if wasCurrent(*dir, src) {
		if err := event.SetCurrent(*dir, log); err != nil {
			return fail(err)
		}
	}
	if !*quiet {
		fmt.Fprintf(os.Stderr, "ask: compacted %s → %s (note by %s, %d bytes from %d)\n",
			idOf(hdr, src), log.ID(), sumID, len(note), len(text))
	}
	return printOutput(log.Path() + "\n")
}

// summarize runs one fresh-context turn over the transcript, in a session
// of its own. Its own session is the point: the note is model-written, so
// the request that produced it has to be as inspectable as any other, and
// it must not land in either the conversation it describes or the one it
// opens.
func summarize(ctx context.Context, dir string, prov provider.Provider, model, spec, text string, quiet bool) (note, id string, code int) {
	log, err := event.Create(dir)
	if err != nil {
		return "", "", fail(err)
	}
	defer log.Close()
	if err := header(log, spec, summarySystem); err != nil {
		return "", "", fail(err)
	}
	c := &chat.Chat{Provider: prov, Model: model, System: summarySystem, MaxTokens: 16384, Log: log}
	if !quiet {
		r := newRenderer(os.Stderr)
		c.OnDelta = r.delta
		log.Observe(r.event)
		fmt.Fprintln(os.Stderr, r.dim(fmt.Sprintf("ask: summarizing %d bytes with %s", len(text), spec)))
	}
	note, err = c.Say(ctx, []provider.Block{{Type: provider.Text, Text: text}})
	switch {
	case errors.Is(err, context.Canceled):
		return "", "", 130
	case errors.Is(err, chat.ErrOverflow):
		// The transcript did not fit either. Say the thing that would fix
		// it rather than the thing that happened.
		fmt.Fprintln(os.Stderr, "ask: the transcript is too large for", spec)
		fmt.Fprintln(os.Stderr, "ask: compact with a wider window: ask compact -m provider/model")
		return "", "", 2
	case err != nil:
		return "", "", fail(err)
	}
	if strings.TrimSpace(note) == "" {
		return "", "", fail(errors.New("the summarizer returned no note"))
	}
	return note, log.ID(), 0
}

// transcript renders a conversation for a reader who was not there.
//
// Reasoning is left out. It is the largest part of a modern session and
// the least transferable — provider-opaque, addressed to a turn that is
// over — and leaving it out is also most of why a transcript fits where
// the conversation it came from did not. Attachments are represented by
// media type, not carried: a note about a photograph is not a photograph.
func transcript(events []event.Event) string {
	var b strings.Builder
	for _, e := range events {
		switch e.Type {
		case event.User:
			u, err := event.As[event.UserData](e)
			if err != nil {
				continue
			}
			who := "asked"
			if u.Source != "" {
				who = u.Source
			}
			fmt.Fprintf(&b, "[%s]\n%s\n\n", who, blocks(u.Content()))
		case event.Assistant:
			t, err := event.As[event.Turn](e)
			if err != nil || t.Partial {
				continue
			}
			if s := blocks(t.Blocks); strings.TrimSpace(s) != "" {
				fmt.Fprintf(&b, "[answered]\n%s\n\n", s)
			}
		case event.Note:
			// A note is not folded, so a model continuing the conversation
			// never saw it — but a handoff is written for someone with no
			// memory of the work, and "the check passed" is exactly the
			// kind of thing they need. It goes in named, like everything
			// else here.
			n, err := event.As[event.NoteData](e)
			if err != nil {
				continue
			}
			label, body := n.Source, n.Text
			if n.Kind != "" {
				label, body = n.Source+":"+n.Kind, string(n.Body)
			}
			fmt.Fprintf(&b, "[%s]\n%s\n\n", label, body)
		}
	}
	return b.String()
}

func blocks(bs []provider.Block) string {
	var parts []string
	for _, bl := range bs {
		switch bl.Type {
		case provider.Text:
			parts = append(parts, bl.Text)
		case provider.Reasoning:
			// left out on purpose; see transcript
		default:
			parts = append(parts, fmt.Sprintf("[%s attachment]", bl.MediaType))
		}
	}
	return strings.Join(parts, "\n")
}

// idOf prefers the id a session recorded for itself and falls back to its
// filename, so a copied session still names something a reader can find.
func idOf(h event.Header, path string) string {
	if h.ID != "" {
		return h.ID
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func wasCurrent(dir, src string) bool {
	cur, err := event.Latest(dir)
	if err != nil {
		return false
	}
	a, err1 := filepath.EvalSymlinks(cur)
	b, err2 := filepath.EvalSymlinks(src)
	return err1 == nil && err2 == nil && a == b
}
