package result

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vismod/vismod/internal/queue"
)

func TestFileSinkAppendsOneLinePerEnvelope(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, id := range []string{"job-1", "job-2", "job-3"} {
		env := envFixture(id)
		if err := s.Write(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "\n"); got != 3 {
		t.Errorf("want 3 lines, got %d: %s", got, b)
	}
}

func TestFileSinkIdempotentPerJobID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	env := envFixture("job-1")
	for range 3 {
		if err := s.Write(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Errorf("redelivery double-wrote: %d lines, want 1", got)
	}
}

func TestFileSinkAppendsToExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	if err := os.WriteFile(p, []byte("{\"pre\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Write(context.Background(), envFixture("job-1")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "{\"pre\":true}") {
		t.Error("existing content was truncated; sink must O_APPEND")
	}
	if got := strings.Count(string(b), "\n"); got != 2 {
		t.Errorf("want 2 lines, got %d", got)
	}
}

func TestFileSinkConcurrentWritesDoNotInterleave(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Write(context.Background(), envFixture("job-"+string(rune('a'+i%26))+string(rune('0'+i/26))))
		}()
	}
	wg.Wait()

	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	lines := 0
	for sc.Scan() {
		lines++
		var env ResultEnvelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			t.Fatalf("line %d is not well-formed JSON (writes interleaved): %v", lines, err)
		}
		if env.JobID == queue.JobID("") {
			t.Fatalf("line %d decoded but has no job_id", lines)
		}
	}
	if lines != n {
		t.Errorf("want %d lines, got %d", n, lines)
	}
}

func TestFileSinkUnwritablePathFailsAtConstruction(t *testing.T) {
	if _, err := NewFileSink(filepath.Join(t.TempDir(), "no-such-dir", "r.jsonl")); err == nil {
		t.Fatal("want construction error for an unwritable path, got nil")
	}
}
