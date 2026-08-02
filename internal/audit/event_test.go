package audit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readPayloads(t *testing.T, path string) []map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode record: %v", err)
		}
		out = append(out, rec.Payload)
	}
	return out
}

// TestAppendEventJoinsTheSameChain: an operational event (the gated
// empty-video override is the one that ships) must be hash-chained with the
// decisions around it. A side channel would let an override be removed
// without breaking verification — the exact thing the chain exists to stop.
func TestAppendEventJoinsTheSameChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(context.Background(), envFor("a", "allow")); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendEvent("empty_video_skip_override", map[string]string{
		"job_id": "b", "asset_id": "clip.mp4", "adapter": "fake",
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(context.Background(), envFor("c", "block")); err != nil {
		t.Fatal(err)
	}
	l.Close()

	n, err := Verify(path)
	if err != nil {
		t.Fatalf("chain with an interleaved event fails verification: %v", err)
	}
	if n != 3 {
		t.Fatalf("Verify counted %d records, want 3", n)
	}

	payloads := readPayloads(t, path)
	event := payloads[1]
	if event["event"] != "empty_video_skip_override" {
		t.Errorf("event kind = %q", event["event"])
	}
	if event["asset_id"] != "clip.mp4" {
		t.Errorf("event fields dropped: %v", event)
	}
}

// TestAppendEventIsNotDeduped: Record is idempotent per JobID because
// at-least-once delivery redelivers the same decision. Events are not
// decisions — two overrides on the same job are two things that happened,
// and collapsing them would understate how often the override fired.
func TestAppendEventIsNotDeduped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := l.AppendEvent("empty_video_skip_override", map[string]string{"job_id": "same"}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	n, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("Verify counted %d event records, want 3", n)
	}
}

// TestAppendEventAfterReopenContinuesTheChain: a restart must not reset the
// sequence or the prev-hash link, or verification of the whole file breaks
// at the seam.
func TestAppendEventAfterReopenContinuesTheChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.AppendEvent("boot", map[string]string{"driver": "memory"}); err != nil {
		t.Fatal(err)
	}
	l.Close()

	l2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := l2.AppendEvent("boot", map[string]string{"driver": "redis"}); err != nil {
		t.Fatal(err)
	}
	l2.Close()

	n, err := Verify(path)
	if err != nil {
		t.Fatalf("chain broken across restart: %v", err)
	}
	if n != 2 {
		t.Errorf("Verify counted %d records, want 2", n)
	}
}
