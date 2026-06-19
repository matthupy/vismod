// Package hashmatch holds HashMatcher implementations. v1 ships only a no-op
// matcher; the PDQ/TMK perceptual-hash matcher (CSAM list membership) is v1.1.
//
// Shipping the seam + the CSAM_HASH_MATCH category + the match_type/match_list
// schema fields in v1 is a hard requirement, even though no real matching runs.
package hashmatch

import (
	"context"

	"github.com/matthupy/vismod/pkg/moderation"
)

// NoOp never matches. It is the v1 default so the pipeline pre-stage exists and
// is exercised end-to-end without shipping any hash list.
type NoOp struct{}

// Match always returns no match.
func (NoOp) Match(_ context.Context, _ moderation.Image) (moderation.HashMatch, error) {
	return moderation.HashMatch{Matched: false}, nil
}

var _ moderation.HashMatcher = NoOp{}
