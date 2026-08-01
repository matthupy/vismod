// Package shieldgemma implements the first SELF-HOSTED adapter: it speaks
// HTTP to an inference server the operator runs (vLLM / TGI / anything
// exposing an OpenAI-compatible chat-completions endpoint) serving
// google/shieldgemma-2-4b-it. vismod still loads no models and runs no
// inference of its own; the "vendor" is simply self-operated, so invariant
// 7 holds unchanged. Model choice, threshold mode, SSRF posture, Limiter
// shape and model_version handling are settled in
// docs/self-hosted-classifiers.md (2026-07-29) — this package implements
// that document's conclusions.
//
// Output model: one request scores ONE policy, returning the probability of
// the Yes token, so three policies mean three requests per frame. That
// probability is a real per-policy [0,1] number and maps onto the existing
// ScoreOrigin "probability" — no new origin, no rollup change, no envelope
// change. Policy text is supplied in the prompt; the model card warns the
// model is highly sensitive to that wording, so the prompts in this file
// are verdict-affecting and must not be edited casually.
//
// THRESHOLD MODE: this adapter refuses to construct unless
// provider_thresholds.mode is "override". The default and per-category
// thresholds hold severity/6 and likelihood-enum magnitudes tuned for the
// cloud vendors; letting a probability inherit them would compare
// quantities that are not the same quantity.
//
// REQUEST AMPLIFICATION RISK (docs/self-hosted-classifiers.md,
// 2026-07-29): TGI returns 429 under load, which moderate.DoJSON treats as
// retryable and backs off correctly. vLLM instead QUEUES — the request does
// not fail, it just gets slower until the client timeout fires, and a
// timeout is also retryable, so vismod would retry work the server is still
// doing and ADD load. Mitigations, both configurable here: set
// timeout_seconds well above the server's observed p99 (a VLM is seconds
// per image, not milliseconds) and keep max_attempts small. In-flight
// concurrency is bounded by queue.workers x frames.concurrency, NOT by
// rate_limit_rps; that product is what has to fit the GPU. The limiter is a
// burst damper only and is per REQUEST, not per frame.
package shieldgemma

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

func init() {
	moderate.Register("shieldgemma", New)
}

// The three policies ShieldGemma 2 is trained against. These names are
// vismod's provider labels: the policy text is supplied in the prompt, so
// the ADAPTER authors its own label set (which is what makes the boot-time
// completeness check exact rather than best-effort).
const (
	policySexuallyExplicit = "sexually_explicit"
	policyDangerousContent = "dangerous_content"
	policyViolenceGore     = "violence_gore"
)

// policyPrompts holds the policy text inserted into each request. The model
// card warns that ShieldGemma 2 is HIGHLY sensitive to this wording, which
// makes these strings verdict-affecting: changing one changes what the
// scores mean without changing config_hash.
var policyPrompts = map[string]string{
	policySexuallyExplicit: "The image shall not contain content that depicts explicit or graphic sexual acts (e.g., pornography, erotic nudity, depictions of rape or sexual assault).",
	policyDangerousContent: "The image shall not contain content that facilitates or encourages activities that could cause real-world harm (e.g., building firearms and explosive devices, promotion of terrorism, instructions for suicide).",
	policyViolenceGore:     "The image shall not contain content that depicts shocking, sensational, or gratuitous violence (e.g., excessive blood and gore, gratuitous violence against animals, extreme injury or moment of death).",
}

// policyCategories maps each policy onto the canonical taxonomy.
//
// dangerous_content -> OTHER is a deliberate, documented choice, not a
// fallback. ShieldGemma's dangerous-content policy spans weapons, illicit
// drugs, terrorism and suicide instruction in ONE score; attributing that
// score to WEAPONS, DRUGS or SELF_HARM would claim a decomposition the
// model never made. OTHER preserves the label and the score, and a
// label: threshold can target it by name, which is exactly the surface
// override mode requires anyway.
var policyCategories = map[string]moderation.Category{
	policySexuallyExplicit: moderation.CategorySexual,
	policyViolenceGore:     moderation.CategoryGoreGraphic,
	policyDangerousContent: moderation.CategoryOther,
}

type options struct {
	// Endpoint is the full chat-completions URL of the operator's inference
	// server. Config-only: it is never read from a job or intake body.
	Endpoint string `json:"endpoint"`
	// ModelVersion is REQUIRED and has no default. For a self-hosted server
	// it is an OPERATOR CLAIM, not a vendor-pinned identifier — see
	// MODEL_LIMITATIONS.md.
	ModelVersion string `json:"model_version"`
	// Policies narrows the scored policy set; empty means all three.
	Policies []string `json:"policies"`
	// RateLimitRPS is a burst damper, not the ceiling. Default 0 (disabled):
	// there is no billing quota to stay under, and concurrency — not RPS —
	// is what protects the box.
	RateLimitRPS   float64 `json:"rate_limit_rps"`
	MaxAttempts    int     `json:"max_attempts"`
	TimeoutSeconds int     `json:"timeout_seconds"`
}

type Moderator struct {
	opts     options
	policies []string // sorted; the declared label set
	client   *http.Client
	limiter  *moderate.Limiter
}

// New is the registry factory. Construction IS boot validation: an absent
// or unsafe endpoint, an absent model_version, a threshold mode other than
// override, or an unknown policy name all refuse to start.
func New(cfg moderate.AdapterConfig) (moderation.Moderator, error) {
	var o options
	b, _ := json.Marshal(cfg.Options)
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("shieldgemma: options: %w", err)
	}
	if err := validateEndpoint(o.Endpoint); err != nil {
		return nil, fmt.Errorf("shieldgemma: %w", err)
	}
	o.ModelVersion = strings.TrimSpace(o.ModelVersion)
	if o.ModelVersion == "" {
		return nil, fmt.Errorf("shieldgemma: model_version is required and has no default — a self-hosted server exposes no vendor-pinned version, so the operator must state what they are running (it is recorded in config_hash)")
	}
	if mode := strings.ToLower(strings.TrimSpace(cfg.ProviderThresholdMode)); mode != config.ProviderModeOverride {
		return nil, fmt.Errorf("shieldgemma: provider_thresholds.mode must be %q, got %q — category and default thresholds hold cloud-vendor magnitudes (severity/6, likelihood buckets) that a self-hosted probability must not inherit",
			config.ProviderModeOverride, cfg.ProviderThresholdMode)
	}

	policies := make([]string, 0, len(policyPrompts))
	if len(o.Policies) == 0 {
		for p := range policyPrompts {
			policies = append(policies, p)
		}
	} else {
		for _, p := range o.Policies {
			name := strings.ToLower(strings.TrimSpace(p))
			if _, ok := policyPrompts[name]; !ok {
				return nil, fmt.Errorf("shieldgemma: unknown policy %q (known: %v)", p, knownPolicies())
			}
			policies = append(policies, name)
		}
	}
	sort.Strings(policies) // stable request and category order

	return &Moderator{
		opts:     o,
		policies: policies,
		client:   newHTTPClient(time.Duration(o.TimeoutSeconds) * time.Second),
		limiter:  moderate.NewLimiter(o.RateLimitRPS),
	}, nil
}

func knownPolicies() []string {
	out := make([]string, 0, len(policyPrompts))
	for p := range policyPrompts {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (m *Moderator) Name() string { return "shieldgemma" }

// ModelVersion is the operator's claim about what the endpoint serves. A
// swapped checkpoint under an unchanged served-model name produces an
// unchanged config_hash over changed behavior; see MODEL_LIMITATIONS.md.
func (m *Moderator) ModelVersion() string { return m.opts.ModelVersion }

func (m *Moderator) Close() error { return nil }

// ProviderLabels declares every label this adapter can emit, so the boot
// step in internal/cli can refuse to start when one of them has no key
// under provider_thresholds.labels. In override mode an unconfigured label
// has no boundaries at all and can never flag or block, so a typo would
// otherwise disarm a hazard silently.
func (m *Moderator) ProviderLabels() []string {
	out := make([]string, len(m.policies))
	copy(out, m.policies)
	return out
}

func (m *Moderator) Capabilities() moderation.Caps {
	return moderation.Caps{
		SupportsVideo: false, // image-only → the pipeline extracts frames
		MaxImageBytes: 20 << 20,
		// Must stay in sync with the value set of policyCategories; the
		// adapter test asserts that.
		Categories: []moderation.Category{
			moderation.CategorySexual, moderation.CategoryGoreGraphic,
			moderation.CategoryOther,
		},
	}
}

// chatRequest is the OpenAI-compatible chat-completions body. max_tokens is
// 1 and logprobs are requested because the SCORE is the probability of that
// single Yes/No token — the generated text itself is discarded.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Logprobs    bool          `json:"logprobs"`
	TopLogprobs int           `json:"top_logprobs"`
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// chatResponse reads only what the score needs.
type chatResponse struct {
	Choices []struct {
		Logprobs struct {
			Content []struct {
				Token       string  `json:"token"`
				Logprob     float64 `json:"logprob"`
				TopLogprobs []struct {
					Token   string  `json:"token"`
					Logprob float64 `json:"logprob"`
				} `json:"top_logprobs"`
			} `json:"content"`
		} `json:"logprobs"`
	} `json:"choices"`
}

// topLogprobs is how many alternatives to ask for. Yes and No are the only
// tokens read, but a server may rank punctuation or whitespace variants
// above them.
const topLogprobs = 8

func dataURI(img moderation.Image) string {
	mime := strings.ToLower(strings.TrimSpace(img.MIME))
	if mime == "" {
		mime = "image/jpeg" // what the ffmpeg frame extractor produces
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Bytes)
}

func (m *Moderator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	uri := dataURI(img)
	cats := make([]moderation.CategoryResult, 0, len(m.policies))
	rawMap := make(map[string]float64, len(m.policies))
	for _, policy := range m.policies {
		p, err := m.scorePolicy(ctx, policy, uri)
		if err != nil {
			// One unscorable policy makes the whole frame unscorable: a
			// partial result would silently drop a hazard.
			return moderation.NormalizedResult{}, err
		}
		score := p
		rawMap[policy] = p
		cats = append(cats, moderation.CategoryResult{
			Category:      policyCategories[policy],
			ProviderLabel: policy,
			Score:         &score,
			ScoreOrigin:   moderation.OriginProbability,
		})
	}
	raw, err := json.Marshal(rawMap) // sanitized: policy names + scores only
	if err != nil {
		return moderation.NormalizedResult{}, err
	}
	return moderation.NormalizedResult{
		Provider:     "shieldgemma",
		ModelVersion: m.ModelVersion(),
		Frames:       []moderation.FrameResult{{Status: moderation.FrameOK, Categories: cats}},
		Raw:          raw,
	}, nil
}

func (m *Moderator) scorePolicy(ctx context.Context, policy, uri string) (float64, error) {
	body, err := json.Marshal(chatRequest{
		Model: m.opts.ModelVersion,
		Messages: []chatMessage{{
			Role: "user",
			Content: []contentPart{
				{Type: "image_url", ImageURL: &imageURL{URL: uri}},
				{Type: "text", Text: policyPrompts[policy]},
			},
		}},
		MaxTokens:   1,
		Temperature: 0,
		Logprobs:    true,
		TopLogprobs: topLogprobs,
	})
	if err != nil {
		return 0, fmt.Errorf("shieldgemma: %s: encode request: %w", policy, err)
	}

	respBody, err := moderate.DoJSON(ctx, m.client, func() (*http.Request, error) {
		// Limiter inside the per-attempt builder: retries take tokens too.
		if err := m.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		// Nothing is added to the request beyond the policy prompt and the
		// image: HTTPError embeds up to 200 bytes of the response body,
		// which a VLM server may echo from the request.
		return moderate.NewJSONRequest(m.opts.Endpoint, body)
	}, m.opts.MaxAttempts, 0, "")
	if err != nil {
		return 0, fmt.Errorf("shieldgemma: %s: %w", policy, err)
	}

	var resp chatResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return 0, fmt.Errorf("shieldgemma: %s: decode response: %w", policy, err)
	}
	return yesProbability(resp, policy)
}

// yesProbability recovers P(violates policy) from the single generated
// token's alternatives, as P(Yes) renormalized over the Yes/No pair —
// the quantity the model card describes.
//
// BOTH tokens are required. Falling back to a lone token's raw probability
// would put a DIFFERENT quantity (unnormalized P(token), a downward-biased
// lower bound) under the same "probability" ScoreOrigin, which is precisely
// the cross-quantity comparison MODEL_LIMITATIONS.md exists to prevent. The
// cost is that a server ranking No outside top_logprobs makes the frame
// unscorable — an error verdict and human review, never an allow. Raise
// top_logprobs before weakening this.
func yesProbability(resp chatResponse, policy string) (float64, error) {
	if len(resp.Choices) == 0 || len(resp.Choices[0].Logprobs.Content) == 0 {
		return 0, fmt.Errorf("shieldgemma: %s: response carried no logprobs (could not evaluate)", policy)
	}
	var pYes, pNo float64
	var haveYes, haveNo bool
	for _, a := range resp.Choices[0].Logprobs.Content[0].TopLogprobs {
		switch strings.ToLower(strings.TrimSpace(a.Token)) {
		case "yes":
			if !haveYes { // first (highest-ranked) occurrence wins
				pYes, haveYes = math.Exp(a.Logprob), true
			}
		case "no":
			if !haveNo {
				pNo, haveNo = math.Exp(a.Logprob), true
			}
		}
	}
	if !haveYes || !haveNo || pYes+pNo <= 0 {
		return 0, fmt.Errorf("shieldgemma: %s: response did not report both a Yes and a No token among the alternatives (could not evaluate)", policy)
	}
	return clamp01(pYes/(pYes+pNo), policy)
}

// clamp01 bounds a probability to [0,1]. A non-finite value is an ERROR,
// not a clamp: silently turning NaN into 0 would mean "confidently safe"
// (invariant 2).
func clamp01(v float64, policy string) (float64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("shieldgemma: %s: logprobs yielded a non-finite probability (could not evaluate)", policy)
	}
	return math.Min(1, math.Max(0, v)), nil
}

var _ moderation.Moderator = (*Moderator)(nil)
