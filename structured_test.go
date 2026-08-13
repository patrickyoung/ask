package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const numberSchema = `{
  "type": "object",
  "properties": {"n": {"type": "integer"}},
  "required": ["n"],
  "additionalProperties": false
}`

func writeSchema(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "answer.schema.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStructuredSchemaCompilesAndValidates(t *testing.T) {
	output, err := loadSchema(writeSchema(t, numberSchema))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output.requestSchema()), `"type": "object"`) {
		t.Errorf("raw schema = %s", output.requestSchema())
	}
	if err := output.validate(`{"n":7}`); err != nil {
		t.Errorf("valid answer rejected: %v", err)
	}
	if err := output.validate(`{"n":"seven"}`); err == nil || !strings.Contains(err.Error(), "does not match schema") {
		t.Errorf("schema mismatch = %v", err)
	}
	if err := output.validate(`not json`); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("invalid JSON = %v", err)
	}
}

func TestStructuredSchemaRejectsBadInput(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
	}{
		{"invalid json", `{`, "not valid JSON"},
		{"not object", `[]`, "must be a JSON object"},
		{"bad schema", `{"type":"wat"}`, "schema:"},
		{"external ref", `{"$ref":"other.json"}`, "schema:"},
		{"unknown dialect", `{"$schema":"https://example.com/schema"}`, "schema:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadSchema(writeSchema(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestStructuredSchemaHasABound(t *testing.T) {
	path := writeSchema(t, `{"description":"`+strings.Repeat("x", maxSchema)+`"}`)
	_, err := loadSchema(path)
	if err == nil || !strings.Contains(err.Error(), "larger than 1 MB") {
		t.Fatalf("oversize schema error = %v", err)
	}
}

func TestSchemaDoesNotInterpretLiteralRefKeys(t *testing.T) {
	path := writeSchema(t, `{"const":{"$ref":"literal data"}}`)
	if _, err := loadSchema(path); err != nil {
		t.Fatal(err)
	}
}
