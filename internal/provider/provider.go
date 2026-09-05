// Package provider normalizes LLM backends behind one streaming interface.
//
// A conversation is []Message; each Message is a list of typed Blocks.
// Adapters map these to provider wire formats, preserving reasoning
// signatures and provider-specific state (as opaque blocks) so a logged
// conversation replays faithfully to the same provider and degrades
// gracefully when the model is switched mid-session.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"strings"
	"time"
)

// Provider streams one model turn.
//
// The iterator yields chunks in stream order: KindText and KindReasoning
// deltas for display, a KindBlock for each completed content block, then
// exactly one KindUsage followed by exactly one KindStop, which is final.
// The concatenated text deltas equal the Text blocks — what streamed is
// what logs — and streamed reasoning survives as a Reasoning or Opaque
// block. On error it yields one (Chunk{}, err) and stops. Implementations
// must honor ctx cancellation and release the underlying stream when the
// consumer breaks early. checkContract in contract_test.go enforces all of
// this; a new adapter earns its keep by passing it over a wire fixture.
type Provider interface {
	Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error]
}

// Request is the normalized form of one LLM call. It is logged as a
// request event before the call is made — everything except Messages
// verbatim, and Messages as Digest, because the conversation is derived:
// Fold produces it from the events before the call, so the log records the
// proof rather than a second copy of what it already holds. See Logged and
// event.Check.
type Request struct {
	Model    string    `json:"model"`
	System   string    `json:"system,omitempty"`
	Messages []Message `json:"messages,omitempty"`

	// Digest stands in for Messages in the log and is never sent: adapters
	// read the fields they need by name, so it is inert on the wire.
	Digest string `json:"digest,omitempty"`

	MaxTokens int             `json:"max_tokens,omitempty"`
	Effort    string          `json:"effort,omitempty"` // reasoning effort: "", "off", "low", "medium", "high"
	Schema    json.RawMessage `json:"schema,omitempty"` // native structured output; never a prompt instruction

	// Session names the conversation this call belongs to, for providers
	// that route by it. It is not model input — nothing about the answer
	// depends on it — and it is not logged: it is always the id of the log
	// the request event is being written to, so recording it would repeat
	// the file's own name on every turn. Adapters that have no use for it
	// ignore it.
	Session string `json:"-"`
}

// schemaObject supplies the SDK's map shape without decoding its values.
// RawMessage preserves numeric tokens, including integers beyond float64's
// precision, and is a JSON marshaler understood by the provider SDKs.
func schemaObject(raw json.RawMessage) map[string]any {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object) // loadSchema admitted one JSON object
	out := make(map[string]any, len(object))
	for key, value := range object {
		out[key] = value
	}
	return out
}

// Logged returns the form of r that enters the session log: the messages
// replaced by their digest. A request event that carried the whole
// conversation would store the history once per turn, growing with the
// square of the turns. The digest proves the same thing in 64 bytes.
func (r Request) Logged() Request {
	r.Digest = Digest(r.Messages)
	r.Messages = nil
	return r
}

// Digest content-addresses a conversation: what the log stores in place of
// the messages, and what event.Check recomputes from a fold to prove the
// two agree. It is empty only when the messages cannot be marshalled at
// all, which the log rejects anyway — and Check treats an empty digest as
// a failure rather than a match.
func Digest(msgs []Message) string {
	b, err := json.Marshal(msgs)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

type BlockType string

const (
	Text      BlockType = "text"
	Reasoning BlockType = "reasoning"
	Opaque    BlockType = "opaque"
	Media     BlockType = "media" // an image, a document, a recording
)

// Block is one unit of message content. It is a flat union: which fields
// are set depends on Type. Flat beats an interface hierarchy here — it
// round-trips through JSON without custom unmarshalers.
type Block struct {
	Type BlockType `json:"type"`

	// Text carries text and reasoning content.
	Text string `json:"text,omitempty"`

	// Signature is the provider's opaque proof for a reasoning block.
	Signature string `json:"signature,omitempty"`

	// Media: the bytes, their type as detected when they were read, and
	// the sanitized basename they arrived under. Data is base64 in JSON by
	// virtue of being []byte, so the log holds the attachment itself and
	// Fold stays a pure function of the log. MediaType is recorded rather
	// than re-derived, so replaying a session never sniffs anything twice.
	MediaType string `json:"media_type,omitempty"`
	Name      string `json:"name,omitempty"`
	Data      []byte `json:"data,omitempty"`

	// Opaque provider state: the provider-native form of an assistant
	// turn, replayed verbatim to the same provider and ignored by others.
	Provider string          `json:"provider,omitempty"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

type ChunkKind int

const (
	KindText      ChunkKind = iota // delta of assistant text
	KindReasoning                  // delta of reasoning text
	KindBlock                      // a completed block, ready to log
	KindUsage
	KindStop
)

type Chunk struct {
	Kind  ChunkKind
	Text  string // KindText, KindReasoning
	Block *Block // KindBlock
	Usage *Usage // KindUsage
	Stop  string // KindStop: "end", "max_tokens", or provider-specific
}

type Usage struct {
	In         int `json:"in"`
	Out        int `json:"out"`
	Reasoning  int `json:"reasoning,omitempty"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`

	// USD for this turn, as the provider billed it — not an estimate from a
	// rate table. Only some report it; zero means "not said", never "free".
	Cost float64 `json:"cost,omitempty"`
}

func (u *Usage) Add(v Usage) {
	u.In += v.In
	u.Out += v.Out
	u.Reasoning += v.Reasoning
	u.CacheRead += v.CacheRead
	u.CacheWrite += v.CacheWrite
	u.Cost += v.Cost
}

// Error is a provider failure with enough structure for the retry loop.
// Status 0 means a transport-level failure.
type Error struct {
	Status     int
	Code       string // provider's machine-readable error code, when available
	RetryAfter time.Duration
	Msg        string
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Msg
	}
	return fmt.Sprintf("provider: %d: %s", e.Status, e.Msg)
}

// Retryable reports whether backing off and retrying may help.
func (e *Error) Retryable() bool {
	return e.Status == 0 || e.Status == 408 || e.Status == 429 || e.Status >= 500
}

// overflowPhrases are fallbacks for providers without a specific error code.
// Each phrase names the input or context: generic size and token limits can
// describe attachments or output settings, neither of which is exit 2.
var overflowPhrases = []string{
	"context length",
	"context_length",
	"context window",
	"prompt is too long",
	"maximum context",
	"reduce the length of the messages",
}

// Overflow reports whether the call failed because the conversation no
// longer fits the model's context window. That failure is permanent — the
// same request will fail identically forever — so ask has to tell it apart
// from the transient kind, or a retry loop grinds against it until someone
// notices. The match is a heuristic over provider prose and is used only
// to turn a permanent error into an outcome a shell can act on.
func (e *Error) Overflow() bool {
	switch e.Status {
	case 400, 413, 422:
	default:
		return false
	}
	if e.Code == "context_length_exceeded" {
		return true
	}
	msg := strings.ToLower(e.Msg)
	if strings.Contains(msg, "input token count") && strings.Contains(msg, "exceeds the maximum") {
		return true
	}
	for _, p := range overflowPhrases {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// Providers are the adapter names New accepts. Named once, so help text,
// the man page, and the completions cannot drift from the code.
var Providers = []string{"anthropic", "openai", "openai-codex", "gemini", "openrouter"}

// Options carries invocation-scoped transport inputs. Authorization is an
// already-formed HTTP Authorization value supplied through Ask's descriptor
// boundary; Ask never acquires, stores, or refreshes it.
type Options struct {
	Authorization  string
	CodexAccountID string
}

// New returns the adapter and bare model name for a spec of the form
// provider/model, e.g. "anthropic/your-model" or
// "openrouter/vendor/model" (OpenRouter models keep their slash).
// <PROVIDER>_BASE_URL replaces an adapter endpoint. An invocation-scoped
// Authorization header may replace API-key authentication; openai-codex
// requires one.
// ANTHROPIC_VERTEX_PROJECT_ID routes anthropic models through Google
// Vertex AI (see vertex.go).
func New(spec string, opts Options) (Provider, string, error) {
	name, model, ok := strings.Cut(spec, "/")
	if !ok || model == "" {
		return nil, "", fmt.Errorf("model must be provider/model, got %q", spec)
	}
	authClient := authHeaderClient(opts.Authorization, opts.CodexAccountID, name == "openai-codex")
	switch name {
	case "anthropic":
		vopts, err := vertexOptions()
		if err != nil {
			return nil, "", err
		}
		k, err := key("ANTHROPIC_API_KEY", authClient != nil || vopts != nil)
		if err != nil {
			return nil, "", err
		}
		base := os.Getenv("ANTHROPIC_BASE_URL")
		if vopts != nil {
			base = "" // Vertex owns the endpoint; ANTHROPIC_VERTEX_BASE_URL overrides it
		}
		return NewAnthropic(k, base, authClient, vopts...), model, nil
	case "openai":
		k, err := key("OPENAI_API_KEY", authClient != nil)
		if err != nil {
			return nil, "", err
		}
		return NewOpenAI(k, os.Getenv("OPENAI_BASE_URL"), authClient), model, nil
	case "openai-codex":
		if authClient == nil {
			return nil, "", fmt.Errorf("openai-codex requires -header-fd; run with oauth with PROFILE -- ask -header-fd 3")
		}
		base := os.Getenv("OPENAI_CODEX_BASE_URL")
		if base == "" {
			base = "https://chatgpt.com/backend-api/codex"
		}
		return NewOpenAICodex(base, authClient), model, nil
	case "gemini":
		k, err := key("GEMINI_API_KEY", authClient != nil)
		if err != nil {
			return nil, "", err
		}
		return NewGemini(k, os.Getenv("GEMINI_BASE_URL"), authClient), model, nil
	case "openrouter":
		k, err := key("OPENROUTER_API_KEY", authClient != nil)
		if err != nil {
			return nil, "", err
		}
		return NewOpenRouter(k, os.Getenv("OPENROUTER_BASE_URL"), authClient), model, nil
	}
	return nil, "", fmt.Errorf("unknown provider %q (want %s)", name, strings.Join(Providers, ", "))
}

// key reads a provider credential. Behind an OAuth gateway or on Vertex
// the real credential lives in the transport (bearer token, Google
// auth), so a missing vendor key stops being an error there.
func key(env string, optional bool) (string, error) {
	if k := os.Getenv(env); k != "" {
		return k, nil
	}
	if optional {
		return "unused", nil
	}
	return "", fmt.Errorf("%s is not set", env)
}

// Merge combines consecutive same-role messages into one. Some providers
// reject or mishandle back-to-back turns from the same role; adapters call
// this before converting.
func Merge(msgs []Message) []Message {
	var out []Message
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Blocks = append(out[n-1].Blocks, m.Blocks...)
			continue
		}
		out = append(out, Message{Role: m.Role, Blocks: append([]Block(nil), m.Blocks...)})
	}
	return out
}
