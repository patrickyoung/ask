package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Anthropic speaks the Messages API. Thinking blocks and their signatures
// are captured verbatim and echoed back on later turns; redacted thinking
// arrives as opaque blocks and is replayed the same way.
type Anthropic struct {
	c anthropic.Client
}

// NewAnthropic builds the adapter; base and hc are optional gateway
// overrides ("" and nil mean the real API over the default transport),
// and extra carries the Vertex reshaping options when Google Vertex AI
// is configured (see vertex.go).
func NewAnthropic(key, base string, hc *http.Client, extra ...option.RequestOption) *Anthropic {
	opts := []option.RequestOption{
		option.WithAPIKey(key),
		option.WithMaxRetries(0), // ask owns retries; they must be visible in the log
	}
	if base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	opts = append(opts, extra...)
	return &Anthropic{c: anthropic.NewClient(opts...)}
}

func (p *Anthropic) Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		params, err := anthropicParams(req)
		if err != nil {
			yield(Chunk{}, err)
			return
		}
		stream := p.c.Messages.NewStreaming(ctx, params)
		defer stream.Close()
		var msg anthropic.Message
		stopped := false
		for stream.Next() {
			ev := stream.Current()
			if _, ok := ev.AsAny().(anthropic.MessageStopEvent); ok {
				stopped = true
			}
			if err := msg.Accumulate(ev); err != nil {
				yield(Chunk{}, err)
				return
			}
			if d, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
				switch v := d.Delta.AsAny().(type) {
				case anthropic.TextDelta:
					if !yield(Chunk{Kind: KindText, Text: v.Text}, nil) {
						return
					}
				case anthropic.ThinkingDelta:
					if !yield(Chunk{Kind: KindReasoning, Text: v.Thinking}, nil) {
						return
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(Chunk{}, anthropicErr(err))
			return
		}
		if !stopped || msg.StopReason == "" {
			yield(Chunk{}, errors.New("anthropic: stream ended without a completed message"))
			return
		}
		for _, c := range msg.Content {
			var b Block
			switch v := c.AsAny().(type) {
			case anthropic.TextBlock:
				b = Block{Type: Text, Text: v.Text}
			case anthropic.ThinkingBlock:
				b = Block{Type: Reasoning, Text: v.Thinking, Signature: v.Signature, Provider: "anthropic"}
			case anthropic.RedactedThinkingBlock:
				raw, _ := json.Marshal(v.Data)
				b = Block{Type: Opaque, Provider: "anthropic", Raw: raw}
			default:
				continue
			}
			if !yield(Chunk{Kind: KindBlock, Block: &b}, nil) {
				return
			}
		}
		u := Usage{
			In:         int(msg.Usage.InputTokens),
			Out:        int(msg.Usage.OutputTokens),
			Reasoning:  int(msg.Usage.OutputTokensDetails.ThinkingTokens),
			CacheRead:  int(msg.Usage.CacheReadInputTokens),
			CacheWrite: int(msg.Usage.CacheCreationInputTokens),
		}
		if !yield(Chunk{Kind: KindUsage, Usage: &u}, nil) {
			return
		}
		yield(Chunk{Kind: KindStop, Stop: anthropicStop(msg.StopReason)}, nil)
	}
}

func anthropicParams(req Request) (anthropic.MessageNewParams, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{
			Text:         req.System,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}
	if req.Effort != "off" {
		params.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}
	}
	if len(req.Schema) > 0 {
		params.OutputConfig.Format = anthropic.JSONOutputFormatParam{Schema: schemaObject(req.Schema)}
	}
	for _, m := range Merge(req.Messages) {
		var blocks []anthropic.ContentBlockParamUnion
		for _, b := range m.Blocks {
			switch b.Type {
			case Text:
				blocks = append(blocks, anthropic.NewTextBlock(b.Text))
			case Reasoning:
				if b.Provider == "anthropic" {
					blocks = append(blocks, anthropic.NewThinkingBlock(b.Signature, b.Text))
				} // foreign reasoning cannot be replayed; drop it
			case Media:
				data := base64.StdEncoding.EncodeToString(b.Data)
				if b.MediaType == "application/pdf" {
					blocks = append(blocks, anthropic.NewDocumentBlock(
						anthropic.Base64PDFSourceParam{Data: data}))
					break
				}
				blocks = append(blocks, anthropic.NewImageBlockBase64(b.MediaType, data))
			case Opaque:
				if b.Provider == "anthropic" {
					var data string
					if err := json.Unmarshal(b.Raw, &data); err == nil {
						blocks = append(blocks, anthropic.NewRedactedThinkingBlock(data))
					}
				}
			}
		}
		if len(blocks) == 0 {
			continue
		}
		role := anthropic.MessageParamRoleUser
		if m.Role == Assistant {
			role = anthropic.MessageParamRoleAssistant
		}
		params.Messages = append(params.Messages, anthropic.MessageParam{Role: role, Content: blocks})
	}
	// Moving cache breakpoint: cache everything up to the latest message.
	// The newest message is the one just asked, so its last block is text;
	// a thinking block carries no cache_control field and is left alone.
	if n := len(params.Messages); n > 0 {
		last := params.Messages[n-1].Content
		if m := len(last); m > 0 && last[m-1].OfText != nil {
			last[m-1].OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
	}
	return params, nil
}

func anthropicStop(r anthropic.StopReason) string {
	switch r {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		return "end"
	case anthropic.StopReasonMaxTokens:
		return "max_tokens"
	}
	return string(r)
}

func anthropicErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		e := &Error{Status: ae.StatusCode, Msg: ae.Error()}
		if ae.Response != nil {
			if d, perr := time.ParseDuration(ae.Response.Header.Get("Retry-After") + "s"); perr == nil {
				e.RetryAfter = d
			}
		}
		return e
	}
	return &Error{Msg: err.Error()}
}
