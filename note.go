// A note is an attributed record, not a message. It makes no model call and
// is not folded, so it cannot change the conversation or any request digest.
// The required -s value says which program wrote it.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/patrickyoung/ask/internal/event"
)

func cmdNote(args []string) (code int) {
	fs := flag.NewFlagSet("note", flag.ContinueOnError)
	var (
		dir      = fs.String("d", askDir(), "conversation directory")
		file     = fs.String("f", "", "session file (default: the current conversation)")
		source   = fs.String("s", "", "the program recording it (required)")
		quiet    = fs.Bool("q", false, "no progress on stderr; errors still print")
		kind     = fs.String("k", "", "structured record kind (requires -json and -seal)")
		jsonBody = fs.String("json", "", "structured record JSON body")
		seal     = fs.Bool("seal", false, "fsync the structured record with a replay-verifiable prefix seal")
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

	structured := *kind != "" || *jsonBody != "" || *seal
	var text string
	var body json.RawMessage
	if structured {
		if *kind == "" || *jsonBody == "" || !*seal {
			return fail(errors.New("a structured note requires -k, -json, and -seal together"))
		}
		if fs.NArg() != 0 {
			return fail(errors.New("structured note body comes from -json; do not also pass text"))
		}
		if strings.ContainsAny(*kind, " \t\n") {
			return fail(fmt.Errorf("-k %q: a record kind cannot contain whitespace", *kind))
		}
		if *jsonBody == "-" {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, maxAttachment+1))
			if err != nil {
				return fail(err)
			}
			if len(b) > maxAttachment {
				return fail(fmt.Errorf("structured note is larger than %d MB", maxAttachment>>20))
			}
			body = json.RawMessage(b)
		} else {
			body = json.RawMessage(*jsonBody)
		}
		if !json.Valid(body) {
			return fail(errors.New("-json is not valid JSON"))
		}
	} else {
		var err error
		text, err = noteText(fs.Args())
		if err != nil {
			return fail(err)
		}
		if strings.TrimSpace(text) == "" {
			return fail(errors.New("nothing to note: pass text as arguments or on stdin"))
		}
	}

	path := *file
	var err error
	if path == "" {
		if path, err = event.Current(*dir); err != nil {
			return fail(fmt.Errorf("no current conversation in %s: %w", *dir, err))
		}
	}
	// Everything that can fail has failed by here except the append itself,
	// so a bad invocation does not touch somebody's conversation.
	log, _, err := event.Open(path)
	if err != nil {
		return fail(err)
	}
	defer closeLog(log, &code)
	note := event.NoteData{Source: *source, Text: text, Kind: *kind, Body: body}
	if structured {
		if _, err := log.AppendSealed(event.Note, note); err != nil {
			return fail(err)
		}
	} else {
		if _, err := log.Append(event.Note, note); err != nil {
			return fail(err)
		}
		if err := log.Sync(); err != nil {
			return fail(err)
		}
	}
	closeLog(log, &code)
	if code != 0 {
		return code
	}
	if !*quiet {
		fmt.Fprintf(os.Stderr, "ask: noted %d bytes in %s as %s\n", len(text)+len(body), log.ID(), *source)
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
