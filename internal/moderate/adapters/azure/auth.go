package azure

import (
	"fmt"
	"net/http"
)

// authProvider applies credentials to an outgoing request. It is a seam: v1
// ships two zero-dependency schemes (API key, static bearer token). Full
// Microsoft Entra ID / Managed Identity via DefaultAzureCredential is a
// documented future step — it pulls the heavy azidentity SDK, so it is NOT
// wired in v1. A static bearer token (acquired out-of-band for scope
// https://cognitiveservices.azure.com/.default) covers Entra today with no dep.
type authProvider interface {
	apply(req *http.Request)
	// describe is a non-secret label for logs/errors (never the credential).
	describe() string
}

// apiKeyAuth uses the subscription-key header (default scheme).
type apiKeyAuth struct{ key string }

func (a apiKeyAuth) apply(req *http.Request) {
	req.Header.Set("Ocp-Apim-Subscription-Key", a.key)
}
func (apiKeyAuth) describe() string { return "apikey" }

// bearerAuth uses an OAuth2 bearer token (Entra ID via externally-acquired
// token). The token is read once at construction from the secret accessor.
type bearerAuth struct{ token string }

func (a bearerAuth) apply(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+a.token)
}
func (bearerAuth) describe() string { return "bearer" }

// newAuth builds the configured auth provider, failing fast when the required
// secret is missing (spec §F.2 — fail at boot, not per-job).
//
// secret reads VISMOD_<KEY>:
//   - apikey scheme -> VISMOD_AZURE_KEY
//   - bearer scheme -> VISMOD_AZURE_TOKEN
func newAuth(mode string, secret func(key string) string) (authProvider, error) {
	switch mode {
	case "", "apikey":
		key := secret("AZURE_KEY")
		if key == "" {
			return nil, fmt.Errorf("azure: apikey auth requires VISMOD_AZURE_KEY (env-only secret)")
		}
		return apiKeyAuth{key: key}, nil
	case "bearer":
		tok := secret("AZURE_TOKEN")
		if tok == "" {
			return nil, fmt.Errorf("azure: bearer auth requires VISMOD_AZURE_TOKEN (env-only secret)")
		}
		return bearerAuth{token: tok}, nil
	default:
		return nil, fmt.Errorf("azure: unknown auth_mode %q (want apikey|bearer)", mode)
	}
}
