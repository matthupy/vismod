package google

import (
	"testing"
	"time"
)

func TestDecodeOptions(t *testing.T) {
	m := map[string]any{
		"endpoint":      "https://vision.example.com/v1/images:annotate",
		"auth_mode":     "bearer",
		"rps":           float64(8),
		"max_retries":   2,
		"retry_backoff": "250ms",
	}
	o := decodeOptions(m)
	if o.endpoint != "https://vision.example.com/v1/images:annotate" {
		t.Errorf("endpoint = %q", o.endpoint)
	}
	if o.authMode != "bearer" {
		t.Errorf("authMode = %q", o.authMode)
	}
	if o.rps != 8 {
		t.Errorf("rps = %v", o.rps)
	}
	if o.maxRetries != 2 {
		t.Errorf("maxRetries = %v", o.maxRetries)
	}
	if o.retryBackoff != 250*time.Millisecond {
		t.Errorf("retryBackoff = %v", o.retryBackoff)
	}
}

// TestDecodeOptionsMaxRetriesSentinel proves an absent max_retries yields the -1
// "unset" sentinel (factory applies default) while an explicit 0 is preserved
// (retries disabled) — the two must be distinguishable.
func TestDecodeOptionsMaxRetriesSentinel(t *testing.T) {
	if got := decodeOptions(map[string]any{}).maxRetries; got != -1 {
		t.Errorf("absent max_retries = %d, want -1 sentinel", got)
	}
	if got := decodeOptions(map[string]any{"max_retries": 0}).maxRetries; got != 0 {
		t.Errorf("explicit max_retries:0 = %d, want 0", got)
	}
}

// TestDecodeOptionsEmpty proves an empty map is safe (all zero values, defaults
// applied by the factory) and never panics.
func TestDecodeOptionsEmpty(t *testing.T) {
	o := decodeOptions(nil)
	if o.endpoint != "" || o.authMode != "" || o.rps != 0 {
		t.Errorf("empty options not zero: %+v", o)
	}
}
