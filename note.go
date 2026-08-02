// A note is something a program recorded about a run.
//
// ask's rule is that text nobody typed is admissible in a session only if
// it is stamped, and `ask note` is that rule as a verb rather than an
// exception to it: -s is required, so there is no way to write an
// unattributed note. It makes no model call, so there is no request event,
// no cost, and nothing new to replay.
//
// What it is for is the verdict. `ply` decides whether work is done by
// running a command, and that verdict lived only on stderr and in an exit
// status — so a session recorded everything that was tried and nothing
// about whether it worked, and a passing run and a failing one were
// byte-for-byte the same shape. ask already states the principle in
// DoneData: what stopped a turn belongs "in the log rather than only in the
// exit code, so a session read later says how it finished". A note is that
// sentence, available to the program that owns the outcome.
//
// It is not folded. A failing check is a user message because the model has
// to act on it; a passing check is addressed to nobody, because the run is
// over. Record, not message — so the conversation a provider sees is
// exactly what it was before, and every digest ever written still matches.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/patrickyoung/ask/internal/event"
)

func cmdNote(args []string) int {
	fs := flag.NewFlagSet("note", flag.ContinueOnError)
	var (
		dir    = fs.String("d", askDir(), "conversation directory")
		file   = fs.String("f", "", "session file (default: the current conversation)")
		source = fs.String("s", "", "the program recording it (required)")
		quiet  = fs.Bool("q", false, "no progress on stderr")
	)
	usage(fs, "ask note -s source [flags] [text ...]")
	if err := fs.Parse(args); err != nil {
		return usageCode(fs, err)
	}
	if strings.TrimSpace(*source) == "" {
		return fail(errors.New("note needs -s: a note nobody signed is the thing this refuses to write"))
	}
	if strings.ContainsAny(*source, " \t\n") {
		return fail(fmt.Errorf("-s %q: a source is one word, the name of the program recording it", *source))
	}

	text, err := noteText(fs.Args())
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(text) == "" {
		return fail(errors.New("nothing to note: pass text as arguments or on stdin"))
	}

	path := *file
	if path == "" {
		if path, err = sessionPath(*dir, ""); err != nil {
			return fail(err)
		}
	}
	// Everything that can fail has failed by here except the append itself,
	// so a bad invocation does not touch somebody's conversation.
	log, _, err := event.Open(path)
	if err != nil {
		return fail(err)
	}
	defer log.Close()
	if _, err := log.Append(event.Note, event.NoteData{Source: *source, Text: text}); err != nil {
		return fail(err)
	}
	if err := log.Sync(); err != nil {
		return fail(err)
	}
	if !*quiet {
		fmt.Fprintf(os.Stderr, "ask: noted %d bytes in %s as %s\n", len(text), log.ID(), *source)
	}
	return 0
}

// noteText takes the note from argv, or from stdin when argv is empty —
// the same composition every other verb uses, and the reason `ply` can pipe
// a typescript in without quoting it.
func noteText(args []string) (string, error) {
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, maxAttachment+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxAttachment {
		return "", fmt.Errorf("note is larger than %d MB", maxAttachment>>20)
	}
	return string(b), nil
}
