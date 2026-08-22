package event

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/patrickyoung/ask/internal/provider"
)

// Fold projects context-bearing events into the conversation a provider
// sees. It is a pure function of the log: user events become user
// messages, non-partial assistant turns become assistant messages, and
// nothing else contributes. This function is the whole reason a session
// file is enough to reconstruct a conversation exactly.
func Fold(events []Event) ([]provider.Message, error) {
	var msgs []provider.Message
	for _, e := range events {
		switch e.Type {
		case User:
			u, err := As[UserData](e)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, provider.Message{Role: provider.User, Blocks: u.Content()})
		case Assistant:
			t, err := As[Turn](e)
			if err != nil {
				return nil, err
			}
			if t.Partial {
				continue
			}
			msgs = append(msgs, provider.Message{Role: provider.Assistant, Blocks: t.Blocks})
		}
	}
	return msgs, nil
}

// Check verifies the replay invariant across a log: sequence numbers are
// contiguous, every request fold reproduces the conversation used, and every
// structured program record has a valid prefix seal. This is what
// "perfectly replayable" means here, made testable — old logs still verify
// against current code.
//
// A request event records its messages as a digest (see provider.Logged),
// so the check is a hash comparison. Logs written before the digest carry
// the messages themselves and are compared structurally; both forms are
// the same assertion, and neither is ever weakened. A request that records
// neither is an error, so a lost field cannot make Check vacuously pass.
func Check(events []Event) error {
	// Real session logs have numbered events beginning at one. Keep accepting
	// old in-memory callers whose synthetic fixtures predate sequence numbers,
	// but once a log opts into numbering it must be contiguous.
	if len(events) > 0 && events[0].Seq > 0 {
		for i, e := range events {
			if want := i + 1; e.Seq != want {
				return fmt.Errorf("event sequence divergence at index %d: got seq %d, want %d", i, e.Seq, want)
			}
		}
	}
	for i, e := range events {
		if e.Type == Note {
			n, err := As[NoteData](e)
			if err != nil {
				return err
			}
			if n.Kind != "" {
				if len(n.Body) == 0 || !json.Valid(n.Body) {
					return fmt.Errorf("structured note seq %d has no valid JSON body", e.Seq)
				}
				if i+1 >= len(events) || events[i+1].Type != Seal {
					return fmt.Errorf("structured note seq %d is not immediately sealed", e.Seq)
				}
			}
		}
		if e.Type == Seal {
			s, err := As[SealData](e)
			if err != nil {
				return err
			}
			wantThrough := 0
			if i > 0 {
				wantThrough = events[i-1].Seq
			}
			if s.Through != wantThrough {
				return fmt.Errorf("seal seq %d names prefix through %d, want %d", e.Seq, s.Through, wantThrough)
			}
			got, err := PrefixDigest(events[:i])
			if err != nil {
				return err
			}
			if got != s.SHA256 {
				return fmt.Errorf("seal divergence at seq %d:\ncomputed: %s\nlogged:   %s", e.Seq, got, s.SHA256)
			}
		}
		if e.Type != Request {
			continue
		}
		req, err := As[provider.Request](e)
		if err != nil {
			return err
		}
		folded, err := Fold(events[:i])
		if err != nil {
			return err
		}
		switch {
		case req.Digest != "":
			if got := provider.Digest(folded); got != req.Digest {
				return fmt.Errorf("replay divergence at request seq %d:\nfolded:  %s\nlogged:  %s", e.Seq, got, req.Digest)
			}
		case req.Messages != nil:
			want, err := json.Marshal(req.Messages)
			if err != nil {
				return err
			}
			got, err := json.Marshal(folded)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, want) {
				return fmt.Errorf("replay divergence at request seq %d:\nfolded:  %s\nlogged:  %s", e.Seq, got, want)
			}
		default:
			return fmt.Errorf("request seq %d records neither messages nor a digest; nothing to replay against", e.Seq)
		}
	}
	return nil
}
