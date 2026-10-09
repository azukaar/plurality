package mcp

import (
	"encoding/json"
	"testing"
)

// Regression test for the vision-turn Gemini 400: array-typed tool parameters
// must always carry a schema-level "items" so Google's GenerateContentRequest
// validation accepts the tool declarations (previously "items: missing field").
func TestSchemaToParametersArrayItems(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"fields": {"type": "array", "items": {"type": "string"}},
			"engines": {"type": "array"},
			"labels": {"type": "array", "items": {"type": "string"}, "description": "labels"}
		}
	}`)

	params := schemaToParameters(raw)
	if params == nil {
		t.Fatal("expected non-nil params")
	}

	fields, ok := params.Properties["fields"]
	if !ok {
		t.Fatal("missing fields property")
	}
	if fields.Items == nil {
		t.Fatal("fields array property must carry items (Gemini rejects arrays without it)")
	}
	if fields.Items.Type != "string" {
		t.Fatalf("fields items type = %q, want string", fields.Items.Type)
	}

	engines, ok := params.Properties["engines"]
	if !ok {
		t.Fatal("missing engines property")
	}
	if engines.Items == nil || engines.Items.Type != "string" {
		t.Fatalf("engines array property must default to items string, got %+v", engines.Items)
	}

	labels, ok := params.Properties["labels"]
	if !ok {
		t.Fatal("missing labels property")
	}
	if labels.Items == nil || labels.Items.Type != "string" {
		t.Fatalf("labels items type = %+v, want string", labels.Items)
	}
}
