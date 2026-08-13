// A note is an attributed record, not a message. It makes no model call and
// is not folded, so it cannot change the conversation or any request digest.
// The required -s value says which program wrote it.
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
		quiet  = fs.Bool("q", false, "no progress on stderr; errors still print")
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
