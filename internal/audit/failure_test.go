package audit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/result"
)

// failingSigner stands in for a signer whose key material has gone away
// (revoked key, unreachable KMS).
type failingSigner struct{}

func (failingSigner) Sign([]byte) ([]byte, error) { return nil, errors.New("signing key unavailable") }

// staticSigner is a deterministic stand-in for a real HMAC/Ed25519 signer.
type staticSigner struct{}

func (staticSigner) Sign(h []byte) ([]byte, error) { return append([]byte("sig:"), h[:4]...), nil }

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return path
}

// TestOpenRefusesAnUndecodableLog: a log that cannot be replayed cannot be
// verified, and appending to it would produce a chain no one can check.
// Refusing to boot is the fail-safe answer.
func TestOpenRefusesAnUndecodableLog(t *testing.T) {
	path := writeLog(t, `{"seq":1,`)
	if _, err := Open(path, nil); err == nil {
		t.Fatal("Open accepted a log it could not parse")
	}
}

// TestOpenRefusesAnUnopenablePath: no audit log means decisions no one can
// account for later. That is a boot failure, not a degraded mode.
func TestOpenRefusesAnUnopenablePath(t *testing.T) {
	if _, err := Open(t.TempDir(), nil); err == nil { // a directory
		t.Fatal("Open succeeded on a path it cannot append to")
	}
}

// TestSignerFailureRefusesTheAppend: with a Signer configured, an entry
// that cannot be signed must not be written unsigned. A silently unsigned
// record would look identical to a tampered one during verification.
func TestSignerFailureRefusesTheAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, failingSigner{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = l.Close() }()

	if err := l.Record(context.Background(), envFor("a", "allow")); err == nil {
		t.Fatal("a failed signature was accepted; the record would be written unsigned")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("a record was appended despite the signing failure: %s", b)
	}
}

// TestSignedRecordsCarryTheirSignature: the Signer seam is the documented
// tamper-RESISTANT upgrade. A signature that never reaches the record
// would leave operators believing in a protection they do not have.
func TestSignedRecordsCarryTheirSignature(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, staticSigner{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Record(context.Background(), envFor("a", "allow")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if rec.Signature == "" {
		t.Error("a signed log wrote an unsigned record")
	}

	// Signing must not change the chain itself: Verify covers the hash
	// chain and is signature-agnostic by design.
	if n, err := Verify(path); err != nil || n != 1 {
		t.Errorf("Verify = (%d, %v), want (1, nil)", n, err)
	}
}

// TestRecordAfterCloseFails: an append that cannot reach disk must be
// reported. Swallowing it would drop the decision from the trail while the
// pipeline believes it was recorded.
func TestRecordAfterCloseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Record(context.Background(), envFor("a", "allow")); err == nil {
		t.Error("an append to a closed log reported success")
	}
}

// TestPayloadForErrorEnvelopeWithoutAResult: a job that failed before any
// result existed still gets an audit record, and that record must say
// verdict=error rather than leaving the field empty — an empty verdict
// reads as "not decided" when the decision was in fact "could not score".
func TestPayloadForErrorEnvelopeWithoutAResult(t *testing.T) {
	p := payloadFor(result.ResultEnvelope{
		JobID:   queue.JobID("boom"),
		ModelID: result.ModelIdentity{Adapter: "fake"},
		Error:   "extraction failed",
	})
	if p["verdict"] != "error" {
		t.Errorf("verdict = %q, want error for a result-less failure", p["verdict"])
	}
	if p["raw_sha256"] != "" {
		t.Errorf("raw_sha256 = %q, want empty when there is no Raw", p["raw_sha256"])
	}
}

// TestVerifyDetectsTampering covers each way the chain can be broken. Every
// one of them must be reported with the offending position, because a
// tamper-evident log that reports "fine" is worse than no log at all.
func TestVerifyDetectsTampering(t *testing.T) {
	// Build a valid two-record chain to mutate.
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if err := l.Record(context.Background(), envFor(id, "allow")); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	_ = l.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 records, got %d", len(lines))
	}

	decode := func(s string) Record {
		var r Record
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return r
	}
	encode := func(r Record) string {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return string(b)
	}

	t.Run("payload edited in place", func(t *testing.T) {
		r := decode(lines[1])
		r.Payload["verdict"] = "allow-but-actually-blocked"
		p := writeLog(t, lines[0], encode(r))
		n, err := Verify(p)
		if err == nil {
			t.Fatal("an edited payload verified clean")
		}
		if n != 1 {
			t.Errorf("Verify reported %d good records before the break, want 1", n)
		}
	})

	t.Run("record removed (truncation)", func(t *testing.T) {
		// Dropping the FIRST record leaves the second with seq 2 at
		// position 1: the seq check is what catches a truncated head.
		p := writeLog(t, lines[1])
		if _, err := Verify(p); err == nil {
			t.Fatal("a truncated chain verified clean")
		}
	})

	t.Run("prev_hash rewritten", func(t *testing.T) {
		r := decode(lines[1])
		r.PrevHash = strings.Repeat("0", hashHexLen)
		p := writeLog(t, lines[0], encode(r))
		if _, err := Verify(p); err == nil {
			t.Fatal("a rewritten prev_hash verified clean")
		}
	})

	t.Run("malformed entry_hash", func(t *testing.T) {
		r := decode(lines[0])
		r.EntryHash = "short"
		p := writeLog(t, encode(r))
		if _, err := Verify(p); err == nil {
			t.Fatal("a malformed entry_hash verified clean")
		}
	})

	t.Run("undecodable line", func(t *testing.T) {
		p := writeLog(t, lines[0], "{not json")
		if _, err := Verify(p); err == nil {
			t.Fatal("an undecodable line verified clean")
		}
	})

	t.Run("blank lines are ignored", func(t *testing.T) {
		// Editors and log shippers add trailing newlines; a blank line is
		// not evidence of tampering and must not fail verification.
		p := writeLog(t, lines[0], "", lines[1], "")
		n, err := Verify(p)
		if err != nil || n != 2 {
			t.Errorf("Verify = (%d, %v), want (2, nil) with blank lines present", n, err)
		}
	})
}

// TestVerifyMissingLogIsEmptyNotAnError: a worker that has recorded nothing
// yet has no file. That is an empty chain, not a broken one.
func TestVerifyMissingLogIsEmptyNotAnError(t *testing.T) {
	n, err := Verify(filepath.Join(t.TempDir(), "never-written.log"))
	if err != nil || n != 0 {
		t.Errorf("Verify(missing) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestVerifyUnreadablePathIsAnError: a log that exists but cannot be read
// must not report a clean chain of zero records.
func TestVerifyUnreadablePathIsAnError(t *testing.T) {
	if _, err := Verify(t.TempDir()); err == nil { // a directory
		t.Error("Verify reported a clean chain for an unreadable path")
	}
}
