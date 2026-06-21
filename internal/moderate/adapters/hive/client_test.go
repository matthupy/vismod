package hive

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matthupy/vismod/pkg/moderation"
)

const okBody = `{"status":[{"response":{"output":[{"time":0,"classes":[
	{"class":"general_nsfw","score":0.91},
	{"class":"general_not_nsfw_not_suggestive","score":0.09}]}]}}]}`

// newTestClient builds a client pointed at srv with retries/jitter tuned for
// fast deterministic tests (nil rng disables jitter; high limiter rate).
func newTestClient(srv *httptest.Server, maxRetries int) *client {
	return newClient(srv.URL, "test-token", 1000, maxRetries, time.Nanosecond, nil)
}

func TestClient_Analyze_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	got, err := newTestClient(srv, 0).analyze(context.Background(), []byte("imgbytes"), "image/png")
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(got.Status) != 1 || len(got.Status[0].Response.Output) != 1 {
		t.Fatalf("unexpected shape: %+v", got)
	}
	cls := got.Status[0].Response.Output[0].Classes
	if len(cls) != 2 || cls[0].Class != "general_nsfw" || cls[0].Score != 0.91 {
		t.Errorf("classes = %+v", cls)
	}
}

func TestClient_Analyze_SendsTokenAuthAndMultipartMedia(t *testing.T) {
	var gotAuth, gotField string
	var gotFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			gotField = p.FormName()
			b, _ := io.ReadAll(p)
			gotFile = string(b)
		}
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	_, err := newTestClient(srv, 0).analyze(context.Background(), []byte("PNGDATA"), "image/png")
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if gotAuth != "Token test-token" {
		t.Errorf("Authorization = %q, want \"Token test-token\"", gotAuth)
	}
	if gotField != "media" {
		t.Errorf("multipart field = %q, want \"media\"", gotField)
	}
	if gotFile != "PNGDATA" {
		t.Errorf("uploaded bytes = %q, want PNGDATA", gotFile)
	}
}

func TestClient_Analyze_RetriesOn429ThenSucceeds(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	_, err := newTestClient(srv, 3).analyze(context.Background(), []byte("x"), "image/png")
	if err != nil {
		t.Fatalf("analyze after retry: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one 429 + one success)", calls)
	}
}

func TestClient_Analyze_TerminalOn4xxNoRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"bad media"}`)
	}))
	defer srv.Close()

	_, err := newTestClient(srv, 3).analyze(context.Background(), []byte("x"), "image/png")
	if err == nil {
		t.Fatal("want terminal error on 400")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 4xx)", calls)
	}
	var ce moderation.CodedError
	if !errors.As(err, &ce) || ce.ErrorCode() == "" {
		t.Errorf("error must expose a non-empty code, got %v", err)
	}
}

func TestClient_Analyze_ExhaustsRetriesOn5xx(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestClient(srv, 2).analyze(context.Background(), []byte("x"), "image/png")
	if err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if calls != 3 { // initial + 2 retries
		t.Errorf("calls = %d, want 3", calls)
	}
	if !strings.Contains(err.Error(), "hive") {
		t.Errorf("error should be hive-scoped: %v", err)
	}
}
