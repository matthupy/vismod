package moderation

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNullableScalarsSerializeAsNull is the executable form of the §E
// serialization contract: Score/Threshold/MaxScore/Confidence/TimestampSec/
// TopCategory MUST serialize as explicit JSON null, never be omitted. Consumers
// read null, not an absent field.
func TestNullableScalarsSerializeAsNull(t *testing.T) {
	res := NormalizedResult{
		SchemaVersion: SchemaVersion,
		Frames: []FrameResult{{
			TimestampSec: nil, // still image
			Status:       FrameStatusOK,
			Categories: []CategoryResult{{
				Category:    CategoryCSAMHashMatch,
				ScoreOrigin: ScoreOriginListMembership,
				Score:       nil, // list membership: no score
				Threshold:   nil,
				Flagged:     true,
			}},
		}},
		Overall: OverallVerdict{
			Verdict:     VerdictBlock,
			Flagged:     true,
			TopCategory: nil,
			MaxScore:    nil,
			Confidence:  nil,
		},
	}

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)

	for _, field := range []string{
		`"timestamp_sec":null`,
		`"score":null`,
		`"threshold":null`,
		`"top_category":null`,
		`"max_score":null`,
		`"confidence":null`,
	} {
		if !strings.Contains(s, field) {
			t.Errorf("missing explicit null %s in:\n%s", field, s)
		}
	}
}

func TestHashMatchRowKeepsFlaggedTrueWithNullScore(t *testing.T) {
	cr := CategoryResult{
		Category:    CategoryCSAMHashMatch,
		ScoreOrigin: ScoreOriginListMembership,
		Score:       nil,
		Flagged:     true,
		MatchList:   "ncmec",
		MatchType:   "pdq",
	}
	b, _ := json.Marshal(cr)
	s := string(b)
	if !strings.Contains(s, `"flagged":true`) || !strings.Contains(s, `"score":null`) {
		t.Fatalf("hash-match row must be flagged:true + score:null, got %s", s)
	}
	if !strings.Contains(s, `"match_list":"ncmec"`) || !strings.Contains(s, `"match_type":"pdq"`) {
		t.Fatalf("hash-match provenance fields missing: %s", s)
	}
}

func TestPtr(t *testing.T) {
	v := 0.5
	p := Ptr(v)
	if p == nil || *p != v {
		t.Fatal("Ptr must return a pointer to its argument")
	}
}
