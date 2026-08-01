package frames

import (
	"fmt"
	"image"
	_ "image/jpeg" // frame decode support
	_ "image/png"
	"math/bits"
	"os"
)

// Dedup is the optional post-extraction stage that removes visually
// near-duplicate frames before they reach the moderation fan-out (saving
// vendor cost, especially when multiple workflows produced overlapping
// frames).
//
// Each frame gets a 64-bit difference hash (dHash: 9x8 grayscale,
// horizontal gradient signs); a frame is dropped when its Hamming
// distance to ANY already-kept frame is <= threshold. Order is
// preserved and the first occurrence always survives, so the result is
// never empty. Conservative failure mode: a frame that cannot be decoded
// or hashed is KEPT — dedup must never discard evidence it could not
// compare.
//
// Known limitation: dHash encodes horizontal gradients only, so all
// UNIFORM (flat-color) frames share the all-zero hash and collapse into
// one regardless of color. For moderation sampling that is acceptable —
// flat frames carry no visual evidence — but don't reuse this hash as a
// general similarity metric.
func Dedup(in []Frame, threshold int) (kept []Frame, removed int) {
	if threshold < 0 || len(in) < 2 {
		return in, 0
	}
	type hashed struct{ h uint64 }
	var keptHashes []hashed
	for _, f := range in {
		h, err := dhashFile(f.Path)
		if err != nil {
			kept = append(kept, f) // unhashable: keep (never drop blind)
			continue
		}
		dup := false
		for _, k := range keptHashes {
			if bits.OnesCount64(h^k.h) <= threshold {
				dup = true
				break
			}
		}
		if dup {
			removed++
			_ = os.Remove(f.Path)
			continue
		}
		keptHashes = append(keptHashes, hashed{h})
		kept = append(kept, f)
	}
	for i := range kept {
		kept[i].Index = i
	}
	return kept, removed
}

// dhashFile computes the 64-bit dHash of an image file: downsample to a
// 9x8 grayscale grid (box average), then emit 1 for each left<right
// horizontal gradient.
func dhashFile(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return 0, fmt.Errorf("decode %s: %w", path, err)
	}

	const gw, gh = 9, 8
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return 0, fmt.Errorf("empty image %s", path)
	}

	var grid [gh][gw]float64
	for gy := 0; gy < gh; gy++ {
		y0, y1 := b.Min.Y+gy*h/gh, b.Min.Y+(gy+1)*h/gh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for gx := 0; gx < gw; gx++ {
			x0, x1 := b.Min.X+gx*w/gw, b.Min.X+(gx+1)*w/gw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sum, n float64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, bl, _ := img.At(x, y).RGBA()
					// Rec. 601 luma over 16-bit channel values.
					sum += 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)
					n++
				}
			}
			grid[gy][gx] = sum / n
		}
	}

	var hash uint64
	for gy := 0; gy < gh; gy++ {
		for gx := 0; gx < gw-1; gx++ {
			hash <<= 1
			if grid[gy][gx] < grid[gy][gx+1] {
				hash |= 1
			}
		}
	}
	return hash, nil
}
