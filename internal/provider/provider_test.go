package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	openai "github.com/openai/openai-go/v3"
	oopt "github.com/openai/openai-go/v3/option"
)

// req returns a request exercising every block type a conversation can
// hold: user text, an assistant turn with signed native reasoning, and a
// foreign-provider turn that must degrade gracefully rather than be
// replayed to a provider that cannot verify it.
func req(native string) Request {
	return Request{
		Model:     "test-model",
		System:    "be brief",
		MaxTokens: 1000,
		Messages: []Message{
			{Role: User, Blocks: []Block{{Type: Text, Text: "list files"}}},
			{Role: Assistant, Blocks: []Block{
				{Type: Reasoning, Text: "I should ls", Signature: "sig-abc", Provider: native},
				{Type: Text, Text: "a.txt"},
			}},
			{Role: User, Blocks: []Block{{Type: Text, Text: "how big is it?"}}},
			{Role: Assistant, Blocks: []Block{
				{Type: Reasoning, Text: "foreign thoughts", Provider: "someone-else"},
				{Type: Text, Text: "12 bytes"},
			}},
		},
	}
}

func marshal(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAnthropicParams(t *testing.T) {
	params, err := anthropicParams(req("anthropic"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(marshal(t, params))
	s := string(raw)

	for _, want := range []string{
		`"signature":"sig-abc"`,    // native reasoning replayed with signature
		`"thinking":"I should ls"`, //
		`"type":"adaptive"`,        // thinking on by default
		`"cache_control"`,          // prompt caching breakpoints set
		`"system"`,                 // system prompt carried
	} {
		if !strings.Contains(s, want) {
			t.Errorf("anthropic params missing %s in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "foreign thoughts") {
		t.Error("foreign reasoning leaked into anthropic params")
	}
}

func TestAnthropicParamsEffortOff(t *testing.T) {
	r := req("anthropic")
	r.Effort = "off"
	params, err := anthropicParams(r)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(marshal(t, params))
	if strings.Contains(string(raw), "adaptive") {
		t.Error("effort=off still enabled thinking")
	}
}

// TestAnthropicCacheBreakpointOnLastMessage pins the moving breakpoint: a
// conversation re-sends its whole history every turn, so without one the
// prefix is billed at full price again on every single ask.
func TestAnthropicCacheBreakpointOnLastMessage(t *testing.T) {
	params, err := anthropicParams(req("anthropic"))
	if err != nil {
		t.Fatal(err)
	}
	n := len(params.Messages)
	if n == 0 {
		t.Fatal("no messages")
	}
	last := params.Messages[n-1].Content
	tail := last[len(last)-1]
	if tail.OfText == nil || tail.OfText.CacheControl.Type == "" {
		t.Errorf("no cache_control on the last block of the last message: %+v", tail)
	}
}

func TestGeminiParams(t *testing.T) {
	r := req("gemini")
	// A gemini-native turn is replayed from its opaque Content, verbatim.
	opaque := `{"role":"model","parts":[{"text":"I should ls","thought":true,"thoughtSignature":"c2ln"},{"text":"a.txt"}]}`
	r.Messages[1].Blocks = append(r.Messages[1].Blocks, Block{Type: Opaque, Provider: "gemini", Raw: json.RawMessage(opaque)})

	cfg, contents, err := geminiParams(r)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ThinkingConfig == nil || !cfg.ThinkingConfig.IncludeThoughts {
		t.Error("thinking not enabled by default")
	}
	if cfg.SystemInstruction == nil {
		t.Error("system prompt not carried")
	}
	if len(contents) != 4 {
		t.Fatalf("got %d contents, want 4", len(contents))
	}
	got := marshal(t, contents[1])
	var want map[string]any
	json.Unmarshal([]byte(opaque), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gemini opaque content not replayed verbatim:\ngot  %v\nwant %v", got, want)
	}
	// Foreign assistant turn (no gemini opaque) builds from normalized blocks.
	last := marshal(t, contents[3])
	if raw, _ := json.Marshal(last); strings.Contains(string(raw), "foreign thoughts") {
		t.Error("foreign reasoning leaked into gemini contents")
	}
}

func TestOpenAIParams(t *testing.T) {
	r := req("openai")
	r.Effort = "xhigh"
	items := `[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"blob"},{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"a.txt","annotations":[]}]}]`
	r.Messages[1].Blocks = append(r.Messages[1].Blocks, Block{Type: Opaque, Provider: "openai", Raw: json.RawMessage(items)})

	params := openaiParams(r)
	m := marshal(t, params)
	raw, _ := json.Marshal(m)
	s := string(raw)

	if m["store"] != false {
		t.Error("store must be false: the log is the state")
	}
	for _, want := range []string{
		`"encrypted_content":"blob"`,                // opaque items replayed raw
		`"include":["reasoning.encrypted_content"]`, // encrypted reasoning requested
		`"summary":"auto"`,                          // reasoning summaries on
		`"effort":"xhigh"`,                          // explicit effort passes through
		`"instructions":"be brief"`,                 // system → instructions
	} {
		if !strings.Contains(s, want) {
			t.Errorf("openai params missing %s in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "foreign thoughts") {
		t.Error("foreign reasoning leaked into openai params")
	}
}

func TestOpenRouterParams(t *testing.T) {
	r := req("openrouter")
	r.Messages[1].Blocks = append(r.Messages[1].Blocks, Block{
		Type: Opaque, Provider: "openrouter",
		Raw: json.RawMessage(`[{"type":"reasoning.text","text":"I should ls","signature":"s1","index":0}]`),
	})
	params, opts, err := openrouterParams(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 1 {
		t.Fatalf("want 1 request option for the reasoning_details splice, got %d", len(opts))
	}
	raw, _ := json.Marshal(marshal(t, params))
	s := string(raw)
	for _, want := range []string{`"role":"system"`, `"content":"be brief"`, `"role":"assistant"`} {
		if !strings.Contains(s, want) {
			t.Errorf("openrouter params missing %s in:\n%s", want, s)
		}
	}
}

func TestStructuredOutputReachesEveryAdapter(t *testing.T) {
	r := req("anthropic")
	r.Schema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`)

	if p, err := anthropicParams(r); err != nil {
		t.Fatal(err)
	} else if s := mustJSON(t, p); !strings.Contains(s, `"output_config":{"format":{"schema":`) || !strings.Contains(s, `"type":"json_schema"`) {
		t.Errorf("anthropic did not receive output_config.format:\n%s", s)
	}

	if s := mustJSON(t, openaiParams(r)); !strings.Contains(s, `"text":{"format":{"name":"answer"`) || !strings.Contains(s, `"strict":true`) {
		t.Errorf("openai did not receive strict text.format:\n%s", s)
	}

	if cfg, _, err := geminiParams(r); err != nil {
		t.Fatal(err)
	} else if s := mustJSON(t, cfg); !strings.Contains(s, `"responseMimeType":"application/json"`) || !strings.Contains(s, `"responseJsonSchema"`) {
		t.Errorf("gemini did not receive response JSON schema:\n%s", s)
	}

	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ = io.ReadAll(request.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, openrouterWire)
	}))
	defer srv.Close()
	r.Model = "openai/gpt-5.6"
	for _, err := range NewOpenRouter("k", srv.URL, nil).Stream(context.Background(), r) {
		if err != nil {
			t.Fatal(err)
		}
	}
	s := string(body)
	for _, want := range []string{`"response_format":{"json_schema"`, `"strict":true`, `"require_parameters":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("openrouter request missing %s:\n%s", want, s)
		}
	}
}

// TestOpenRouterPromptCache pins the cache breakpoint to the wire rather
// than to the params struct: cache_control travels as a root-level field
// spliced in by a request option, so only the marshaled body proves it
// went out. It belongs on Claude models and nowhere else — a request
// carrying it is routed only to endpoints that support it, which on any
// other model narrows routing to buy nothing.
func TestOpenRouterPromptCache(t *testing.T) {
	for _, c := range []struct {
		model string
		want  bool
	}{
		{"anthropic/claude-sonnet-4.5", true},
		{"~anthropic/claude-sonnet-latest", true},
		{"moonshotai/kimi-k3", false},
		{"openai/gpt-5.2", false},
	} {
		t.Run(c.model, func(t *testing.T) {
			var body []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, openrouterWire)
			}))
			defer srv.Close()

			r := req("openrouter")
			r.Model = c.model
			for _, err := range NewOpenRouter("k", srv.URL, nil).Stream(context.Background(), r) {
				if err != nil {
					t.Fatalf("stream: %v", err)
				}
			}

			var sent map[string]any
			if err := json.Unmarshal(body, &sent); err != nil {
				t.Fatalf("request body: %v\n%s", err, body)
			}
			cc, got := sent["cache_control"]
			if got != c.want {
				t.Fatalf("root cache_control present = %v, want %v; body:\n%s", got, c.want, body)
			}
			if got && !reflect.DeepEqual(cc, map[string]any{"type": "ephemeral"}) {
				t.Errorf("cache_control = %v, want {type: ephemeral}", cc)
			}
			// Whatever else changes, the conversation itself must still go
			// out intact — a caching hint that dropped messages would be a
			// far more expensive bug than the one it fixes.
			if msgs, _ := sent["messages"].([]any); len(msgs) != 5 {
				t.Errorf("sent %d messages, want 5 (system + 4)", len(msgs))
			}
		})
	}
}

// TestOpenRouterSessionHeader: a warm cache only pays if the next turn
// reaches the endpoint holding it, and OpenRouter pins a conversation only
// after it has seen a hit — leaving the turn that establishes the cache free
// to land elsewhere. Naming the session pins it from the first request. It
// travels as a header because it is routing metadata, not model input.
func TestOpenRouterSessionHeader(t *testing.T) {
	for _, c := range []struct{ session, want string }{
		{"20260801-160948-7a6975f693e864cb", "20260801-160948-7a6975f693e864cb"},
		{"", ""}, // no session, no header — never an empty one
	} {
		var got string
		var seen bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, seen = r.Header.Get("X-Session-Id"), r.Header.Values("X-Session-Id") != nil
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, openrouterWire)
		}))
		defer srv.Close()

		r := req("openrouter")
		r.Session = c.session
		for _, err := range NewOpenRouter("k", srv.URL, nil).Stream(context.Background(), r) {
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
		}
		if got != c.want {
			t.Errorf("session %q sent x-session-id %q, want %q", c.session, got, c.want)
		}
		if c.session == "" && seen {
			t.Error("sent an empty x-session-id header; omit it instead")
		}
	}
}

// TestOpenRouterUsageExtras pins the two usage numbers the SDK's structs do
// not carry. Both were being dropped: a cached run logged cache_write 0 and
// so looked cheaper than it was, and dollars had to be estimated from a rate
// table while the provider was reporting them exactly. The wire fixture is a
// real OpenRouter response body.
func TestOpenRouterUsageExtras(t *testing.T) {
	const wire = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"anthropic/claude-sonnet-4.5","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5631,"completion_tokens":4,"total_tokens":5635,"cost":0.00180225,"prompt_tokens_details":{"cached_tokens":5615,"cache_write_tokens":13,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}

data: [DONE]

`
	srv := serve(t, 200, sseHeader(), wire)
	var got Usage
	for c, err := range NewOpenRouter("k", srv.URL, nil).Stream(context.Background(), req("openrouter")) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if c.Kind == KindUsage {
			got = *c.Usage
		}
	}
	want := Usage{In: 5631, Out: 4, CacheRead: 5615, CacheWrite: 13, Cost: 0.00180225}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// TestOpenRouterUsageAbsentExtras: a provider that reports neither field must
// read as zero, not as a parse failure — and zero cost has to stay
// distinguishable from free, which is why the renderer omits it entirely.
func TestOpenRouterUsageAbsentExtras(t *testing.T) {
	const wire = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":0}}}

data: [DONE]

`
	srv := serve(t, 200, sseHeader(), wire)
	var got Usage
	for c, err := range NewOpenRouter("k", srv.URL, nil).Stream(context.Background(), req("openrouter")) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if c.Kind == KindUsage {
			got = *c.Usage
		}
	}
	if want := (Usage{In: 10, Out: 2}); got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestDetailsAssembly(t *testing.T) {
	d := newDetails()
	d.add(`[{"type":"reasoning.text","index":0,"text":"I sho"}]`)
	d.add(`[{"type":"reasoning.text","index":0,"text":"uld ls","signature":"sig"}]`)
	d.add(`[{"type":"reasoning.text","index":1,"text":"more"}]`)
	var got []map[string]any
	if err := json.Unmarshal(d.raw(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d details, want 2", len(got))
	}
	if got[0]["text"] != "I should ls" || got[0]["signature"] != "sig" {
		t.Errorf("detail 0 misassembled: %v", got[0])
	}
	if d.reasoning() != "I should lsmore" {
		t.Errorf("reasoning text = %q", d.reasoning())
	}
}

func TestReplayProvider(t *testing.T) {
	p := &Replay{Turns: []ReplayTurn{{
		Blocks: []Block{
			{Type: Reasoning, Text: "hmm"},
			{Type: Text, Text: "hi"},
		},
		Usage: Usage{In: 10, Out: 5},
		Stop:  "end",
	}}}
	var kinds []ChunkKind
	for c, err := range p.Stream(context.Background(), Request{}) {
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, c.Kind)
	}
	want := []ChunkKind{KindReasoning, KindBlock, KindText, KindBlock, KindUsage, KindStop}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("chunk kinds = %v, want %v", kinds, want)
	}
	// Exhausted: next call errors.
	for _, err := range p.Stream(context.Background(), Request{}) {
		if err == nil {
			t.Fatal("expected out-of-turns error")
		}
	}
	if len(p.Requests) != 2 {
		t.Errorf("recorded %d requests, want 2", len(p.Requests))
	}
}

// TestOpenAICodexItemsAreNotDoubled: codex streams each completed item as
// it lands and then repeats it in the final response. Taking both would log
// the answer twice and send a doubled assistant turn on the next ask, so an
// item is counted once. The contract's streamed-text == logged-text rule is
// what would break first, which is exactly why it is a rule.
func TestOpenAICodexItemsAreNotDoubled(t *testing.T) {
	const item = `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"sequence_number\":1,\"item\":%s}\n\n", item)
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[%s],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"output_tokens_details\":{\"reasoning_tokens\":0},\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n", item)
	}))
	defer s.Close()

	var texts []string
	var stop string
	for ch, err := range NewOpenAICodex(s.URL, s.Client()).Stream(context.Background(),
		Request{Model: "gpt-5.6-sol", Messages: []Message{{Role: User, Blocks: []Block{{Type: Text, Text: "hi"}}}}}) {
		if err != nil {
			t.Fatal(err)
		}
		if ch.Kind == KindBlock && ch.Block.Type == Text {
			texts = append(texts, ch.Block.Text)
		}
		if ch.Kind == KindStop {
			stop = ch.Stop
		}
	}
	if len(texts) != 1 || texts[0] != "Hello world" {
		t.Fatalf("text blocks = %q, want exactly one %q", texts, "Hello world")
	}
	if stop != "end" {
		t.Errorf("stop = %q, want end", stop)
	}
}

// TestLoggedReplacesMessagesWithDigest pins what a request event carries:
// everything the call was made with, and the conversation as a hash rather
// than a second copy of the log's own contents.
func TestLoggedReplacesMessagesWithDigest(t *testing.T) {
	msgs := []Message{
		{Role: User, Blocks: []Block{{Type: Text, Text: "hi"}}},
		{Role: Assistant, Blocks: []Block{{Type: Text, Text: "hello"}}},
	}
	req := Request{Model: "m", System: "sys", Messages: msgs, MaxTokens: 32, Effort: "low", Session: "sess-1"}
	got := req.Logged()

	if got.Messages != nil {
		t.Error("Logged kept the messages; the log would still store the fold twice")
	}
	if got.Digest == "" {
		t.Fatal("Logged set no digest")
	}
	if got.Model != "m" || got.System != "sys" || got.MaxTokens != 32 || got.Effort != "low" {
		t.Errorf("Logged dropped a field that is not derived: %+v", got)
	}
	// Session is routing metadata, not part of the call's meaning, and it
	// always names the log being written to — so it must not reach the log,
	// where it would repeat the file's own name on every turn.
	if b, _ := json.Marshal(got); strings.Contains(string(b), "sess-1") {
		t.Errorf("Logged carried the session id into the event: %s", b)
	}
	if req.Messages == nil {
		t.Error("Logged mutated its receiver; the request being sent lost its messages")
	}
	if want := Digest(msgs); got.Digest != want {
		t.Errorf("digest = %s, want %s", got.Digest, want)
	}

	// Any difference in the conversation must change the hash, or Check
	// would wave through a divergent replay.
	other := []Message{
		{Role: User, Blocks: []Block{{Type: Text, Text: "hi"}}},
		{Role: Assistant, Blocks: []Block{{Type: Text, Text: "hello!"}}},
	}
	if Digest(other) == Digest(msgs) {
		t.Error("different conversations share a digest")
	}
	if Digest(nil) == Digest(msgs) {
		t.Error("empty and non-empty conversations share a digest")
	}
	// The same conversation must hash the same on every run, or old logs
	// stop verifying against new binaries.
	if Digest(msgs) != Digest(msgs) {
		t.Error("digest is not stable")
	}
}

// TestOverflowIsToldFromTransient pins the one classification the retry
// loop cannot get wrong: a full context is permanent, so retrying it is
// how a supervisor loop grinds forever. Phrasings are the real ones the
// providers return.
func TestOverflowIsToldFromTransient(t *testing.T) {
	overflow := []Error{
		{Status: 400, Code: "context_length_exceeded", Msg: "request rejected"},
		{Status: 400, Msg: "prompt is too long: 213458 tokens > 200000 maximum"},
		{Status: 400, Msg: "This model's maximum context length is 128000 tokens, however you requested 131204 tokens"},
		{Status: 400, Msg: `{"error":{"code":"context_length_exceeded"}}`},
		{Status: 400, Msg: "The input token count (1052342) exceeds the maximum number of tokens allowed"},
		{Status: 413, Msg: "Request too large: reduce the length of the messages"},
	}
	for _, e := range overflow {
		if !e.Overflow() {
			t.Errorf("Overflow() = false for %q", e.Msg)
		}
		if e.Retryable() {
			t.Errorf("Retryable() = true for a permanent failure: %q", e.Msg)
		}
	}

	transient := []Error{
		{Status: 400, Msg: "image dimensions exceeds the maximum allowed size"},
		{Status: 413, Msg: "request size exceeds the maximum of 32 MB"},
		{Status: 422, Msg: "max_tokens exceeds the maximum number of tokens allowed"},
		{Status: 400, Msg: "output token limit exceeded"},
		{Status: 400, Msg: "too many tokens requested for output"},
		{Status: 400, Msg: "input token count must be positive"},
		{Status: 429, Msg: "rate limit exceeded"},
		{Status: 500, Msg: "internal error"},
		{Status: 529, Msg: "overloaded"},
		{Status: 0, Msg: "connection reset by peer"},
		{Status: 400, Msg: "invalid model id"},
		{Status: 401, Msg: "invalid api key: context length"}, // right words, wrong failure
	}
	for _, e := range transient {
		if e.Overflow() {
			t.Errorf("Overflow() = true for %q", e.Msg)
		}
	}
}

func TestOpenAIOverflowCodeSurvivesNormalization(t *testing.T) {
	srv := serve(t, 400, nil, `{"error":{"code":"context_length_exceeded","message":"request rejected","type":"invalid_request_error"}}`)
	p := NewOpenAI("k", srv.URL, nil)
	var got error
	for _, err := range p.Stream(context.Background(), contractReq()) {
		got = err
	}
	var pe *Error
	if !errors.As(got, &pe) || pe.Code != "context_length_exceeded" || !pe.Overflow() {
		t.Fatalf("normalized error=%v; want context overflow identified by code", got)
	}
}

// TestUnknownProviderNamesTheRealOnes: the error a typo produces has to
// list what is actually supported, and that list has to come from the same
// place New switches on.
func TestUnknownProviderNamesTheRealOnes(t *testing.T) {
	_, _, err := New("anthropoid/claude", Options{})
	if err == nil {
		t.Fatal("unknown provider accepted")
	}
	for _, p := range Providers {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not name provider %q", err, p)
		}
	}
}

// TestProviderNamesAgree: Providers is the one list of names, and every
// table keyed by a provider has to be keyed by exactly it. A name New
// accepts but the attachment table has never heard of refuses every
// attachment with "unknown provider"; a name in the table that New rejects
// is a second, undocumented spelling — and two names for one thing is how
// documentation starts being false.
func TestProviderNamesAgree(t *testing.T) {
	for _, name := range Providers {
		if _, ok := accepts[name]; !ok {
			t.Errorf("provider %q has no attachment policy", name)
		}
		if _, _, err := New(name+"/m", Options{}); err != nil && strings.Contains(err.Error(), "unknown provider") {
			t.Errorf("Providers lists %q but New does not know it", name)
		}
	}
	for name := range accepts {
		if !slices.Contains(Providers, name) {
			t.Errorf("attachment policy for %q, which is not in Providers", name)
		}
	}
}

// TestOpenAIErrorRecoversUnrecognisedBody: the Codex backend reports
// problems as {"detail":"..."}, a shape the SDK does not parse, and it
// then renders the error as a bare status line. Losing that sentence
// costs the user the actual reason — and costs Overflow() the prose it
// classifies on, which is the difference between exit 2 (stop) and exit 1
// (retry forever).
func TestOpenAIErrorRecoversUnrecognisedBody(t *testing.T) {
	for _, c := range []struct {
		name     string
		body     string
		want     string
		overflow bool
	}{
		{
			name: "wrong model",
			body: `{"detail":"The 'gpt-5.1-codex' model is not supported when using Codex with a ChatGPT account."}`,
			want: "not supported when using Codex",
		},
		{
			name:     "context full in an unrecognised shape",
			body:     `{"detail":"Your input exceeds the context window of this model."}`,
			want:     "context window",
			overflow: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, 400, http.Header{"Content-Type": []string{"application/json"}}, c.body)
			p := &OpenAI{c: openai.NewClient(oopt.WithAPIKey("k"), oopt.WithBaseURL(srv.URL), oopt.WithMaxRetries(0))}
			var got error
			for _, err := range p.Stream(context.Background(), contractReq()) {
				got = err
			}
			var pe *Error
			if !errors.As(got, &pe) {
				t.Fatalf("error is %T (%v), want *provider.Error", got, got)
			}
			if !strings.Contains(pe.Msg, c.want) {
				t.Errorf("error lost the provider's own words.\n got: %s\nwant it to contain: %s", pe.Msg, c.want)
			}
			if pe.Overflow() != c.overflow {
				t.Errorf("Overflow() = %v, want %v for %q", pe.Overflow(), c.overflow, pe.Msg)
			}
		})
	}
}

// TestMediaReachesEveryAdapter: an attachment has to arrive in each
// provider's own shape. A block that silently vanished in conversion would
// look exactly like a model that ignored the picture.
func TestMediaReachesEveryAdapter(t *testing.T) {
	img := Block{Type: Media, MediaType: "image/png", Name: "shot.png", Data: []byte("\x89PNG\r\n\x1a\nDATA")}
	doc := Block{Type: Media, MediaType: "application/pdf", Name: "report.pdf", Data: []byte("%PDF-1.7 DATA")}
	withMedia := func(bs ...Block) Request {
		r := Request{Model: "test-model", System: "be brief", MaxTokens: 100}
		r.Messages = []Message{{Role: User, Blocks: append(bs, Block{Type: Text, Text: "what is this?"})}}
		return r
	}
	// base64 of the PNG bytes, which is what must appear on every wire.
	// Computed, not written out: a hand-typed constant tests the typist.
	b64 := base64.StdEncoding.EncodeToString(img.Data)

	t.Run("anthropic image", func(t *testing.T) {
		p, err := anthropicParams(withMedia(img))
		if err != nil {
			t.Fatal(err)
		}
		s := mustJSON(t, p)
		for _, want := range []string{`"type":"image"`, `"media_type":"image/png"`, b64, "what is this?"} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %s in:\n%s", want, s)
			}
		}
	})
	t.Run("anthropic pdf", func(t *testing.T) {
		p, err := anthropicParams(withMedia(doc))
		if err != nil {
			t.Fatal(err)
		}
		if s := mustJSON(t, p); !strings.Contains(s, `"type":"document"`) {
			t.Errorf("pdf did not become a document block:\n%s", s)
		}
	})
	t.Run("openai", func(t *testing.T) {
		s := mustJSON(t, openaiParams(withMedia(img, doc)))
		for _, want := range []string{
			`"type":"input_image"`, "data:image/png;base64," + b64,
			`"type":"input_file"`, `"filename":"report.pdf"`, `"type":"input_text"`,
		} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %s in:\n%s", want, s)
			}
		}
	})
	t.Run("gemini", func(t *testing.T) {
		_, contents, err := geminiParams(withMedia(img))
		if err != nil {
			t.Fatal(err)
		}
		s := mustJSON(t, contents)
		if !strings.Contains(s, `"mimeType":"image/png"`) || !strings.Contains(s, b64) {
			t.Errorf("gemini inlineData missing:\n%s", s)
		}
	})
	t.Run("openrouter", func(t *testing.T) {
		params, _, err := openrouterParams(withMedia(img, doc))
		if err != nil {
			t.Fatal(err)
		}
		s := mustJSON(t, params)
		for _, want := range []string{
			`"type":"image_url"`, "data:image/png;base64," + b64,
			`"type":"file"`, `"filename":"report.pdf"`,
		} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %s in:\n%s", want, s)
			}
		}
	})
}

// TestTextOnlyMessagesAreUnchanged: adding attachments must not alter the
// shape of the request every other ask sends.
func TestTextOnlyMessagesAreUnchanged(t *testing.T) {
	s := mustJSON(t, openaiParams(req("openai")))
	if strings.Contains(s, "input_text") {
		t.Errorf("a text-only message grew a content list:\n%s", s)
	}
}

// TestAcceptsIsProviderShaped pins the capability table, including that it
// says what the provider does take.
func TestAcceptsIsProviderShaped(t *testing.T) {
	for _, c := range []struct {
		spec, mediaType string
		ok              bool
	}{
		{"anthropic/claude-sonnet-5", "image/png", true},
		{"anthropic/claude-sonnet-5", "application/pdf", true},
		{"anthropic/claude-sonnet-5", "audio/wav", false},
		{"anthropic/claude-sonnet-5", "video/mp4", false},
		{"gemini/gemini-3-flash", "audio/wav", true},
		{"gemini/gemini-3-flash", "video/mp4", true},
		{"openai-codex/gpt-5.6-sol", "image/jpeg", true},
		{"openai-codex/gpt-5.6-sol", "audio/mpeg", false},
		{"openrouter/anthropic/claude-sonnet-4.5", "image/webp", true},
		{"openrouter/anthropic/claude-sonnet-4.5", "video/mp4", false},
	} {
		err := Accepts(c.spec, c.mediaType)
		if (err == nil) != c.ok {
			t.Errorf("Accepts(%q, %q) = %v, want ok=%v", c.spec, c.mediaType, err, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "it takes") {
			t.Errorf("refusal does not say what is accepted: %v", err)
		}
	}
	if err := Accepts("nosuch/model", "image/png"); err == nil {
		t.Error("unknown provider accepted")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
