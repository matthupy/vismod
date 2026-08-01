package hive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/internal/moderate/adapters/golden"
	"github.com/vismod/vismod/pkg/moderation"
)

// fixture pins the v2 sync envelope verified against Hive's docs on
// 2026-07-29. Every class name below is a documented head except the
// deliberately synthetic brand_new_future_head, which exercises the
// never-drop-a-head fallback.
const fixture = `{"status":[{"response":{"output":[{"time":0,"classes":[
	{"class":"general_nsfw","score":0.97},
	{"class":"general_suggestive","score":0.42},
	{"class":"gun_in_hand","score":0.11},
	{"class":"knife_not_in_hand","score":0.08},
	{"class":"very_bloody","score":0.03},
	{"class":"a_little_bloody","score":0.07},
	{"class":"yes_marijuana","score":0.21},
	{"class":"yes_smoking","score":0.30},
	{"class":"yes_gambling","score":0.15},
	{"class":"yes_middle_finger","score":0.60},
	{"class":"animated","score":0.88},
	{"class":"medical_injectables","score":0.12},
	{"class":"yes_female_swimwear","score":0.71},
	{"class":"yes_child_present","score":0.33},
	{"class":"no_sexual_activity","score":0.02},
	{"class":"brand_new_future_head","score":0.5}]}]}}]}`

func secretEnv(key string) string {
	if key == "hive.api_token" {
		return "test-token"
	}
	return ""
}

func newTestModerator(t *testing.T, url string) moderation.Moderator {
	t.Helper()
	m, err := New(moderate.AdapterConfig{
		Name:    "hive",
		Options: map[string]any{"endpoint": url, "rate_limit_rps": 1000.0},
		Secret:  secretEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAnalyzeImageGolden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token test-token" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	m := newTestModerator(t, srv.URL)
	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, "sync_task", res)

	byLabel := map[string]moderation.CategoryResult{}
	for _, c := range res.Frames[0].Categories {
		byLabel[c.ProviderLabel] = c
	}
	// Known heads map; probability carried as-is.
	if c := byLabel["general_nsfw"]; c.Category != moderation.CategorySexual || *c.Score != 0.97 || c.ScoreOrigin != moderation.OriginProbability {
		t.Errorf("general_nsfw = %+v", c)
	}
	// Heads whose names were corrected on the 2026-07-29 doc re-verification:
	// these are the real Hive class names, and they must not fall to OTHER.
	for label, want := range map[string]moderation.Category{
		"gun_in_hand":       moderation.CategoryWeapons,
		"knife_not_in_hand": moderation.CategoryWeapons,
		"very_bloody":       moderation.CategoryGoreGraphic,
		"a_little_bloody":   moderation.CategoryGoreGraphic,
		// Illicit drugs and legal vice are separate categories: an
		// operator thresholds a joint and a cigarette differently.
		"yes_marijuana":     moderation.CategoryDrugs,
		"yes_smoking":       moderation.CategoryAlcoholTobacco,
		"yes_gambling":      moderation.CategoryGambling,
		"yes_middle_finger": moderation.CategoryOffensiveGesture,
		// Provenance carriers, not harm signals.
		"animated":            moderation.CategoryAnimatedSynthetic,
		"medical_injectables": moderation.CategoryMedical,
	} {
		if c := byLabel[label]; c.Category != want {
			t.Errorf("%s = %+v, want %s", label, c, want)
		}
	}
	// Deliberately unmapped, per the audit in MODEL_LIMITATIONS.md: an
	// ordinary-apparel head must not inherit the SUGGESTIVE_RACY
	// threshold, and a child-related head gets no vismod-defined meaning.
	// Both still carry their label and score.
	for _, label := range []string{"yes_female_swimwear", "yes_child_present", "no_sexual_activity"} {
		c := byLabel[label]
		if c.Category != moderation.CategoryOther {
			t.Errorf("%s = %s, want OTHER (deliberately unmapped)", label, c.Category)
		}
		if c.Score == nil {
			t.Errorf("%s lost its score", label)
		}
	}
	// Unmapped heads are never dropped: OTHER, raw label + score preserved.
	if c := byLabel["brand_new_future_head"]; c.Category != moderation.CategoryOther || *c.Score != 0.5 {
		t.Errorf("unmapped head must be OTHER with its score: %+v", c)
	}
	if len(res.Frames[0].Categories) != 16 {
		t.Errorf("heads dropped: got %d, want 16", len(res.Frames[0].Categories))
	}
}

// TestRequestIsDocumentedMultipart pins the REQUEST shape against Hive's
// documented sync task API. The other tests here accept any body, so
// without this a regression to an undocumented encoding (this adapter
// previously posted JSON {"media_b64": ...}) would pass every test and
// fail every production call.
func TestRequestIsDocumentedMultipart(t *testing.T) {
	const wantBytes = "frame-bytes"
	var gotCT, gotFilename string
	var gotPart []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("body is not multipart/form-data: %v", err)
			return
		}
		f, hdr, err := r.FormFile("media")
		if err != nil {
			t.Errorf(`no "media" file part: %v`, err)
			return
		}
		defer f.Close()
		gotFilename = hdr.Filename
		gotPart, _ = io.ReadAll(f)
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	m := newTestModerator(t, srv.URL)
	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{
		Bytes: []byte(wantBytes),
		MIME:  "image/png",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data;") {
		t.Errorf("Content-Type = %q, want multipart/form-data", gotCT)
	}
	if string(gotPart) != wantBytes {
		t.Errorf("media part = %q, want %q", gotPart, wantBytes)
	}
	if gotFilename != "frame.png" {
		t.Errorf("filename = %q, want frame.png (derived from MIME)", gotFilename)
	}
}

// TestCapsCoverClassMap keeps the declared capability surface honest: a
// head mapped to a category the adapter does not advertise is a silent
// lie to anything reading Caps.
func TestCapsCoverClassMap(t *testing.T) {
	declared := map[moderation.Category]bool{}
	for _, c := range (&Moderator{}).Capabilities().Categories {
		declared[c] = true
	}
	for label, cat := range defaultClassMap {
		if moderation.Canonicalize(cat) != cat {
			t.Errorf("%s maps to non-canonical category %q", label, cat)
		}
		if !declared[cat] {
			t.Errorf("%s maps to %s, which Capabilities() does not declare", label, cat)
		}
	}
}

func TestEmptyOutputIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":[]}`))
	}))
	defer srv.Close()
	m := newTestModerator(t, srv.URL)
	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")}); err == nil {
		t.Error("empty output must be could-not-evaluate error, never a clean result")
	}
}

func TestClassMapOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":[{"response":{"output":[{"time":0,"classes":[{"class":"custom_head","score":0.9}]}]}}]}`))
	}))
	defer srv.Close()
	m, err := New(moderate.AdapterConfig{
		Options: map[string]any{
			"endpoint":       srv.URL,
			"rate_limit_rps": 1000.0,
			"class_map":      map[string]any{"custom_head": "HATE"},
		},
		Secret: secretEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Frames[0].Categories[0]; c.Category != moderation.CategoryHate {
		t.Errorf("class_map override not applied: %+v", c)
	}
}

func TestMissingTokenFailsFast(t *testing.T) {
	_, err := New(moderate.AdapterConfig{Options: map[string]any{}, Secret: func(string) string { return "" }})
	if err == nil {
		t.Error("missing token must fail at construction")
	}
}
