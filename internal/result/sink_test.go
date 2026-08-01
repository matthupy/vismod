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
