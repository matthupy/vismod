package shieldgemma

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/internal/moderate/adapters/golden"
	"github.com/vismod/vismod/pkg/moderation"
)

// logprobResponse renders an OpenAI-compatible chat-completion response
// carrying top_logprobs for the single generated token. pYes is the
// probability the fixture encodes; the logprobs below are its natural log
// and that of its complement, which is how a real server reports it.
func logprobResponse(pYes float64) string {
	lpYes := math.Log(pYes)
	lpNo := math.Log(1 - pYes)
	return fmt.Sprintf(`{"choices":[{"logprobs":{"content":[{"token":"Yes","logprob":%v,
		"top_logprobs":[{"token":"Yes","logprob":%v},{"token":"No","logprob":%v}]}]}}]}`,
		lpYes, lpYes, lpNo)
}

// scoreByPolicy is the fixture: one probability per policy.
var scoreByPolicy = map[string]float64{
	policySexuallyExplicit: 0.93,
	policyDangerousContent: 0.12,
	policyViolenceGore:     0.44,
}

// policyServer answers each request with the fixture score for whichever
// policy the request body carries, and records the requests it saw.
func policyServer(t *testing.T, seen *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("request body is not JSON: %v", err)
			return
		}
		if seen != nil {
			*seen = append(*seen, req)
		}
		for policy, score := range scoreByPolicy {
			if strings.Contains(string(body), policyPrompts[policy]) {
				_, _ = w.Write([]byte(logprobResponse(score)))
				return
			}
		}
		t.Errorf("request carried no known policy prompt: %s", body)
	}))
}

func newTestModerator(t *testing.T, url string, extra map[string]any) moderation.Moderator {
	t.Helper()
	opts := map[string]any{"endpoint": url, "model_version": "shieldgemma-2-4b-it@test"}
	for k, v := range extra {
		opts[k] = v
	}
	m, err := New(moderate.AdapterConfig{
		Name:                  "shieldgemma",
		Options:               opts,
		Secret:                func(string) string { return "" },
		ProviderThresholdMode: config.ProviderModeOverride,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestConstructionIsBootValidation: every one of these is a
// refuse-to-start, not a runtime surprise.
func TestConstructionIsBootValidation(t *testing.T) {
	cases := []struct {
		name    string
		opts    map[string]any
		mode    string
		wantErr bool
	}{
		{"valid", map[string]any{"endpoint": "http://127.0.0.1:8000/v1/chat/completions", "model_version": "x"}, config.ProviderModeOverride, false},
		{"endpoint absent", map[string]any{"model_version": "x"}, config.ProviderModeOverride, true},
		{"endpoint invalid", map[string]any{"endpoint": "http://gpu.example.com/v1", "model_version": "x"}, config.ProviderModeOverride, true},
		{"model_version absent", map[string]any{"endpoint": "http://127.0.0.1:8000/v1"}, config.ProviderModeOverride, true},
		{"model_version blank", map[string]any{"endpoint": "http://127.0.0.1:8000/v1", "model_version": "  "}, config.ProviderModeOverride, true},
		{"mode off", map[string]any{"endpoint": "http://127.0.0.1:8000/v1", "model_version": "x"}, config.ProviderModeOff, true},
		{"mode hybrid", map[string]any{"endpoint": "http://127.0.0.1:8000/v1", "model_version": "x"}, config.ProviderModeHybrid, true},
		{"mode unset", map[string]any{"endpoint": "http://127.0.0.1:8000/v1", "model_version": "x"}, "", true},
		{"unknown policy", map[string]any{"endpoint": "http://127.0.0.1:8000/v1", "model_version": "x", "policies": []any{"nonsense"}}, config.ProviderModeOverride, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(moderate.AdapterConfig{
				Options:               tc.opts,
				Secret:                func(string) string { return "" },
				ProviderThresholdMode: tc.mode,
			})
			if tc.wantErr && err == nil {
				t.Fatal("construction succeeded, want failure")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("construction failed: %v", err)
			}
		})
	}
}

func TestAnalyzeImageGolden(t *testing.T) {
	var seen []map[string]any
	srv := policyServer(t, &seen)
	defer srv.Close()

	m := newTestModerator(t, srv.URL, nil)
	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img"), MIME: "image/jpeg"})
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, "chat_completion", res)

	// One request per enabled policy per frame — this is what multiplies
	// the per-frame request count for a self-hosted box.
	if len(seen) != len(scoreByPolicy) {
		t.Fatalf("sent %d requests, want one per policy (%d)", len(seen), len(scoreByPolicy))
	}
	for _, req := range seen {
		if req["model"] != "shieldgemma-2-4b-it@test" {
			t.Errorf("model = %v, want the configured model_version", req["model"])
		}
		if req["logprobs"] != true {
			t.Errorf("logprobs = %v, want true (the score IS the Yes-token probability)", req["logprobs"])
		}
	}

	byLabel := map[string]moderation.CategoryResult{}
	for _, c := range res.Frames[0].Categories {
		byLabel[c.ProviderLabel] = c
	}
	if len(byLabel) != len(scoreByPolicy) {
		t.Fatalf("got %d categories, want %d", len(byLabel), len(scoreByPolicy))
	}
	for label, want := range map[string]moderation.Category{
		policySexuallyExplicit: moderation.CategorySexual,
		policyViolenceGore:     moderation.CategoryGoreGraphic,
		// Documented choice: "dangerous content" spans weapons, illicit
		// drugs and self-harm in ONE score, so mapping it to any of those
		// canonical categories would attribute a signal it did not make.
		policyDangerousContent: moderation.CategoryOther,
	} {
		c := byLabel[label]
		if c.Category != want {
			t.Errorf("%s -> %s, want %s", label, c.Category, want)
		}
		if c.ScoreOrigin != moderation.OriginProbability {
			t.Errorf("%s origin = %q, want probability", label, c.ScoreOrigin)
		}
		if c.Score == nil {
			t.Fatalf("%s lost its score", label)
		}
		if math.Abs(*c.Score-scoreByPolicy[label]) > 1e-6 {
			t.Errorf("%s score = %v, want %v", label, *c.Score, scoreByPolicy[label])
		}
	}
}

// TestPolicySubsetDrivesRequestsAndLabels: the adapter authors its own
// label set, so a narrowed policy list must narrow BOTH the requests and
// the declaration the boot-time completeness check reads.
func TestPolicySubsetDrivesRequestsAndLabels(t *testing.T) {
	var seen []map[string]any
	srv := policyServer(t, &seen)
	defer srv.Close()

	m := newTestModerator(t, srv.URL, map[string]any{"policies": []any{policyViolenceGore}})
	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Errorf("sent %d requests, want 1 for a one-policy config", len(seen))
	}
	if got := len(res.Frames[0].Categories); got != 1 {
		t.Fatalf("got %d categories, want 1", got)
	}
	labels := m.(*Moderator).ProviderLabels()
	if len(labels) != 1 || labels[0] != policyViolenceGore {
		t.Errorf("ProviderLabels() = %v, want [%s]", labels, policyViolenceGore)
	}
}

func TestProviderLabelsDefaultToAllPolicies(t *testing.T) {
	m := newTestModerator(t, "http://127.0.0.1:8000/v1", nil)
	got := m.(*Moderator).ProviderLabels()
	if len(got) != len(policyPrompts) {
		t.Fatalf("ProviderLabels() = %v, want all %d policies", got, len(policyPrompts))
	}
	for _, l := range got {
		if _, ok := policyPrompts[l]; !ok {
			t.Errorf("declared label %q is not a policy", l)
		}
	}
}

// TestCapsCoverPolicyMap keeps the declared capability surface honest.
func TestCapsCoverPolicyMap(t *testing.T) {
	declared := map[moderation.Category]bool{}
	for _, c := range (&Moderator{}).Capabilities().Categories {
		declared[c] = true
	}
	for label, cat := range policyCategories {
		if moderation.Canonicalize(cat) != cat {
			t.Errorf("%s maps to non-canonical category %q", label, cat)
		}
		if !declared[cat] {
			t.Errorf("%s maps to %s, which Capabilities() does not declare", label, cat)
		}
	}
}

// --- fail-safe: every transport and parse failure must error, never allow ---

func TestRetriesThenSucceeds(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&calls, 1) == 1 {
					// Retry-After: 1 keeps the 429 path off its 2s floor.
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(logprobResponse(0.5)))
			}))
			defer srv.Close()

			m := newTestModerator(t, srv.URL, map[string]any{
				"policies": []any{policyViolenceGore}, "max_attempts": 3,
			})
			if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")}); err != nil {
				t.Fatalf("retryable %d should have been retried to success: %v", status, err)
			}
			if got := atomic.LoadInt32(&calls); got != 2 {
				t.Errorf("server saw %d calls, want 2 (one failure, one retry)", got)
			}
		})
	}
}

func TestTerminal4xxIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound) // model not loaded / wrong served name
	}))
	defer srv.Close()

	m := newTestModerator(t, srv.URL, map[string]any{"policies": []any{policyViolenceGore}, "max_attempts": 3})
	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
	if err == nil {
		t.Fatal("terminal 4xx must surface an error, never a clean result")
	}
	assertNotAllowable(t, res)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server saw %d calls, want 1 (no retry on terminal 4xx)", got)
	}
}

func TestConnectionRefusedIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now: the box is off

	m := newTestModerator(t, url, map[string]any{"policies": []any{policyViolenceGore}, "max_attempts": 1})
	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
	if err == nil {
		t.Fatal("connection refused must surface an error, never a clean result")
	}
	assertNotAllowable(t, res)
}

func TestUnparseableBodiesAreErrors(t *testing.T) {
	cases := map[string]string{
		"not json":           `<html>502 Bad Gateway</html>`,
		"no choices":         `{"choices":[]}`,
		"no logprobs":        `{"choices":[{"message":{"content":"Yes"}}]}`,
		"no yes or no token": `{"choices":[{"logprobs":{"content":[{"token":"maybe","logprob":-0.1,"top_logprobs":[{"token":"maybe","logprob":-0.1}]}]}}]}`,
		"empty top_logprobs": `{"choices":[{"logprobs":{"content":[{"token":"Yes","logprob":-0.1,"top_logprobs":[]}]}}]}`,
		// A lone Yes is an unnormalized P(token), a different quantity from
		// the Yes/No-renormalized score. Scoring it would smuggle two
		// meanings under one ScoreOrigin.
		"yes without no": `{"choices":[{"logprobs":{"content":[{"token":"Yes","logprob":-0.1,"top_logprobs":[{"token":"Yes","logprob":-0.1}]}]}}]}`,
		"empty body":     ``,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			m := newTestModerator(t, srv.URL, map[string]any{"policies": []any{policyViolenceGore}, "max_attempts": 1})
			res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
			if err == nil {
				t.Fatal("a 200 with an unusable body must be could-not-evaluate, never a clean result")
			}
			assertNotAllowable(t, res)
		})
	}
}

// assertNotAllowable checks the failure result carries no score that could
// be read as "confidently safe" (invariant 2: 0 means safe, nil means
// unknown).
func assertNotAllowable(t *testing.T, res moderation.NormalizedResult) {
	t.Helper()
	for _, f := range res.Frames {
		for _, c := range f.Categories {
			if c.Score != nil {
				t.Errorf("failed analysis emitted a score (%v) for %s", *c.Score, c.ProviderLabel)
			}
		}
	}
}
