// Package azure implements the Azure AI Content Safety image:analyze adapter —
// the first real Moderator (spec §C). It calls the REST data plane directly
// (no Go SDK), normalizes the trimmed severity scale (0/2/4/6 -> severity/6.0),
// and emits the canonical schema. Exactly one Moderator is active per process;
// its shared token-bucket rate limiter gates all fan-out.
//
// Fail-safe: on any provider error AnalyzeImage returns an error and the
// pipeline records a frame Status=error — it NEVER yields allow.
package azure

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/pkg/moderation"
	"golang.org/x/time/rate"
)

const adapterName = "azure"

// defaultAPIVersion is the GA api-version. Microsoft uses a ~90-day deprecation
// cadence, so it is overridable via options.api_version.
const defaultAPIVersion = "2024-09-01"

// maxImageBytes is Azure's hard 4 MB input cap (§C). Surfaced via Caps so the
// pipeline pre-flights oversize images before calling AnalyzeImage.
const maxImageBytes = 4 * 1024 * 1024

// defaultRPS is Azure F0 (free tier) = 5 requests/sec (§C). Overridable.
const defaultRPS = 5.0

// defaultMaxRetries is the transient-failure retry count when unset. An explicit
// max_retries: 0 disables retries (see the sentinel in decodeOptions).
const defaultMaxRetries = 3

// allowedMIME is the input format allow-list (§C). MIME is sniffed from the
// source extension upstream; an unsupported type is a terminal per-frame error.
var allowedMIME = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/bmp":  true,
	"image/tiff": true,
	"image/webp": true,
}

func init() {
	moderate.Register(adapterName, New)
}

type azure struct {
	client     *client
	apiVersion string
}

// New builds the Azure adapter from config. It fails fast (spec §F.2) when the
// endpoint or the auth secret is missing.
//
// Options (yaml, non-secret):
//
//	endpoint     string  https://<resource>.cognitiveservices.azure.com
//	auth_mode    string  "apikey" (default) | "bearer"
//	api_version  string  default "2024-09-01"
//	rps          float64 shared limiter rate, default 5 (Azure F0)
//	max_retries  int     transient-failure retries, default 3
//	retry_backoff string time.Duration base backoff, default "500ms"
//
// Secrets (env-only, VISMOD_ prefix): VISMOD_AZURE_KEY (apikey),
// VISMOD_AZURE_TOKEN (bearer). Endpoint may also come from VISMOD_AZURE_ENDPOINT.
func New(cfg moderate.AdapterConfig) (moderation.Moderator, error) {
	opts := decodeOptions(cfg.Options)

	endpoint := opts.endpoint
	if endpoint == "" {
		endpoint = cfg.Secret("AZURE_ENDPOINT")
	}
	if endpoint == "" {
		return nil, fmt.Errorf("azure: endpoint required (adapter.options.endpoint or VISMOD_AZURE_ENDPOINT)")
	}

	auth, err := newAuth(opts.authMode, cfg.Secret)
	if err != nil {
		return nil, err
	}

	apiVersion := opts.apiVersion
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
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

	c := &client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		endpoint:   endpoint,
		apiVersion: apiVersion,
		auth:       auth,
		// Shared bucket: burst 1 keeps aggregate rate == rps regardless of
		// worker x frame parallelism (spec §F.3).
		limiter:    rate.NewLimiter(rate.Limit(rps), 1),
		maxRetries: maxRetries,
		backoff:    backoff,
		// Seeded RNG drives backoff jitter so concurrent workers that all hit a
		// 429 don't retry in lockstep (thundering herd) against the shared limiter.
		rng: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x9E3779B97F4A7C15)),
	}
	return &azure{client: c, apiVersion: apiVersion}, nil
}

func (a *azure) Name() string { return adapterName }

func (a *azure) Capabilities() moderation.Caps {
	return moderation.Caps{
		SupportsVideo: false, // image:analyze is single-image; video framing is one layer up
		MaxImageBytes: maxImageBytes,
		Categories: []moderation.Category{
			moderation.CategoryHate,
			moderation.CategorySelfHarm,
			moderation.CategorySexual,
			moderation.CategoryViolence,
		},
	}
}

func (a *azure) Close() error { return nil }

// AnalyzeImage validates input, calls the REST API, and normalizes the result
// into a single-frame NormalizedResult. The pipeline stamps Threshold/Flagged
// and the asset rollup. ModelVersion is the api-version (§E single source).
func (a *azure) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	if err := validateInput(img); err != nil {
		return moderation.NormalizedResult{}, err // terminal, never allow
	}

	resp, err := a.client.analyze(ctx, img.Bytes)
	if err != nil {
		return moderation.NormalizedResult{}, err
	}

	// image:analyze guarantees the 4 categories. An empty analysis is an
	// unexpected provider state — treat it as could-not-evaluate (fail-safe),
	// never a clean frame. Don't rely on the rollup's all-nil catch downstream.
	cats := normalize(resp)
	if len(cats) == 0 {
		return moderation.NormalizedResult{}, fmt.Errorf("azure: empty categoriesAnalysis (provider returned no analysis)")
	}

	return moderation.NormalizedResult{
		Provider:     adapterName,
		ModelVersion: a.apiVersion,
		MediaType:    "image",
		Frames: []moderation.FrameResult{{
			TimestampSec: nil,
			Status:       moderation.FrameStatusOK,
			Categories:   cats,
		}},
	}, nil
}

// validateInput enforces the hard 4 MB cap and the format allow-list (§C).
// Both are terminal (a retry won't fix oversize or an unsupported type).
func validateInput(img moderation.Image) error {
	if len(img.Bytes) == 0 {
		return fmt.Errorf("azure: empty image")
	}
	if int64(len(img.Bytes)) > maxImageBytes {
		return fmt.Errorf("azure: image %d bytes exceeds 4 MB cap", len(img.Bytes))
	}
	if img.MIME != "" && !allowedMIME[img.MIME] {
		return fmt.Errorf("azure: unsupported MIME %q (allowed: jpeg/png/gif/bmp/tiff/webp)", img.MIME)
	}
	return nil
}
