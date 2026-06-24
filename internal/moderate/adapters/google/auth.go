package google

import (
	"fmt"
	"net/http"
)

// authProvider applies credentials to an outgoing request. It is a seam: v1
// ships two zero-dependency schemes (API key, static bearer token). Full
// Application Default Credentials / service-account auto-refresh via the heavy
// google.golang.org/api SDK is a documented future step — it is NOT wired in v1.
// A static bearer token (acquired out-of-band, e.g. `gcloud auth
// print-access-token`) covers OAuth2/ADC today with no dependency.
type authProvider interface {
	apply(req *http.Request)
	// describe is a non-secret label for logs/errors (never the credential).
	describe() string
}

// apiKeyAuth applies the API key as a ?key= query param — the Vision REST apikey
// scheme. The key never goes in a header.
type apiKeyAuth struct{ key string }

func (a apiKeyAuth) apply(req *http.Request) {
	q := req.URL.Query()
	q.Set("key", a.key)
	req.URL.RawQuery = q.Encode()
}
func (apiKeyAuth) describe() string { return "apikey" }

// bearerAuth uses an OAuth2 bearer token (ADC / service-account access token
// acquired out-of-band). project, when set, is sent as x-goog-user-project so
// billing/quota attribute to the caller's project (required for some orgs).
type bearerAuth struct {
	token   string
	project string
}

func (a bearerAuth) apply(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+a.token)
	if a.project != "" {
		req.Header.Set("x-goog-user-project", a.project)
	}
}
func (bearerAuth) describe() string { return "bearer" }

// newAuth builds the configured auth provider, failing fast when the required
// secret is missing (spec §F.2 — fail at boot, not per-job).
//
// secret reads VISMOD_<KEY>:
//   - apikey scheme -> VISMOD_GOOGLE_API_KEY
//   - bearer scheme -> VISMOD_GOOGLE_TOKEN (+ optional VISMOD_GOOGLE_PROJECT)
func newAuth(mode string, secret func(key string) string) (authProvider, error) {
	switch mode {
	case "", "apikey":
		key := secret("GOOGLE_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("google: apikey auth requires VISMOD_GOOGLE_API_KEY (env-only secret)")
		}
		return apiKeyAuth{key: key}, nil
	case "bearer":
		tok := secret("GOOGLE_TOKEN")
		if tok == "" {
			return nil, fmt.Errorf("google: bearer auth requires VISMOD_GOOGLE_TOKEN (env-only secret)")
		}
		return bearerAuth{token: tok, project: secret("GOOGLE_PROJECT")}, nil
	default:
		return nil, fmt.Errorf("google: unknown auth_mode %q (want apikey|bearer)", mode)
	}
}
