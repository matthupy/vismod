package azure

import (
	"net/http"
	"testing"
	"time"

	"github.com/matthupy/vismod/internal/moderate"
)

func TestDecodeOptions(t *testing.T) {
	o := decodeOptions(map[string]any{
		"endpoint":      "https://x.cognitiveservices.azure.com",
		"auth_mode":     "bearer",
		"api_version":   "2025-01-01",
		"rps":           float64(10),
		"max_retries":   2,
		"retry_backoff": "250ms",
		"ignored":       "noise",
	})
	if o.endpoint == "" || o.authMode != "bearer" || o.apiVersion != "2025-01-01" {
		t.Errorf("string opts wrong: %+v", o)
	}
	if o.rps != 10 || o.maxRetries != 2 || o.retryBackoff != 250*time.Millisecond {
		t.Errorf("numeric/duration opts wrong: %+v", o)
	}
}

func TestDecodeOptionsMaxRetriesSentinel(t *testing.T) {
	// Absent key -> sentinel -1 so the factory can tell "unset" from explicit 0.
	if o := decodeOptions(map[string]any{}); o.maxRetries != -1 {
		t.Errorf("absent max_retries = %d, want sentinel -1", o.maxRetries)
	}
	// Explicit 0 is preserved (operator wants retries OFF).
	if o := decodeOptions(map[string]any{"max_retries": 0}); o.maxRetries != 0 {
		t.Errorf("explicit max_retries:0 = %d, want 0", o.maxRetries)
	}
}

func TestFactoryMaxRetries(t *testing.T) {
	// Unset -> default.
	az := mustNew(t, map[string]any{"endpoint": "https://x.cognitiveservices.azure.com"}).(*azure)
	if az.client.maxRetries != defaultMaxRetries {
		t.Errorf("unset max_retries -> %d, want default %d", az.client.maxRetries, defaultMaxRetries)
	}
	// Explicit 0 -> retries disabled (regression: was silently coerced to 3).
	az0 := mustNew(t, map[string]any{
		"endpoint":    "https://x.cognitiveservices.azure.com",
		"max_retries": 0,
	}).(*azure)
	if az0.client.maxRetries != 0 {
		t.Errorf("explicit max_retries:0 -> %d, want 0 (retries disabled)", az0.client.maxRetries)
	}
}

func TestNumberCoercion(t *testing.T) {
	// viper may deliver ints, int64s, or float64s depending on source.
	if asInt(int64(7)) != 7 || asInt(float64(7)) != 7 || asInt("x") != 0 {
		t.Error("asInt coercion")
	}
	if asFloat(int(3)) != 3 || asFloat(int64(3)) != 3 || asFloat("x") != 0 {
		t.Error("asFloat coercion")
	}
}

func TestBearerAuthAppliesHeader(t *testing.T) {
	m, err := New(moderate.AdapterConfig{
		Name:    "azure",
		Options: map[string]any{"endpoint": "https://x.cognitiveservices.azure.com", "auth_mode": "bearer"},
		Secret:  secretFunc(map[string]string{"AZURE_TOKEN": "tok123"}),
	})
	if err != nil {
		t.Fatalf("New bearer: %v", err)
	}
	az := m.(*azure)
	req, _ := http.NewRequest(http.MethodPost, "https://x/", nil)
	az.client.auth.apply(req)
	if got := req.Header.Get("Authorization"); got != "Bearer tok123" {
		t.Errorf("Authorization = %q", got)
	}
	if az.client.auth.describe() != "bearer" {
		t.Errorf("describe = %q", az.client.auth.describe())
	}
}

func TestCloseIsNoError(t *testing.T) {
	if err := mustNew(t, nil).Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}
