package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAI speaks the Responses API — the only one that returns reasoning.
// Runs are stateless (store: false); reasoning items round-trip through the
// log as encrypted content inside one opaque block per turn, replayed
// verbatim via param.Override.
type OpenAI struct {
	c     openai.Client
	codex bool
}

// NewOpenAI builds the adapter; base and hc are optional gateway
// overrides ("" and nil mean the real API over the default transport).
func NewOpenAI(key, base string, hc *http.Client) *OpenAI {
	opts := []option.RequestOption{
		option.WithAPIKey(key),
		option.WithMaxRetries(0), // ask owns retries
	}
	if base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	return &OpenAI{c: openai.NewClient(opts...)}
}

func NewOpenAICodex(base string, hc *http.Client) *OpenAI {
	p := NewOpenAI("unused", base, hc)
	p.codex = true
	return p
}

func (p *OpenAI) Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		params := p.params(req)
		stream := p.c.Responses.NewStreaming(ctx, params)
		defer stream.Close()
		var final *responses.Response
		var rawItems []json.RawMessage
		// Codex streams completed items as they land, and the final
		// response repeats them. Yielding both would log the answer twice
		// and send a doubled assistant turn on the next call, so an item
		// is taken from whichever arrives first and skipped after that.
		seen := map[string]bool{}
		stop := "end"
		for stream.Next() {
			switch ev := stream.Current().AsAny().(type) {
			case responses.ResponseTextDeltaEvent:
				if !yield(Chunk{Kind: KindText, Text: ev.Delta}, nil) {
					return
				}
			case responses.ResponseReasoningSummaryTextDeltaEvent:
				if !yield(Chunk{Kind: KindReasoning, Text: ev.Delta}, nil) {
					return
				}
			case responses.ResponseCompletedEvent:
				final = &ev.Response
			case responses.ResponseIncompleteEvent:
				final = &ev.Response
				stop = "incomplete"
			case responses.ResponseOutputItemDoneEvent:
				if p.codex {
					seen[ev.Item.ID] = true
					rawItems = append(rawItems, json.RawMessage(ev.Item.RawJSON()))
					if b, ok := openaiBlock(ev.Item); ok {
						if !yield(Chunk{Kind: KindBlock, Block: &b}, nil) {
							return
						}
					}
				}
			case responses.ResponseFailedEvent:
				yield(Chunk{}, &Error{Msg: ev.Response.Error.Message})
				return
			}
		}
		if err := stream.Err(); err != nil {
			yield(Chunk{}, openaiErr(err))
			return
		}
		if final == nil {
			yield(Chunk{}, errors.New("openai: stream ended without a completed response"))
			return
		}
		for _, item := range final.Output {
			if seen[item.ID] {
				continue
			}
			rawItems = append(rawItems, json.RawMessage(item.RawJSON()))
			b, ok := openaiBlock(item)
			if !ok {
				continue
			}
			if !yield(Chunk{Kind: KindBlock, Block: &b}, nil) {
				return
			}
		}
		if len(rawItems) > 0 {
			raw, err := json.Marshal(rawItems)
			if err != nil {
				yield(Chunk{}, err)
				return
			}
			if !yield(Chunk{Kind: KindBlock, Block: &Block{Type: Opaque, Provider: "openai", Raw: raw}}, nil) {
				return
			}
		}
		u := Usage{
			In:        int(final.Usage.InputTokens),
			Out:       int(final.Usage.OutputTokens),
			Reasoning: int(final.Usage.OutputTokensDetails.ReasoningTokens),
			CacheRead: int(final.Usage.InputTokensDetails.CachedTokens),
		}
		if !yield(Chunk{Kind: KindUsage, Usage: &u}, nil) {
			return
		}
		switch final.IncompleteDetails.Reason {
		case "max_output_tokens":
			stop = "max_tokens"
		case "":
		default:
			stop = final.IncompleteDetails.Reason
		}
		yield(Chunk{Kind: KindStop, Stop: stop}, nil)
	}
}

func openaiBlock(item responses.ResponseOutputItemUnion) (Block, bool) {
	switch item.Type {
	case "message":
		var text strings.Builder
		for _, c := range item.Content {
			text.WriteString(c.Text)
		}
		return Block{Type: Text, Text: text.String()}, true
	case "reasoning":
		var sum strings.Builder
		for _, s := range item.Summary {
			sum.WriteString(s.Text)
		}
		return Block{Type: Reasoning, Text: sum.String(), Provider: "openai"}, true
	default:
		return Block{}, false
	}
}

func openaiParams(req Request) responses.ResponseNewParams {
	return (&OpenAI{}).params(req)
}

func (p *OpenAI) params(req Request) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(req.Model),
		Store: openai.Bool(false), // no server-side state; the log is the state
	}
	if req.System != "" {
		params.Instructions = openai.String(req.System)
	}
	if req.MaxTokens > 0 && !p.codex {
		params.MaxOutputTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.Effort != "off" {
		params.Reasoning = shared.ReasoningParam{Summary: shared.ReasoningSummaryAuto}
		if req.Effort != "" {
			params.Reasoning.Effort = shared.ReasoningEffort(req.Effort)
		}
		params.Include = []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent}
	}
	if len(req.Schema) > 0 {
		format := responses.ResponseFormatTextConfigParamOfJSONSchema("answer", schemaObject(req.Schema))
		format.OfJSONSchema.Strict = openai.Bool(true)
		params.Text.Format = format
	}
	var items responses.ResponseInputParam
	for _, m := range req.Messages {
		if m.Role == Assistant {
			if raw := openaiOpaque(m); raw != nil {
				for _, item := range raw {
					items = append(items, param.Override[responses.ResponseInputItemUnionParam](item))
				}
				continue
			}
		}
		role := responses.EasyInputMessageRoleUser
		if m.Role == Assistant {
			role = responses.EasyInputMessageRoleAssistant
		}
		// A message carrying attachments becomes one item with a content
		// list; a plain text message keeps the simpler string form it has
		// always had.
		if hasMedia(m) {
			var content responses.ResponseInputMessageContentListParam
			for _, b := range m.Blocks {
				switch b.Type {
				case Text:
					content = append(content, responses.ResponseInputContentParamOfInputText(b.Text))
				case Media:
					content = append(content, openaiMedia(b))
				}
			}
			items = append(items, responses.ResponseInputItemParamOfMessage(content, role))
			continue
		}
		for _, b := range m.Blocks {
			switch b.Type {
			case Text:
				items = append(items, responses.ResponseInputItemParamOfMessage(b.Text, role))
			case Reasoning: // foreign; not replayable
			}
		}
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: items}
	return params
}

func hasMedia(m Message) bool {
	for _, b := range m.Blocks {
		if b.Type == Media {
			return true
		}
	}
	return false
}

// openaiMedia maps one attachment. Images travel as a data URL; everything
// else is a file, which is the only shape that carries a filename — and
// the API wants one.
func openaiMedia(b Block) responses.ResponseInputContentUnionParam {
	url := dataURL(b.MediaType, b.Data)
	if strings.HasPrefix(b.MediaType, "image/") {
		return responses.ResponseInputContentUnionParam{
			OfInputImage: &responses.ResponseInputImageParam{
				ImageURL: openai.String(url),
				Detail:   responses.ResponseInputImageDetailAuto,
			},
		}
	}
	name := b.Name
	if name == "" {
		name = "attachment"
	}
	return responses.ResponseInputContentUnionParam{
		OfInputFile: &responses.ResponseInputFileParam{
			Filename: openai.String(name),
			FileData: openai.String(url),
		},
	}
}

// dataURL is the base64 data: form every OpenAI-shaped API takes.
func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// openaiOpaque returns the turn's raw output items when it came from
// OpenAI; nil means build from normalized blocks.
func openaiOpaque(m Message) []json.RawMessage {
	for _, b := range m.Blocks {
		if b.Type == Opaque && b.Provider == "openai" {
			var items []json.RawMessage
			if err := json.Unmarshal(b.Raw, &items); err == nil {
				return items
			}
		}
	}
	return nil
}

func openaiErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ae *openai.Error
	if errors.As(err, &ae) {
		e := &Error{Status: ae.StatusCode, Code: ae.Code, Msg: ae.Error()}
		if ae.Response != nil {
			if d, perr := time.ParseDuration(ae.Response.Header.Get("Retry-After") + "s"); perr == nil {
				e.RetryAfter = d
			}
			// The SDK formats the body it recognises. An endpoint that
			// answers in another shape — the Codex backend says
			// {"detail":"..."} — leaves it with the status line alone,
			// dropping the one sentence that says what is wrong. Recover
			// the body rather than report "400 Bad Request" and nothing.
			// This is not only cosmetic: Overflow() reads provider prose,
			// so a swallowed body turns a full context window into a
			// generic error and costs the caller the exit code that tells
			// it to stop retrying.
			if ae.RawJSON() == "" {
				if body := responseBody(ae.DumpResponse(true)); body != "" {
					e.Msg += " " + body
				}
			}
		}
		return e
	}
	return &Error{Msg: err.Error()}
}

// responseBody returns the body from an httputil response dump: everything
// after the blank line that ends the headers.
func responseBody(dump []byte) string {
	_, body, ok := strings.Cut(string(dump), "\r\n\r\n")
	if !ok {
		return ""
	}
	return strings.TrimSpace(body)
}
