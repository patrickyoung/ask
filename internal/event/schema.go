// Package event defines ask's append-only session log.
//
// A session is one JSONL file of events. Context-bearing events determine
// what the model sees: Fold projects them into the conversation. Everything
// else is telemetry and is ignored by Fold. Each LLM call is preceded by a
// request event holding the exact normalized request, with its conversation
// recorded as a digest rather than a second copy, which makes replay
// self-verifying: Fold over the events before a request must reproduce it
// (see Check).
package event

import (
	"encoding/json"
	"fmt"
	"time"

	"ask/internal/provider"
)

type Type string

// Context-bearing event types. User and Assistant are what Fold projects
// into model context; Session, Request, and Done are the interpretive
// frame around them — header, checkpoint, outcome.
const (
	Session   Type = "session"   // Header: one per file, first line
	User      Type = "user"      // UserData
	Request   Type = "request"   // provider.Request, messages as a digest (see provider.Logged)
	Assistant Type = "assistant" // Turn
	Done      Type = "done"      // DoneData: terminal
)

// Telemetry event types, ignored by Fold.
const (
	Retry Type = "retry" // RetryData
	Abort Type = "abort" // no payload: interrupted, and the log stays usable
)

type Event struct {
	Seq  int             `json:"seq"`
	Time time.Time       `json:"time"`
	Type Type            `json:"type"`
	Data json.RawMessage `json:"data"`
}

// As decodes an event's payload.
func As[T any](e Event) (T, error) {
	var v T
	if err := json.Unmarshal(e.Data, &v); err != nil {
		return v, fmt.Errorf("event %d (%s): %w", e.Seq, e.Type, err)
	}
	return v, nil
}

// Header is the session event payload: everything needed to interpret the
// rest of the file, recorded verbatim at creation time. The system prompt
// is not part of the conversation Fold produces, so the log keeps it here
// as well as on every request — a session says what shaped it.
type Header struct {
	ID      string            `json:"id"`
	Version string            `json:"ask"`
	Go      string            `json:"go,omitempty"`
	SDKs    map[string]string `json:"sdks,omitempty"`
	Model   string            `json:"model"`
	System  string            `json:"system"`
}

// UserData is one message from the user. Blocks carries it in the order it
// was given — attachments in the order they were named on the command
// line, then the text — because that order is meaningful and only the
// message itself knows it.
//
// Text is what a message used to be, and is still what a text-only message
// is: the common case stays one readable JSON string, so a session log
// remains something grep and jq can work with. Fold reads Blocks when they
// are present and falls back to Text, which is also how logs written
// before attachments existed keep replaying.
type UserData struct {
	Text   string           `json:"text,omitempty"`
	Blocks []provider.Block `json:"blocks,omitempty"`
}

// Content is the message as the provider sees it. Exactly one of the two
// representations is authoritative, and Blocks wins when it is present, so
// there is never a question of which was sent.
func (u UserData) Content() []provider.Block {
	if len(u.Blocks) > 0 {
		return u.Blocks
	}
	return []provider.Block{{Type: provider.Text, Text: u.Text}}
}

// Turn is one assistant response. Blocks are logged exactly as streamed,
// including reasoning signatures and opaque provider state, so they can be
// replayed. Partial turns (aborted mid-stream) are excluded from Fold.
type Turn struct {
	Blocks  []provider.Block `json:"blocks"`
	Usage   provider.Usage   `json:"usage"`
	Model   string           `json:"model"`
	Stop    string           `json:"stop,omitempty"`
	Partial bool             `json:"partial,omitempty"`
	MS      int64            `json:"ms"`
}

// DoneData ends a turn. Reason is end|max_tokens|overflow|error: what
// stopped it, in the log rather than only in the exit code, so a session
// read later says how it finished.
type DoneData struct {
	Reason string `json:"reason"`
	Error  string `json:"error,omitempty"`
}

// RetryData records a provider failure that was worth waiting out. Status
// 0 means the request never reached an HTTP response — a reset, a DNS
// failure, a dropped connection — and for those the status alone says
// nothing, so the error travels with it. Without that, a run that stalled
// for minutes reports six identical zeroes and the log cannot answer the
// only question being asked of it.
type RetryData struct {
	Attempt int    `json:"attempt"`
	Status  int    `json:"status"`
	WaitMS  int64  `json:"wait_ms"`
	Error   string `json:"error,omitempty"`
}
