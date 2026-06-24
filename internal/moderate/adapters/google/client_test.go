package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testClient builds a client pointed at srv with apikey auth, no retries, no
// jitter (deterministic), unless overridden.
func testClient(srv *httptest.Server, maxRetries int) *client {
	auth := apiKeyAuth{key: "test-key"}
	return newClient(srv.URL, auth, 1000 /*rps*/, maxRetries, time.Millisecond, nil /*rng*/)
}

// TestClientRequestShape proves the client sends the documented request: JSON
// body with base64 content + SAFE_SEARCH_DETECTION feature, and the apikey
// applied as a ?key= query param.
func TestClientRequestShape(t *testing.T) {
	var gotBody annotateRequest
	var gotKey, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("key")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = io.WriteString(w, `{"responses":[{"safeSearchAnnotation":{"adult":"LIKELY","spoof":"UNKNOWN","medical":"UNKNOWN","violence":"UNKNOWN","racy":"UNKNOWN"}}]}`)
	}))
	defer srv.Close()

	c := testClient(srv, 0)
	if _, err := c.analyze(context.Background(), []byte("PNGDATA")); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if gotKey != "test-key" {
		t.Errorf("key query = %q", gotKey)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if len(gotBody.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(gotBody.Requests))
	}
	req := gotBody.Requests[0]
	if len(req.Features) != 1 || req.Features[0].Type != "SAFE_SEARCH_DETECTION" {
		t.Errorf("features = %+v, want one SAFE_SEARCH_DETECTION", req.Features)
	}
	want := base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	if req.Image.Content != want {
		t.Errorf("content = %q, want base64 %q", req.Image.Content, want)
	}
}

// TestClientParsesAnnotation proves a 200 is decoded into the typed response.
func TestClientParsesAnnotation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"responses":[{"safeSearchAnnotation":{"adult":"VERY_LIKELY","spoof":"UNKNOWN","medical":"UNKNOWN","violence":"UNKNOWN","racy":"UNKNOWN"}}]}`)
	}))
	defer srv.Close()

	resp, err := testClient(srv, 0).analyze(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	ann, ok := firstAnnotation(resp)
	if !ok || ann.Adult != "VERY_LIKELY" {
		t.Errorf("annotation = %+v, ok=%v", ann, ok)
	}
}

// TestClientRetriesThenSucceeds: a 429 is retryable; with one retry budget the
// second attempt's 200 succeeds.
func TestClientRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":429,"message":"rate"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"responses":[{"safeSearchAnnotation":{"adult":"UNKNOWN","spoof":"UNKNOWN","medical":"UNKNOWN","violence":"UNKNOWN","racy":"UNKNOWN"}}]}`)
	}))
	defer srv.Close()

	if _, err := testClient(srv, 2).analyze(context.Background(), []byte("x")); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

// TestClientTerminalOnBadRequest: a 400 is terminal (no retry) and surfaces the
// provider error code via CodedError.
func TestClientTerminalOnBadRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"bad image","status":"INVALID_ARGUMENT"}}`)
	}))
	defer srv.Close()

	_, err := testClient(srv, 3).analyze(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("want error on 400")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no retry on terminal)", calls.Load())
	}
	var ce interface{ ErrorCode() string }
	if !errors.As(err, &ce) {
		t.Fatalf("error %v does not implement CodedError", err)
	}
}

// TestClientExhaustsRetries: a persistent 500 retries up to budget then errors.
func TestClientExhaustsRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(srv, 2).analyze(context.Background(), []byte("x")); err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if calls.Load() != 3 { // initial + 2 retries
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

// TestClientTerminalOnBadJSON: a 200 with unparseable body is terminal.
func TestClientTerminalOnBadJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{not json`)
	}))
	defer srv.Close()

	if _, err := testClient(srv, 3).analyze(context.Background(), []byte("x")); err == nil {
		t.Fatal("want error on unparseable 200")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (decode error is terminal)", calls.Load())
	}
}
