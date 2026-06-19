package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func payload(id string) Payload {
	return Payload{JobID: id, Verdict: "block", RawSHA256: RawSHA256([]byte(id)), Adapter: "stub"}
}

func TestAppendVerifyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, ok, err := l.Append(payload(id), "2026-06-18T00:00:00Z"); err != nil || !ok {
			t.Fatalf("append %s: ok=%v err=%v", id, ok, err)
		}
	}
	if broken, err := Verify(path); err != nil {
		t.Fatalf("verify intact chain: broken@%d %v", broken, err)
	}
}

func TestAppendIdempotentPerJobID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, _ := Open(path)
	_, ok1, _ := l.Append(payload("dup"), "2026-06-18T00:00:00Z")
	_, ok2, _ := l.Append(payload("dup"), "2026-06-18T00:00:01Z")
	if !ok1 || ok2 {
		t.Fatalf("first append must write (ok=%v), second must skip (ok=%v)", ok1, ok2)
	}
	// Reopen must recover seq/index and still verify.
	if broken, err := Verify(path); err != nil {
		t.Fatalf("verify: broken@%d %v", broken, err)
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, _ := Open(path)
	_, _, _ = l.Append(payload("a"), "2026-06-18T00:00:00Z")
	_, _, _ = l.Append(payload("b"), "2026-06-18T00:00:00Z")

	// Tamper: flip a verdict in-place in the first record.
	raw, _ := os.ReadFile(path)
	tampered := []byte(string(raw))
	// crude in-place edit: change "block" to "allow" in the first line
	out := []byte{}
	replaced := false
	s := string(tampered)
	if i := indexOf(s, "block"); i >= 0 && !replaced {
		out = append(out, s[:i]...)
		out = append(out, []byte("allow")...)
		out = append(out, s[i+5:]...)
		replaced = true
	}
	if !replaced {
		t.Fatal("setup: no verdict to tamper")
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}

	broken, err := Verify(path)
	if err == nil {
		t.Fatal("verify must detect tamper")
	}
	if broken != 1 {
		t.Fatalf("first broken link should be seq 1, got %d", broken)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
