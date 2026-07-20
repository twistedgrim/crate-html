// Package server is the HTTP daemon. It serves /api endpoints (bearer-token
// authed) for managing sites and a public path-routed static server for
// /<site>/... so deployed sites are reachable in a browser.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Twistedgrim/crate-html/internal/builtin"
	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/storage"
	"github.com/Twistedgrim/crate-html/internal/token"
	"github.com/Twistedgrim/crate-html/internal/wire"
)

// Version is the daemon version reported by /api/status. It's a var, not a
// const, so release builds can stamp it via ldflags:
//
//	go build -ldflags "-X github.com/Twistedgrim/crate-html/internal/server.Version=v0.2.0"
//
// The default value is what appears in dev builds and in `go install`.
var Version = "0.1.0-dev"

const defaultExpiry = 24 * time.Hour

// Server bundles the HTTP handlers.
type Server struct {
	store    *storage.Store
	tokens   *token.Store
	cfg      config.Config
	log      *log.Logger
	builtins []builtin.Site
	expiryMu sync.Mutex
}

// New returns a Server. Pass nil for builtins to skip embedded sites and nil
// for tokens to accept only the root config token.
func New(cfg config.Config, store *storage.Store, tokens *token.Store, builtins []builtin.Site, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{store: store, tokens: tokens, cfg: cfg, log: logger, builtins: builtins}
}

// Handler returns the root http.Handler for the daemon.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/sites", s.requireAuth(s.handleListSites))
	mux.HandleFunc("PUT /api/sites/{name}", s.requireAuth(s.handlePutSite))
	mux.HandleFunc("DELETE /api/sites/{name}", s.requireAuth(s.handleDeleteSite))

	// Token management is root-only: minted tokens can manage sites but can
	// never mint, list, or revoke tokens. This keeps privilege escalation
	// off the table without introducing scopes.
	mux.HandleFunc("POST /api/tokens", s.requireRoot(s.handleCreateToken))
	mux.HandleFunc("GET /api/tokens", s.requireRoot(s.handleListTokens))
	mux.HandleFunc("DELETE /api/tokens/{id}", s.requireRoot(s.handleRevokeToken))

	// Static + index
	mux.HandleFunc("GET /", s.handlePublic)

	return mux
}

// bearer extracts the bearer value from the Authorization header, or "" if
// the header is missing/malformed.
func bearer(r *http.Request) string {
	h := r.Header.Get(wire.HeaderAuth)
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}

func (s *Server) isRoot(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) == 1
}

// requireAuth admits the root config token or any minted, unexpired API token.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := bearer(r)
		if got == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if s.isRoot(got) {
			next(w, r)
			return
		}
		if s.tokens != nil {
			if _, ok := s.tokens.Verify(got, time.Now()); ok {
				next(w, r)
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "invalid token")
	}
}

// requireRoot admits only the root config token.
func (s *Server) requireRoot(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := bearer(r)
		if got == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if s.isRoot(got) {
			next(w, r)
			return
		}
		// A valid minted token is authenticated but not authorized here.
		if s.tokens != nil {
			if _, ok := s.tokens.Verify(got, time.Now()); ok {
				writeError(w, http.StatusForbidden, "token management requires the root token")
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "invalid token")
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	sites, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wire.StatusResponse{
		Version:   Version,
		SiteCount: len(sites),
	})
}

func (s *Server) handleListSites(w http.ResponseWriter, _ *http.Request) {
	sites, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wire.ListSitesResponse{Sites: sites})
}

func (s *Server) handlePutSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := storage.ValidateName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer r.Body.Close()

	expiry, err := parseExpiry(r.Header.Get(wire.HeaderExpires))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.expiryMu.Lock()
	defer s.expiryMu.Unlock()

	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	site, err := s.store.ReplaceFromTarWithExpiry(name, body, expiry)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) || errors.Is(err, storage.ErrSiteTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("upload exceeds %d bytes (max_upload_bytes in config.yaml)", s.cfg.MaxUploadBytes))
			return
		}
		if errors.Is(err, storage.ErrUnsafePath) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Printf("put site %s: %v", name, err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wire.PutSiteResponse{
		Site: site,
		URL:  s.cfg.BaseURL + "/" + name + "/",
	})
}

// DeleteExpired serializes broker cleanup with uploads so a replacement and
// its new deadline cannot be split by the cleanup pass.
func (s *Server) DeleteExpired(now time.Time) ([]string, error) {
	s.expiryMu.Lock()
	defer s.expiryMu.Unlock()
	return s.store.DeleteExpired(now)
}

func parseExpiry(value string) (*time.Duration, error) {
	if value == "never" {
		return nil, nil
	}
	if value == "" {
		value = defaultExpiry.String()
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return nil, fmt.Errorf("invalid expiry %q: use a positive duration (for example 24h) or never", value)
	}
	return &d, nil
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		writeError(w, http.StatusInternalServerError, "token store unavailable")
		return
	}
	defer r.Body.Close()
	var req wire.CreateTokenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	ttl, err := parseTokenExpiry(req.Expires)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plaintext, rec, err := s.tokens.Create(req.Name, ttl, time.Now())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, token.ErrDuplicateName) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	s.log.Printf("token created: %s (%s)", rec.Name, rec.ID)
	writeJSON(w, http.StatusCreated, wire.CreateTokenResponse{
		Token: plaintext,
		Info:  tokenInfo(rec),
	})
}

func (s *Server) handleListTokens(w http.ResponseWriter, _ *http.Request) {
	if s.tokens == nil {
		writeError(w, http.StatusInternalServerError, "token store unavailable")
		return
	}
	recs := s.tokens.List()
	out := make([]wire.TokenInfo, len(recs))
	for i, r := range recs {
		out[i] = tokenInfo(r)
	}
	writeJSON(w, http.StatusOK, wire.ListTokensResponse{Tokens: out})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		writeError(w, http.StatusInternalServerError, "token store unavailable")
		return
	}
	id := r.PathValue("id")
	if err := s.tokens.Revoke(id); err != nil {
		if errors.Is(err, token.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Printf("token revoked: %s", id)
	w.WriteHeader(http.StatusNoContent)
}

func tokenInfo(r token.Record) wire.TokenInfo {
	return wire.TokenInfo{
		ID:         r.ID,
		Name:       r.Name,
		CreatedAt:  r.CreatedAt,
		ExpiresAt:  r.ExpiresAt,
		LastUsedAt: r.LastUsedAt,
	}
}

// parseTokenExpiry interprets CreateTokenRequest.Expires. Unlike site expiry,
// the default is "never" — tokens are managed credentials, not artifacts.
func parseTokenExpiry(value string) (*time.Duration, error) {
	if value == "" || value == "never" {
		return nil, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return nil, fmt.Errorf("invalid expiry %q: use a positive duration (for example 720h) or never", value)
	}
	return &d, nil
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := storage.ValidateName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.Delete(name); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePublic serves /<site>/... and an index at /. Disk sites win; if no
// disk site exists for the requested name, falls through to the matching
// builtin (if any).
func (s *Server) handlePublic(w http.ResponseWriter, r *http.Request) {
	// /api/* is owned by the auth handlers; if we got here it didn't match — 404.
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		http.NotFound(w, r)
		return
	}

	if r.URL.Path == "/" {
		s.renderIndex(w, r)
		return
	}

	// Extract site name (first path segment).
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	name := parts[0]
	if err := storage.ValidateName(name); err != nil {
		http.NotFound(w, r)
		return
	}

	// Disk first.
	exists, err := s.store.Exists(name)
	if err == nil && exists {
		s.serveDisk(w, r, name, parts)
		return
	}

	// Then builtin.
	if site, ok := s.findBuiltin(name); ok {
		s.serveBuiltin(w, r, site, parts)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) serveDisk(w http.ResponseWriter, r *http.Request, name string, parts []string) {
	siteDir, err := s.store.Path(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		http.Redirect(w, r, "/"+name+"/", http.StatusFound)
		return
	}
	rest := parts[1]
	if rest == "" || strings.HasSuffix(rest, "/") {
		rest = path.Join(rest, "index.html")
	}
	cleaned := path.Clean("/" + rest)
	http.ServeFile(w, r, siteDir+cleaned)
}

func (s *Server) serveBuiltin(w http.ResponseWriter, r *http.Request, site builtin.Site, parts []string) {
	if len(parts) == 1 {
		http.Redirect(w, r, "/"+site.Name+"/", http.StatusFound)
		return
	}
	rest := parts[1]
	if rest == "" || strings.HasSuffix(rest, "/") {
		rest = path.Join(rest, "index.html")
	}
	cleaned := strings.TrimPrefix(path.Clean("/"+rest), "/")
	http.ServeFileFS(w, r, site.FS, cleaned)
}

func (s *Server) findBuiltin(name string) (builtin.Site, bool) {
	for _, b := range s.builtins {
		if b.Name == name {
			return b, true
		}
	}
	return builtin.Site{}, false
}

func (s *Server) renderIndex(w http.ResponseWriter, _ *http.Request) {
	sites, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Track which builtins are shadowed by a disk site of the same name.
	diskNames := make(map[string]bool, len(sites))
	for _, s := range sites {
		diskNames[s.Name] = true
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintln(w, "<!doctype html><meta charset=utf-8><title>crate</title>")
	fmt.Fprintln(w, "<style>body{font-family:system-ui,sans-serif;max-width:40em;margin:2em auto;padding:0 1em;line-height:1.55}.tag{display:inline-block;font-size:.72em;padding:.15em .55em;border-radius:999px;background:rgba(80,180,120,.2);color:#2f9e63;margin-left:.5em;vertical-align:middle;font-weight:600;letter-spacing:.03em}</style>")
	fmt.Fprintln(w, "<h1>crate</h1>")

	hasAny := len(sites) > 0
	for _, b := range s.builtins {
		if !diskNames[b.Name] {
			hasAny = true
			break
		}
	}
	if !hasAny {
		fmt.Fprintln(w, "<p>No sites deployed yet. Try <code>crate push ./dir name</code>.</p>")
		return
	}

	fmt.Fprintln(w, "<ul>")
	for _, site := range sites {
		fmt.Fprintf(w, "<li><a href=\"/%s/\">%s</a> &middot; %d files &middot; %d bytes</li>\n",
			html.EscapeString(site.Name), html.EscapeString(site.Name), site.FileCount, site.SizeBytes)
	}
	for _, b := range s.builtins {
		if diskNames[b.Name] {
			continue // shadowed by a disk site with the same name
		}
		files, size := countEmbedded(b.FS)
		fmt.Fprintf(w, "<li><a href=\"/%s/\">%s</a> <span class=\"tag\">built-in</span> &middot; %d files &middot; %d bytes</li>\n",
			html.EscapeString(b.Name), html.EscapeString(b.Name), files, size)
	}
	fmt.Fprintln(w, "</ul>")
}

func countEmbedded(fsys fs.FS) (files int, size int64) {
	_ = fs.WalkDir(fsys, ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr == nil {
			size += info.Size()
			files++
		}
		return nil
	})
	return files, size
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, wire.ErrorResponse{Error: msg})
}
