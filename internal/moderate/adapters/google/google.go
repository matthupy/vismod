// Package google implements the Google Cloud Vision SafeSearch adapter — the
// third real Moderator (after azure + hive) and a second normalization shape:
// ordinal likelihood enums rather than severities or per-head probabilities. It
// calls the v1 images:annotate REST data plane directly (no SDK), requesting the
// SAFE_SEARCH_DETECTION feature, and maps the five likelihood fields
// (adult/spoof/medical/violence/racy) into the canonical schema (normalize.go).
//
// Scope: image-only (Caps.SupportsVideo=false). Cloud Video Intelligence is a
// separate API and a documented future enhancement via the optional
// moderation.VideoModerator interface; v1 lets the pipeline drive framing via
// videosift, parallel to azure/hive.
//
// Fail-safe: on any provider error — transport, a non-2xx status, an HTTP-200
// per-response error, or a missing annotation — AnalyzeImage returns an error
// and the pipeline records a frame Status=error. It NEVER yields allow.
package google

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/pkg/moderation"
)

const adapterName = "google"

// modelVersion is the Vision API version in the endpoint path. SafeSearch has no
// separately-versioned model id, so the API version is the stable identity
// stamped into NormalizedResult.ModelVersion (§E single source of truth).
const modelVersion = "v1"

// defaultEndpoint is the v1 annotate endpoint. Overridable via options.endpoint
// (e.g. a regional host or a test server).
const defaultEndpoint = "https://vision.googleapis.com/v1/images:annotate"

// maxImageBytes is Vision's documented per-image limit (20 MB). Surfaced via
// Caps so the pipeline pre-flights oversize images before calling AnalyzeImage.
const maxImageBytes = 20 * 1024 * 1024

// defaultRPS is a conservative default for the shared limiter; tune per the
// project's Vision quota via options.rps.
const defaultRPS = 5.0

// defaultMaxRetries is the transient-failure retry count when unset. An explicit
// max_retries: 0 disables retries (decodeOptions -1 sentinel distinguishes them).
const defaultMaxRetries = 3

// allowedMIME is Vision's image input allow-list. An unsupported type is a
// terminal per-frame error (a retry won't fix it).
var allowedMIME = map[string]bool{
	"image/jpeg":   true,
	"image/png":    true,
	"image/gif":    true,
	"image/bmp":    true,
	"image/webp":   true,
	"image/tiff":   true,
	"image/x-icon": true,
}

func init() {
	moderate.Register(adapterName, New)
}

type google struct {
	client *client
}

// New builds the Google SafeSearch adapter from config. It fails fast (spec
// §F.2) when the required auth secret is missing.
//
// Options (yaml, non-secret) — the key set matches the §L model-fingerprint
// whitelist exactly, so this adapter adds no new key to guard:
//
//	endpoint      string  default https://vision.googleapis.com/v1/images:annotate
//	auth_mode     string  "apikey" (default) | "bearer"
//	rps           float64 shared limiter rate, default 5
//	max_retries   int     transient-failure retries, default 3
//	retry_backoff string  time.Duration base backoff, default "500ms"
//
// Secrets (env-only, VISMOD_ prefix): VISMOD_GOOGLE_API_KEY (apikey),
// VISMOD_GOOGLE_TOKEN (bearer), optional VISMOD_GOOGLE_PROJECT (bearer billing
// project, sent as x-goog-user-project). The GCP project is intentionally NOT an
// adapter.option — keeping it env-only avoids adding an unclassified option key.
func New(cfg moderate.AdapterConfig) (moderation.Moderator, error) {
	opts := decodeOptions(cfg.Options)

	auth, err := newAuth(opts.authMode, cfg.Secret)
	if err != nil {
		return nil, err
	}

	endpoint := opts.endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	rps := opts.rps
	if rps <= 0 {
		rps = defaultRPS
	}
	// opts.maxRetries is -1 only when unset (decodeOptions sentinel); an explicit
	// 0 is honored to disable retries. Any other negative is nonsense -> default.
	maxRetries := opts.maxRetries
	if maxRetries < 0 {
		maxRetries = defaultMaxRetries
	}
	backoff := opts.retryBackoff
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}

	// Seeded RNG drives backoff jitter so concurrent workers that all hit a 429
	// don't retry in lockstep (thundering herd) against the shared limiter.
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x9E3779B97F4A7C15))
	return &google{client: newClient(endpoint, auth, rps, maxRetries, backoff, rng)}, nil
}

func (g *google) Name() string { return adapterName }

func (g *google) Capabilities() moderation.Caps {
	return moderation.Caps{
		SupportsVideo: false, // SafeSearch is single-image; video framing is one layer up
		MaxImageBytes: maxImageBytes,
		Categories: []moderation.Category{
			moderation.CategorySexual,
			moderation.CategorySuggestiveRacy,
			moderation.CategoryViolence,
			moderation.CategoryMedical,
			moderation.CategorySpoof,
		},
	}
}

func (g *google) Close() error { return nil }

// AnalyzeImage validates input, calls images:annotate, and normalizes the
// SafeSearch annotation into a single-frame NormalizedResult. The pipeline
// stamps Threshold/Flagged and the asset rollup. ModelVersion is the API
// version (§E single source).
func (g *google) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	if err := validateInput(img); err != nil {
		return moderation.NormalizedResult{}, err // terminal, never allow
	}

	resp, err := g.client.analyze(ctx, img.Bytes)
	if err != nil {
		return moderation.NormalizedResult{}, err
	}

	// An HTTP 200 can still carry a PER-RESPONSE failure (responses[].error).
	// Surface it as a coded error so observability can label
	// vismod_adapter_errors_total{code} — fail-safe (error, never allow).
	if err := responseError(resp); err != nil {
		return moderation.NormalizedResult{}, err
	}

	// A missing safeSearchAnnotation is an unexpected provider state (the feature
	// was requested): treat as could-not-evaluate, never a clean frame.
	ann, ok := firstAnnotation(resp)
	if !ok {
		return moderation.NormalizedResult{}, &apiError{Status: 200, Code: "empty_output", Message: "no safeSearchAnnotation in response"}
	}

	return moderation.NormalizedResult{
		Provider:     adapterName,
		ModelVersion: modelVersion,
		MediaType:    "image",
		Frames: []moderation.FrameResult{{
			TimestampSec: nil,
			Status:       moderation.FrameStatusOK,
			Categories:   normalize(ann),
		}},
	}, nil
}

// responseError returns a coded error when the first response entry carries a
// per-response error object, else nil. The code is response_<n> so it stays
// distinct from transport codes (network/decode/http_<n>) in metrics.
func responseError(resp annotateResponse) error {
	if len(resp.Responses) == 0 || resp.Responses[0].Error == nil {
		return nil
	}
	e := resp.Responses[0].Error
	msg := e.Message
	if msg == "" {
		msg = "provider response failed"
	}
	return &apiError{Status: 200, Code: fmt.Sprintf("response_%d", e.Code), Message: msg}
}

// validateInput enforces the 20 MB cap and the format allow-list. Both are
// terminal (a retry won't fix oversize or an unsupported type).
func validateInput(img moderation.Image) error {
	if len(img.Bytes) == 0 {
		return fmt.Errorf("google: empty image")
	}
	if int64(len(img.Bytes)) > maxImageBytes {
		return fmt.Errorf("google: image %d bytes exceeds 20 MB cap", len(img.Bytes))
	}
	if img.MIME != "" && !allowedMIME[img.MIME] {
		return fmt.Errorf("google: unsupported MIME %q (allowed: jpeg/png/gif/bmp/webp/tiff/ico)", img.MIME)
	}
	return nil
}
