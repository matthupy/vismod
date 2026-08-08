package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

// The pipeline hashes the provider response itself and hands the digest
// over on RawSHA256, because Raw may not enter an envelope. payloadFor
// still hashes an envelope that carries Raw directly, for envelopes built
// outside the pipeline. These pin down which one wins and that neither
// path lets the response itself into the log.

func TestPayloadForUsesTheDigestSuppliedByThePipeline(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	p := payloadFor(result.ResultEnvelope{
		JobID:     queue.JobID("j1"),
		ModelID:   result.ModelIdentity{Adapter: "fake"},
		RawSHA256: digest,
		Result: &moderation.NormalizedResult{
			AssetID: "asset-1",
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow},
		},
	})
	if p["raw_sha256"] != digest {
		t.Errorf("raw_sha256 = %q, want the pipeline-supplied digest %q", p["raw_sha256"], digest)
	}
}

// A pipeline-supplied digest is authoritative. It is computed over the
// evidence the pipeline actually saw — for a video, an array spanning
// every scanned frame — whereas Result.Raw at most holds one response.
// Preferring Raw here would silently narrow the binding.
func TestPipelineDigestWinsOverAnEnvelopeCarryingRaw(t *testing.T) {
	digest := strings.Repeat("cd", 32)
	env := envFor("j2", "block")
	env.RawSHA256 = digest

	p := payloadFor(env)

	if p["raw_sha256"] != digest {
		t.Errorf("raw_sha256 = %q, want %q", p["raw_sha256"], digest)
	}
	sum := sha256.Sum256(env.Result.Raw)
	if p["raw_sha256"] == hex.EncodeToString(sum[:]) {
		t.Error("payloadFor re-hashed Result.Raw instead of using the digest the pipeline computed")
	}
}

// The fallback: an envelope built outside the pipeline still binds by
// hash rather than recording nothing.
func TestPayloadForHashesRawWhenNoDigestWasSupplied(t *testing.T) {
	env := envFor("j3", "flag")

	p := payloadFor(env)

	sum := sha256.Sum256(env.Result.Raw)
	if want := hex.EncodeToString(sum[:]); p["raw_sha256"] != want {
		t.Errorf("raw_sha256 = %q, want %q", p["raw_sha256"], want)
	}
}

// Neither path may put the response in the log, and a supplied digest
// must survive the round trip to disk unchanged.
func TestSuppliedDigestIsRecordedWithoutTheResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	digest := strings.Repeat("ef", 32)
	env := envFor("j4", "allow")
	env.RawSHA256 = digest
	env.Result.Raw = json.RawMessage(`{"provider":"fake","secret_looking":"value"}`)
	if err := l.Record(context.Background(), env); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(b), digest) {
		t.Errorf("the supplied digest is not in the record:\n%s", b)
	}
	if strings.Contains(string(b), "secret_looking") {
		t.Fatal("audit log stored the provider response itself")
	}
	if _, err := Verify(path); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}
