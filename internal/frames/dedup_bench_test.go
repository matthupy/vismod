package frames

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/vismod/vismod/internal/config"
)

// writeBenchPNG renders a full-HD frame, the size the shipped workflows
// actually produce: the keyframe workflow has no scale filter, so extracted
// PNGs are source resolution.
func writeBenchPNG(tb testing.TB, dir, name string, rgb bool) string {
	tb.Helper()
	const w, h = 1920, 1080
	var img image.Image
	if rgb {
		m := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				m.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255})
			}
		}
		img = m
	} else {
		m := image.NewGray(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				m.SetGray(x, y, color.Gray{Y: uint8((x ^ y) >> 2)})
			}
		}
		img = m
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		tb.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		tb.Fatal(err)
	}
	return p
}

// genericLuma is the pre-optimization sampler: the image.Image interface
// path, boxing a color.Color per pixel.
func genericLuma(img image.Image) func(x, y int) float64 {
	return func(x, y int) float64 {
		r, g, b, _ := img.At(x, y).RGBA()
		return 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
	}
}

// The typed fast paths are an optimization, not a redefinition: for every
// pixel they must return exactly what the interface path returns. A path
// that is merely close silently shifts cell averages, which flips gradient
// bits, which changes which frames a vendor is paid to look at.
func TestLumaSamplerFastPathsMatchTheGenericPath(t *testing.T) {
	const w, h = 17, 13 // deliberately not a multiple of anything

	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	nrgba := image.NewNRGBA(image.Rect(0, 0, w, h))
	gray := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			// Include alpha values that actually exercise premultiplication,
			// zero and full among them.
			a := uint8((x*31 + y*17) % 256)
			rgba.SetRGBA(x, y, color.RGBA{R: uint8(x * 15), G: uint8(y * 19), B: uint8(x ^ y), A: 255})
			nrgba.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 15), G: uint8(y * 19), B: uint8(x ^ y), A: a})
			gray.SetGray(x, y, color.Gray{Y: uint8(x*7 + y*3)})
		}
	}

	for _, tc := range []struct {
		name string
		img  image.Image
	}{
		{"RGBA", rgba},
		{"NRGBA", nrgba},
		{"Gray", gray},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fast, slow := lumaSampler(tc.img), genericLuma(tc.img)
			for y := range h {
				for x := range w {
					if got, want := fast(x, y), slow(x, y); got != want {
						t.Fatalf("luma at (%d,%d) = %v, generic path = %v", x, y, got, want)
					}
				}
			}
		})
	}
}

// An unlisted format (YCbCr, what a JPEG frame decodes to) must fall back
// to the interface path rather than silently hashing as black.
func TestLumaSamplerFallsBackForUnlistedFormats(t *testing.T) {
	img := image.NewYCbCr(image.Rect(0, 0, 8, 8), image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = uint8(i * 3)
	}
	fast, slow := lumaSampler(img), genericLuma(img)
	for y := range 8 {
		for x := range 8 {
			if got, want := fast(x, y), slow(x, y); got != want {
				t.Fatalf("luma at (%d,%d) = %v, generic path = %v", x, y, got, want)
			}
		}
	}
}

// step must never return 0 (an infinite loop) and must stay exact for the
// small images the existing dedup tests hash.
func TestStepBounds(t *testing.T) {
	for n := range 200 {
		s := step(n)
		if s < 1 {
			t.Fatalf("step(%d) = %d, must be >= 1", n, s)
		}
		if n <= maxCellSamples && s != 1 {
			t.Errorf("step(%d) = %d, want 1: small cells must stay exact", n, s)
		}
		if n > 0 && (n+s-1)/s > maxCellSamples {
			t.Errorf("step(%d) = %d yields %d samples, want <= %d", n, s, (n+s-1)/s, maxCellSamples)
		}
	}
}

// BenchmarkDhashFile measures the per-frame cost of the dedup stage. This
// runs once per extracted frame per video job, before any vendor call, so
// it is pure latency added to every job that dedup is enabled for.
func BenchmarkDhashFile(b *testing.B) {
	for _, tc := range []struct {
		name string
		rgb  bool
	}{
		{"gray1080p", false},
		{"rgba1080p", true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			p := writeBenchPNG(b, b.TempDir(), "frame.png", tc.rgb)
			b.ResetTimer()
			for b.Loop() {
				if _, err := dhashFile(p); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRenderWorkflow covers the per-job template work: every arg of
// every selected workflow is rendered on every job.
func BenchmarkRenderWorkflow(b *testing.B) {
	wf := config.DefaultWorkflows()["interval"]
	v := TemplateValues{Input: "/data/in.mp4", WorkDir: "/work", MaxFrames: 64, MaxWidth: 1280}
	b.ResetTimer()
	for b.Loop() {
		if _, err := RenderWorkflow(wf, v); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRenderWorkflowUncached is the pre-cache behavior: a fresh parse
// of every arg on every render. Kept as the comparison point for the cache.
func BenchmarkRenderWorkflowUncached(b *testing.B) {
	wf := config.DefaultWorkflows()["interval"]
	v := TemplateValues{Input: "/data/in.mp4", WorkDir: "/work", MaxFrames: 64, MaxWidth: 1280}
	b.ResetTimer()
	for b.Loop() {
		argTemplates.Clear()
		if _, err := RenderWorkflow(wf, v); err != nil {
			b.Fatal(err)
		}
	}
}
