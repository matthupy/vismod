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

// Dropping the TAIL is the truncation that matters and the one a bare chain
// cannot see: the surviving records still link genesis->1->2, so the chain
// is internally perfect. An insider who deletes the last N lines removes
// every block verdict in an incident window and `vismod audit verify` says
// the log is fine. The head anchor written alongside the log is what makes
// the missing records detectable.
func TestTailTruncationDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := l.Record(context.Background(), envFor(id, "block")); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	raw, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(raw), "\n")
	// Keep only the first record: a valid chain of length 1.
	if err := os.WriteFile(path, []byte(lines[0]), 0o600); err != nil {
		t.Fatal(err)
	}

	// Sanity: the surviving chain really is self-consistent, so nothing but
	// the anchor can catch this.
	if _, err := verifyChainOnly(path); err != nil {
		t.Fatalf("premise wrong: truncated chain should self-verify, got %v", err)
	}

	_, err = Verify(path)
	if err == nil {
		t.Fatal("tail truncation must fail verification")
	}
	if !strings.Contains(err.Error(), "truncat") {
		t.Errorf("error %q should name truncation", err)
	}
}

// A crash between the log append and the anchor update leaves the anchor one
// record behind. That must verify clean: an anchor that alarms on ordinary
// crash recovery gets switched off, and a disabled control detects nothing.
func TestAnchorBehindTheChainIsNotTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, _ := Open(path, nil)
	if err := l.Record(context.Background(), envFor("a", "allow")); err != nil {
		t.Fatal(err)
	}
	l.Close()

	// Freeze the anchor at seq 1, then append two more records to the log
	// exactly as a post-crash process would.
	head, ok, err := readHead(path)
	if err != nil || !ok {
		t.Fatalf("readHead = (%v, %v, %v), want a head", head, ok, err)
	}
	l2, _ := Open(path, nil)
	for _, id := range []string{"b", "c"} {
		if err := l2.Record(context.Background(), envFor(id, "allow")); err != nil {
			t.Fatal(err)
		}
	}
	l2.Close()
	if err := writeHead(path, head); err != nil {
		t.Fatal(err)
	}

	n, err := Verify(path)
	if err != nil || n != 3 {
		t.Fatalf("Verify = (%d, %v), want (3, nil): a lagging anchor is not tampering", n, err)
	}
}

// A log written before anchoring existed has no sidecar. It must still
// verify as a plain chain rather than failing closed — otherwise upgrading
// vismod makes every existing audit log unverifiable.
func TestVerifyWithoutAnAnchorFallsBackToChainOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, _ := Open(path, nil)
	if err := l.Record(context.Background(), envFor("a", "allow")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if err := os.Remove(headPath(path)); err != nil {
		t.Fatal(err)
	}

	n, err := Verify(path)
	if err != nil || n != 1 {
		t.Fatalf("Verify = (%d, %v), want (1, nil)", n, err)
	}
}

// Open refuses a truncated log for the same reason it refuses a broken
// chain: appending onto it would rebuild a valid-looking chain over the
// gap and destroy the evidence permanently.
func TestOpenRefusesATruncatedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, _ := Open(path, nil)
	for _, id := range []string{"a", "b"} {
		if err := l.Record(context.Background(), envFor(id, "block")); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	raw, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(raw), "\n")
	if err := os.WriteFile(path, []byte(lines[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, nil); err == nil {
		t.Fatal("Open must refuse to append to a truncated log")
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
