// Package hive implements the thehive.ai Visual Moderation adapter.
//
// Response schema verified against Hive's docs (docs.thehive.ai:
// "Using Hive's Visual Moderation API", "Creating Tasks in the API",
// "Async API Examples", and the visual content moderation class list),
// checked 2026-07-29: the sync task returns a `status` ARRAY whose entries
// carry `response.output[]`, each output holding `time` and
// `classes[{class, score}]` — the shape read below. Class names were
// re-checked against the published head list; five names carried here
// (gore, violence, yes_drugs, yes_gun, yes_knife) do not exist in Hive's
// taxonomy and were replaced with the documented heads. The request
// encoding was verified in the same pass: media is uploaded as a
// multipart/form-data `media` part, the only documented key that fits
// in-memory frame bytes.
//
// Output model: a multi-head classifier; each head yields a probability
// in [0,1]. Normalization takes each head's probability as Score with
// ScoreOrigin="probability". Any head with no canonical mapping goes to
// OTHER preserving the raw label — heads are never dropped, and an
// unknown value is nil, never 0.
package hive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

const defaultEndpoint = "https://api.thehive.ai/api/v2/task/sync"

func init() {
	moderate.Register("hive", New)
}

type options struct {
	Endpoint     string            `json:"endpoint"`       // default Hive sync endpoint
	RateLimitRPS float64           `json:"rate_limit_rps"` // default 10
	MaxAttempts  int               `json:"max_attempts"`
	ClassMap     map[string]string `json:"class_map"` // extra head->canonical overrides
}

// defaultClassMap maps Hive visual-moderation heads onto the canonical
// taxonomy. Every key appears in Hive's published class list (checked
// 2026-07-29); the full head-by-head audit, including why each unmapped
// head is unmapped, is the table in MODEL_LIMITATIONS.md.
//
// Three groups deliberately stay unmapped and fall through to OTHER with
// the label and score preserved:
//   - Negative/absence heads (no_*, general_not_*, no_tongue). They are
//     the complement of a mapped head, so scoring them as harm would
//     double-count the same signal inverted.
//   - Ordinary-apparel heads (swimwear, sports bra, miniskirt, bodysuit,
//     shirtless). Mapping clothing to SUGGESTIVE_RACY is the swimwear
//     over-flagging MODEL_LIMITATIONS.md warns about; anatomy and intent
//     heads are mapped instead.
//   - Child-related heads (yes_child_safety, yes_child_present). vismod
//     defines no special category and adds no detection logic of its own
//     (invariant 7); these stay vendor-scoped signals.
//
// All three remain fully reachable: they carry their score, they flag
// against the OTHER threshold, and per-label thresholds can target them
// by name.
var defaultClassMap = map[string]moderation.Category{
	// Sexual: explicit acts, nudity, anatomy, and stated intent.
	"general_nsfw":               moderation.CategorySexual,
	"yes_sexual_activity":        moderation.CategorySexual,
	"yes_realistic_nsfw":         moderation.CategorySexual,
	"yes_sexual_intent":          moderation.CategorySexual,
	"yes_undressed":              moderation.CategorySexual,
	"yes_female_nudity":          moderation.CategorySexual,
	"yes_male_nudity":            moderation.CategorySexual,
	"yes_genitals":               moderation.CategorySexual,
	"yes_breast":                 moderation.CategorySexual,
	"yes_sex_toy":                moderation.CategorySexual,
	"animal_genitalia_and_human": moderation.CategorySexual,
	// Suggestive: underwear, partial-anatomy and contact heads.
	"general_suggestive":   moderation.CategorySuggestiveRacy,
	"yes_female_underwear": moderation.CategorySuggestiveRacy,
	"yes_male_underwear":   moderation.CategorySuggestiveRacy,
	"yes_bra":              moderation.CategorySuggestiveRacy,
	"yes_panties":          moderation.CategorySuggestiveRacy,
	"yes_negligee":         moderation.CategorySuggestiveRacy,
	"yes_cleavage":         moderation.CategorySuggestiveRacy,
	"yes_bulge":            moderation.CategorySuggestiveRacy,
	"yes_butt":             moderation.CategorySuggestiveRacy,
	"kissing":              moderation.CategorySuggestiveRacy,
	"licking":              moderation.CategorySuggestiveRacy,
	// Gore: blood and corpses.
	"very_bloody":     moderation.CategoryGoreGraphic,
	"a_little_bloody": moderation.CategoryGoreGraphic,
	"other_blood":     moderation.CategoryGoreGraphic,
	"human_corpse":    moderation.CategoryGoreGraphic,
	"animated_corpse": moderation.CategoryGoreGraphic,
	// Violence.
	"yes_fight":        moderation.CategoryViolence,
	"yes_animal_abuse": moderation.CategoryViolence,
	// Self-harm. yes_emaciated_body follows Hive's own grouping; it
	// over-flags medical and famine imagery, so tune it by label.
	"hanging":            moderation.CategorySelfHarm,
	"noose":              moderation.CategorySelfHarm,
	"yes_self_harm":      moderation.CategorySelfHarm,
	"yes_emaciated_body": moderation.CategorySelfHarm,
	// Hate: symbols and affiliations targeting protected groups.
	"yes_nazi":        moderation.CategoryHate,
	"yes_kkk":         moderation.CategoryHate,
	"yes_confederate": moderation.CategoryHate,
	"yes_terrorist":   moderation.CategoryHate,
	// Offensive but not group-targeted.
	"yes_middle_finger": moderation.CategoryOffensiveGesture,
	// Drugs: illicit only. Legal vice is ALCOHOL_TOBACCO.
	"illicit_injectables":  moderation.CategoryDrugs,
	"yes_pills":            moderation.CategoryDrugs,
	"yes_marijuana":        moderation.CategoryDrugs,
	"yes_alcohol":          moderation.CategoryAlcoholTobacco,
	"yes_drinking_alcohol": moderation.CategoryAlcoholTobacco,
	"animated_alcohol":     moderation.CategoryAlcoholTobacco,
	"yes_smoking":          moderation.CategoryAlcoholTobacco,
	"yes_gambling":         moderation.CategoryGambling,
	// Weapons. Culinary knives are explicitly NOT weapons signals, but
	// are mapped rather than left to fall through so the intent is
	// visible in the map itself.
	"gun_in_hand":                moderation.CategoryWeapons,
	"gun_not_in_hand":            moderation.CategoryWeapons,
	"animated_gun":               moderation.CategoryWeapons,
	"knife_in_hand":              moderation.CategoryWeapons,
	"knife_not_in_hand":          moderation.CategoryWeapons,
	"culinary_knife_in_hand":     moderation.CategoryOther,
	"culinary_knife_not_in_hand": moderation.CategoryOther,
	// Provenance, not harm: does this depict reality?
	"animated":                  moderation.CategoryAnimatedSynthetic,
	"hybrid":                    moderation.CategoryAnimatedSynthetic,
	"natural":                   moderation.CategoryAnimatedSynthetic,
	"yes_drawing":               moderation.CategoryAnimatedSynthetic,
	"animated_animal_genitalia": moderation.CategoryAnimatedSynthetic,
	// Medical provenance (Google's medical signal has the same meaning).
	"medical_injectables": moderation.CategoryMedical,
}

type Moderator struct {
	opts     options
	token    string
	client   *http.Client
	limiter  *moderate.Limiter
	classMap map[string]moderation.Category
}

// New is the registry factory. Token auth is env-only:
// VISMOD_HIVE_API_TOKEN.
func New(cfg moderate.AdapterConfig) (moderation.Moderator, error) {
	var o options
	b, _ := json.Marshal(cfg.Options)
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("hive: options: %w", err)
	}
	if o.Endpoint == "" {
		o.Endpoint = defaultEndpoint
	}
	if o.RateLimitRPS == 0 {
		o.RateLimitRPS = 10
	}
	token := cfg.Secret("hive.api_token")
	if token == "" {
		return nil, fmt.Errorf("hive: secret VISMOD_HIVE_API_TOKEN is required")
	}

	cm := make(map[string]moderation.Category, len(defaultClassMap)+len(o.ClassMap))
	for k, v := range defaultClassMap {
		cm[k] = v
	}
	for k, v := range o.ClassMap {
		cm[strings.ToLower(k)] = moderation.Canonicalize(moderation.Category(strings.ToUpper(v)))
	}

	return &Moderator{
		opts:     o,
		token:    token,
		client:   &http.Client{Timeout: 60 * time.Second},
		limiter:  moderate.NewLimiter(o.RateLimitRPS),
		classMap: cm,
	}, nil
}

func (m *Moderator) Name() string         { return "hive" }
func (m *Moderator) ModelVersion() string { return "visual-moderation-v2" }
func (m *Moderator) Close() error         { return nil }

func (m *Moderator) Capabilities() moderation.Caps {
	return moderation.Caps{
		// Hive has video-capable endpoints; v1 of this adapter moderates
		// per-frame like the others. If the native video endpoint is
		// verified at integration time, implement moderation.VideoModerator
		// and flip this to true.
		SupportsVideo: false,
		MaxImageBytes: 20 << 20,
		// Must stay in sync with the value set of defaultClassMap; the
		// adapter test asserts that.
		Categories: []moderation.Category{
			moderation.CategorySexual, moderation.CategorySuggestiveRacy,
			moderation.CategoryViolence, moderation.CategoryGoreGraphic,
			moderation.CategoryWeapons, moderation.CategorySelfHarm,
			moderation.CategoryHate, moderation.CategoryOffensiveGesture,
			moderation.CategoryDrugs, moderation.CategoryAlcoholTobacco,
			moderation.CategoryGambling, moderation.CategoryMedical,
			moderation.CategoryAnimatedSynthetic, moderation.CategoryOther,
		},
	}
}

// syncResponse is the assumed v2 sync envelope: status[0].response.output
// is a list of per-frame outputs, each with classes [{class, score}].
type syncResponse struct {
	Status []struct {
		Response struct {
			Output []struct {
				Time    float64 `json:"time"`
				Classes []struct {
					Class string  `json:"class"`
					Score float64 `json:"score"`
				} `json:"classes"`
			} `json:"output"`
		} `json:"response"`
	} `json:"status"`
}

// mediaField is the documented form key for streaming a file through the
// sync task POST ("Creating Tasks in the API" / "Sync API Examples",
// docs.thehive.ai, checked 2026-07-29). The only other documented media
// keys are `url` (fetched by Hive) and `text_data`; there is no base64
// key. vismod holds frames in memory and never hands a vendor a URL to
// fetch, so `media` is the only applicable one.
const mediaField = "media"

// mediaFilename derives the upload filename from the frame's MIME type.
// Hive keys off the extension; an unrecognized type falls back to .jpg,
// which is what the ffmpeg frame extractor produces.
func mediaFilename(mimeType string) string {
	switch strings.ToLower(mimeType) {
	case "image/png":
		return "frame.png"
	case "image/webp":
		return "frame.webp"
	case "image/gif":
		return "frame.gif"
	default:
		return "frame.jpg"
	}
}

func (m *Moderator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	filename := mediaFilename(img.MIME)
	respBody, err := moderate.DoJSON(ctx, m.client, func() (*http.Request, error) {
		// Limiter inside the per-attempt builder: retries take tokens too.
		if err := m.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		r, err := moderate.NewMultipartRequest(m.opts.Endpoint, mediaField, filename, img.Bytes)
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Token "+m.token)
		return r, nil
	}, m.opts.MaxAttempts, 0, "")
	if err != nil {
		return moderation.NormalizedResult{}, fmt.Errorf("hive: %w", err)
	}

	var resp syncResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return moderation.NormalizedResult{}, fmt.Errorf("hive: decode response: %w", err)
	}
	return m.normalize(resp)
}

func (m *Moderator) normalize(resp syncResponse) (moderation.NormalizedResult, error) {
	if len(resp.Status) == 0 || len(resp.Status[0].Response.Output) == 0 {
		return moderation.NormalizedResult{}, fmt.Errorf("hive: response contained no output (could not evaluate)")
	}
	out := resp.Status[0].Response.Output[0]
	cats := make([]moderation.CategoryResult, 0, len(out.Classes))
	for _, c := range out.Classes {
		canonical, ok := m.classMap[strings.ToLower(c.Class)]
		if !ok {
			canonical = moderation.CategoryOther
		}
		score := c.Score
		cats = append(cats, moderation.CategoryResult{
			Category:      canonical,
			ProviderLabel: c.Class,
			Score:         &score,
			ScoreOrigin:   moderation.OriginProbability,
		})
	}
	raw, err := json.Marshal(out) // sanitized: class labels + scores only
	if err != nil {
		return moderation.NormalizedResult{}, err
	}
	return moderation.NormalizedResult{
		Provider:     "hive",
		ModelVersion: m.ModelVersion(),
		Frames:       []moderation.FrameResult{{Status: moderation.FrameOK, Categories: cats}},
		Raw:          raw,
	}, nil
}
