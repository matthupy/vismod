package hashmatch

import (
	"context"
	"testing"

	"github.com/matthupy/vismod/pkg/moderation"
)

func TestNoOpNeverMatches(t *testing.T) {
	m, err := NoOp{}.Match(context.Background(), moderation.Image{Bytes: []byte("anything")})
	if err != nil {
		t.Fatal(err)
	}
	if m.Matched {
		t.Fatal("no-op matcher must never match in v1")
	}
}
