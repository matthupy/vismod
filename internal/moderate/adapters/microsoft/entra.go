package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// staticToken is a fixed Entra bearer token supplied via env
// (VISMOD_MICROSOFT_ACCESS_TOKEN) — suitable when an external process
// handles token acquisition/rotation.
type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// imdsTokenSource acquires Managed Identity tokens from the Azure IMDS
// endpoint (scope https://cognitiveservices.azure.com/.default) and caches
// them until shortly before expiry. Only reachable on Azure compute.
type imdsTokenSource struct {
	client *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

const imdsURL = "http://169.254.169.254/metadata/identity/oauth2/token" +
	"?api-version=2018-02-01&resource=https%3A%2F%2Fcognitiveservices.azure.com%2F"

func newIMDSTokenSource(client *http.Client) *imdsTokenSource {
	return &imdsTokenSource{client: client}
}

func (s *imdsTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expiry.Add(-2*time.Minute)) {
		return s.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata", "true")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("IMDS request (managed identity requires Azure compute): %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IMDS returned %d", resp.StatusCode)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresOn   string `json:"expires_on"` // unix seconds as string
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("IMDS returned empty token")
	}
	s.token = tr.AccessToken
	if secs, err := strconv.ParseInt(tr.ExpiresOn, 10, 64); err == nil {
		s.expiry = time.Unix(secs, 0)
	} else {
		s.expiry = time.Now().Add(30 * time.Minute)
	}
	return s.token, nil
}
