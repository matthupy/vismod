// Package microsoft implements the Azure AI Content Safety image adapter.
//
// Research-anchored contract (GA api-version=2024-09-01; re-verify against
// live docs before bumping — the service deprecates versions on a ~90-day
// cadence):
//   - POST {endpoint}/contentsafety/image:analyze?api-version=2024-09-01
//     (synchronous; no Go SDK for the data plane — direct REST).
//   - Response top level has ONLY categoriesAnalysis:
//     [{"category":"Hate","severity":0}, ...] — exactly 4 categories
//     (Hate, SelfHarm, Sexual, Violence), no score/decision fields.
//   - IMAGE severity is the TRIMMED scale ONLY: discrete 0,2,4,6.
//     Normalized Score = severity/6.0, ScoreOrigin="severity".
//   - Max image size 4 MB (Caps.MaxImageBytes); no batch API; F0 = 5 RPS.
//
// SSRF/egress: v1 sends inline base64 content ONLY. blobUrl (or any
// url/s3 source) is a remote-fetch vector and stays disabled; enabling it
// requires a host/scheme allow-list forbidding RFC1918 / 169.254.0.0/16 /
// ::1 (see SECURITY.md).
//
// Detection scope, special-category handling, and acceptable use are
// governed by Azure's terms — operators are responsible for compliance.
package microsoft

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

// DefaultAPIVersion is configurable (options.api_version) because Azure
// deprecates Content Safety API versions on a ~90-day cadence.
const DefaultAPIVersion = "2024-09-01"

const maxImageBytes = 4 << 20 // 4 MB service limit

func init() {
	moderate.Register("microsoft", New)
}

type options struct {
	Endpoint     string  `json:"endpoint"`       // https://<resource>.cognitiveservices.azure.com
	APIVersion   string  `json:"api_version"`    // default DefaultAPIVersion
	AuthMode     string  `json:"auth_mode"`      // "key" (default) | "entra"
	RateLimitRPS float64 `json:"rate_limit_rps"` // default 5 (F0 tier)
	MaxAttempts  int     `json:"max_attempts"`   // HTTP retry budget, default 3
}

// Moderator is the Azure Content Safety adapter.
type Moderator struct {
	opts    options
	client  *http.Client
	limiter *moderate.Limiter
	// auth
	key    string
	tokens tokenSource
}

// tokenSource yields Entra bearer tokens (static env token or managed
// identity IMDS).
type tokenSource interface {
	Token(ctx context.Context) (string, error)
}

// New is the registry factory. Fail-fast: missing endpoint or credentials
// is a boot error, not a per-job failure.
func New(cfg moderate.AdapterConfig) (moderation.Moderator, error) {
	var o options
	if err := decodeOptions(cfg.Options, &o); err != nil {
		return nil, fmt.Errorf("microsoft: options: %w", err)
	}
	if o.Endpoint == "" {
		return nil, fmt.Errorf("microsoft: adapter.options.endpoint is required (https://<resource>.cognitiveservices.azure.com)")
	}
	o.Endpoint = strings.TrimRight(o.Endpoint, "/")
	if o.APIVersion == "" {
		o.APIVersion = DefaultAPIVersion
	}
	if o.RateLimitRPS == 0 {
		// Azure F0 moderation quota is 5 RPS (verified against live docs
		// 2026-07). Default BELOW it: pacing at exactly the quota races
		// the provider's window boundaries and still draws 429s.
		o.RateLimitRPS = 4
	}

	m := &Moderator{
		opts:    o,
		client:  &http.Client{Timeout: 30 * time.Second},
		limiter: moderate.NewLimiter(o.RateLimitRPS),
	}
	switch o.AuthMode {
	case "", "key":
		m.key = cfg.Secret("microsoft.api_key")
		if m.key == "" {
			return nil, fmt.Errorf("microsoft: secret VISMOD_MICROSOFT_API_KEY is required for auth_mode=key")
		}
	case "entra":
		if tok := cfg.Secret("microsoft.access_token"); tok != "" {
			m.tokens = staticToken(tok)
		} else {
			// Managed identity via IMDS (works on Azure compute).
			m.tokens = newIMDSTokenSource(m.client)
		}
	default:
		return nil, fmt.Errorf("microsoft: unknown auth_mode %q (key|entra)", o.AuthMode)
	}
	return m, nil
}

func (m *Moderator) Name() string         { return "microsoft" }
func (m *Moderator) ModelVersion() string { return m.opts.APIVersion }
func (m *Moderator) Close() error         { return nil }

func (m *Moderator) Capabilities() moderation.Caps {
	return moderation.Caps{
		SupportsVideo: false, // frames, per-image
		MaxImageBytes: maxImageBytes,
		Categories: []moderation.Category{
			moderation.CategoryHate, moderation.CategorySelfHarm,
			moderation.CategorySexual, moderation.CategoryViolence,
		},
	}
}

// analyzeRequest is the v1 request: inline content only (see SSRF note).
type analyzeRequest struct {
	Image struct {
		Content string `json:"content"`
	} `json:"image"`
	OutputType string `json:"outputType"`
}

const outputTypeFourSeverity = "FourSeverityLevels"

// encodeAnalyzeRequest writes the analyze body in one pass.
//
// The obvious spelling — base64 into a string, then json.Marshal the struct
// — holds three copies of the frame at once: the raw bytes, the base64
// string, and marshal's output. At frames.concurrency in-flight frames of
// up to maxImageBytes that is the largest resident allocation in the
// process, and it is all avoidable: the base64 alphabet contains no
// character JSON escapes, and the encoded length is known exactly, so the
// body can be built into a single right-sized buffer.
//
// It must stay byte-identical to the json.Marshal form; the test asserts
// that against marshal itself rather than against a hand-written literal.
func encodeAnalyzeRequest(raw []byte) []byte {
	const prefix = `{"image":{"content":"`
	const suffix = `"},"outputType":"` + outputTypeFourSeverity + `"}`

	buf := make([]byte, 0, len(prefix)+base64.StdEncoding.EncodedLen(len(raw))+len(suffix))
	buf = append(buf, prefix...)
	buf = base64.StdEncoding.AppendEncode(buf, raw)
	return append(buf, suffix...)
}

type analyzeResponse struct {
	CategoriesAnalysis []struct {
		Category string `json:"category"`
		Severity int    `json:"severity"`
	} `json:"categoriesAnalysis"`
}

// categoryMap: Hate→HATE, Sexual→SEXUAL, Violence→VIOLENCE,
// SelfHarm→SELF_HARM. Anything else (future) falls back to OTHER.
var categoryMap = map[string]moderation.Category{
	"Hate":     moderation.CategoryHate,
	"Sexual":   moderation.CategorySexual,
	"Violence": moderation.CategoryViolence,
	"SelfHarm": moderation.CategorySelfHarm,
}

func (m *Moderator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	if int64(len(img.Bytes)) > maxImageBytes {
		return moderation.NormalizedResult{}, fmt.Errorf("microsoft: image %d bytes exceeds 4 MB limit (terminal)", len(img.Bytes))
	}

	body := encodeAnalyzeRequest(img.Bytes)

	url := fmt.Sprintf("%s/contentsafety/image:analyze?api-version=%s", m.opts.Endpoint, m.opts.APIVersion)
	respBody, err := moderate.DoJSON(ctx, m.client, func() (*http.Request, error) {
		// The limiter sits INSIDE the per-attempt builder so retries also
		// take a token — otherwise a 429 storm has retries stacking on
		// top of fresh requests and the aggregate rate exceeds the quota.
		if err := m.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		r, err := moderate.NewJSONRequest(url, body)
		if err != nil {
			return nil, err
		}
		if err := m.authorize(ctx, r); err != nil {
			return nil, err
		}
		return r, nil
	}, m.opts.MaxAttempts, 0, "x-ms-error-code")
	if err != nil {
		return moderation.NormalizedResult{}, fmt.Errorf("microsoft: %w", err)
	}

	var resp analyzeResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return moderation.NormalizedResult{}, fmt.Errorf("microsoft: decode response: %w", err)
	}
	return m.normalize(resp)
}

// normalize maps the trimmed image severity scale (0,2,4,6) to
// Score = severity/6.0 with ScoreOrigin="severity". Unmapped categories
// fall back to OTHER, preserving the raw label — never dropped.
func (m *Moderator) normalize(resp analyzeResponse) (moderation.NormalizedResult, error) {
	cats := make([]moderation.CategoryResult, 0, len(resp.CategoriesAnalysis))
	for _, ca := range resp.CategoriesAnalysis {
		canonical, ok := categoryMap[ca.Category]
		if !ok {
			canonical = moderation.CategoryOther
		}
		score := float64(ca.Severity) / 6.0
		cats = append(cats, moderation.CategoryResult{
			Category:      canonical,
			ProviderLabel: ca.Category,
			Score:         &score,
			ScoreOrigin:   moderation.OriginSeverity,
		})
	}
	raw, err := json.Marshal(resp) // sanitized by construction: numeric severities only
	if err != nil {
		return moderation.NormalizedResult{}, err
	}
	return moderation.NormalizedResult{
		Provider:     "microsoft",
		ModelVersion: m.opts.APIVersion,
		Frames:       []moderation.FrameResult{{Status: moderation.FrameOK, Categories: cats}},
		Raw:          raw,
	}, nil
}

func (m *Moderator) authorize(ctx context.Context, r *http.Request) error {
	if m.key != "" {
		r.Header.Set("Ocp-Apim-Subscription-Key", m.key)
		return nil
	}
	tok, err := m.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("entra token: %w", err)
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

func decodeOptions(in map[string]any, out *options) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
