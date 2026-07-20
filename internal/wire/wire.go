// Package wire defines the HTTP API contract between crate (CLI) and crated (daemon).
package wire

import "time"

// HeaderAuth is the bearer-token header name used on all /api endpoints.
const HeaderAuth = "Authorization"

// HeaderExpires carries either a Go duration (for example "24h") or "never".
const HeaderExpires = "X-Crate-Expires"

// Path constants for the daemon API.
const (
	PathAPISites  = "/api/sites"
	PathAPIStatus = "/api/status"
	PathAPITokens = "/api/tokens"
)

// Site describes a deployed site.
type Site struct {
	Name      string     `json:"name"`
	UpdatedAt time.Time  `json:"updated_at"`
	SizeBytes int64      `json:"size_bytes"`
	FileCount int        `json:"file_count"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// ListSitesResponse is returned by GET /api/sites.
type ListSitesResponse struct {
	Sites []Site `json:"sites"`
}

// PutSiteResponse is returned by PUT /api/sites/{name}.
// The request body is a tar stream (no JSON envelope) to keep uploads streamable.
type PutSiteResponse struct {
	Site Site   `json:"site"`
	URL  string `json:"url"`
}

// StatusResponse is returned by GET /api/status.
type StatusResponse struct {
	Version   string `json:"version"`
	SiteCount int    `json:"site_count"`
}

// TokenInfo describes a minted API token. The secret is never included;
// it is returned exactly once, in CreateTokenResponse.Token.
type TokenInfo struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// CreateTokenRequest is the body of POST /api/tokens. Expires is a Go
// duration (for example "720h"), "never", or empty (same as "never" —
// tokens are long-lived credentials, unlike sites).
type CreateTokenRequest struct {
	Name    string `json:"name"`
	Expires string `json:"expires,omitempty"`
}

// CreateTokenResponse is returned by POST /api/tokens. Token is the only
// place the plaintext secret ever appears.
type CreateTokenResponse struct {
	Token string    `json:"token"`
	Info  TokenInfo `json:"info"`
}

// ListTokensResponse is returned by GET /api/tokens.
type ListTokensResponse struct {
	Tokens []TokenInfo `json:"tokens"`
}

// ErrorResponse is the body of any non-2xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}
