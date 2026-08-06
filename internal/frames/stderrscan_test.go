package frames

import (
	"fmt"
	"strings"
	"testing"
)

// ffmpeg's stderr arrives in arbitrary chunks, so a showinfo line can be
// split anywhere — including through the pts_time value. A scanner that
// only matched within a single Write would drop those frames' timestamps
// and silently downgrade them to ordinals.
func TestStderrScannerReassemblesSplitLines(t *testing.T) {
	full := "n:0 pts_time:0.5 x\nn:1 pts_time:1.25 x\nn:2 pts_time:2.75 x\n"
	want := []float64{0.5, 1.25, 2.75}

	for _, chunk := range []int{1, 2, 3, 7, 13, len(full)} {
		t.Run(fmt.Sprintf("chunk%d", chunk), func(t *testing.T) {
			s := &stderrScanner{}
			for i := 0; i < len(full); i += chunk {
				end := min(i+chunk, len(full))
				if _, err := s.Write([]byte(full[i:end])); err != nil {
					t.Fatal(err)
				}
			}
			s.Flush()

			got := s.Timestamps()
			if len(got) != len(want) {
				t.Fatalf("timestamps = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("timestamp %d = %v, want %v", i, got[i], want[i])
				}
			}
		})
	}
}

// The final line often has no trailing newline and is the one that explains
// a failure, so Flush must scan it.
func TestStderrScannerFlushScansTrailingLine(t *testing.T) {
	s := &stderrScanner{}
	if _, err := s.Write([]byte("n:0 pts_time:9.5 no newline")); err != nil {
		t.Fatal(err)
	}
	if len(s.Timestamps()) != 0 {
		t.Fatal("an unterminated line was scanned before Flush")
	}
	s.Flush()
	if got := s.Timestamps(); len(got) != 1 || got[0] != 9.5 {
		t.Errorf("timestamps after Flush = %v, want [9.5]", got)
	}
}

// The point of the scanner: memory must track what we keep, not what
// ffmpeg emits. showinfo on a long input produces one line per frame.
func TestStderrScannerBoundsRetainedBytes(t *testing.T) {
	s := &stderrScanner{}
	const lines = 200_000
	for i := range lines {
		if _, err := fmt.Fprintf(s, "n:%d pts_time:%d.0 lots of padding text to bulk out the line\n", i, i); err != nil {
			t.Fatal(err)
		}
	}
	s.Flush()

	if got := len(s.tail); got > stderrTailBytes {
		t.Errorf("retained tail = %d bytes, want <= %d", got, stderrTailBytes)
	}
	// Timestamps are the one thing that legitimately scales, and they are
	// 8 bytes each against ~60 bytes of text per frame.
	if got := len(s.Timestamps()); got != lines {
		t.Errorf("timestamps = %d, want %d: extraction must not be lossy", got, lines)
	}
	// The tail must still hold the most recent output, which is what the
	// error message shows.
	if !strings.Contains(s.Tail(), fmt.Sprintf("n:%d ", lines-1)) {
		t.Error("tail does not contain the final line")
	}
}

// Output with no newlines at all must not grow the partial-line buffer
// without bound.
func TestStderrScannerCapsAnUnterminatedLine(t *testing.T) {
	s := &stderrScanner{}
	blob := strings.Repeat("x", 1<<20)
	for range 8 {
		if _, err := s.Write([]byte(blob)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.partial); got > stderrLineMax {
		t.Errorf("partial line buffer = %d bytes, want <= %d", got, stderrLineMax)
	}
	if got := len(s.tail); got > stderrTailBytes {
		t.Errorf("retained tail = %d bytes, want <= %d", got, stderrTailBytes)
	}
}

// Write must report the full byte count it was handed: os/exec treats a
// short write as an error and would fail the extraction.
func TestStderrScannerReportsFullWriteLength(t *testing.T) {
	s := &stderrScanner{}
	for _, in := range []string{"", "no newline", "one\n", "a\nb\nc\n", strings.Repeat("z", 100_000)} {
		n, err := s.Write([]byte(in))
		if err != nil {
			t.Fatalf("Write(%d bytes): %v", len(in), err)
		}
		if n != len(in) {
			t.Errorf("Write returned %d, want %d", n, len(in))
		}
	}
}
