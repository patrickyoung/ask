package provider

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	aopt "github.com/anthropics/anthropic-sdk-go/option"
	openai "github.com/openai/openai-go/v3"
	oopt "github.com/openai/openai-go/v3/option"
)

// This file is the stream contract. Every adapter — current and future —
// must pass checkContract over a canned wire response. Adding a provider
// means adding a wireCase; the fixtures double as documentation of each
// provider's wire format.

// digest is what one successful stream adds up to.
type digest struct {
	deltaText   string // concatenated KindText deltas
	deltaReason string // concatenated KindReasoning deltas
	blocks      []Block
	usage       Usage
	stop        string
}

// checkContract consumes a stream to completion and enforces the Provider
// contract: exactly one KindUsage then one KindStop at the end, nothing
// after stop, streamed text identical to the text blocks (the user must
// see what the log records), and streamed reasoning surviving as a
// reasoning or opaque block (or replay is not faithful).
func checkContract(t *testing.T, s iter.Seq2[Chunk, error]) digest {
	t.Helper()
	var d digest
	var kinds []ChunkKind
	var text, reason, blockText strings.Builder
	reasonLogged := false
	stopped := false
	for c, err := range s {
		if stopped {
			t.Fatalf("chunk kind %d after KindStop; stop must be last", c.Kind)
		}
		if err != nil {
			t.Fatalf("stream failed: %v", err)
		}
		kinds = append(kinds, c.Kind)
		switch c.Kind {
		case KindText:
			text.WriteString(c.Text)
		case KindReasoning:
			reason.WriteString(c.Text)
		case KindBlock:
			if c.Block == nil {
				t.Fatal("KindBlock chunk with nil Block")
			}
			d.blocks = append(d.blocks, *c.Block)
			switch c.Block.Type {
			case Text:
				blockText.WriteString(c.Block.Text)
			case Reasoning, Opaque:
				reasonLogged = true
			}
		case KindUsage:
			if c.Usage == nil {
				t.Fatal("KindUsage chunk with nil Usage")
			}
			d.usage = *c.Usage
		case KindStop:
			d.stop = c.Stop
			stopped = true
		default:
			t.Errorf("unknown chunk kind %d", c.Kind)
		}
	}
	n := len(kinds)
	if n < 2 || kinds[n-1] != KindStop || kinds[n-2] != KindUsage {
		t.Fatalf("stream must end with KindUsage then KindStop, got %v", kinds)
	}
	usages := 0
	for _, k := range kinds {
		if k == KindUsage {
			usages++
		}
	}
	if usages != 1 {
		t.Errorf("want exactly one KindUsage, got %d", usages)
	}
	if text.String() != blockText.String() {
		t.Errorf("streamed text differs from logged text blocks:\n stream: %q\n blocks: %q", text.String(), blockText.String())
	}
	if reason.Len() > 0 && !reasonLogged {
		t.Errorf("reasoning streamed (%d bytes) but no reasoning or opaque block logged", reason.Len())
	}
	d.deltaText, d.deltaReason = text.String(), reason.String()
	return d
}

// contractReq is the one request every wire fixture answers: it exercises
// system and message conversion in each adapter's params path.
func contractReq() Request {
	return Request{
		Model:     "test-model",
		System:    "be terse",
		MaxTokens: 100,
		Messages:  []Message{{Role: User, Blocks: []Block{{Type: Text, Text: "hi"}}}},
	}
}

// Every fixture encodes the same turn — reasoning "pondering", text
// "Hello world", usage 10 in / 25 out — in its provider's wire format, so
// the assertions below stay uniform.

const anthropicWire = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":25}}

event: message_stop
data: {"type":"message_stop"}

`

const openaiWire = `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"test-model","output":[],"parallel_tool_calls":false,"tool_choice":"none","tools":[]}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","sequence_number":1,"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"pondering"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":1,"content_index":0,"delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":1,"content_index":0,"delta":" world"}

event: response.completed
data: {"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"test-model","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"pondering"}],"encrypted_content":"enc-1"},{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}],"parallel_tool_calls":false,"tool_choice":"none","tools":[],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens":25,"output_tokens_details":{"reasoning_tokens":5},"total_tokens":35}}}

`

const geminiWire = `data:{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"pondering","thought":true,"thoughtSignature":"c2lnLTE="}]}}]}

data:{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hello"}]}}]}

data:{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":" world"}]}}]}

data:{"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":25,"thoughtsTokenCount":5,"cachedContentTokenCount":3}}

`

const openrouterWire = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"pond","reasoning_details":[{"type":"reasoning.text","text":"pond","index":0}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"reasoning":"ering","reasoning_details":[{"type":"reasoning.text","text":"ering","index":0}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":25,"completion_tokens_details":{"reasoning_tokens":5},"prompt_tokens_details":{"cached_tokens":3},"cost":0.0004}}

data: [DONE]

`

var wireCases = []struct {
	name string
	wire string
	make func(url string) Provider
}{
	{
		name: "anthropic",
		wire: anthropicWire,
		make: func(u string) Provider {
			return &Anthropic{c: anthropic.NewClient(aopt.WithAPIKey("k"), aopt.WithBaseURL(u), aopt.WithMaxRetries(0))}
		},
	},
	{
		name: "openai",
		wire: openaiWire,
		make: func(u string) Provider {
			return &OpenAI{c: openai.NewClient(oopt.WithAPIKey("k"), oopt.WithBaseURL(u), oopt.WithMaxRetries(0))}
		},
	},
	{
		name: "gemini",
		wire: geminiWire,
		make: func(u string) Provider { return &Gemini{key: "k", base: u} },
	},
	{
		name: "openrouter",
		wire: openrouterWire,
		make: func(u string) Provider {
			return &OpenRouter{c: openai.NewClient(oopt.WithAPIKey("k"), oopt.WithBaseURL(u), oopt.WithMaxRetries(0))}
		},
	},
	{
		name: "replay",
		make: func(string) Provider {
			return &Replay{Turns: []ReplayTurn{{
				Blocks: []Block{
					{Type: Reasoning, Text: "pondering"},
					{Type: Text, Text: "Hello world"},
				},
				Usage: Usage{In: 10, Out: 25},
				Stop:  "end",
			}}}
		},
	},
}

// serve returns a server that answers every request with body.
func serve(t *testing.T, status int, header http.Header, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sseHeader() http.Header {
	return http.Header{"Content-Type": []string{"text/event-stream"}}
}

func TestStreamContract(t *testing.T) {
	for _, c := range wireCases {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, 200, sseHeader(), c.wire)
			p := c.make(srv.URL)
			d := checkContract(t, p.Stream(context.Background(), contractReq()))
			if d.deltaText != "Hello world" {
				t.Errorf("text deltas = %q, want %q", d.deltaText, "Hello world")
			}
			if d.deltaReason != "pondering" {
				t.Errorf("reasoning deltas = %q, want %q", d.deltaReason, "pondering")
			}
			if d.usage.In != 10 || d.usage.Out != 25 {
				t.Errorf("usage = %+v, want in 10 / out 25", d.usage)
			}
			if d.stop != "end" {
				t.Errorf("stop = %q, want %q", d.stop, "end")
			}
		})
	}
}

// TestReplayableReasoning pins the property the whole log rests on: every
// adapter that streams reasoning also logs something that can be sent back
// to it. A signature on a reasoning block or an opaque provider block both
// count; nothing at all does not, because the next turn would then arrive
// with reasoning the provider cannot verify.
func TestReplayableReasoning(t *testing.T) {
	for _, c := range wireCases {
		if c.name == "replay" {
			continue // the test double has no wire form to preserve
		}
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, 200, sseHeader(), c.wire)
			d := checkContract(t, c.make(srv.URL).Stream(context.Background(), contractReq()))
			for _, b := range d.blocks {
				if b.Type == Opaque && len(b.Raw) > 0 && b.Provider == c.name {
					return
				}
				if b.Type == Reasoning && b.Signature != "" && b.Provider == c.name {
					return
				}
			}
			t.Errorf("no replayable reasoning state logged; blocks = %+v", d.blocks)
		})
	}
}

// TestStreamErrorTerminal: an HTTP failure yields exactly one (Chunk{}, err)
// carrying a retryable *Error, and nothing after it.
func TestStreamErrorTerminal(t *testing.T) {
	body := `{"type":"error","error":{"type":"rate_limit_error","code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`
	hdr := http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"7"}}
	for _, c := range wireCases {
		if c.name == "replay" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, 429, hdr, body)
			p := c.make(srv.URL)
			yields := 0
			var got error
			for chunk, err := range p.Stream(context.Background(), contractReq()) {
				yields++
				got = err
				if err == nil {
					t.Errorf("expected an error yield, got chunk kind %d", chunk.Kind)
				}
			}
			if yields != 1 {
				t.Fatalf("want exactly one yield, got %d", yields)
			}
			var pe *Error
			if !errors.As(got, &pe) {
				t.Fatalf("error is %T (%v), want *provider.Error", got, got)
			}
			if pe.Status != 429 || !pe.Retryable() {
				t.Errorf("error = status %d retryable %v, want 429/true", pe.Status, pe.Retryable())
			}
		})
	}
}

// TestStreamEarlyBreak: a consumer that stops early must terminate the
// iterator cleanly. A yield after break panics under range-over-func, so
// finishing this test at all is the assertion.
func TestStreamEarlyBreak(t *testing.T) {
	for _, c := range wireCases {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, 200, sseHeader(), c.wire)
			p := c.make(srv.URL)
			saw := false
			for range p.Stream(context.Background(), contractReq()) {
				saw = true
				break
			}
			if !saw {
				t.Fatal("stream yielded nothing")
			}
		})
	}
}

// TestLiveContract runs the same contract against the real APIs. Opt in
// with ASK_LIVE=1; providers without keys are skipped.
func TestLiveContract(t *testing.T) {
	if os.Getenv("ASK_LIVE") == "" {
		t.Skip("set ASK_LIVE=1 to run live contract checks")
	}
	specs := []string{
		"anthropic/claude-sonnet-4-5",
		"openai/gpt-5-mini",
		"gemini/gemini-2.5-flash",
		"openrouter/anthropic/claude-sonnet-4.5",
	}
	for _, spec := range specs {
		name, _, _ := strings.Cut(spec, "/")
		t.Run(name, func(t *testing.T) {
			p, model, err := New(spec)
			if err != nil {
				t.Skip(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			req := Request{
				Model:     model,
				MaxTokens: 2048,
				Messages:  []Message{{Role: User, Blocks: []Block{{Type: Text, Text: "Reply with the single word: hello"}}}},
			}
			d := checkContract(t, p.Stream(ctx, req))
			if d.deltaText == "" {
				t.Error("live stream produced no text")
			}
		})
	}
}
