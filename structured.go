package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const maxSchema = 1 << 20

type structuredOutput struct {
	raw      json.RawMessage
	compiled *jsonschema.Schema
}

// loadSchema reads and compiles one bounded JSON Schema before a session is
// created. The compiler supplies schema semantics; ask adds no dialect or
// reference policy of its own.
func loadSchema(path string) (*structuredOutput, error) {
	var r io.Reader
	var close func() error
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
		r, close = f, f.Close
	}
	if close != nil {
		defer close()
	}
	b, err := io.ReadAll(io.LimitReader(r, maxSchema+1))
	if err != nil {
		return nil, fmt.Errorf("reading schema: %w", err)
	}
	if len(b) > maxSchema {
		return nil, fmt.Errorf("schema is larger than %d MB", maxSchema>>20)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("schema is not valid JSON: %w", err)
	}
	object, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema must be a JSON object")
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	// A hierarchical base makes relative references resolve away from this
	// document, where the compiler's absent URL loader rejects them. Fragment
	// references still resolve inside the resource.
	const location = "https://ask.invalid/schema"
	if err := c.AddResource(location, object); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	compiled, err := c.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &structuredOutput{raw: append(json.RawMessage(nil), b...), compiled: compiled}, nil
}

func (s *structuredOutput) validate(answer string) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewBufferString(answer))
	if err != nil {
		return fmt.Errorf("structured output is not valid JSON: %w", err)
	}
	if err := s.compiled.Validate(doc); err != nil {
		return fmt.Errorf("structured output does not match schema: %w", err)
	}
	return nil
}

func (s *structuredOutput) requestSchema() json.RawMessage {
	if s == nil {
		return nil
	}
	return s.raw
}
