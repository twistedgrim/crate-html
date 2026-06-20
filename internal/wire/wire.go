// Package wire defines the HTTP API contract between crate (CLI) and crated (daemon).
package wire

import "time"

// HeaderAuth is the bearer-token header name used on all /api endpoints.
const HeaderAuth = "Authorization"

// Path constants for the daemon API.
const (
	PathAPISites  = "/api/sites"
	PathAPIStatus = "/api/status"
)

// Site describes a deployed site.
type Site struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
	SizeBytes int64     `json:"size_bytes"`
	FileCount int       `json:"file_count"`
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

// ErrorResponse is the body of any non-2xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}
