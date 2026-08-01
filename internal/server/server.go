// Package server is the HTTP daemon. It serves /api endpoints (bearer-token
// authed) for managing sites and a public path-routed static server for
// /<site>/... so deployed sites are reachable in a browser.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Twistedgrim/crate-html/internal/builtin"
	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/storage"
	"github.com/Twistedgrim/crate-html/internal/telemetry"
	"github.com/Twistedgrim/crate-html/internal/token"
	"github.com/Twistedgrim/crate-html/internal/wire"
)

//go:embed index.tmpl
var indexTmplSrc string

// defaultIndexTmpl is the embedded index template, parsed once at init. A
// custom operator-supplied template (config.IndexTemplate) replaces it per
// Server; the embedded one is the fallback.
var defaultIndexTmpl = template.Must(template.New("index").Parse(indexTmplSrc))

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
	store      ReadBackend
	mutable    Backend
	tokens     *token.Store
	cfg        config.Config
	log        *slog.Logger
	builtins   []builtin.Site
	indexTmpl  *template.Template
	metrics    telemetry.BrokerMetrics
	mutationMu sync.Mutex
}

// New returns a Server. Pass nil for builtins to skip embedded sites and nil
// for tokens to accept only the root config token.
func New(cfg config.Config, store Backend, tokens *token.Store, builtins []builtin.Site, logger *slog.Logger) *Server {
	return NewWithMetrics(cfg, store, tokens, builtins, logger, nil)
}

// NewWithMetrics returns a broker-capable Server with an explicit metrics
// dependency. Passing nil leaves metrics disabled, which keeps embedders and
// unit tests opt-in and prevents the public web role from creating them.
func NewWithMetrics(cfg config.Config, store Backend, tokens *token.Store, builtins []builtin.Site, logger *slog.Logger, metrics telemetry.BrokerMetrics) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = telemetry.DisabledMetrics()
	}
	return &Server{
		store: store, mutable: store, tokens: tokens, cfg: cfg, log: logger,
		builtins: builtins, indexTmpl: defaultIndexTmpl, metrics: metrics,
	}
}

// NewReadOnly returns a Server with no mutation backend or token store. It is
// used by the public web role so only read operations are reachable from that
// process's HTTP layer.
func NewReadOnly(cfg config.Config, store ReadBackend, builtins []builtin.Site, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store: store, cfg: cfg, log: logger, builtins: builtins, indexTmpl: defaultIndexTmpl,
	}
}

// UseIndexTemplateFile parses path and uses it for the root/group index
// instead of the embedded default. It is operator-controlled (config +
// filesystem access), deliberately not a pushed-site mechanism, so it may
// contain template logic the embedded default does not. Returns an error if
// the file cannot be read or parsed, so callers can fail fast at startup.
//
// See examples/index.tmpl for a working template with the view model
// documented inline.
//
// The template is executed with an indexView (see renderIndex): a struct
// of {Title string, Group bool, Empty bool, Groups []siteGroup}, where each
// siteGroup is {Name, Href string, Grouped bool, Rows []siteRow} and each
// siteRow is {Name, Label, Href string, Builtin bool, FileCount int,
// SizeHuman, UpdatedRel, UpdatedAbs, ExpiryLabel string}.
func (s *Server) UseIndexTemplateFile(path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read index template %q: %w", path, err)
	}
	t, err := template.New("index").Parse(string(src))
	if err != nil {
		return fmt.Errorf("parse index template %q: %w", path, err)
	}
	s.indexTmpl = t
	return nil
}

// Handler returns the combined broker + public handler used by the default
// all-in-one deployment.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	s.registerBrokerRoutes(mux)
	s.registerPublicRoutes(mux)
	return mux
}

// BrokerHandler returns only the authenticated control-plane API plus health.
// Public crate URLs deliberately return 404 on this surface.
func (s *Server) BrokerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	s.registerBrokerRoutes(mux)
	return mux
}

// PublicHandler returns only the human-facing index/static server plus health.
// No /api route is registered on this surface.
func (s *Server) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	s.registerPublicRoutes(mux)
	mux.HandleFunc("/", http.NotFound)
	return mux
}

func (s *Server) registerBrokerRoutes(mux *http.ServeMux) {
	s.registerBrokerRoute(mux, "GET /api/status", "/api/status", s.handleStatus)
	s.registerBrokerRoute(mux, "GET /api/sites", "/api/sites", s.requireAuth(s.handleListSites))
	s.registerBrokerRoute(mux, "PUT /api/sites/{name}", "/api/sites/{name}", s.requireAuth(s.handlePutSite))
	s.registerBrokerRoute(mux, "DELETE /api/sites/{name}", "/api/sites/{name}", s.requireAuth(s.handleDeleteSite))

	// Token management is root-only: minted tokens can manage sites but can
	// never mint, list, or revoke tokens. This keeps privilege escalation
	// off the table without introducing scopes.
	s.registerBrokerRoute(mux, "POST /api/tokens", "/api/tokens", s.requireRoot(s.handleCreateToken))
	s.registerBrokerRoute(mux, "GET /api/tokens", "/api/tokens", s.requireRoot(s.handleListTokens))
	s.registerBrokerRoute(mux, "DELETE /api/tokens/{id}", "/api/tokens/{id}", s.requireRoot(s.handleRevokeToken))
}

func (s *Server) registerBrokerRoute(mux *http.ServeMux, pattern, route string, next http.HandlerFunc) {
	mux.Handle(pattern, s.logBrokerRequest(route, next))
}

type brokerAudit struct {
	requestID string
	authKind  string
	tokenID   string
	tokenName string
	rejection string
}

type brokerAuditContextKey struct{}

type brokerResponseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *brokerResponseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *brokerResponseRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func (w *brokerResponseRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type countingRequestBody struct {
	io.ReadCloser
	bytes int64
}

func (r *countingRequestBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytes += int64(n)
	return n, err
}

func (s *Server) logBrokerRequest(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		audit := &brokerAudit{requestID: newRequestID(), authKind: "anonymous"}
		r = r.WithContext(context.WithValue(r.Context(), brokerAuditContextKey{}, audit))
		body := &countingRequestBody{ReadCloser: r.Body}
		r.Body = body
		w.Header().Set(wire.HeaderRequestID, audit.requestID)
		recorder := &brokerResponseRecorder{ResponseWriter: w}

		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		s.metrics.HTTP(r.Method, route, recorder.status, time.Since(started), body.bytes)
		if audit.rejection == "missing_bearer" || audit.rejection == "invalid_token" || audit.rejection == "root_required" {
			s.metrics.AuthRejection(audit.rejection)
		}
		if r.Method == http.MethodPut && route == "/api/sites/{name}" {
			s.metrics.Mutation("push", mutationOutcome(recorder.status))
		}
		if r.Method == http.MethodDelete && route == "/api/sites/{name}" {
			s.metrics.Mutation("delete", mutationOutcome(recorder.status))
		}
		attrs := []any{
			"request_id", audit.requestID,
			"method", r.Method,
			"route", route,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(started).Milliseconds(),
			"content_length", r.ContentLength,
			"request_bytes", body.bytes,
			"response_bytes", recorder.bytes,
			"auth_kind", audit.authKind,
		}
		if audit.tokenID != "" {
			attrs = append(attrs, "token_id", audit.tokenID, "token_name", audit.tokenName)
		}
		if audit.rejection != "" {
			attrs = append(attrs, "rejection", audit.rejection)
		}
		switch {
		case recorder.status >= http.StatusInternalServerError:
			s.log.ErrorContext(r.Context(), "broker request", attrs...)
		case recorder.status >= http.StatusBadRequest:
			s.log.WarnContext(r.Context(), "broker request", attrs...)
		default:
			s.log.InfoContext(r.Context(), "broker request", attrs...)
		}
	})
}

func mutationOutcome(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "success"
	case status >= 400 && status < 500:
		return "rejected"
	default:
		return "error"
	}
}

func newRequestID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func auditFromRequest(r *http.Request) *brokerAudit {
	audit, _ := r.Context().Value(brokerAuditContextKey{}).(*brokerAudit)
	return audit
}

func markRejection(r *http.Request, reason string) {
	if audit := auditFromRequest(r); audit != nil && audit.rejection == "" {
		audit.rejection = reason
	}
}

func (s *Server) logRequestEvent(r *http.Request, level slog.Level, message string, attrs ...any) {
	if audit := auditFromRequest(r); audit != nil {
		base := []any{"request_id", audit.requestID, "auth_kind", audit.authKind}
		if audit.tokenID != "" {
			base = append(base, "token_id", audit.tokenID, "token_name", audit.tokenName)
		}
		attrs = append(base, attrs...)
	}
	s.log.Log(r.Context(), level, message, attrs...)
}

func (s *Server) registerPublicRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.handlePublic)
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
			if audit := auditFromRequest(r); audit != nil {
				audit.rejection = "missing_bearer"
			}
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if s.isRoot(got) {
			if audit := auditFromRequest(r); audit != nil {
				audit.authKind = "root"
			}
			next(w, r)
			return
		}
		if s.tokens != nil {
			if rec, ok := s.tokens.Verify(got, time.Now()); ok {
				if audit := auditFromRequest(r); audit != nil {
					audit.authKind = "token"
					audit.tokenID = rec.ID
					audit.tokenName = rec.Name
				}
				next(w, r)
				return
			}
		}
		if audit := auditFromRequest(r); audit != nil {
			audit.rejection = "invalid_token"
		}
		writeError(w, http.StatusUnauthorized, "invalid token")
	}
}

// requireRoot admits only the root config token.
func (s *Server) requireRoot(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := bearer(r)
		if got == "" {
			if audit := auditFromRequest(r); audit != nil {
				audit.rejection = "missing_bearer"
			}
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if s.isRoot(got) {
			if audit := auditFromRequest(r); audit != nil {
				audit.authKind = "root"
			}
			next(w, r)
			return
		}
		// A valid minted token is authenticated but not authorized here.
		if s.tokens != nil {
			if rec, ok := s.tokens.Verify(got, time.Now()); ok {
				if audit := auditFromRequest(r); audit != nil {
					audit.authKind = "token"
					audit.tokenID = rec.ID
					audit.tokenName = rec.Name
					audit.rejection = "root_required"
				}
				writeError(w, http.StatusForbidden, "token management requires the root token")
				return
			}
		}
		if audit := auditFromRequest(r); audit != nil {
			audit.rejection = "invalid_token"
		}
		writeError(w, http.StatusUnauthorized, "invalid token")
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	sites, err := s.store.List()
	s.metrics.Storage("list", storageOutcome(err), time.Since(started))
	if err != nil {
		s.logRequestEvent(r, slog.LevelError, "status storage query failed", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.metrics.SetSiteCount(len(sites))
	writeJSON(w, http.StatusOK, wire.StatusResponse{
		Version:   Version,
		SiteCount: len(sites),
		PublicURL: s.cfg.EffectivePublicURL(),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, wire.HealthResponse{Version: Version})
}

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	sites, err := s.store.List()
	s.metrics.Storage("list", storageOutcome(err), time.Since(started))
	if err != nil {
		s.logRequestEvent(r, slog.LevelError, "site list failed", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.metrics.SetSiteCount(len(sites))
	writeJSON(w, http.StatusOK, wire.ListSitesResponse{Sites: sites})
}

func (s *Server) handlePutSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := storage.ValidateName(name); err != nil {
		markRejection(r, "invalid_site_name")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer r.Body.Close()

	expiry, err := parseExpiry(r.Header.Get(wire.HeaderExpires))
	if err != nil {
		markRejection(r, "invalid_expiry")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	staged, err := os.CreateTemp("", "crate-upload-*.tar")
	if err != nil {
		s.logRequestEvent(r, slog.LevelError, "site upload staging failed", "site", name, "err", err)
		writeError(w, http.StatusInternalServerError, "stage upload failed")
		return
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()
	if _, err := io.Copy(staged, body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			markRejection(r, "upload_too_large")
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("upload exceeds %d bytes (max_upload_bytes in config.yaml)", s.cfg.MaxUploadBytes))
			return
		}
		markRejection(r, "upload_read_failed")
		s.logRequestEvent(r, slog.LevelWarn, "site upload read failed", "site", name, "err", err)
		writeError(w, http.StatusBadRequest, "read upload failed")
		return
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		s.logRequestEvent(r, slog.LevelError, "site upload rewind failed", "site", name, "err", err)
		writeError(w, http.StatusInternalServerError, "stage upload failed")
		return
	}

	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	storageStarted := time.Now()
	site, err := s.mutable.ReplaceFromTarWithExpiry(name, staged, expiry)
	s.metrics.Storage("replace", storageOutcome(err), time.Since(storageStarted))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) || errors.Is(err, storage.ErrSiteTooLarge) {
			markRejection(r, "upload_too_large")
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("upload exceeds %d bytes (max_upload_bytes in config.yaml)", s.cfg.MaxUploadBytes))
			return
		}
		if errors.Is(err, storage.ErrUnsafePath) {
			markRejection(r, "unsafe_archive_path")
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.logRequestEvent(r, slog.LevelError, "site store failed", "site", name, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	expiresAt := any("never")
	if site.ExpiresAt != nil {
		expiresAt = site.ExpiresAt.UTC()
	}
	s.logRequestEvent(r, slog.LevelInfo, "site stored",
		"site", site.Name,
		"files", site.FileCount,
		"bytes", site.SizeBytes,
		"expires_at", expiresAt,
	)
	s.refreshMetricSiteCount()
	writeJSON(w, http.StatusOK, wire.PutSiteResponse{
		Site: site,
		URL:  strings.TrimRight(s.cfg.EffectivePublicURL(), "/") + "/" + name + "/",
	})
}

// DeleteExpired serializes broker cleanup with uploads so a replacement and
// its new deadline cannot be split by the cleanup pass.
func (s *Server) DeleteExpired(now time.Time) ([]string, error) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	started := time.Now()
	deleted, err := s.mutable.DeleteExpired(now)
	s.metrics.Storage("delete_expired", storageOutcome(err), time.Since(started))
	s.metrics.Expiry(time.Since(started), len(deleted), err)
	if err == nil {
		s.refreshMetricSiteCount()
	}
	return deleted, err
}

// RefreshMetricSiteCount updates the cached site gauge from storage. It is
// called at broker startup and after mutations, never by a metric scrape.
func (s *Server) RefreshMetricSiteCount() {
	s.refreshMetricSiteCount()
}

func (s *Server) refreshMetricSiteCount() {
	started := time.Now()
	sites, err := s.store.List()
	s.metrics.Storage("list", storageOutcome(err), time.Since(started))
	if err == nil {
		s.metrics.SetSiteCount(len(sites))
	}
}

func storageOutcome(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
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
		s.logRequestEvent(r, slog.LevelError, "token store unavailable")
		writeError(w, http.StatusInternalServerError, "token store unavailable")
		return
	}
	defer r.Body.Close()
	var req wire.CreateTokenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		markRejection(r, "invalid_json")
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	ttl, err := parseTokenExpiry(req.Expires)
	if err != nil {
		markRejection(r, "invalid_token_expiry")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plaintext, rec, err := s.tokens.Create(req.Name, ttl, time.Now())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, token.ErrDuplicateName) {
			status = http.StatusConflict
			markRejection(r, "duplicate_token_name")
		} else {
			markRejection(r, "invalid_token_request")
		}
		writeError(w, status, err.Error())
		return
	}
	s.logRequestEvent(r, slog.LevelInfo, "token created", "token_id", rec.ID, "token_name", rec.Name)
	writeJSON(w, http.StatusCreated, wire.CreateTokenResponse{
		Token: plaintext,
		Info:  tokenInfo(rec),
	})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		s.logRequestEvent(r, slog.LevelError, "token store unavailable")
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
		s.logRequestEvent(r, slog.LevelError, "token store unavailable")
		writeError(w, http.StatusInternalServerError, "token store unavailable")
		return
	}
	id := r.PathValue("id")
	if err := s.tokens.Revoke(id); err != nil {
		if errors.Is(err, token.ErrNotFound) {
			markRejection(r, "token_not_found")
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		s.logRequestEvent(r, slog.LevelError, "token revoke failed", "revoked_token", id, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logRequestEvent(r, slog.LevelInfo, "token revoked", "revoked_token", id)
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
		markRejection(r, "invalid_site_name")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	storageStarted := time.Now()
	err := s.mutable.Delete(name)
	s.metrics.Storage("delete", storageOutcome(err), time.Since(storageStarted))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			markRejection(r, "site_not_found")
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		s.logRequestEvent(r, slog.LevelError, "site delete failed", "site", name, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logRequestEvent(r, slog.LevelInfo, "site deleted", "site", name)
	s.refreshMetricSiteCount()
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

	// Stored sites first. Expiry is enforced on every read so a stopped broker
	// can delay garbage collection but can never extend public availability.
	site, err := s.store.Stat(name)
	if err == nil {
		if expired(site, time.Now()) {
			http.NotFound(w, r)
			return
		}
		fsys, oerr := s.store.Open(name)
		if oerr != nil {
			if errors.Is(oerr, storage.ErrNotFound) {
				http.NotFound(w, r) // deleted between Exists and Open
				return
			}
			s.log.ErrorContext(r.Context(), "open site failed", "site", name, "err", oerr)
			writeError(w, http.StatusInternalServerError, "open site failed")
			return
		}
		s.serveSite(w, r, name, fsys, parts)
		return
	}

	// Then builtin.
	if site, ok := s.findBuiltin(name); ok {
		s.serveSite(w, r, site.Name, site.FS, parts)
		return
	}

	// Then a synthetic per-project index: no exact site or builtin owns this
	// name, but one or more disk sites are dot-namespaced under it
	// (name.child). An exact site/builtin always wins over this, so pushing a
	// real "myproject" site shadows the synthetic group index.
	//
	// This is on the 404 hot path (every unknown path, e.g. /favicon.ico,
	// lands here), so only a cheap name scan runs now — the full metadata
	// List() is deferred to renderGroupIndex, which fires solely on an actual
	// group-index render.
	if s.hasGroupChildren(name) {
		if len(parts) == 1 {
			http.Redirect(w, r, "/"+name+"/", http.StatusFound)
			return
		}
		if parts[1] == "" {
			s.renderGroupIndex(w, r, name)
			return
		}
	}

	http.NotFound(w, r)
}

// hasGroupChildren reports whether any disk site is dot-namespaced under
// prefix (prefix.child), using a name-only scan (no per-site stat). A site
// named exactly prefix is not a child — that is an exact match resolved
// before this is reached.
func (s *Server) hasGroupChildren(prefix string) bool {
	names, err := s.store.Names()
	if err != nil {
		return false
	}
	for _, name := range names {
		if strings.HasPrefix(name, prefix+".") {
			return true
		}
	}
	return false
}

// serveSite serves one file out of a site's fs.FS. Stored sites and embedded
// built-ins share this path — a site is just a name plus an fs.FS, wherever
// its bytes came from.
func (s *Server) serveSite(w http.ResponseWriter, r *http.Request, name string, fsys fs.FS, parts []string) {
	if len(parts) == 1 {
		http.Redirect(w, r, "/"+name+"/", http.StatusFound)
		return
	}
	rest := parts[1]
	if rest == "" || strings.HasSuffix(rest, "/") {
		rest = path.Join(rest, "index.html")
	}
	// fs.FS paths are unrooted, so Clean against "/" to collapse any traversal
	// and then drop the leading separator.
	cleaned := strings.TrimPrefix(path.Clean("/"+rest), "/")
	http.ServeFileFS(w, r, fsys, cleaned)
}

func (s *Server) findBuiltin(name string) (builtin.Site, bool) {
	for _, b := range s.builtins {
		if b.Name == name {
			return b, true
		}
	}
	return builtin.Site{}, false
}

// indexView is the data the index template is executed with. It is documented
// on UseIndexTemplateFile because custom templates depend on this shape.
type indexView struct {
	Title  string
	Group  bool
	Empty  bool
	Groups []siteGroup
}

// siteGroup is a header + rows on the index. Grouped false means a bare row
// with no header (an ungrouped, non-namespaced site or a builtin).
type siteGroup struct {
	Name    string
	Href    string
	Grouped bool
	Rows    []siteRow
}

// siteRow is one site on the index. Label is the link text (the child suffix
// inside a group, else the full name); Name is always the full site name and
// appears in the link title.
type siteRow struct {
	Name        string
	Label       string
	Href        string
	Builtin     bool
	FileCount   int
	SizeHuman   string
	UpdatedRel  string
	UpdatedAbs  string
	ExpiryLabel string
}

func (s *Server) renderIndex(w http.ResponseWriter, _ *http.Request) {
	sites, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sites = unexpiredSites(sites, time.Now())

	// Track which builtins are shadowed by a disk site of the same name.
	diskNames := make(map[string]bool, len(sites))
	for _, site := range sites {
		diskNames[site.Name] = true
	}

	groups := groupDiskSites(sites)
	for _, b := range s.builtins {
		if diskNames[b.Name] {
			continue // shadowed by a disk site with the same name
		}
		files, size := countEmbedded(b.FS)
		groups = append(groups, siteGroup{Rows: []siteRow{{
			Name:      b.Name,
			Label:     b.Name,
			Href:      "/" + b.Name + "/",
			Builtin:   true,
			FileCount: files,
			SizeHuman: humanSize(size),
		}}})
	}

	s.executeIndex(w, indexView{
		Title:  "crate",
		Empty:  len(groups) == 0,
		Groups: groups,
	})
}

// renderGroupIndex serves the synthetic /<prefix>/ page listing the disk sites
// namespaced under prefix. The caller has confirmed (cheaply) that at least
// one child exists; this is where the full metadata scan happens.
func (s *Server) renderGroupIndex(w http.ResponseWriter, r *http.Request, prefix string) {
	sites, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sites = unexpiredSites(sites, time.Now())
	rows := make([]siteRow, 0)
	for _, site := range sites {
		if strings.HasPrefix(site.Name, prefix+".") {
			rows = append(rows, diskRow(site, strings.TrimPrefix(site.Name, prefix+".")))
		}
	}
	if len(rows) == 0 {
		http.NotFound(w, r) // race: children vanished between check and render
		return
	}
	s.executeIndex(w, indexView{
		Title:  prefix,
		Group:  true,
		Groups: []siteGroup{{Rows: rows}},
	})
}

func expired(site wire.Site, now time.Time) bool {
	return site.ExpiresAt != nil && !site.ExpiresAt.After(now)
}

func unexpiredSites(sites []wire.Site, now time.Time) []wire.Site {
	out := sites[:0]
	for _, site := range sites {
		if !expired(site, now) {
			out = append(out, site)
		}
	}
	return out
}

func (s *Server) executeIndex(w http.ResponseWriter, view indexView) {
	// Render into a buffer first so a template that parses but fails at
	// execution (a custom operator template referencing a missing field)
	// yields a 500, not a 200 with truncated HTML.
	var buf bytes.Buffer
	if err := s.indexTmpl.Execute(&buf, view); err != nil {
		s.log.Error("render index failed", "err", err)
		writeError(w, http.StatusInternalServerError, "index render failed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// groupDiskSites turns a sorted site list into ordered index groups. Sites
// whose name contains a dot are grouped under the prefix before the first dot
// (myproject.docs, myproject.plan -> a "myproject" group); undotted names
// render as bare, headerless rows. A group with a single member is only
// promoted to a header when that member is itself namespaced.
func groupDiskSites(sites []wire.Site) []siteGroup {
	order := make([]string, 0)
	byPrefix := make(map[string][]wire.Site)
	for _, site := range sites {
		prefix := site.Name
		if i := strings.Index(site.Name, "."); i >= 0 {
			prefix = site.Name[:i]
		}
		if _, seen := byPrefix[prefix]; !seen {
			order = append(order, prefix)
		}
		byPrefix[prefix] = append(byPrefix[prefix], site)
	}

	groups := make([]siteGroup, 0, len(order))
	for _, prefix := range order {
		members := byPrefix[prefix]
		grouped := len(members) > 1 || strings.Contains(members[0].Name, ".")
		rows := make([]siteRow, 0, len(members))
		for _, site := range members {
			label := site.Name
			if grouped {
				label = strings.TrimPrefix(site.Name, prefix+".")
				if label == "" {
					label = site.Name
				}
			}
			rows = append(rows, diskRow(site, label))
		}
		groups = append(groups, siteGroup{
			Name:    prefix,
			Href:    "/" + prefix + "/",
			Grouped: grouped,
			Rows:    rows,
		})
	}
	return groups
}

func diskRow(site wire.Site, label string) siteRow {
	row := siteRow{
		Name:      site.Name,
		Label:     label,
		Href:      "/" + site.Name + "/",
		FileCount: site.FileCount,
		SizeHuman: humanSize(site.SizeBytes),
	}
	if !site.UpdatedAt.IsZero() {
		row.UpdatedRel = relTime(site.UpdatedAt, time.Now())
		row.UpdatedAbs = site.UpdatedAt.Format(time.RFC3339)
	}
	if site.ExpiresAt != nil {
		if d := time.Until(*site.ExpiresAt); d > 0 {
			row.ExpiryLabel = "expires in " + humanDuration(d)
		} else {
			row.ExpiryLabel = "expired"
		}
	}
	return row
}

// humanSize formats a byte count as a short human-readable string.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// relTime renders t relative to now as a compact "5m ago"/"2h ago"/"3d ago",
// falling back to an absolute date beyond a week.
func relTime(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		return t.Format("2006-01-02")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// humanDuration renders a positive future duration compactly (e.g. "23h",
// "45m", "3d").
func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return "<1m"
	}
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
