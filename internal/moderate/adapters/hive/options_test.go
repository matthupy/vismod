package hive

import (
	"testing"
	"time"
)

func TestDecodeOptions_Defaults(t *testing.T) {
	// An empty options map leaves every field at its zero value so the Factory
	// can apply defaults. max_retries uses a -1 "unset" sentinel (an explicit 0
	// disables retries and must be distinguishable from absent).
	o := decodeOptions(map[string]any{})
	if o.endpoint != "" || o.rps != 0 || o.retryBackoff != 0 {
		t.Errorf("empty map should zero string/float/duration fields, got %+v", o)
	}
	if o.maxRetries != -1 {
		t.Errorf("max_retries unset sentinel = %d, want -1", o.maxRetries)
	}
}

func TestDecodeOptions_ReadsKnownKeys(t *testing.T) {
	o := decodeOptions(map[string]any{
		"endpoint":      "https://api.thehive.ai/api/v2/task/sync",
		"rps":           2.5,
		"max_retries":   0, // explicit 0 -> retries disabled, NOT the -1 sentinel
		"retry_backoff": "250ms",
	})
	if o.endpoint != "https://api.thehive.ai/api/v2/task/sync" {
		t.Errorf("endpoint = %q", o.endpoint)
	}
	if o.rps != 2.5 {
		t.Errorf("rps = %v, want 2.5", o.rps)
	}
	if o.maxRetries != 0 {
		t.Errorf("explicit max_retries = %d, want 0 (not the -1 sentinel)", o.maxRetries)
	}
	if o.retryBackoff != 250*time.Millisecond {
		t.Errorf("retry_backoff = %v, want 250ms", o.retryBackoff)
	}
}
