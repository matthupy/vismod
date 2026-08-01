package frames

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func writePNG(t *testing.T, dir, name string, render func(x, y int) uint8) Frame {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			img.SetGray(x, y, color.Gray{Y: render(x, y)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return Frame{Path: p}
}

func gradient(x, _ int) uint8 { return uint8(x * 8) }

// gradientNoisy darkens the top-right corner region, flipping the
// rightmost gradient comparison in the top grid row only: a small,
// deterministic Hamming distance (>=1, <<8) from gradient.
func gradientNoisy(x, y int) uint8 {
	if y < 4 && x >= 28 {
		return 0
	}
	return uint8(x * 8)
}
func checker(x, y int) uint8 {
	if (x/4+y/4)%2 == 0 {
		return 255
	}
	return 0
}

func TestDedupRemovesNearDuplicates(t *testing.T) {
	dir := t.TempDir()
	f1 := writePNG(t, dir, "a.png", gradient)
	f2 := writePNG(t, dir, "b.png", gradientNoisy) // near-dup of f1
	f3 := writePNG(t, dir, "c.png", checker)       // distinct

	kept, removed := Dedup([]Frame{f1, f2, f3}, 8)
	if removed != 1 || len(kept) != 2 {
		t.Fatalf("kept=%d removed=%d, want 2/1", len(kept), removed)
	}
	if kept[0].Path != f1.Path || kept[1].Path != f3.Path {
		t.Errorf("first occurrence must survive, distinct frame kept: %+v", kept)
	}
	if kept[0].Index != 0 || kept[1].Index != 1 {
		t.Errorf("indices must be renumbered: %+v", kept)
	}
	// Removed frame's file is deleted; kept files remain.
	if _, err := os.Stat(f2.Path); !os.IsNotExist(err) {
		t.Error("removed duplicate file should be deleted")
	}
	if _, err := os.Stat(f1.Path); err != nil {
		t.Error("kept file must remain")
	}
}

func TestDedupThresholdZeroKeepsNearDuplicates(t *testing.T) {
	dir := t.TempDir()
	f1 := writePNG(t, dir, "a.png", gradient)
	f2 := writePNG(t, dir, "b.png", gradientNoisy)
	kept, removed := Dedup([]Frame{f1, f2}, 0)
	// threshold 0 removes only EXACT hash matches; the noisy variant
	// differs by a few bits and survives.
	if removed != 0 || len(kept) != 2 {
		t.Fatalf("kept=%d removed=%d, want 2/0", len(kept), removed)
	}
}

func TestDedupIdenticalFramesAtThresholdZero(t *testing.T) {
	dir := t.TempDir()
	f1 := writePNG(t, dir, "a.png", gradient)
	f2 := writePNG(t, dir, "b.png", gradient)
	kept, removed := Dedup([]Frame{f1, f2}, 0)
	if removed != 1 || len(kept) != 1 {
		t.Fatalf("identical frames must dedupe even at threshold 0: kept=%d removed=%d", len(kept), removed)
	}
}

func TestDedupNeverEmptiesAndKeepsUnhashable(t *testing.T) {
	dir := t.TempDir()
	f1 := writePNG(t, dir, "a.png", gradient)
	bad := Frame{Path: filepath.Join(dir, "not-an-image.png")}
	if err := os.WriteFile(bad.Path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	kept, removed := Dedup([]Frame{f1, bad}, 64)
	if len(kept) != 2 || removed != 0 {
		t.Fatalf("unhashable frames must be KEPT (never drop evidence blind): kept=%d removed=%d", len(kept), removed)
	}

	single, removed := Dedup([]Frame{f1}, 64)
	if len(single) != 1 || removed != 0 {
		t.Fatal("single frame passes through untouched")
	}
}
