// Package audit implements an append-only, hash-chained decision log and its
// verifier. It binds each decision to its inputs BY HASH (it stores
// SHA-256(Raw) + ModelIdentity + verdict, never Raw itself).
//
// Scope honesty: a bare chain detects truncation and in-place edits, NOT a
// full-chain rewrite by a write-capable insider. The tamper-RESISTANT upgrade
// (HMAC/Ed25519 signing or head-hash anchoring) is a documented future seam.
//
// M0 ships a working chain + `verify`. M4 hardens canonicalization to RFC 8785
// JCS and wires the pipeline to append on every decision.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
)

// Payload is the verdict-affecting content bound into the chain. It never
// contains media bytes or Raw free-text.
type Payload struct {
	JobID        string `json:"job_id"`
	Verdict      string `json:"verdict"`
	RawSHA256    string `json:"raw_sha256"`
	Adapter      string `json:"adapter"`
	ModelVersion string `json:"model_version"`
	ConfigHash   string `json:"config_hash"`
}

// Record is one chain entry as persisted (one JSON object per line).
type Record struct {
	Seq       uint64  `json:"seq"`
	Timestamp string  `json:"timestamp"` // RFC3339 UTC nanoseconds
	PrevHash  string  `json:"prev_hash"` // hex
	Payload   Payload `json:"payload"`
	EntryHash string  `json:"entry_hash"` // hex
}

var zeroHash [32]byte

// Log is a file-backed append-only hash chain. Appends are idempotent per
// JobID (a JobID already in the chain is skipped — no new seq, no gap).
type Log struct {
	mu    sync.Mutex
	path  string
	seq   uint64
	prev  [32]byte
	seen  map[string]struct{}
	ready bool
}

// Open loads (or initializes) the chain at path, replaying it to recover the
// head hash, next seq and the JobID index.
func Open(path string) (*Log, error) {
	l := &Log{path: path, prev: zeroHash, seen: map[string]struct{}{}}
	recs, err := readAll(path)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		h, herr := hex.DecodeString(r.EntryHash)
		if herr != nil || len(h) != 32 {
			return nil, fmt.Errorf("audit: bad entry_hash at seq %d", r.Seq)
		}
		copy(l.prev[:], h)
		l.seq = r.Seq
		l.seen[r.Payload.JobID] = struct{}{}
	}
	l.ready = true
	return l, nil
}

// Append adds a payload to the chain unless its JobID is already present.
// Returns the written record (or the existing-skip with ok=false).
func (l *Log) Append(p Payload, timestamp string) (Record, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ready {
		return Record{}, false, errors.New("audit: log not opened")
	}
	if _, dup := l.seen[p.JobID]; dup {
		return Record{}, false, nil // idempotent skip; no new seq
	}

	seq := l.seq + 1
	entry := computeHash(seq, timestamp, l.prev, p)
	rec := Record{
		Seq:       seq,
		Timestamp: timestamp,
		PrevHash:  hex.EncodeToString(l.prev[:]),
		Payload:   p,
		EntryHash: hex.EncodeToString(entry[:]),
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Record{}, false, err
	}
	defer f.Close()
	line, _ := json.Marshal(rec)
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Record{}, false, err
	}

	l.seq = seq
	l.prev = entry
	l.seen[p.JobID] = struct{}{}
	return rec, true, nil
}

// Verify recomputes the whole chain and returns the seq of the first broken
// link (0 + nil error means intact).
func Verify(path string) (brokenSeq uint64, err error) {
	recs, err := readAll(path)
	if err != nil {
		return 0, err
	}
	prev := zeroHash
	var lastSeq uint64
	for i, r := range recs {
		if r.Seq != lastSeq+1 {
			return r.Seq, fmt.Errorf("audit: non-monotonic seq at index %d (got %d, want %d)", i, r.Seq, lastSeq+1)
		}
		if r.PrevHash != hex.EncodeToString(prev[:]) {
			return r.Seq, fmt.Errorf("audit: prev_hash mismatch at seq %d", r.Seq)
		}
		want := computeHash(r.Seq, r.Timestamp, prev, r.Payload)
		if r.EntryHash != hex.EncodeToString(want[:]) {
			return r.Seq, fmt.Errorf("audit: entry_hash mismatch at seq %d (tampered)", r.Seq)
		}
		prev = want
		lastSeq = r.Seq
	}
	return 0, nil
}

// computeHash = SHA-256(seq[8]BE || timestamp || prev[32] || canonical(payload)),
// length-prefixed to avoid ambiguous concatenation.
func computeHash(seq uint64, timestamp string, prev [32]byte, p Payload) [32]byte {
	h := sha256.New()
	var seqb [8]byte
	binary.BigEndian.PutUint64(seqb[:], seq)
	writeField(h, seqb[:])
	writeField(h, []byte(timestamp))
	writeField(h, prev[:])
	writeField(h, canonical(p))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func writeField(h interface{ Write([]byte) (int, error) }, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}

// canonical serializes the payload as RFC 8785 JCS (JSON Canonicalization
// Scheme): object members sorted lexicographically by key, compact (no
// insignificant whitespace), UTF-8. This makes `audit verify` recompute
// byte-identical hashes across processes and implementations, independent of
// Go struct declaration order. The payload is all-string, so JCS number
// formatting edge cases do not arise.
func canonical(p Payload) []byte {
	b, _ := json.Marshal(p)
	out, err := jcs(b)
	if err != nil {
		// A struct we just marshalled is always valid JSON; fall back rather
		// than panic in the hashing path.
		return b
	}
	return out
}

// jcs re-emits arbitrary JSON in RFC 8785 canonical form: object keys sorted,
// compact, numbers preserved verbatim via json.Number.
func jcs(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			ks, _ := json.Marshal(k)
			buf.Write(ks)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case json.Number:
		buf.WriteString(t.String())
	default: // string, bool, nil
		enc, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(enc)
	}
	return nil
}

func readAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("audit: malformed record: %w", err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// ReadRecords returns every persisted chain record in order (empty if the log
// does not exist yet). Intended for inspection/testing — Verify is the
// integrity check.
func ReadRecords(path string) ([]Record, error) { return readAll(path) }

// RawSHA256 hashes raw provider output for binding into the chain.
func RawSHA256(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
