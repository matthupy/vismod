package result

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/vismod/vismod/pkg/moderation"
)

func TestJSONLSinkIdempotentPerJobID(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONLSink(&buf)
	env := ResultEnvelope{
		JobID:  "job-1",
		Source: moderation.Source{Kind: "file", Ref: "x.png", MediaType: "image"},
		Result: &moderation.NormalizedResult{Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow}},
	}
	for range 3 {
		if err := s.Write(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(buf.String(), "\n"); got != 1 {
		t.Errorf("redelivery double-wrote: %d lines, want 1", got)
	}

	env.JobID = "job-2"
	if err := s.Write(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Errorf("distinct job suppressed: %d lines, want 2", got)
	}
}

func TestNullableFieldsSerializeAsNull(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONLSink(&buf)
	err := s.Write(context.Background(), ResultEnvelope{
		JobID: "job-nulls",
		Result: &moderation.NormalizedResult{
			Frames: []moderation.FrameResult{{
				Status: moderation.FrameOK,
				Categories: []moderation.CategoryResult{{
					Category: moderation.CategoryHate, Score: nil, Threshold: nil,
					ScoreOrigin: moderation.OriginLikelihoodEnum,
				}},
			}},
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictError},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{`"score":null`, `"threshold":null`, `"max_score":null`, `"confidence":null`, `"top_category":null`, `"timestamp_sec":null`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in output: %s", want, out)
		}
	}
}

func TestURLSourceSerializesRedactedRefAndDigest(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONLSink(&buf)
	err := s.Write(context.Background(), ResultEnvelope{
		JobID: "job-url",
		Source: moderation.Source{
			Kind:      "url",
			Ref:       "https://bucket.s3.amazonaws.com/clip.mp4",
			RefDigest: "abc123",
			MediaType: "video",
		},
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictError},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"ref_digest":"abc123"`) {
		t.Errorf("ref_digest missing: %s", out)
	}
	if strings.Contains(out, "X-Amz") {
		t.Errorf("query string leaked: %s", out)
	}
}

func TestFileSourceOmitsRefDigest(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONLSink(&buf)
	_ = s.Write(context.Background(), ResultEnvelope{
		JobID:  "job-file",
		Source: moderation.Source{Kind: "file", Ref: "x.png", MediaType: "image"},
		Result: &moderation.NormalizedResult{},
	})
	if strings.Contains(buf.String(), "ref_digest") {
		t.Errorf("ref_digest must be omitted for file sources: %s", buf.String())
	}
}

func TestSchemaVersionIsBumped(t *testing.T) {
	if moderation.SchemaVersion != "1.2.0" {
		t.Errorf("SchemaVersion = %q, want 1.2.0 after the additive ref_digest field", moderation.SchemaVersion)
	}
}
