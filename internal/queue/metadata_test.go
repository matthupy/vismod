package queue

import (
	"encoding/json"
	"strings"
	"testing"
)

// objectOfSize builds a compacted JSON object of exactly n bytes.
// `{"k":"` is 6 bytes and `"}` is 2, so the value is n-8 bytes.
func objectOfSize(n int) json.RawMessage {
	return json.RawMessage(`{"k":"` + strings.Repeat("a", n-8) + `"}`)
}

func TestValidateMetadataAccepts(t *testing.T) {
	tests := map[string]json.RawMessage{
		"empty is absent": nil,
		"simple object":   json.RawMessage(`{"ticket":"T-1"}`),
		"nested object":   json.RawMessage(`{"a":{"b":[1,2,{"c":true}]}}`),
		"empty object":    json.RawMessage(`{}`),
		"exactly the cap": objectOfSize(MaxMetadataBytes),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ValidateMetadata(in)
			if err != nil {
				t.Fatalf("ValidateMetadata(%s) = error %v, want accepted", in, err)
			}
			if len(in) == 0 && got != nil {
				t.Errorf("absent metadata must stay nil, got %s", got)
			}
		})
	}
}

func TestValidateMetadataRejects(t *testing.T) {
	tests := map[string]json.RawMessage{
		"array":            json.RawMessage(`["a","b"]`),
		"string scalar":    json.RawMessage(`"hello"`),
		"number scalar":    json.RawMessage(`42`),
		"null literal":     json.RawMessage(`null`),
		"malformed json":   json.RawMessage(`{"a":`),
		"trailing garbage": json.RawMessage(`{"a":1} oops`),
		"one over the cap": objectOfSize(MaxMetadataBytes + 1),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateMetadata(in); err == nil {
				t.Fatalf("ValidateMetadata(%.40s) = nil error, want rejected", in)
			}
		})
	}
}

// The cap measures CONTENT, not formatting: an object that is oversize
// as submitted but in-bounds once compacted is accepted, and what gets
// stored is the compacted form.
func TestValidateMetadataCompacts(t *testing.T) {
	padded := json.RawMessage("{\n\t\"ticket\"  :  \"T-1\"" +
		strings.Repeat(" ", MaxMetadataBytes) + "\n}")
	got, err := ValidateMetadata(padded)
	if err != nil {
		t.Fatalf("padded object must be accepted after compaction: %v", err)
	}
	if string(got) != `{"ticket":"T-1"}` {
		t.Errorf("metadata must be stored compacted, got %s", got)
	}
	if len(got) > MaxMetadataBytes {
		t.Errorf("compacted metadata must be within the cap, got %d bytes", len(got))
	}
}
