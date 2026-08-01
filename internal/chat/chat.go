// Package chat turns one message into one answer.
//
// The whole loop is four steps: fold the log into a conversation, log the
// request, stream the turn, log it. There is no tool loop and no agent
// loop — the model answers, and the answer is the product. What makes the
// session worth keeping is that every step is a line in an append-only
// file, and the request line proves the fold that produced it.
package chat

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"ask/internal/event"
	"ask/internal/provider"
)

// ErrOverflow is terminal and permanent: the conversation no longer fits
// the model's context window, so the same request will fail identically
// forever. It is not an error to retry — it is an outcome, and the caller
// turns it into an exit code a shell can branch on.
var ErrOverflow = errors.New("context window is full")

const maxAttempts = 6 // per-turn provider retries

type Chat struct {
	Provider  provider.Provider
	Model     string // bare model name, as the provider wants it
	System    string
	MaxTokens int
	Effort    string

	Log    *event.Log
	events []event.Event

	// OnDelta streams text as it generates. It is a display hook — the log
	// is the record.
	OnDelta func(kind provider.ChunkKind, text string)
}

// Load seeds the in-memory view from an existing session.
func (c *Chat) Load(events []event.Event) { c.events = events }

// Turns reports how many answers this session already holds.
func (c *Chat) Turns() int {
	n := 0
	for _, e := range c.events {
		if e.Type == event.Assistant {
			n++
		}
	}
	return n
}

// Say appends a message, asks the model, and returns the answer. The
// message is an ordered list of blocks: attachments as they were named,
// then the text.
func (c *Chat) Say(ctx context.Context, content []provider.Block) (string, error) {
	if _, err := c.append(event.User, userData(content)); err != nil {
		return "", err
	}
	msgs, err := event.Fold(c.events)
	if err != nil {
		return "", err
	}
	req := provider.Request{
		Model:     c.Model,
		System:    c.System,
		Messages:  msgs,
		MaxTokens: c.MaxTokens,
		Effort:    c.Effort,
		// The log's own id is the conversation's name, which is how
		// providers that pin a cache by session find the warm one.
		Session: c.Log.ID(),
	}
	if _, err := c.append(event.Request, req.Logged()); err != nil {
		return "", err
	}
	turn, err := c.stream(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			// Mark the abort even when nothing streamed, so an interrupted
			// session is distinguishable from a crash. A partial turn is
			// logged as partial: Fold skips it, so continuing this session
			// asks the same question again rather than pretending half an
			// answer was the answer.
			if len(turn.Blocks) > 0 {
				turn.Partial = true
				c.append(event.Assistant, turn)
			}
			c.append(event.Abort, struct{}{})
			c.Log.Sync()
			return "", ctx.Err()
		}
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Overflow() {
			return "", fmt.Errorf("%w: %s", ErrOverflow, pe.Msg)
		}
		return "", err
	}
	if _, err := c.append(event.Assistant, turn); err != nil {
		return "", err
	}
	if err := c.Log.Sync(); err != nil {
		return "", err
	}
	return answer(turn.Blocks), nil
}

// userData picks how a message is recorded. A message that is only text —
// which is nearly all of them — is written as one JSON string, so a
// session log stays something grep and jq can read. Anything richer is
// written as its blocks, in order.
func userData(content []provider.Block) event.UserData {
	if len(content) == 1 && content[0].Type == provider.Text {
		return event.UserData{Text: content[0].Text}
	}
	return event.UserData{Blocks: content}
}

func (c *Chat) append(t event.Type, data any) (event.Event, error) {
	e, err := c.Log.Append(t, data)
	if err != nil {
		return e, err
	}
	c.events = append(c.events, e)
	return e, nil
}

// stream runs one turn, retrying the failures worth retrying. Every wait
// is logged: a slow answer should say why it was slow.
func (c *Chat) stream(ctx context.Context, req provider.Request) (event.Turn, error) {
	for attempt := 1; ; attempt++ {
		turn, err := c.once(ctx, req)
		if err == nil || ctx.Err() != nil {
			return turn, err
		}
		var pe *provider.Error
		if !errors.As(err, &pe) || !pe.Retryable() || attempt >= maxAttempts {
			return turn, err
		}
		// A server-sent Retry-After is an instruction, not a suggestion:
		// clamping it to the backoff cap burns an attempt into a guaranteed
		// 429. Honor it up to a sanity bound; only the computed backoff
		// stays under a minute.
		wait := min(pe.RetryAfter, 5*time.Minute)
		if wait <= 0 {
			wait = time.Duration(float64(time.Second) * float64(int(1)<<attempt) * (0.5 + rand.Float64()))
			wait = min(wait, time.Minute)
		}
		c.append(event.Retry, event.RetryData{
			Attempt: attempt, Status: pe.Status, WaitMS: wait.Milliseconds(), Error: pe.Msg,
		})
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return turn, ctx.Err()
		}
	}
}

// once streams a single turn. The returned Turn is complete on nil error
// and best-effort partial otherwise (deltas only — a torn stream has no
// trustworthy blocks).
func (c *Chat) once(ctx context.Context, req provider.Request) (event.Turn, error) {
	start := time.Now()
	turn := event.Turn{Model: req.Model}
	var texts, reasons strings.Builder
	partial := func() event.Turn {
		p := event.Turn{Model: req.Model, MS: time.Since(start).Milliseconds()}
		if reasons.Len() > 0 {
			p.Blocks = append(p.Blocks, provider.Block{Type: provider.Reasoning, Text: reasons.String()})
		}
		if texts.Len() > 0 {
			p.Blocks = append(p.Blocks, provider.Block{Type: provider.Text, Text: texts.String()})
		}
		return p
	}
	for chunk, err := range c.Provider.Stream(ctx, req) {
		if err != nil {
			return partial(), err
		}
		switch chunk.Kind {
		case provider.KindText:
			texts.WriteString(chunk.Text)
		case provider.KindReasoning:
			reasons.WriteString(chunk.Text)
		case provider.KindBlock:
			turn.Blocks = append(turn.Blocks, *chunk.Block)
		case provider.KindUsage:
			turn.Usage = *chunk.Usage
		case provider.KindStop:
			turn.Stop = chunk.Stop
		}
		if chunk.Kind == provider.KindText || chunk.Kind == provider.KindReasoning {
			if c.OnDelta != nil {
				c.OnDelta(chunk.Kind, chunk.Text)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return partial(), err
	}
	turn.MS = time.Since(start).Milliseconds()
	return turn, nil
}

// answer is the turn's text: what goes to stdout. Reasoning is not part of
// it — it streamed to stderr for the human and stays in the log for the
// record, but a filter's output is the answer alone.
func answer(blocks []provider.Block) string {
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == provider.Text {
			b.WriteString(bl.Text)
		}
	}
	return strings.TrimSpace(b.String())
}
