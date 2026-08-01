package chat

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"

	"ask/internal/event"
	"ask/internal/provider"
)

// newChat wires a chat over a scripted provider and a real log in a temp
// directory, so every test exercises the same path a run does.
func newChat(t *testing.T, turns ...provider.ReplayTurn) (*Chat, *provider.Replay, *event.Log) {
	t.Helper()
	log, err := event.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	p := &provider.Replay{Turns: turns}
	return &Chat{Provider: p, Model: "m", System: "be terse", MaxTokens: 100, Log: log}, p, log
}

func textTurn(s string) provider.ReplayTurn {
	return provider.ReplayTurn{
		Blocks: []provider.Block{{Type: provider.Text, Text: s}},
		Usage:  provider.Usage{In: 10, Out: 5},
		Stop:   "end",
	}
}

func events(t *testing.T, log *event.Log) []event.Event {
	t.Helper()
	if err := log.Sync(); err != nil {
		t.Fatal(err)
	}
	got, err := event.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestSayLogsTheWholeTurn: one ask writes user, request, assistant — in
// that order — and the log verifies.
func TestSayLogsTheWholeTurn(t *testing.T) {
	c, p, log := newChat(t, textTurn("  hello  "))
	answer, err := c.Say(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "hello" {
		t.Errorf("answer = %q, want %q (trimmed)", answer, "hello")
	}

	got := events(t, log)
	want := []event.Type{event.User, event.Request, event.Assistant}
	if len(got) != len(want) {
		t.Fatalf("logged %d events, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Type != w {
			t.Errorf("event %d = %s, want %s", i, got[i].Type, w)
		}
	}
	if err := event.Check(got); err != nil {
		t.Errorf("log does not replay: %v", err)
	}

	// The request is logged before the call, and carries what was sent.
	if len(p.Requests) != 1 {
		t.Fatalf("made %d provider calls, want 1", len(p.Requests))
	}
	if p.Requests[0].System != "be terse" || p.Requests[0].MaxTokens != 100 {
		t.Errorf("request lost a field: %+v", p.Requests[0])
	}
	if p.Requests[0].Session != log.ID() {
		t.Errorf("session = %q, want the log id %q", p.Requests[0].Session, log.ID())
	}
}

// TestConversationAccumulates: the second ask sees the first exchange, and
// the log still verifies — which is the whole point of the fold.
func TestConversationAccumulates(t *testing.T) {
	c, p, log := newChat(t, textTurn("four"), textTurn("five"))
	if _, err := c.Say(context.Background(), "2+2?"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Say(context.Background(), "and one more?"); err != nil {
		t.Fatal(err)
	}
	second := p.Requests[1].Messages
	if len(second) != 3 {
		t.Fatalf("second call sent %d messages, want 3", len(second))
	}
	if second[0].Blocks[0].Text != "2+2?" || second[1].Blocks[0].Text != "four" {
		t.Errorf("history not replayed: %+v", second)
	}
	if err := event.Check(events(t, log)); err != nil {
		t.Errorf("multi-turn log does not replay: %v", err)
	}
	if c.Turns() != 2 {
		t.Errorf("Turns() = %d, want 2", c.Turns())
	}
}

// TestLoadContinuesAnExistingSession: continuing reads the log back and
// picks up the conversation, which is what `ask` does by default.
func TestLoadContinuesAnExistingSession(t *testing.T) {
	c, _, log := newChat(t, textTurn("first"))
	if _, err := c.Say(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	path := log.Path()
	log.Close()

	reopened, prev, err := event.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	c2 := &Chat{Provider: &provider.Replay{Turns: []provider.ReplayTurn{textTurn("second")}}, Model: "m", Log: reopened}
	c2.Load(prev)
	if c2.Turns() != 1 {
		t.Errorf("Turns() after load = %d, want 1", c2.Turns())
	}
	if _, err := c2.Say(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	all, err := event.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := event.Check(all); err != nil {
		t.Errorf("continued log does not replay: %v", err)
	}
	if n := len(all); n != 6 {
		t.Errorf("continued log has %d events, want 6", n)
	}
}

// TestRetryIsLoggedThenSucceeds: a retryable failure is recorded as a
// retry event, so a slow answer says why it was slow.
func TestRetryIsLoggedThenSucceeds(t *testing.T) {
	c, _, log := newChat(t,
		provider.ReplayTurn{Err: &provider.Error{Status: 429, Msg: "slow down"}},
		textTurn("ok"),
	)
	answer, err := c.Say(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "ok" {
		t.Errorf("answer = %q", answer)
	}
	var retries int
	for _, e := range events(t, log) {
		if e.Type == event.Retry {
			retries++
			d, err := event.As[event.RetryData](e)
			if err != nil {
				t.Fatal(err)
			}
			if d.Status != 429 || d.WaitMS <= 0 {
				t.Errorf("retry event = %+v", d)
			}
		}
	}
	if retries != 1 {
		t.Errorf("logged %d retry events, want 1", retries)
	}
}

// TestPermanentErrorIsNotRetried: a 400 is not worth a second call, and
// the caller sees the provider's own words.
func TestPermanentErrorIsNotRetried(t *testing.T) {
	c, p, _ := newChat(t, provider.ReplayTurn{Err: &provider.Error{Status: 400, Msg: "bad model"}})
	if _, err := c.Say(context.Background(), "hi"); err == nil {
		t.Fatal("permanent error was swallowed")
	} else if !strings.Contains(err.Error(), "bad model") {
		t.Errorf("error = %v, want the provider's words", err)
	}
	if len(p.Requests) != 1 {
		t.Errorf("made %d calls for a permanent failure, want 1", len(p.Requests))
	}
}

// TestOverflowIsNamed: a full context window is an outcome a shell can
// branch on, not one more error string.
func TestOverflowIsNamed(t *testing.T) {
	c, _, _ := newChat(t, provider.ReplayTurn{
		Err: &provider.Error{Status: 400, Msg: "prompt is too long: 300000 tokens > 200000 maximum"},
	})
	_, err := c.Say(context.Background(), "hi")
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("error = %v, want ErrOverflow", err)
	}
}

// TestCancelLogsPartialAndAbort: ^C mid-stream must leave a session that
// still folds — the partial turn is recorded for the record but excluded
// from context, so continuing asks the question again rather than
// pretending half an answer was the answer.
func TestCancelLogsPartialAndAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, _, log := newChat(t)
	c.Provider = cancelling{cancel: cancel}
	if _, err := c.Say(ctx, "hi"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	got := events(t, log)
	var sawPartial, sawAbort bool
	for _, e := range got {
		switch e.Type {
		case event.Assistant:
			turn, err := event.As[event.Turn](e)
			if err != nil {
				t.Fatal(err)
			}
			sawPartial = turn.Partial
		case event.Abort:
			sawAbort = true
		}
	}
	if !sawPartial {
		t.Error("interrupted turn was not marked partial")
	}
	if !sawAbort {
		t.Error("no abort event logged")
	}
	msgs, err := event.Fold(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Role != provider.User {
		t.Errorf("fold after abort = %+v, want just the unanswered question", msgs)
	}
	if err := event.Check(got); err != nil {
		t.Errorf("aborted log does not replay: %v", err)
	}
}

// cancelling streams one delta, then cancels the context mid-stream.
type cancelling struct{ cancel context.CancelFunc }

func (c cancelling) Stream(ctx context.Context, req provider.Request) iter.Seq2[provider.Chunk, error] {
	return func(yield func(provider.Chunk, error) bool) {
		if !yield(provider.Chunk{Kind: provider.KindText, Text: "half an ans"}, nil) {
			return
		}
		c.cancel()
		yield(provider.Chunk{}, ctx.Err())
	}
}

// TestAnswerExcludesReasoning: stdout carries the answer, never the
// thinking — a filter's output is the product alone. The reasoning still
// has to be in the log.
func TestAnswerExcludesReasoning(t *testing.T) {
	c, _, log := newChat(t, provider.ReplayTurn{
		Blocks: []provider.Block{
			{Type: provider.Reasoning, Text: "let me think", Signature: "sig", Provider: "anthropic"},
			{Type: provider.Text, Text: "42"},
		},
		Stop: "end",
	})
	answer, err := c.Say(context.Background(), "the question?")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "42" {
		t.Errorf("answer = %q, want %q", answer, "42")
	}
	raw, _ := json.Marshal(events(t, log))
	if !strings.Contains(string(raw), "let me think") {
		t.Error("reasoning was dropped from the log; it must survive for replay")
	}
}

// TestDeltasReachTheDisplay: what streams to the human is what lands in
// the log, which is the contract every adapter is held to.
func TestDeltasReachTheDisplay(t *testing.T) {
	c, _, _ := newChat(t, textTurn("streamed"))
	var seen strings.Builder
	c.OnDelta = func(kind provider.ChunkKind, text string) {
		if kind == provider.KindText {
			seen.WriteString(text)
		}
	}
	answer, err := c.Say(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if seen.String() != answer {
		t.Errorf("streamed %q but answered %q", seen.String(), answer)
	}
}
