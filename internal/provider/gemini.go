package provider

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"

	"google.golang.org/genai"
)

// Gemini speaks the Gen AI API. Thought signatures attach to individual
// parts, so each assistant turn also carries one opaque block holding the
// full model Content; replay to Gemini uses that verbatim and ignores the
// normalized blocks.
type Gemini struct {
	key  string
	base string       // alternate endpoint; "" means the real API. Tests point this at a fixture server.
	hc   *http.Client // optional authenticated transport for gateways
}

// NewGemini builds the adapter; base and hc are optional gateway
// overrides ("" and nil mean the real API over the default transport).
func NewGemini(key, base string, hc *http.Client) *Gemini {
	return &Gemini{key: key, base: base, hc: hc}
}

func (p *Gemini) Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		client, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:      p.key,
			Backend:     genai.BackendGeminiAPI,
			HTTPOptions: genai.HTTPOptions{BaseURL: p.base},
			HTTPClient:  p.hc,
		})
		if err != nil {
			yield(Chunk{}, geminiErr(err))
			return
		}
		cfg, contents, err := geminiParams(req)
		if err != nil {
			yield(Chunk{}, err)
			return
		}
		content := genai.Content{Role: genai.RoleModel}
		var usage *genai.GenerateContentResponseUsageMetadata
		stop := ""
		for resp, err := range client.Models.GenerateContentStream(ctx, req.Model, contents, cfg) {
			if err != nil {
				yield(Chunk{}, geminiErr(err))
				return
			}
			if len(resp.Candidates) == 0 {
				continue
			}
			cand := resp.Candidates[0]
			if cand.Content != nil {
				for _, part := range cand.Content.Parts {
					accumulate(&content, part)
					switch {
					case part.Thought && part.Text != "":
						if !yield(Chunk{Kind: KindReasoning, Text: part.Text}, nil) {
							return
						}
					case part.Text != "":
						if !yield(Chunk{Kind: KindText, Text: part.Text}, nil) {
							return
						}
					}
				}
			}
			if cand.FinishReason != "" {
				stop = geminiStop(cand.FinishReason)
			}
			if resp.UsageMetadata != nil {
				usage = resp.UsageMetadata
			}
		}
		if stop == "" {
			yield(Chunk{}, errors.New("gemini: stream ended without a finish reason"))
			return
		}
		for _, part := range content.Parts {
			var b Block
			switch {
			case part.Thought:
				b = Block{Type: Reasoning, Text: part.Text, Provider: "gemini"}
			case part.Text != "":
				b = Block{Type: Text, Text: part.Text}
			default:
				continue
			}
			if !yield(Chunk{Kind: KindBlock, Block: &b}, nil) {
				return
			}
		}
		// The verbatim Content, thought signatures and all, for replay.
		if len(content.Parts) > 0 {
			raw, err := json.Marshal(content)
			if err != nil {
				yield(Chunk{}, err)
				return
			}
			if !yield(Chunk{Kind: KindBlock, Block: &Block{Type: Opaque, Provider: "gemini", Raw: raw}}, nil) {
				return
			}
		}
		u := Usage{}
		if usage != nil {
			u = Usage{
				In:        int(usage.PromptTokenCount),
				Out:       int(usage.CandidatesTokenCount),
				Reasoning: int(usage.ThoughtsTokenCount),
				CacheRead: int(usage.CachedContentTokenCount),
			}
		}
		if !yield(Chunk{Kind: KindUsage, Usage: &u}, nil) {
			return
		}
		yield(Chunk{Kind: KindStop, Stop: stop}, nil)
	}
}

// accumulate appends a streamed part, merging adjacent text deltas of the
// same kind. A thought signature arriving on a delta sticks to the merged
// part.
func accumulate(c *genai.Content, p *genai.Part) {
	if n := len(c.Parts); n > 0 {
		if last := c.Parts[n-1]; last.Thought == p.Thought {
			last.Text += p.Text
			if len(p.ThoughtSignature) > 0 {
				last.ThoughtSignature = p.ThoughtSignature
			}
			return
		}
	}
	cp := *p
	c.Parts = append(c.Parts, &cp)
}

func geminiParams(req Request) (*genai.GenerateContentConfig, []*genai.Content, error) {
	cfg := &genai.GenerateContentConfig{}
	if req.System != "" {
		cfg.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: req.System}}}
	}
	if req.MaxTokens > 0 {
		cfg.MaxOutputTokens = int32(req.MaxTokens)
	}
	if req.Effort != "off" {
		cfg.ThinkingConfig = &genai.ThinkingConfig{IncludeThoughts: true}
	}
	if len(req.Schema) > 0 {
		cfg.ResponseMIMEType = "application/json"
		// The SDK deep-copies config through float64. Set the native schema
		// after that conversion, immediately before request serialization.
		cfg.HTTPOptions = &genai.HTTPOptions{
			ExtrasRequestProvider: func(body map[string]any) map[string]any {
				body["generationConfig"].(map[string]any)["responseJsonSchema"] = req.Schema
				return body
			},
		}
	}
	var contents []*genai.Content
	for _, m := range Merge(req.Messages) {
		if m.Role == Assistant {
			if c := geminiOpaque(m); c != nil {
				contents = append(contents, c)
				continue
			}
		}
		c := &genai.Content{Role: genai.RoleUser}
		if m.Role == Assistant {
			c.Role = genai.RoleModel
		}
		for _, b := range m.Blocks {
			switch b.Type {
			case Text:
				c.Parts = append(c.Parts, &genai.Part{Text: b.Text})
			case Media:
				// Gemini takes the raw bytes; the SDK base64s them.
				c.Parts = append(c.Parts, &genai.Part{
					InlineData: &genai.Blob{MIMEType: b.MediaType, Data: b.Data},
				})
			case Reasoning: // foreign or signature-less; not replayable
			}
		}
		if len(c.Parts) > 0 {
			contents = append(contents, c)
		}
	}
	return cfg, contents, nil
}

// geminiOpaque restores the verbatim model Content when this turn came from
// Gemini; nil means build from normalized blocks instead.
func geminiOpaque(m Message) *genai.Content {
	for _, b := range m.Blocks {
		if b.Type == Opaque && b.Provider == "gemini" {
			var c genai.Content
			if err := json.Unmarshal(b.Raw, &c); err == nil {
				return &c
			}
		}
	}
	return nil
}

func geminiStop(r genai.FinishReason) string {
	switch r {
	case genai.FinishReasonStop:
		return "end"
	case genai.FinishReasonMaxTokens:
		return "max_tokens"
	}
	return string(r)
}

func geminiErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ae genai.APIError
	if errors.As(err, &ae) {
		return &Error{Status: ae.Code, Msg: ae.Message}
	}
	return &Error{Msg: err.Error()}
}
