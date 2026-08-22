package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/patrickyoung/ask/internal/event"
	"github.com/patrickyoung/ask/internal/provider"
)

// renderer turns the event stream into terminal progress on stderr. It is
// one of two views over the same events — the other is raw JSONL — and
// never a second logging pipeline.
type renderer struct {
	w      io.Writer
	color  bool
	inText bool // mid-stream of assistant text/reasoning deltas
	usage  provider.Usage
	start  time.Time
}

func newRenderer(w io.Writer) *renderer {
	color := false
	if f, ok := w.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == "" {
			color = true
		}
	}
	return &renderer{w: w, color: color, start: time.Now()}
}

func (r *renderer) dim(s string) string {
	if !r.color {
		return s
	}
	return "\033[2m" + s + "\033[0m"
}

// delta streams model output as it generates. Reasoning renders dim, so a
// watching human can tell thinking from answer at a glance.
func (r *renderer) delta(kind provider.ChunkKind, text string) {
	if kind == provider.KindReasoning {
		text = r.dim(text)
	}
	fmt.Fprint(r.w, text)
	r.inText = true
}

func (r *renderer) line(s string) {
	if r.inText {
		fmt.Fprintln(r.w)
		r.inText = false
	}
	fmt.Fprintln(r.w, s)
}

func (r *renderer) event(e event.Event) {
	switch e.Type {
	case event.User:
		u, _ := event.As[event.UserData](e)
		r.line(r.dim("» " + firstLine(u.Text, 120)))
	case event.Assistant:
		t, _ := event.As[event.Turn](e)
		r.usage.Add(t.Usage)
		if r.inText {
			fmt.Fprintln(r.w)
			r.inText = false
		}
		if t.Stop == "max_tokens" {
			r.line(r.dim("ask: answer was cut off at the output limit (raise -max-tokens)"))
		}
	case event.Retry:
		d, _ := event.As[event.RetryData](e)
		// Status 0 means the request never reached a response, and "http 0"
		// tells nobody anything — name the failure instead.
		why := fmt.Sprintf("http %d", d.Status)
		if d.Status == 0 {
			why = firstLine(d.Error, 60)
		}
		r.line(r.dim(fmt.Sprintf("ask: retry %d in %.0fs (%s)", d.Attempt, float64(d.WaitMS)/1000, why)))
	case event.Done:
		d, _ := event.As[event.DoneData](e)
		// Cost only when the provider actually reported one: an omitted
		// price must not render as "$0.00", which reads as free.
		cost := ""
		if r.usage.Cost > 0 {
			cost = fmt.Sprintf(" · $%.4f", r.usage.Cost)
		}
		note := ""
		if d.Reason != "end" {
			note = d.Reason + " · "
		}
		r.line(r.dim(fmt.Sprintf("ask: %s%s in / %s out / %s reasoning · %s%s",
			note, k(r.usage.In), k(r.usage.Out), k(r.usage.Reasoning),
			time.Since(r.start).Round(time.Millisecond), cost)))
	case event.Note:
		n, _ := event.As[event.NoteData](e)
		// Named, always. A note renders as its source and its first line
		// because the source is the part a reader needs: this is the one
		// kind of text in a session that nobody typed.
		label, content := n.Source, n.Text
		if n.Kind != "" {
			label, content = n.Source+":"+n.Kind, string(n.Body)
		}
		r.line(r.dim("[" + label + "] " + firstLine(content, 120)))
	case event.Seal:
		s, _ := event.As[event.SealData](e)
		r.line(r.dim(fmt.Sprintf("[ask:seal] through %d · %s", s.Through, firstLine(s.SHA256, 24))))
	case event.Abort:
		r.line(r.dim("ask: interrupted"))
	}
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

func k(n int) string {
	if n >= 10000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
