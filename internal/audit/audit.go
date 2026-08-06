// Package audit implements the tamper-EVIDENT decision log: append-only,
// hash-chained records binding each verdict to its inputs by hash. It
// never stores media bytes or provider Raw payloads — only SHA-256(Raw),
// the ModelIdentity, and the verdict.
//
// Honest scope: a bare hash chain detects truncation and in-place edits,
// not a full-chain rewrite by a write-capable insider. The tamper-
// RESISTANT upgrade seam (HMAC/Ed25519 signing or head-hash anchoring) is
// the Signer interface below. See SECURITY.md.
package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

// Signer is the tamper-resistant upgrade seam: implementations sign each
// entry hash (HMAC, Ed25519) or anchor the head hash externally. v1 ships
// no default Signer; the chain alone is tamper-evident only.
type Signer interface {
	Sign(entryHash []byte) ([]byte, error)
}

// Verifier is the read side of Signer. Verify (and `vismod audit verify`)
// stay signature-agnostic by design — checking a signature requires the key,
// which the verifying process may not hold. Pass a Verifier to VerifyWith
// when it does; without one, a signed log is still only chain-verified and
// the signature is decoration.
type Verifier interface {
	VerifySignature(entryHash, signature []byte) error
}

// Head is the anchor: the (seq, entry_hash) of the last record known to
// have been written. It is what makes TAIL truncation detectable.
//
// Dropping the final N records leaves a chain that is internally perfect —
// genesis still links to 1 to 2 — so nothing inside the file can reveal the
// loss. Verify compares the file against this anchor instead.
//
// Honest scope, consistent with the package doc: the default anchor is a
// sidecar on the same filesystem, so an insider with write access to both
// can update them together. What it buys is that truncation is no longer a
// SINGLE unlogged operation, and that accidental loss (partial copy, log
// rotation, truncated restore) is always caught. Real resistance needs the
// head shipped off-box — pass VerifyOptions.Anchor from wherever that is.
type Head struct {
	Seq       uint64 `json:"seq"`
	EntryHash string `json:"entry_hash"`
}

// VerifyOptions configures VerifyWith. The zero value equals Verify.
type VerifyOptions struct {
	// Anchor is the expected head. Nil reads the sidecar next to the log;
	// pass one explicitly to check against an externally-held head.
	Anchor *Head
	// Verifier checks each record's signature. Nil skips signatures.
	Verifier Verifier
}

// Record is one audit log line.
type Record struct {
	Seq       uint64            `json:"seq"`
	Timestamp string            `json:"timestamp"` // RFC 3339 UTC, nanoseconds
	PrevHash  string            `json:"prev_hash"` // hex; genesis = 64 zeros
	Payload   map[string]string `json:"payload"`
	EntryHash string            `json:"entry_hash"`
	Signature string            `json:"signature,omitempty"`
}

const hashHexLen = 64

var genesisPrev = make([]byte, 32)

// Log is the append-only, hash-chained audit log. Appends are idempotent
// per JobID: the JobID is looked up under the append lock first; if
// present the append is skipped and no new seq is consumed.
type Log struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	seq    uint64
	prev   []byte
	seen   map[string]bool
	signer Signer
}

// Open opens (or creates) the audit log at path with O_APPEND, replaying
// the existing chain to restore seq, prev-hash, and the JobID dedupe set.
// A corrupt existing chain is a fatal boot error: appending to a broken
// chain would mask tampering.
func Open(path string, signer Signer) (*Log, error) {
	existing, err := readRecords(path)
	if err != nil {
		return nil, fmt.Errorf("audit: replay %s: %w", path, err)
	}
	l := &Log{seen: map[string]bool{}, prev: genesisPrev, signer: signer, path: path}
	for i, r := range existing {
		if err := verifyRecord(r, uint64(i+1), l.prev); err != nil {
			return nil, fmt.Errorf("audit: existing chain is broken at seq %d: %w (refusing to append to a tampered log)", r.Seq, err)
		}
		l.seq = r.Seq
		l.prev, _ = hex.DecodeString(r.EntryHash)
		if id := r.Payload["job_id"]; id != "" {
			l.seen[id] = true
		}
	}
	// Refuse a truncated log for the same reason a broken chain is fatal:
	// appending would relink a valid-looking chain over the gap, which
	// destroys the only evidence that records went missing.
	if err := checkAnchor(path, existing, nil); err != nil {
		return nil, fmt.Errorf("%w (refusing to append to a truncated log)", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	l.f = f
	return l, nil
}

// Record appends one entry for a completed job (idempotent per JobID).
func (l *Log) Record(_ context.Context, env result.ResultEnvelope) error {
	payload := payloadFor(env)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen[payload["job_id"]] {
		return nil // idempotent: no new seq, no duplicate append
	}
	return l.appendLocked(payload)
}

// AppendEvent records an operational event (e.g. the gated fail-safe
// override) into the same chain.
func (l *Log) AppendEvent(kind string, fields map[string]string) error {
	payload := map[string]string{"event": kind}
	for k, v := range fields {
		payload[k] = v
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(payload)
}

func (l *Log) appendLocked(payload map[string]string) error {
	seq := l.seq + 1
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	entry, err := entryHash(seq, ts, l.prev, payload)
	if err != nil {
		return err
	}
	rec := Record{
		Seq:       seq,
		Timestamp: ts,
		PrevHash:  hex.EncodeToString(l.prev),
		Payload:   payload,
		EntryHash: hex.EncodeToString(entry),
	}
	if l.signer != nil {
		sig, err := l.signer.Sign(entry)
		if err != nil {
			return fmt.Errorf("audit: sign: %w", err)
		}
		rec.Signature = hex.EncodeToString(sig)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("audit: append: %w", err)
	}
	// Advance the anchor after the record is on disk, never before: an
	// anchor ahead of the log is indistinguishable from truncation and
	// would fail every later verification. Lagging by one after a crash is
	// the safe direction and checkAnchor tolerates it.
	if err := writeHead(l.path, Head{Seq: seq, EntryHash: rec.EntryHash}); err != nil {
		return fmt.Errorf("audit: write head anchor: %w", err)
	}
	l.seq = seq
	l.prev = entry
	if id := payload["job_id"]; id != "" {
		l.seen[id] = true
	}
	return nil
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// payloadFor binds the decision to its inputs BY HASH: SHA-256(Raw) plus
// ModelIdentity plus verdict. Raw itself, media bytes, and free-text never
// enter the audit log.
func payloadFor(env result.ResultEnvelope) map[string]string {
	p := map[string]string{
		"job_id":        string(env.JobID),
		"asset_id":      "",
		"adapter":       env.ModelID.Adapter,
		"model_version": env.ModelID.ModelVersion,
		"config_hash":   env.ModelID.ConfigHash,
		"verdict":       "",
		"raw_sha256":    "",
	}
	if env.Result != nil {
		p["asset_id"] = env.Result.AssetID
		p["verdict"] = string(env.Result.Overall.Verdict)
		if len(env.Result.Raw) > 0 {
			sum := sha256.Sum256(env.Result.Raw)
			p["raw_sha256"] = hex.EncodeToString(sum[:])
		}
	} else if env.Error != "" {
		p["verdict"] = string(moderation.VerdictError)
	}
	return p
}

// entryHash = SHA-256(seq ‖ timestamp ‖ prev_hash ‖ canonical(payload))
// with fixed encodings: seq as 8-byte big-endian; timestamp and payload
// length-prefixed (8-byte BE length); prev_hash as raw 32 bytes.
func entryHash(seq uint64, ts string, prev []byte, payload map[string]string) ([]byte, error) {
	canon, err := canonicalJSON(payload)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	var seqB [8]byte
	binary.BigEndian.PutUint64(seqB[:], seq)
	h.Write(seqB[:])
	writeLenPrefixed(h.Write, []byte(ts))
	h.Write(prev)
	writeLenPrefixed(h.Write, canon)
	return h.Sum(nil), nil
}

// writeLenPrefixed feeds w the 8-byte big-endian length of b followed by b
// itself. w is always a hash.Hash's Write, which is documented never to
// return an error, so the returns are discarded explicitly rather than
// propagated — there is no failure mode here to fail safe against.
func writeLenPrefixed(w func([]byte) (int, error), b []byte) {
	var lenB [8]byte
	binary.BigEndian.PutUint64(lenB[:], uint64(len(b)))
	_, _ = w(lenB[:])
	_, _ = w(b)
}

// canonicalJSON emits the payload per RFC 8785 JCS. The payload is a flat
// map of strings, for which JCS reduces to: sorted keys, compact
// separators, UTF-8, minimal escaping (no HTML escaping).
func canonicalJSON(payload map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeJCSString(&buf, k); err != nil {
			return nil, err
		}
		buf.WriteByte(':')
		if err := writeJCSString(&buf, payload[k]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func writeJCSString(buf *bytes.Buffer, s string) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	// json.Encoder appends a newline; strip it.
	buf.Truncate(buf.Len() - 1)
	return nil
}

// Verify recomputes the whole chain at path and returns the number of
// valid records, reporting the FIRST broken link it finds. It also checks
// the log against its head anchor, which is what detects tail truncation.
// Signatures are not checked; see Verifier and VerifyWith.
func Verify(path string) (int, error) {
	return VerifyWith(path, VerifyOptions{})
}

// VerifyWith is Verify with an explicit anchor and/or signature verifier.
func VerifyWith(path string, opts VerifyOptions) (int, error) {
	records, err := readRecords(path)
	if err != nil {
		return 0, err
	}
	prev := genesisPrev
	for i, r := range records {
		if err := verifyRecord(r, uint64(i+1), prev); err != nil {
			return i, fmt.Errorf("audit: chain broken at seq %d (record %d): %w", r.Seq, i+1, err)
		}
		if opts.Verifier != nil {
			if err := verifySignature(opts.Verifier, r); err != nil {
				return i, fmt.Errorf("audit: signature invalid at seq %d (record %d): %w", r.Seq, i+1, err)
			}
		}
		prev, _ = hex.DecodeString(r.EntryHash)
	}
	if err := checkAnchor(path, records, opts.Anchor); err != nil {
		return len(records), err
	}
	return len(records), nil
}

// checkAnchor reports whether the log still contains the record the anchor
// names. A chain LONGER than the anchor is normal — a crash between the
// append and the anchor update leaves the anchor one behind, and refusing
// that would make ordinary recovery look like an attack.
func checkAnchor(path string, records []Record, anchor *Head) error {
	if anchor == nil {
		h, ok, err := readHead(path)
		if err != nil {
			return fmt.Errorf("audit: read head anchor: %w", err)
		}
		if !ok {
			return nil // logs written before anchoring: chain-only
		}
		anchor = &h
	}
	if anchor.Seq == 0 {
		return nil
	}
	if uint64(len(records)) < anchor.Seq {
		return fmt.Errorf("audit: log is truncated: head anchor names seq %d but the log ends at seq %d (%d record(s) missing)",
			anchor.Seq, len(records), anchor.Seq-uint64(len(records)))
	}
	if got := records[anchor.Seq-1].EntryHash; got != anchor.EntryHash {
		return fmt.Errorf("audit: record at seq %d does not match the head anchor (rewritten history)", anchor.Seq)
	}
	return nil
}

func verifySignature(v Verifier, r Record) error {
	if r.Signature == "" {
		return fmt.Errorf("record is unsigned")
	}
	sig, err := hex.DecodeString(r.Signature)
	if err != nil {
		return fmt.Errorf("malformed signature: %w", err)
	}
	entry, err := hex.DecodeString(r.EntryHash)
	if err != nil {
		return fmt.Errorf("malformed entry_hash: %w", err)
	}
	return v.VerifySignature(entry, sig)
}

// verifyChainOnly is the pre-anchor behavior, kept for tests that need to
// show a truncated chain is self-consistent.
func verifyChainOnly(path string) (int, error) {
	records, err := readRecords(path)
	if err != nil {
		return 0, err
	}
	prev := genesisPrev
	for i, r := range records {
		if err := verifyRecord(r, uint64(i+1), prev); err != nil {
			return i, err
		}
		prev, _ = hex.DecodeString(r.EntryHash)
	}
	return len(records), nil
}

// headPath is the anchor sidecar for a log at path.
func headPath(path string) string { return path + ".head" }

// writeHead replaces the anchor atomically: a torn anchor would fail every
// subsequent verification and look exactly like tampering.
func writeHead(path string, h Head) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp := headPath(path) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, headPath(path))
}

// readHead loads the anchor. A missing sidecar is not an error: logs
// written before anchoring existed must stay verifiable.
func readHead(path string) (Head, bool, error) {
	b, err := os.ReadFile(headPath(path))
	if os.IsNotExist(err) {
		return Head{}, false, nil
	}
	if err != nil {
		return Head{}, false, err
	}
	var h Head
	if err := json.Unmarshal(bytes.TrimSpace(b), &h); err != nil {
		return Head{}, false, fmt.Errorf("malformed head anchor %s: %w", headPath(path), err)
	}
	return h, true, nil
}

func verifyRecord(r Record, wantSeq uint64, prev []byte) error {
	if r.Seq != wantSeq {
		return fmt.Errorf("out-of-order seq: want %d, got %d", wantSeq, r.Seq)
	}
	if r.PrevHash != hex.EncodeToString(prev) {
		return fmt.Errorf("prev_hash mismatch")
	}
	if len(r.EntryHash) != hashHexLen {
		return fmt.Errorf("malformed entry_hash")
	}
	want, err := entryHash(r.Seq, r.Timestamp, prev, r.Payload)
	if err != nil {
		return err
	}
	if hex.EncodeToString(want) != r.EntryHash {
		return fmt.Errorf("entry_hash mismatch (payload or header tampered)")
	}
	return nil
}

func readRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only replay; close cannot lose data
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
