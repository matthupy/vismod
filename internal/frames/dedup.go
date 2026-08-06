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

// maxCellSamples bounds how many pixels are averaged PER AXIS in each of
// the 72 grid cells, so hashing costs a fixed ~4.6k samples instead of
// scaling with resolution.
//
// The cell is already an average feeding a single gradient-sign
// comparison; sampling it densely adds precision the hash immediately
// discards. At 1080p the old full scan cost ~2M reads — and through the
// image.Image interface, ~2M boxed color values — per frame, once per
// extracted frame per video job, before any vendor call.
//
// Small images are unaffected: cells no wider than maxCellSamples keep a
// step of 1 and hash exactly as they did before.
const maxCellSamples = 8

// step spreads at most maxCellSamples reads across a span of n pixels.
func step(n int) int {
	if n <= maxCellSamples {
		return 1
	}
	return (n + maxCellSamples - 1) / maxCellSamples
}

// lumaSampler returns a Rec. 601 luma reader over 16-bit channel values.
//
// The generic path (img.At(x,y).RGBA()) boxes a color.Color per pixel,
// which dominated the profile. The typed paths read the pixel buffer
// directly and reproduce the color model conversions EXACTLY — the same
// 8->16-bit expansion (v*0x101) and the same NRGBA alpha premultiplication
// — so the hash is bit-identical to the generic path. Formats not listed
// (notably YCbCr from JPEG) fall back rather than approximate: dedup
// decides which frames a vendor never sees, so "close enough" is not.
func lumaSampler(img image.Image) func(x, y int) float64 {
	const (
		kr, kg, kb = 0.299, 0.587, 0.114
		expand     = 0x101 // 8-bit -> 16-bit, as color.RGBA.RGBA() does
	)
	switch m := img.(type) {
	case *image.Gray:
		return func(x, y int) float64 {
			// All three channels equal Y. Keep the three-term sum rather
			// than collapsing it to y: kr+kg+kb is 1 in decimal but not in
			// binary floating point, and folding it changes the last bit
			// relative to the generic path.
			v := float64(uint32(m.Pix[m.PixOffset(x, y)]) * expand)
			return kr*v + kg*v + kb*v
		}
	case *image.RGBA:
		return func(x, y int) float64 {
			i := m.PixOffset(x, y)
			p := m.Pix[i : i+3 : i+3]
			return kr*float64(uint32(p[0])*expand) +
				kg*float64(uint32(p[1])*expand) +
				kb*float64(uint32(p[2])*expand)
		}
	case *image.NRGBA:
		return func(x, y int) float64 {
			i := m.PixOffset(x, y)
			p := m.Pix[i : i+4 : i+4]
			a := uint32(p[3])
			// Premultiply exactly as color.NRGBA.RGBA() does.
			prem := func(v uint8) float64 {
				return float64(uint32(v) * expand * a / 0xff)
			}
			return kr*prem(p[0]) + kg*prem(p[1]) + kb*prem(p[2])
		}
	default:
		return func(x, y int) float64 {
			r, g, b, _ := img.At(x, y).RGBA()
			return kr*float64(r) + kg*float64(g) + kb*float64(b)
		}
	}
}

// dhashFile computes the 64-bit dHash of an image file: downsample to a
// 9x8 grayscale grid (box average over a bounded sample), then emit 1 for
// each left<right horizontal gradient.
func dhashFile(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }() // read-only decode; close cannot lose data
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

	lumaAt := lumaSampler(img)

	var grid [gh][gw]float64
	for gy := 0; gy < gh; gy++ {
		y0, y1 := b.Min.Y+gy*h/gh, b.Min.Y+(gy+1)*h/gh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		stepY := step(y1 - y0)
		for gx := 0; gx < gw; gx++ {
			x0, x1 := b.Min.X+gx*w/gw, b.Min.X+(gx+1)*w/gw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			stepX := step(x1 - x0)
			var sum, n float64
			for y := y0; y < y1; y += stepY {
				for x := x0; x < x1; x += stepX {
					sum += lumaAt(x, y)
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
