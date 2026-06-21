// Package hive implements the Hive (thehive.ai) visual moderation adapter — the
// second real Moderator and the project's normalization stress test (spec §E).
// It calls the v2 /task/sync REST data plane directly (no Go SDK), submitting
// the image as multipart/form-data, and maps Hive's flat per-head class scores
// into the canonical schema (see taxonomy.go / normalize.go).
//
// Scope: image-only (Caps.SupportsVideo=false). Hive is video-native — its sync
// API can sample frames itself — but v1 keeps the adapter parallel to azure and
// lets the pipeline drive framing via videosift. Native video is a documented
// future enhancement via the optional moderation.VideoModerator interface.
//
// Fail-safe: on any provider error AnalyzeImage returns an error and the pipeline
// records a frame Status=error — it NEVER yields allow.
package hive

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/pkg/moderation"
)

const adapterName = "hive"

// defaultEndpoint is the v2 synchronous task endpoint. Overridable via
// options.endpoint (e.g. a regional host or a test server).
const defaultEndpoint = "https://api.thehive.ai/api/v2/task/sync"

// defaultRPS is a conservative default request rate for the shared limiter; tune
// per the project's Hive plan via options.rps.
const defaultRPS = 5.0

// defaultMaxRetries is the transient-failure retry count when unset. An explicit
// max_retries: 0 disables retries (decodeOptions -1 sentinel distinguishes them).
const defaultMaxRetries = 3

// allowedMIME is Hive's image input allow-list (jpg/png/gif/webp). An
// unsupported type is a terminal per-frame error (a retry won't fix it).
var allowedMIME = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

func init() {
	moderate.Register(adapterName, New)
}

type hive struct {
	client *client
}

// New builds the Hive adapter from config. It fails fast (spec §F.2) when the
// API token is missing.
//
// Options (yaml, non-secret):
//
//	endpoint      string  default https://api.thehive.ai/api/v2/task/sync
//	rps           float64 shared limiter rate, default 5
//	max_retries   int     transient-failure retries, default 3
//	retry_backoff string  time.Duration base backoff, default "500ms"
//
// Secret (env-only): VISMOD_HIVE_TOKEN — the project API token.
func New(cfg moderate.AdapterConfig) (moderation.Moderator, error) {
	opts := decodeOptions(cfg.Options)

	token := cfg.Secret("HIVE_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("hive: API token required (env VISMOD_HIVE_TOKEN)")
	}

	endpoint := opts.endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	rps := opts.rps
	if rps <= 0 {
		rps = defaultRPS
	}
	maxRetries := opts.maxRetries
	if maxRetries < 0 {
		maxRetries = defaultMaxRetries
	}
	backoff := opts.retryBackoff
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}

	c := newClient(endpoint, token, rps, maxRetries, backoff,
		// Seeded RNG drives backoff jitter so concurrent workers that all hit a
		// 429 don't retry in lockstep against the shared limiter.
		rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x9E3779B97F4A7C15)))
	return &hive{client: c}, nil
}

func (h *hive) Name() string { return adapterName }

func (h *hive) Capabilities() moderation.Caps {
	return moderation.Caps{
		SupportsVideo: false, // image-only; native video is a documented future step
		MaxImageBytes: 0,     // Hive publishes no small hard cap; no pre-flight size gate
		Categories:    adapterCategories(),
	}
}

func (h *hive) Close() error { return nil }

// AnalyzeImage validates input, calls the Hive sync API, and normalizes the
// flat per-head classes into a single-frame NormalizedResult. The pipeline
// stamps Threshold/Flagged and the asset rollup.
func (h *hive) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	if err := validateInput(img); err != nil {
		return moderation.NormalizedResult{}, err // terminal, never allow
	}

	resp, err := h.client.analyze(ctx, img.Bytes, img.MIME)
	if err != nil {
		return moderation.NormalizedResult{}, err
	}

	// One image -> one status entry -> one output frame. An empty output is an
	// unexpected provider state: treat as could-not-evaluate (fail-safe), never a
	// clean frame.
	classes, ok := firstOutputClasses(resp)
	if !ok {
		return moderation.NormalizedResult{}, fmt.Errorf("hive: empty output (provider returned no analysis)")
	}

	return moderation.NormalizedResult{
		Provider:  adapterName,
		MediaType: "image",
		Frames: []moderation.FrameResult{{
			TimestampSec: nil,
			Status:       moderation.FrameStatusOK,
			Categories:   normalize(classes),
		}},
	}, nil
}

// firstOutputClasses extracts the first frame's class list, reporting ok=false
// when the response carries no status/output.
func firstOutputClasses(resp hiveResponse) ([]hiveClass, bool) {
	if len(resp.Status) == 0 || len(resp.Status[0].Response.Output) == 0 {
		return nil, false
	}
	return resp.Status[0].Response.Output[0].Classes, true
}

// validateInput rejects empty bytes and unsupported formats (terminal — a retry
// won't fix an unsupported type).
func validateInput(img moderation.Image) error {
	if len(img.Bytes) == 0 {
		return fmt.Errorf("hive: empty image")
	}
	if img.MIME != "" && !allowedMIME[img.MIME] {
		return fmt.Errorf("hive: unsupported MIME %q (allowed: jpeg/png/gif/webp)", img.MIME)
	}
	return nil
}
