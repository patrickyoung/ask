package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/patrickyoung/ask/internal/provider"
)

// Attachments are read whole into memory, base64'd into a JSON event, and
// then again into a provider request, so a megabyte on disk is several
// megabytes live. These are the fixed bounds that keeps that arithmetic
// knowable. They are constants rather than flags on purpose: a limit you
// can raise from the command line is a limit that will be raised until
// something falls over somewhere less obvious.
const (
	maxAttachment  = 16 << 20 // one file
	maxAttached    = 32 << 20 // all files in one message
	maxAttachments = 16       // how many
)

// mediaTypes is every attachment type ask will carry. Anything not on this
// list is refused locally, by name, rather than sent for a provider to
// reject in less useful words. The list is the boundary; there is no
// sniffing beyond it and nothing here is ever decoded — no dimensions, no
// duration, no parsers, and so no parser bugs.
var mediaTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"application/pdf": true,
	"audio/wav":       true, "audio/mpeg": true, "audio/mp4": true,
	"audio/aiff": true, "audio/ogg": true, "audio/flac": true,
	"video/mp4": true, "video/quicktime": true, "video/webm": true,
}

// attachFlag collects a repeatable -a.
type attachFlag []string

func (a *attachFlag) String() string { return strings.Join(*a, ",") }
func (a *attachFlag) Set(v string) error {
	// stdin already has a meaning, and a second spelling for it would be a
	// second way to do one thing.
	if v == "-" {
		return fmt.Errorf(`-a - is not a thing: pipe it instead (cmd | ask "...")`)
	}
	if v == "" {
		return fmt.Errorf("-a needs a file")
	}
	*a = append(*a, v)
	return nil
}

// attach reads the files named by -a, in the order they were named, and
// returns them as blocks. Every failure here happens before the session
// log is touched: an attachment this run cannot carry must never become a
// user event, or every later fold would rebuild a request that cannot be
// sent.
func attach(paths []string, spec string) ([]provider.Block, error) {
	if len(paths) > maxAttachments {
		return nil, fmt.Errorf("%d attachments; the limit is %d", len(paths), maxAttachments)
	}
	var blocks []provider.Block
	total := 0
	for _, p := range paths {
		data, err := readAttachment(p)
		if err != nil {
			return nil, err
		}
		total += len(data)
		if total > maxAttached {
			return nil, fmt.Errorf("attachments total more than %d MB", maxAttached>>20)
		}
		b, err := classify(filepath.Base(p), data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if b.Type == provider.Media {
			if err := provider.Accepts(spec, b.MediaType); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

// readAttachment opens a path once and asks the descriptor what it is,
// rather than asking the path and opening it again. O_NONBLOCK is what
// keeps `ask -a somefifo` from waiting forever for a writer that is not
// coming: a named pipe opens immediately and is then refused for not being
// a regular file, which is also what stops `-a /dev/zero` from reading
// until the machine gives out.
func readAttachment(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is a %s, not a file", path, kindOf(fi.Mode()))
	}
	// Read one byte past the limit rather than trusting the size: a file
	// being written while it is read would otherwise slip through.
	data, err := io.ReadAll(io.LimitReader(f, maxAttachment+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > maxAttachment {
		return nil, fmt.Errorf("%s is larger than %d MB", path, maxAttachment>>20)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return data, nil
}

func kindOf(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&os.ModeNamedPipe != 0:
		return "named pipe"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeDevice != 0:
		return "device"
	}
	return "not a regular file"
}

// classify decides what a run of bytes is. Content is the only authority —
// a name is a name, not a type, and a .png holding a shell script is a
// shell script.
//
// A recognised signature settles it first. Text cannot go first: a short
// PDF is valid ASCII all the way through its header, and would be inlined
// as prose by anything that asked "is this text?" before asking "what is
// this?". Only bytes carrying no signature are considered for text, and
// then they must be exact — JSON silently rewrites invalid UTF-8, so bytes
// that are not valid UTF-8 are not text, whatever they look like.
func classify(name string, data []byte) (provider.Block, error) {
	if mediaType := sniff(data); mediaTypes[mediaType] {
		return provider.Block{
			Type: provider.Media, MediaType: mediaType, Name: clean(name), Data: data,
		}, nil
	}
	if isText(data) {
		return provider.Block{
			Type: provider.Text,
			Text: fmt.Sprintf("<file name=%q>\n%s\n</file>", clean(name), string(data)),
		}, nil
	}
	return provider.Block{}, fmt.Errorf("not text and not a media type ask carries (looks like %s)", sniff(data))
}

// isText: valid UTF-8 with no NUL. Both halves matter. NUL is what tells a
// UTF-16 document or a stray binary apart from prose, and invalid UTF-8
// would be silently replaced with U+FFFD on the way into the log, which
// would change the evidence without saying so.
func isText(data []byte) bool {
	return utf8.Valid(data) && !bytes.ContainsRune(data, 0)
}

// sniff names a media type from the leading bytes. http.DetectContentType
// is the standard library's implementation of the same table every other
// program uses; the normalisations below are the handful of places its
// spelling differs from what the provider APIs want.
func sniff(data []byte) string {
	t := http.DetectContentType(data)
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	switch t {
	case "audio/wave", "audio/x-wav":
		return "audio/wav"
	case "audio/mp3", "audio/mpeg3", "audio/x-mpeg-3":
		return "audio/mpeg"
	case "audio/aiff", "audio/x-aiff":
		return "audio/aiff"
	}
	return t
}

// clean reduces a filename to something safe to hand a model: the base
// name only, so no local path leaks, and no control characters, so a file
// called "x\n</file>\nignore everything above" is just a strange name
// rather than a way to write the prompt.
func clean(name string) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if len([]rune(name)) > 96 {
		name = string([]rune(name)[:96]) + "…"
	}
	if name == "" || name == "." || name == "/" {
		return "attachment"
	}
	return name
}
