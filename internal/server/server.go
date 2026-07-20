// Package server is the HTTP daemon. It serves /api endpoints (bearer-token
// authed) for managing sites and a public path-routed static server for
// /<site>/... so deployed sites are reachable in a browser.
package server

import (
	"bytes"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Twistedgrim/crate-html/internal/builtin"
	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/storage"
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
	store     *storage.Store
	cfg       config.Config
	log       *log.Logger
	builtins  []builtin.Site
	indexTmpl *template.Template
	expiryMu  sync.Mutex
}

// New returns a Server. Pass nil for builtins to skip embedded sites.
func New(cfg config.Config, store *storage.Store, builtins []builtin.Site, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{store: store, cfg: cfg, log: logger, builtins: builtins, indexTmpl: defaultIndexTmpl}
}

// UseIndexTemplateFile parses path and uses it for the root/group index
// instead of the embedded default. It is operator-controlled (config +
// filesystem access), deliberately not a pushed-site mechanism, so it may
// contain template logic the embedded default does not. Returns an error if
// the file cannot be read or parsed, so callers can fail fast at startup.
//
// The template is executed with an indexView (see renderIndexView): a struct
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

	expiry, err := parseExpiry(r.Header.Get(wire.HeaderExpires))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.expiryMu.Lock()
	defer s.expiryMu.Unlock()

	site, err := s.store.ReplaceFromTarWithExpiry(name, r.Body, expiry)
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

func (s *Server) executeIndex(w http.ResponseWriter, view indexView) {
	// Render into a buffer first so a template that parses but fails at
	// execution (a custom operator template referencing a missing field)
	// yields a 500, not a 200 with truncated HTML.
	var buf bytes.Buffer
	if err := s.indexTmpl.Execute(&buf, view); err != nil {
		s.log.Printf("render index: %v", err)
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
