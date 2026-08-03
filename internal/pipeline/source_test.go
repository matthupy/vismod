package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

type stubFetcher struct {
	path    string
	err     error
	gotURL  string
	cleaned bool
}

func (s *stubFetcher) Fetch(_ context.Context, rawURL, _ string) (string, func(), error) {
	s.gotURL = rawURL
	return s.path, func() { s.cleaned = true }, s.err
}

func urlJob() queue.Job {
	return queue.Job{
		ID: "job-url",
		Source: moderation.Source{
			Kind:      "url",
			Ref:       "https://media.example.com/clip.mp4?sig=secret",
			MediaType: "image",
		},
	}
}

func TestURLSourceIsFetchedAndRewrittenForAnalysis(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "source.png")
	if err := os.WriteFile(local, []byte("benign"), 0o600); err != nil {
		t.Fatal(err)
	}
	sf := &stubFetcher{path: local}
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, _ := newTestPipeline(t, mod, nil)
	p.Fetcher = sf

	env, disp, _ := p.ProcessJob(context.Background(), urlJob())

	if sf.gotURL != "https://media.example.com/clip.mp4?sig=secret" {
		t.Errorf("fetcher must receive the FULL url: %q", sf.gotURL)
	}
	if !sf.cleaned {
		t.Error("cleanup must run before the job finishes")
	}
	if env.Source.Kind != "url" {
		t.Errorf("envelope must record the original kind, got %q", env.Source.Kind)
	}
	if strings.Contains(env.Source.Ref, "secret") {
		t.Errorf("credential leaked into the envelope: %q", env.Source.Ref)
	}
	if env.Source.Ref != "https://media.example.com/clip.mp4" {
		t.Errorf("envelope ref: got %q", env.Source.Ref)
	}
	if len(env.Source.RefDigest) != 64 {
		t.Errorf("ref_digest must be set: %q", env.Source.RefDigest)
	}
	if env.Result.AssetID != "https://media.example.com/clip.mp4" {
		t.Errorf("asset_id must be the redacted url, not the temp path: %q", env.Result.AssetID)
	}
	if disp != queue.Ack {
		t.Errorf("disposition: got %v want Ack", disp)
	}
}

func TestFetchFailureIsErrorVerdictNeverAllow(t *testing.T) {
	sf := &stubFetcher{err: errors.New("host not in allow-list")}
	p, _ := newTestPipeline(t, &fakeModerator{}, nil)
	p.Fetcher = sf

	env, disp, _ := p.ProcessJob(context.Background(), urlJob())

	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("verdict = %q, want error — a fetch failure must NEVER allow", env.Result.Overall.Verdict)
	}
	if disp != queue.DeadLetter {
		t.Errorf("disposition = %v, want DeadLetter", disp)
	}
	if !sf.cleaned {
		t.Error("cleanup must run even when the fetch failed")
	}
	if strings.Contains(env.Error, "secret") {
		t.Errorf("credential leaked into the error string: %q", env.Error)
	}
	if strings.Contains(env.Source.Ref, "secret") {
		t.Errorf("credential leaked into the envelope source: %q", env.Source.Ref)
	}
}

func TestURLSourceWithNoFetcherIsErrorVerdict(t *testing.T) {
	p, _ := newTestPipeline(t, &fakeModerator{}, nil)
	p.Fetcher = nil // no fetcher wired

	env, disp, _ := p.ProcessJob(context.Background(), urlJob())

	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("verdict = %q, want error", env.Result.Overall.Verdict)
	}
	if disp != queue.DeadLetter {
		t.Errorf("disposition = %v, want DeadLetter", disp)
	}
	if !strings.Contains(env.Error, "no url fetcher") {
		t.Errorf("error should explain that no fetcher is wired: %q", env.Error)
	}
}

func TestFileSourceIsUnaffected(t *testing.T) {
	sf := &stubFetcher{err: errors.New("must not be called")}
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, _ := newTestPipeline(t, mod, nil)
	p.Fetcher = sf

	local := writeInput(t, "benign")
	j := queue.Job{ID: "job-file", Source: moderation.Source{Kind: "file", Ref: local, MediaType: "image"}}

	env, _, _ := p.ProcessJob(context.Background(), j)
	if sf.gotURL != "" {
		t.Error("a file source must not touch the fetcher")
	}
	if env.Source.Ref != local {
		t.Errorf("file ref must pass through unchanged: %q", env.Source.Ref)
	}
	if env.Source.RefDigest != "" {
		t.Error("file sources carry no ref_digest")
	}
}

// The pipeline must report fetch outcomes through OnFetch with a label
// from fetch.Reason's fixed set — never an error string, which could
// carry a url into a Prometheus label.
func TestOnFetchReceivesBoundedReasonAndBytes(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "source.png")
	if err := os.WriteFile(local, []byte("benign"), 0o600); err != nil {
		t.Fatal(err)
	}

	type call struct {
		bytes  int64
		reason string
	}
	var got []call
	record := func(_ time.Duration, n int64, reason string) {
		got = append(got, call{n, reason})
	}

	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, _ := newTestPipeline(t, mod, nil)
	p.Fetcher = &stubFetcher{path: local}
	p.OnFetch = record
	if _, _, err := p.ProcessJob(context.Background(), urlJob()); err != nil {
		t.Fatalf("ProcessJob: %v", err)
	}

	p2, _ := newTestPipeline(t, &fakeModerator{}, nil)
	p2.Fetcher = &stubFetcher{err: errors.New("fetch: host \"x\" is not in source.url.allow_hosts")}
	p2.OnFetch = record
	_, _, _ = p2.ProcessJob(context.Background(), urlJob())

	if len(got) != 2 {
		t.Fatalf("OnFetch called %d time(s), want 2", len(got))
	}
	if got[0].reason != "" {
		t.Errorf("success reason = %q, want empty", got[0].reason)
	}
	if got[0].bytes != int64(len("benign")) {
		t.Errorf("bytes = %d, want %d (size on disk, not Content-Length)", got[0].bytes, len("benign"))
	}
	if got[1].reason != "rejected_url" {
		t.Errorf("failure reason = %q, want rejected_url", got[1].reason)
	}
}

// A file source must never touch the fetch metrics: a nonzero
// vismod_fetch_* series for a file-only deployment is a false signal.
func TestOnFetchNotCalledForFileSources(t *testing.T) {
	called := false
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, _ := newTestPipeline(t, mod, nil)
	p.OnFetch = func(time.Duration, int64, string) { called = true }
	j := queue.Job{ID: "job-file", Source: moderation.Source{Kind: "file", Ref: writeInput(t, "benign"), MediaType: "image"}}
	if _, _, err := p.ProcessJob(context.Background(), j); err != nil {
		t.Fatalf("ProcessJob: %v", err)
	}
	if called {
		t.Error("OnFetch fired for a file source")
	}
}
