package audit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

func envFor(id, verdict string) result.ResultEnvelope {
	return result.ResultEnvelope{
		JobID:   queue.JobID(id),
		ModelID: result.ModelIdentity{Adapter: "fake", ModelVersion: "v1", ConfigHash: "h"},
		Result: &moderation.NormalizedResult{
			AssetID: "asset-" + id,
			Overall: moderation.OverallVerdict{Verdict: moderation.Verdict(verdict)},
			Raw:     json.RawMessage(`{"provider":"fake"}`),
		},
	}
}

func TestChainAppendAndVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := l.Record(context.Background(), envFor(id, "allow")); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	n, err := Verify(path)
	if err != nil || n != 3 {
		t.Fatalf("Verify = (%d, %v), want (3, nil)", n, err)
	}
}

func TestIdempotentPerJobID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, _ := Open(path, nil)
	for range 3 {
		if err := l.Record(context.Background(), envFor("same", "flag")); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	n, err := Verify(path)
	if err != nil || n != 1 {
		t.Fatalf("redelivered job must not re-append: got %d records, err=%v", n, err)
	}

	// Idempotency survives restart (replay rebuilds the dedupe set).
	l2, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Record(context.Background(), envFor("same", "flag")); err != nil {
		t.Fatal(err)
	}
	l2.Close()
	if n, _ := Verify(path); n != 1 {
		t.Fatalf("post-restart re-append: %d records, want 1", n)
	}
}

func TestTamperDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, _ := Open(path, nil)
	for _, id := range []string{"a", "b", "c"} {
		_ = l.Record(context.Background(), envFor(id, "allow"))
	}
	l.Close()

	// Tamper: flip a verdict in record 2.
	raw, _ := os.ReadFile(path)
	tampered := strings.Replace(string(raw), `"verdict":"allow"`, `"verdict":"block"`, 2)
	tampered = strings.Replace(tampered, `"verdict":"block"`, `"verdict":"allow"`, 1) // restore record 1
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Verify(path); err == nil {
		t.Fatal("tampered chain must fail verification")
	}
	// Appending to a tampered log must be refused at Open.
	if _, err := Open(path, nil); err == nil {
		t.Fatal("Open must refuse a broken chain")
	}
}

func TestTruncationDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, _ := Open(path, nil)
	for _, id := range []string{"a", "b", "c"} {
		_ = l.Record(context.Background(), envFor(id, "allow"))
	}
	l.Close()

	raw, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(raw), "\n")
	// Drop the middle record: seq becomes non-contiguous.
	if err := os.WriteFile(path, []byte(lines[0]+lines[2]), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path); err == nil {
		t.Fatal("gap in chain must fail verification")
	}
}

func TestNeverStoresRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, _ := Open(path, nil)
	_ = l.Record(context.Background(), envFor("a", "allow"))
	l.Close()
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), `"provider":"fake"`) {
		t.Fatal("audit log must store SHA-256(Raw), never Raw itself")
	}
	if !strings.Contains(string(raw), `"raw_sha256":"`) {
		t.Fatal("audit log must bind the decision to inputs by hash")
	}
}
