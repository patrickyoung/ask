package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"sort"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"
)

// OpenRouter speaks Chat Completions against openrouter.ai. Reasoning
// arrives in OpenRouter's extension fields: a plain "reasoning" string for
// display and a "reasoning_details" array that must be echoed back
// unmodified and in order — it is stored as this turn's opaque block and
// re-attached to the assistant message on replay.
type OpenRouter struct {
	c openai.Client
}

// NewOpenRouter builds the adapter; base and hc are optional gateway
// overrides ("" and nil mean openrouter.ai over the default transport).
func NewOpenRouter(key, base string, hc *http.Client) *OpenRouter {
	if base == "" {
		base = "https://openrouter.ai/api/v1"
	}
	opts := []option.RequestOption{
		option.WithAPIKey(key),
		option.WithBaseURL(base),
		option.WithHeader("HTTP-Referer", "https://github.com/patrickyoung/ask"),
		option.WithHeader("X-Title", "ask"),
		option.WithMaxRetries(0), // ask owns retries
	}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	return &OpenRouter{c: openai.NewClient(opts...)}
}

func (p *OpenRouter) Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		params, opts, err := openrouterParams(req)
		if err != nil {
			yield(Chunk{}, err)
			return
		}
		stream := p.c.Chat.Completions.NewStreaming(ctx, params, opts...)
		defer stream.Close()
		var acc openai.ChatCompletionAccumulator
		details := newDetails()
		var extra Usage
		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)
			// Read the usage extensions off the chunk, not the accumulator:
			// the accumulator sums the fields it knows and drops the JSON
			// metadata, so extras are only visible here. Usage usually
			// arrives on a final chunk carrying no choices, which is why
			// this sits above the skip below.
			if w := extraInt(chunk.Usage.PromptTokensDetails.JSON.ExtraFields, "cache_write_tokens"); w > 0 {
				extra.CacheWrite = w
			}
			if c := extraFloat(chunk.Usage.JSON.ExtraFields, "cost"); c > 0 {
				extra.Cost = c
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			delta := chunk.Choices[0].Delta
			if delta.Content != "" {
				if !yield(Chunk{Kind: KindText, Text: delta.Content}, nil) {
					return
				}
			}
			if f, ok := delta.JSON.ExtraFields["reasoning"]; ok {
				var s string
				if json.Unmarshal([]byte(f.Raw()), &s) == nil && s != "" {
					if !yield(Chunk{Kind: KindReasoning, Text: s}, nil) {
						return
					}
				}
			}
			if f, ok := delta.JSON.ExtraFields["reasoning_details"]; ok {
				if err := details.add(f.Raw()); err != nil {
					yield(Chunk{}, fmt.Errorf("openrouter: reasoning_details: %w", err))
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(Chunk{}, openaiErr(err))
			return
		}
		if len(acc.Choices) == 0 {
			yield(Chunk{}, &Error{Msg: "openrouter: empty response"})
			return
		}
		choice := acc.Choices[0]
		if r := details.reasoning(); r != "" {
			if !yield(Chunk{Kind: KindBlock, Block: &Block{Type: Reasoning, Text: r, Provider: "openrouter"}}, nil) {
				return
			}
		}
		if choice.Message.Content != "" {
			if !yield(Chunk{Kind: KindBlock, Block: &Block{Type: Text, Text: choice.Message.Content}}, nil) {
				return
			}
		}
		if raw := details.raw(); raw != nil {
			if !yield(Chunk{Kind: KindBlock, Block: &Block{Type: Opaque, Provider: "openrouter", Raw: raw}}, nil) {
				return
			}
		}
		// cache_write_tokens and cost are OpenRouter extensions the SDK's
		// structs do not carry, so they come off the raw JSON — the same
		// door reasoning_details comes through. Without the first, a cached
		// run logged cache_write 0 and looked cheaper than it was; without
		// the second, dollars had to be estimated from a rate table when
		// the provider was already reporting them exactly.
		u := Usage{
			In:         int(acc.Usage.PromptTokens),
			Out:        int(acc.Usage.CompletionTokens),
			Reasoning:  int(acc.Usage.CompletionTokensDetails.ReasoningTokens),
			CacheRead:  int(acc.Usage.PromptTokensDetails.CachedTokens),
			CacheWrite: extra.CacheWrite,
			Cost:       extra.Cost,
		}
		if !yield(Chunk{Kind: KindUsage, Usage: &u}, nil) {
			return
		}
		stop := "end"
		if choice.FinishReason == "length" {
			stop = "max_tokens"
		}
		yield(Chunk{Kind: KindStop, Stop: stop}, nil)
	}
}

func openrouterParams(req Request) (openai.ChatCompletionNewParams, []option.RequestOption, error) {
	params := openai.ChatCompletionNewParams{Model: req.Model}
	var opts []option.RequestOption
	if req.MaxTokens > 0 {
		params.MaxTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.Effort != "" && req.Effort != "off" {
		opts = append(opts, option.WithJSONSet("reasoning", map[string]any{"effort": req.Effort}))
	}
	// Prompt caching. An agent session re-sends its whole history every
	// turn, so with no cache breakpoint the prefix is billed at full price
	// again each time — measured at 2.04M input tokens over a 44-turn run
	// that should have paid for about a seventh of that. OpenRouter takes
	// one field at the request root and places the breakpoint itself,
	// advancing it as the conversation grows: the moving breakpoint the
	// Anthropic adapter builds by hand, without ask having to guess which
	// message shapes carry cache_control.
	if cacheable(req.Model) {
		opts = append(opts, option.WithJSONSet("cache_control", map[string]string{"type": "ephemeral"}))
	}
	// A warm cache only pays if the next turn reaches the endpoint holding
	// it. OpenRouter pins a conversation to one endpoint on its own, but
	// only once it has seen a hit — so the turn that would establish the
	// cache is the one turn free to land elsewhere. Naming the session pins
	// it from the first request instead. It matters most where ask fans out:
	// several workspaces working the same model at once are several
	// conversations, and each should keep its own endpoint.
	if req.Session != "" {
		opts = append(opts, option.WithHeader("x-session-id", req.Session))
	}
	if req.System != "" {
		params.Messages = append(params.Messages, openai.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		if m.Role == Assistant {
			msg := openai.ChatCompletionAssistantMessageParam{}
			var raw json.RawMessage
			for _, b := range m.Blocks {
				switch b.Type {
				case Text:
					msg.Content.OfString = openai.String(b.Text)
				case Opaque:
					if b.Provider == "openrouter" {
						raw = b.Raw
					}
				}
			}
			idx := len(params.Messages)
			params.Messages = append(params.Messages, openai.ChatCompletionMessageParamUnion{OfAssistant: &msg})
			if raw != nil {
				// The SDK has no per-message extra fields; splice
				// reasoning_details into the marshaled body by path.
				opts = append(opts, option.WithJSONSet(fmt.Sprintf("messages.%d.reasoning_details", idx), raw))
			}
			continue
		}
		for _, b := range m.Blocks {
			if b.Type == Text {
				params.Messages = append(params.Messages, openai.UserMessage(b.Text))
			}
		}
	}
	return params, opts, nil
}

// extraInt and extraFloat read a number the SDK has no field for. A key
// that is absent, null, or not a number reads as zero: these are telemetry,
// and a turn is not worth failing over a usage field.
func extraInt(fields map[string]respjson.Field, key string) int {
	return int(extraFloat(fields, key))
}

func extraFloat(fields map[string]respjson.Field, key string) float64 {
	f, ok := fields[key]
	if !ok {
		return 0
	}
	var v float64
	if json.Unmarshal([]byte(f.Raw()), &v) != nil {
		return 0
	}
	return v
}

// cacheable reports whether a root-level cache_control belongs on a
// request for this model. Only the Claude family: the field is honored by
// the endpoints that serve those models, and OpenRouter routes a request
// carrying it *only* to endpoints that support it. On a model with no such
// endpoint that would narrow routing for nothing, since everything else
// already caches provider-side without being asked. Ids are vendor/model,
// optionally with the "~" floating-version prefix (~anthropic/claude-sonnet-latest).
func cacheable(model string) bool {
	return strings.HasPrefix(strings.TrimPrefix(model, "~"), "anthropic/")
}

// details reassembles OpenRouter's streamed reasoning_details entries.
// Entries sharing an index are one detail: text and summary accumulate,
// signature and everything else take the latest value — mirroring how the
// non-streaming response would look, which is what must be echoed back.
type details struct {
	byIndex map[int]map[string]any
	order   []int
}

func newDetails() *details { return &details{byIndex: map[int]map[string]any{}} }

func (d *details) add(raw string) error {
	var entries []map[string]any
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return err
	}
	for _, e := range entries {
		idx := 0
		if v, ok := e["index"].(float64); ok {
			idx = int(v)
		}
		slot, ok := d.byIndex[idx]
		if !ok {
			slot = map[string]any{}
			d.byIndex[idx] = slot
			d.order = append(d.order, idx)
		}
		for k, v := range e {
			switch k {
			case "text", "summary", "data":
				prev, _ := slot[k].(string)
				next, _ := v.(string)
				slot[k] = prev + next
			default:
				slot[k] = v
			}
		}
	}
	return nil
}

func (d *details) raw() json.RawMessage {
	if len(d.order) == 0 {
		return nil
	}
	sort.Ints(d.order)
	out := make([]map[string]any, 0, len(d.order))
	for _, i := range d.order {
		out = append(out, d.byIndex[i])
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return raw
}

// reasoning returns the readable reasoning text assembled from details.
func (d *details) reasoning() string {
	sort.Ints(d.order)
	var s string
	for _, i := range d.order {
		if t, ok := d.byIndex[i]["text"].(string); ok {
			s += t
		} else if t, ok := d.byIndex[i]["summary"].(string); ok {
			s += t
		}
	}
	return s
}
