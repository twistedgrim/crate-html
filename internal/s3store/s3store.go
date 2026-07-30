// Package s3store keeps sites in S3-compatible object storage instead of on a
// local filesystem, so crated can run somewhere with no writable volume.
//
// # Layout
//
// A site is stored as a single tar object under a version-scoped key, plus a
// small metadata object that names the live version:
//
//	<prefix>meta/<name>.json           {version, file_count, size_bytes, ...}
//	<prefix>sites/<name>/<version>.tar the archive as uploaded
//
// # Publishing is a pointer flip
//
// Object storage has no rename, so the local backend's stage-and-rename trick
// does not translate. Instead a push writes a brand-new version object — which
// nothing references yet, so it is invisible — and then overwrites the metadata
// object to point at it. That second PUT is a single atomic operation, which
// makes it the commit point: readers see either the old version or the new one
// and never a partial site. If the metadata write fails, the orphaned version
// object is garbage rather than a corrupted site.
//
// Metadata is also the existence record: a site with no metadata object does
// not exist, regardless of what content objects are lying around.
//
// # Caching
//
// Content is expanded into memory on first read and served from there. Cache
// keys include the version, so a push makes previous entries unreachable
// without explicit invalidation.
package s3store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/Twistedgrim/crate-html/internal/storage"
	"github.com/Twistedgrim/crate-html/internal/wire"
)

// Config describes how to reach the bucket.
type Config struct {
	// Endpoint is the S3 host, with or without a scheme (e.g.
	// "localhost:9000", "https://s3.amazonaws.com").
	Endpoint string
	// Bucket must already exist; the daemon does not create it.
	Bucket string
	// Region is optional for most S3-compatible servers.
	Region string
	// AccessKey and SecretKey authenticate to the endpoint. When both are
	// empty the AWS credential chain (env, IAM role) is used instead.
	AccessKey string
	SecretKey string
	// Prefix optionally scopes every key, so one bucket can host more than one
	// crate deployment. A missing trailing "/" is added.
	Prefix string
	// UseSSL selects https. Defaults on unless the endpoint says otherwise.
	UseSSL bool
	// MaxSiteBytes caps a site's total extracted size. 0 disables the cap.
	MaxSiteBytes int64
	// CacheBytes budgets the in-memory content cache. 0 disables caching.
	CacheBytes int64
	// MetaTTL is how long a listing may be served from memory before it is
	// re-read from the bucket. It bounds staleness when another replica writes.
	MetaTTL time.Duration
}

const (
	defaultMetaTTL    = 10 * time.Second
	defaultCacheBytes = 256 << 20 // 256 MiB
)

// Store implements the server's Backend over object storage.
type Store struct {
	client *minio.Client
	bucket string
	prefix string

	maxSiteBytes int64
	metaTTL      time.Duration

	content *cache

	mu        sync.RWMutex
	meta      map[string]siteMeta
	metaAt    time.Time
	metaValid bool
}

// siteMeta is the JSON metadata object. Version names the live content object.
type siteMeta struct {
	Name      string     `json:"name"`
	Version   string     `json:"version"`
	FileCount int        `json:"file_count"`
	SizeBytes int64      `json:"size_bytes"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (m siteMeta) toWire() wire.Site {
	return wire.Site{
		Name:      m.Name,
		UpdatedAt: m.UpdatedAt,
		SizeBytes: m.SizeBytes,
		FileCount: m.FileCount,
		ExpiresAt: m.ExpiresAt,
	}
}

// New connects to the bucket and verifies it is reachable, so a misconfigured
// endpoint fails at startup rather than on the first push.
func New(ctx context.Context, cfg Config) (*Store, error) {
	endpoint := cfg.Endpoint
	useSSL := cfg.UseSSL
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		endpoint, useSSL = strings.TrimPrefix(endpoint, "https://"), true
	case strings.HasPrefix(endpoint, "http://"):
		endpoint, useSSL = strings.TrimPrefix(endpoint, "http://"), false
	}
	endpoint = strings.TrimSuffix(endpoint, "/")
	if endpoint == "" {
		return nil, errors.New("s3: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}

	creds := credentials.NewEnvAWS()
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  creds,
		Secure: useSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: client: %w", err)
	}

	prefix := cfg.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	// Probe the exact metadata prefix the store needs instead of issuing a
	// bucket-wide HEAD. This permits public web credentials to scope
	// s3:ListBucket by s3:prefix and prevents startup from requiring visibility
	// into private state/ keys.
	for obj := range client.ListObjects(ctx, cfg.Bucket, minio.ListObjectsOptions{
		Prefix:  prefix + "meta/",
		MaxKeys: 1,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("s3: reach bucket %q at %s: %w", cfg.Bucket, endpoint, obj.Err)
		}
	}

	metaTTL := cfg.MetaTTL
	if metaTTL <= 0 {
		metaTTL = defaultMetaTTL
	}
	cacheBytes := cfg.CacheBytes
	if cacheBytes == 0 {
		cacheBytes = defaultCacheBytes
	}

	return &Store{
		client:       client,
		bucket:       cfg.Bucket,
		prefix:       prefix,
		maxSiteBytes: cfg.MaxSiteBytes,
		metaTTL:      metaTTL,
		content:      newCache(cacheBytes),
		meta:         make(map[string]siteMeta),
	}, nil
}

func (s *Store) metaKey(name string) string { return s.prefix + "meta/" + name + ".json" }
func (s *Store) metaPrefix() string         { return s.prefix + "meta/" }
func (s *Store) sitePrefix(name string) string {
	return s.prefix + "sites/" + name + "/"
}
func (s *Store) versionKey(name, version string) string {
	return s.sitePrefix(name) + version + ".tar"
}

// --- reads -----------------------------------------------------------------

// snapshot returns the metadata map, refreshing it from the bucket when the
// cached copy has aged past MetaTTL.
func (s *Store) snapshot() (map[string]siteMeta, error) {
	s.mu.RLock()
	fresh := s.metaValid && time.Since(s.metaAt) < s.metaTTL
	if fresh {
		out := make(map[string]siteMeta, len(s.meta))
		for k, v := range s.meta {
			out[k] = v
		}
		s.mu.RUnlock()
		return out, nil
	}
	s.mu.RUnlock()
	return s.refresh(context.Background())
}

// refresh re-reads every metadata object. Sites are few and the objects are
// tiny, but this is still one request per site, so it is rate-limited by
// MetaTTL rather than run per request.
func (s *Store) refresh(ctx context.Context) (map[string]siteMeta, error) {
	next := make(map[string]siteMeta)
	opts := minio.ListObjectsOptions{Prefix: s.metaPrefix(), Recursive: true}
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, opts) {
		if obj.Err != nil {
			return nil, fmt.Errorf("s3: list metadata: %w", obj.Err)
		}
		if strings.HasSuffix(obj.Key, ".json") {
			keys = append(keys, obj.Key)
		}
	}

	// Fetch concurrently — the listing is sequential but the GETs need not be.
	type result struct {
		meta siteMeta
		err  error
	}
	results := make([]result, len(keys))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, key := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m, err := s.getMeta(ctx, key)
			results[i] = result{meta: m, err: err}
		}()
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			// A metadata object that vanished mid-listing is normal (a delete
			// raced the scan); anything else is a real failure.
			if isNotFound(r.err) {
				continue
			}
			return nil, r.err
		}
		if storage.ValidateName(r.meta.Name) != nil {
			continue
		}
		next[r.meta.Name] = r.meta
	}

	s.mu.Lock()
	s.meta = next
	s.metaAt = time.Now()
	s.metaValid = true
	out := make(map[string]siteMeta, len(next))
	for k, v := range next {
		out[k] = v
	}
	s.mu.Unlock()
	return out, nil
}

func (s *Store) getMeta(ctx context.Context, key string) (siteMeta, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return siteMeta{}, err
	}
	defer obj.Close()
	var m siteMeta
	if err := json.NewDecoder(obj).Decode(&m); err != nil {
		if isNotFound(err) {
			return siteMeta{}, err
		}
		return siteMeta{}, fmt.Errorf("s3: decode %s: %w", key, err)
	}
	return m, nil
}

// lookup returns one site's metadata, preferring the cached map.
func (s *Store) lookup(name string) (siteMeta, bool, error) {
	snap, err := s.snapshot()
	if err != nil {
		return siteMeta{}, false, err
	}
	m, ok := snap[name]
	return m, ok, nil
}

// List implements Backend.
func (s *Store) List() ([]wire.Site, error) {
	snap, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	out := make([]wire.Site, 0, len(snap))
	for _, m := range snap {
		out = append(out, m.toWire())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Names implements Backend.
func (s *Store) Names() ([]string, error) {
	snap, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(snap))
	for name := range snap {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Exists implements Backend.
func (s *Store) Exists(name string) (bool, error) {
	if err := storage.ValidateName(name); err != nil {
		return false, err
	}
	_, ok, err := s.lookup(name)
	return ok, err
}

// Stat returns metadata for a single site.
func (s *Store) Stat(name string) (wire.Site, error) {
	if err := storage.ValidateName(name); err != nil {
		return wire.Site{}, err
	}
	m, ok, err := s.lookup(name)
	if err != nil {
		return wire.Site{}, err
	}
	if !ok {
		return wire.Site{}, storage.ErrNotFound
	}
	return m.toWire(), nil
}

// Open implements Backend, expanding the site's archive into memory on a cache
// miss and serving from the cache thereafter.
func (s *Store) Open(name string) (fs.FS, error) {
	if err := storage.ValidateName(name); err != nil {
		return nil, err
	}
	m, ok, err := s.lookup(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, storage.ErrNotFound
	}
	if fsys, hit := s.content.get(name, m.Version); hit {
		return fsys, nil
	}

	ctx := context.Background()
	obj, err := s.client.GetObject(ctx, s.bucket, s.versionKey(name, m.Version), minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	defer obj.Close()

	fsys, st, err := unpackTar(obj, s.maxSiteBytes)
	if err != nil {
		if isNotFound(err) {
			return nil, storage.ErrNotFound
		}
		return nil, fmt.Errorf("s3: unpack %s: %w", name, err)
	}
	s.content.put(name, m.Version, fsys, st.sizeBytes)
	return fsys, nil
}

// --- writes ----------------------------------------------------------------

// ReplaceFromTar replaces a site's content, retaining it indefinitely.
func (s *Store) ReplaceFromTar(name string, r io.Reader) (wire.Site, error) {
	return s.ReplaceFromTarWithExpiry(name, r, nil)
}

// ReplaceFromTarWithExpiry implements Backend.
//
// The archive is buffered in memory so it can be validated before anything is
// written — a rejected upload must not be able to leave objects behind. That
// bound is the caller's upload cap, which the HTTP layer already enforces.
func (s *Store) ReplaceFromTarWithExpiry(name string, r io.Reader, expiry *time.Duration) (wire.Site, error) {
	if err := storage.ValidateName(name); err != nil {
		return wire.Site{}, err
	}
	ctx := context.Background()

	raw, err := io.ReadAll(r)
	if err != nil {
		return wire.Site{}, fmt.Errorf("s3: read upload: %w", err)
	}
	// Unpack purely to validate entry names and measure the site; the bytes
	// stored are the original archive.
	fsys, st, err := unpackTar(bytes.NewReader(raw), s.maxSiteBytes)
	if err != nil {
		return wire.Site{}, err
	}

	version, err := newVersion()
	if err != nil {
		return wire.Site{}, err
	}

	// 1. Write the content under a fresh version key. Nothing references it
	//    yet, so a failure here is invisible to readers.
	if _, err := s.client.PutObject(ctx, s.bucket, s.versionKey(name, version),
		bytes.NewReader(raw), int64(len(raw)),
		minio.PutObjectOptions{ContentType: "application/x-tar"}); err != nil {
		return wire.Site{}, fmt.Errorf("s3: put content: %w", err)
	}

	var expiresAt *time.Time
	if expiry != nil {
		t := time.Now().UTC().Add(*expiry)
		expiresAt = &t
	}
	m := siteMeta{
		Name:      name,
		Version:   version,
		FileCount: st.fileCount,
		SizeBytes: st.sizeBytes,
		UpdatedAt: time.Now().UTC(),
		ExpiresAt: expiresAt,
	}

	// Read the previous version before committing so its objects can be
	// collected afterwards.
	prev, hadPrev, _ := s.lookup(name)

	// 2. Commit: overwriting the metadata object is the atomic pointer flip
	//    that makes the new content live.
	if err := s.putMeta(ctx, m); err != nil {
		// Best-effort cleanup of the now-orphaned content object.
		_ = s.client.RemoveObject(ctx, s.bucket, s.versionKey(name, version), minio.RemoveObjectOptions{})
		return wire.Site{}, fmt.Errorf("s3: commit metadata: %w", err)
	}

	s.applyLocal(name, m)
	s.content.put(name, version, fsys, st.sizeBytes)

	// 3. Collect the superseded content object. Losing this race only leaks an
	//    unreferenced object, never live data.
	if hadPrev && prev.Version != version {
		_ = s.client.RemoveObject(ctx, s.bucket, s.versionKey(name, prev.Version), minio.RemoveObjectOptions{})
	}
	return m.toWire(), nil
}

func (s *Store) putMeta(ctx context.Context, m siteMeta) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, s.bucket, s.metaKey(m.Name),
		bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: "application/json"})
	return err
}

// applyLocal folds a just-written change into the cached map so this process
// sees its own writes immediately, without waiting out MetaTTL.
func (s *Store) applyLocal(name string, m siteMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta == nil {
		s.meta = make(map[string]siteMeta)
	}
	s.meta[name] = m
}

func (s *Store) forgetLocal(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.meta, name)
}

// Delete implements Backend. Metadata goes first: once it is gone the site no
// longer exists, and the content objects behind it are unreferenced.
func (s *Store) Delete(name string) error {
	if err := storage.ValidateName(name); err != nil {
		return err
	}
	ctx := context.Background()
	_, ok, err := s.lookup(name)
	if err != nil {
		return err
	}
	if !ok {
		return storage.ErrNotFound
	}
	if err := s.client.RemoveObject(ctx, s.bucket, s.metaKey(name), minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("s3: remove metadata: %w", err)
	}
	s.forgetLocal(name)
	s.content.dropAll(name)

	// Sweep every version, including any orphans from interrupted pushes.
	opts := minio.ListObjectsOptions{Prefix: s.sitePrefix(name), Recursive: true}
	for obj := range s.client.ListObjects(ctx, s.bucket, opts) {
		if obj.Err != nil {
			return fmt.Errorf("s3: list content: %w", obj.Err)
		}
		_ = s.client.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{})
	}
	return nil
}

// DeleteExpired implements Backend.
func (s *Store) DeleteExpired(now time.Time) ([]string, error) {
	// Force a read-through so a replica does not reap based on a stale map.
	snap, err := s.refresh(context.Background())
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(snap))
	for name := range snap {
		names = append(names, name)
	}
	sort.Strings(names)

	var deleted []string
	for _, name := range names {
		m := snap[name]
		if m.ExpiresAt == nil || m.ExpiresAt.After(now) {
			continue
		}
		if err := s.Delete(name); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return deleted, err
		}
		deleted = append(deleted, name)
	}
	return deleted, nil
}

// newVersion returns an opaque, monotonic-ish content version. The timestamp
// prefix keeps a site's objects sorted by age in a listing, which makes manual
// inspection and orphan cleanup easier to reason about.
func newVersion() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("s3: version: %w", err)
	}
	return fmt.Sprintf("%d-%s", time.Now().UTC().UnixNano(), hex.EncodeToString(b[:])), nil
}

// isNotFound reports whether err is an object-missing error from any layer.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, storage.ErrNotFound) {
		return true
	}
	resp := minio.ToErrorResponse(err)
	if resp.StatusCode == http.StatusNotFound {
		return true
	}
	switch resp.Code {
	case "NoSuchKey", "NoSuchBucket", "NotFound":
		return true
	}
	return false
}
