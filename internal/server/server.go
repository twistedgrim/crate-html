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
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/storage"
	"github.com/Twistedgrim/crate-html/internal/wire"
)

// Version is the daemon version reported by /api/status.
const Version = "0.1.0-dev"

// Server bundles the HTTP handlers.
type Server struct {
	store *storage.Store
	cfg   config.Config
	log   *log.Logger
}

// New returns a Server.
func New(cfg config.Config, store *storage.Store, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{store: store, cfg: cfg, log: logger}
}

// Handler returns the root http.Handler for the daemon.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/sites", s.requireAuth(s.handleListSites))
	mux.HandleFunc("PUT /api/sites/{name}", s.requireAuth(s.handlePutSite))
	mux.HandleFunc("DELETE /api/sites/{name}", s.requireAuth(s.handleDeleteSite))

	// Static + index
	mux.HandleFunc("GET /", s.handlePublic)

	return mux
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get(wire.HeaderAuth)
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		got := strings.TrimPrefix(h, prefix)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r)
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

	site, err := s.store.ReplaceFromTar(name, r.Body)
	if err != nil {
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

// handlePublic serves /<site>/... and an index at /.
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

	siteDir, err := s.store.Path(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	exists, err := s.store.Exists(name)
	if err != nil || !exists {
		http.NotFound(w, r)
		return
	}

	// If the URL is /<name> (no trailing slash), redirect so relative links work.
	if len(parts) == 1 {
		http.Redirect(w, r, "/"+name+"/", http.StatusFound)
		return
	}

	rest := parts[1]
	if rest == "" || strings.HasSuffix(rest, "/") {
		rest = path.Join(rest, "index.html")
	}
	// path.Clean prevents directory traversal: any ../ resolves before the join,
	// and an absolute "/" gets reduced. The site root is then joined with the
	// cleaned relative path.
	cleaned := path.Clean("/" + rest)
	full := siteDir + cleaned
	http.ServeFile(w, r, full)
}

func (s *Server) renderIndex(w http.ResponseWriter, _ *http.Request) {
	sites, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintln(w, "<!doctype html><meta charset=utf-8><title>crate</title>")
	fmt.Fprintln(w, "<style>body{font-family:system-ui,sans-serif;max-width:40em;margin:2em auto;padding:0 1em}</style>")
	fmt.Fprintln(w, "<h1>crate</h1>")
	if len(sites) == 0 {
		fmt.Fprintln(w, "<p>No sites deployed yet. Try <code>crate push ./dir name</code>.</p>")
		return
	}
	fmt.Fprintln(w, "<ul>")
	for _, site := range sites {
		fmt.Fprintf(w, "<li><a href=\"/%s/\">%s</a> &middot; %d files &middot; %d bytes</li>\n",
			html.EscapeString(site.Name), html.EscapeString(site.Name), site.FileCount, site.SizeBytes)
	}
	fmt.Fprintln(w, "</ul>")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, wire.ErrorResponse{Error: msg})
}
