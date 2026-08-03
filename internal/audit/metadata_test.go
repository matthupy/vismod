package audit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

func metaEnvelope(meta json.RawMessage) result.ResultEnvelope {
	return result.ResultEnvelope{
		JobID:    queue.JobID("job-1"),
		Source:   moderation.Source{Kind: "file", Ref: "/data/a.png", MediaType: "image"},
		ModelID:  result.ModelIdentity{Adapter: "fake", ModelVersion: "v1", ConfigHash: "h"},
		Metadata: meta,
		Result: &moderation.NormalizedResult{
			SchemaVersion: moderation.SchemaVersion,
			AssetID:       "asset-1",
			MediaType:     "image",
			Overall:       moderation.OverallVerdict{Verdict: moderation.VerdictAllow},
		},
	}
}

// The audit chain must stay free of caller free text. This locks that
// invariant: it must be structurally impossible for payloadFor to copy
// envelope metadata into the hash-chained log, whether or not a caller
// supplied any.
//
// NOTE: we deliberately do NOT compare entry_hash across the two writes
// below. entryHash mixes in Record.Timestamp (a wall-clock value captured
// per append in appendLocked), so two logs written at different instants
// will almost never produce equal hashes even when their payloads are
// identical in every field that matters. Comparing hashes here would be a
// flaky assertion, not a real guard. Instead we assert directly on what
// we care about: the raw file bytes never contain the metadata marker or
// the word "metadata", and the decoded Payload has the same key set (and
// the same values for the non-timestamp-derived fields) regardless of
// whether metadata was present on the envelope.
func TestAuditIgnoresEnvelopeMetadata(t *testing.T) {
	write := func(t *testing.T, meta json.RawMessage) (fileContents string, rec Record) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "audit.jsonl")
		l, err := Open(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Record(context.Background(), metaEnvelope(meta)); err != nil {
			t.Fatal(err)
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
			t.Fatal(err)
		}
		return string(b), rec
	}

	plainFile, plainRec := write(t, nil)
	metaFile, metaRec := write(t, json.RawMessage(`{"secret_marker":"do-not-audit-me"}`))

	if strings.Contains(metaFile, "do-not-audit-me") {
		t.Errorf("caller metadata marker leaked into the audit log:\n%s", metaFile)
	}
	if strings.Contains(metaFile, "metadata") {
		t.Errorf("the word \"metadata\" must never appear in the audit log:\n%s", metaFile)
	}
	if strings.Contains(plainFile, "do-not-audit-me") || strings.Contains(plainFile, "metadata") {
		t.Errorf("sanity check failed: plain-envelope log unexpectedly mentions metadata:\n%s", plainFile)
	}

	plainKeys := sortedKeys(plainRec.Payload)
	metaKeys := sortedKeys(metaRec.Payload)
	if strings.Join(plainKeys, ",") != strings.Join(metaKeys, ",") {
		t.Fatalf("payload key sets differ between no-metadata and with-metadata runs: %v vs %v", plainKeys, metaKeys)
	}

	// The two envelopes are identical except for Metadata, and each write
	// goes to its own fresh temp-dir log, so every payload field must
	// match exactly.
	for _, k := range plainKeys {
		if plainRec.Payload[k] != metaRec.Payload[k] {
			t.Errorf("payload[%q] differs: %q (no metadata) vs %q (with metadata)", k, plainRec.Payload[k], metaRec.Payload[k])
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
