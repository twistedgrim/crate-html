// Package config loads and persists crate-html configuration under the XDG
// base directories. On first run it generates a bearer token and writes a
// default config file.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/adrg/xdg"
	"gopkg.in/yaml.v3"
)

const (
	appName        = "crate"
	configFileName = "config.yaml"
	tokensFileName = "tokens.yaml"
	defaultPort    = 7777
	tokenBytes     = 32

	// defaultMaxUploadBytes caps a single PUT /api/sites body (100 MiB).
	defaultMaxUploadBytes = 100 << 20
)

// Environment variable names that override the corresponding config fields.
// Used primarily for containerized deployments where the on-disk config
// can't carry deploy-specific values (notably ListenAddr, which must be
// 0.0.0.0 inside a container even when the user wants 127.0.0.1 outside it).
//
// There is intentionally no CRATE_PORT — Port is only used to compose
// default ListenAddr and URL strings, and overriding it without also
// overriding those would silently no-op. Set CRATE_LISTEN_ADDR and the
// appropriate URL variables directly.
const (
	EnvListenAddr    = "CRATE_LISTEN_ADDR"
	EnvBaseURL       = "CRATE_BASE_URL"
	EnvAPIURL        = "CRATE_API_URL"
	EnvPublicURL     = "CRATE_PUBLIC_URL"
	EnvToken         = "CRATE_TOKEN"
	EnvIndexTemplate = "CRATE_INDEX_TEMPLATE"

	// Storage backend selection and S3 settings. These are env-first by
	// design: the whole point of the S3 backend is running where there is no
	// durable config file to read, so a deployment must be able to configure
	// storage entirely from the environment.
	EnvStorageBackend = "CRATE_STORAGE_BACKEND"
	EnvS3Endpoint     = "CRATE_S3_ENDPOINT"
	EnvS3Bucket       = "CRATE_S3_BUCKET"
	EnvS3Region       = "CRATE_S3_REGION"
	EnvS3AccessKey    = "CRATE_S3_ACCESS_KEY"
	EnvS3SecretKey    = "CRATE_S3_SECRET_KEY"
	EnvS3Prefix       = "CRATE_S3_PREFIX"
	EnvS3CacheBytes   = "CRATE_S3_CACHE_BYTES"
)

// Storage backend identifiers for Config.StorageBackend.
const (
	BackendLocal = "local"
	BackendS3    = "s3"
)

// Config is the on-disk shape of config.yaml.
type Config struct {
	// BaseURL is the legacy combined API/public URL. Existing deployments
	// continue to use it as the fallback for both APIURL and PublicURL.
	BaseURL string `yaml:"base_url"`
	// APIURL is what the CLI dials for broker operations. Empty falls back to
	// BaseURL so existing single-daemon configurations keep working.
	APIURL string `yaml:"api_url,omitempty"`
	// PublicURL is the human-facing origin used for published site links.
	// Empty falls back to BaseURL.
	PublicURL string `yaml:"public_url,omitempty"`
	// ListenAddr is the host:port crated binds. Defaults to 127.0.0.1:<Port>
	// so the daemon is unreachable from other hosts on the network.
	ListenAddr string `yaml:"listen_addr"`
	// Port is the default port used by ListenAddr and BaseURL.
	Port int `yaml:"port"`
	// Token is the root bearer token. It authenticates all /api endpoints
	// and is the only credential accepted by /api/tokens (token management).
	Token string `yaml:"token"`
	// MaxUploadBytes caps a single site upload body. Defaults to 100 MiB.
	MaxUploadBytes int64 `yaml:"max_upload_bytes"`
	// IndexTemplate is an optional path to an html/template file the daemon
	// renders for the root "/" and per-project "/<prefix>/" index pages
	// instead of the embedded default. Empty means use the built-in template.
	// Operator-supplied (config + filesystem access), never pushed over the
	// API.
	IndexTemplate string `yaml:"index_template"`
	// StorageBackend selects where sites live: "local" (the default) keeps
	// them in SitesDir, "s3" keeps them in an S3-compatible bucket.
	StorageBackend string `yaml:"storage_backend"`
	// S3 configures the object-storage backend. Ignored unless StorageBackend
	// is "s3".
	S3 S3Config `yaml:"s3"`
}

// EffectiveAPIURL returns the broker origin after applying the legacy
// base_url fallback. It also makes directly constructed Config values in
// tests and embedders behave like configs loaded through this package.
func (c Config) EffectiveAPIURL() string {
	if c.APIURL != "" {
		return c.APIURL
	}
	return c.BaseURL
}

// EffectivePublicURL returns the human-facing origin after applying the
// legacy base_url fallback.
func (c Config) EffectivePublicURL() string {
	if c.PublicURL != "" {
		return c.PublicURL
	}
	return c.BaseURL
}

// S3Config describes the bucket backing the "s3" storage backend.
type S3Config struct {
	// Endpoint is the S3 host, with or without a scheme. A bare host defaults
	// to https; use an http:// prefix for a plaintext dev endpoint.
	Endpoint string `yaml:"endpoint"`
	// Bucket must already exist — the daemon never creates it, so it cannot
	// mask a typo by silently making a new bucket.
	Bucket string `yaml:"bucket"`
	// Region is optional for most S3-compatible servers.
	Region string `yaml:"region"`
	// AccessKey and SecretKey are usually supplied by env rather than written
	// to disk. When both are empty the standard AWS credential chain is used,
	// which is what lets this run under an IAM role with no secrets at all.
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	// Prefix scopes every key, so one bucket can host multiple deployments.
	Prefix string `yaml:"prefix"`
	// CacheBytes budgets the in-memory site cache. 0 selects the built-in
	// default; a negative value disables caching entirely.
	CacheBytes int64 `yaml:"cache_bytes"`
}

// Paths bundles the resolved on-disk locations used by both binaries.
type Paths struct {
	ConfigFile string
	TokensFile string
	SitesDir   string
	LogDir     string
}

// resolvePaths returns the XDG-backed paths used by crate-html. Writers create
// the directories eagerly; read-only callers only derive their names so they
// can run against a read-only or not-yet-initialized data mount.
func resolvePaths(create bool) (Paths, error) {
	configDir := filepath.Join(xdg.ConfigHome, appName)
	dataDir := filepath.Join(xdg.DataHome, appName)
	stateDir := filepath.Join(xdg.StateHome, appName)

	sitesDir := filepath.Join(dataDir, "sites")
	logDir := filepath.Join(stateDir, "log")

	if create {
		for _, d := range []string{configDir, sitesDir, logDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return Paths{}, fmt.Errorf("mkdir %s: %w", d, err)
			}
		}
	}

	return Paths{
		ConfigFile: filepath.Join(configDir, configFileName),
		TokensFile: filepath.Join(configDir, tokensFileName),
		SitesDir:   sitesDir,
		LogDir:     logDir,
	}, nil
}

// ResolvePaths returns the XDG-backed paths used by crate-html and creates the
// directories needed by the broker and CLI.
func ResolvePaths() (Paths, error) {
	return resolvePaths(true)
}

// ResolvePathsReadOnly derives the XDG-backed paths without creating anything.
// The public web role uses this so its config, data, and state mounts can all
// be genuinely read-only.
func ResolvePathsReadOnly() (Paths, error) {
	return resolvePaths(false)
}

// LoadOrInit reads the config file, creating a default one (with a freshly
// generated token) if it does not yet exist. Environment variables override
// fields after defaults are applied but the saved file is not rewritten —
// env-var overrides stay process-local.
func LoadOrInit(paths Paths) (Config, error) {
	data, err := os.ReadFile(paths.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		cfg, gerr := defaultConfig()
		if gerr != nil {
			return Config{}, gerr
		}
		if werr := Save(paths, cfg); werr != nil {
			return Config{}, werr
		}
		applyEnv(&cfg)
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)
	applyEnv(&cfg)
	return cfg, nil
}

// LoadReadOnly loads configuration without creating a config file or root
// token. It is intended for the public web role, which only needs storage and
// presentation settings and should not require access to broker credentials.
//
// If a config file is supplied it may contain a token for compatibility with
// existing deployments, but the returned config always clears it.
func LoadReadOnly(paths Paths) (Config, error) {
	data, err := os.ReadFile(paths.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		var cfg Config
		applyDefaults(&cfg)
		applyEnv(&cfg)
		cfg.Token = ""
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)
	applyEnv(&cfg)
	cfg.Token = ""
	return cfg, nil
}

// Save writes cfg to disk with 0600 permissions (it contains a secret).
// The parent directory is created if missing so callers can pass an explicit
// --config path under a fresh directory without a separate mkdir step.
func Save(paths Paths, cfg Config) error {
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	if err := os.WriteFile(paths.ConfigFile, out, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func defaultConfig() (Config, error) {
	tok, err := generateToken()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Port:  defaultPort,
		Token: tok,
	}
	applyDefaults(&cfg)
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = fmt.Sprintf("http://localhost:%d", cfg.Port)
	}
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = defaultMaxUploadBytes
	}
	if cfg.StorageBackend == "" {
		cfg.StorageBackend = BackendLocal
	}
}

// ValidateStorage checks the storage settings are coherent before anything
// tries to use them, so a misconfiguration surfaces at startup with a clear
// message instead of on the first push.
func (c Config) ValidateStorage() error {
	switch c.StorageBackend {
	case BackendLocal:
		return nil
	case BackendS3:
		var missing []string
		if c.S3.Endpoint == "" {
			missing = append(missing, EnvS3Endpoint+" (s3.endpoint)")
		}
		if c.S3.Bucket == "" {
			missing = append(missing, EnvS3Bucket+" (s3.bucket)")
		}
		if len(missing) > 0 {
			return fmt.Errorf("storage_backend %q requires %s", BackendS3, strings.Join(missing, ", "))
		}
		return nil
	default:
		return fmt.Errorf("unknown storage_backend %q (want %q or %q)", c.StorageBackend, BackendLocal, BackendS3)
	}
}

func applyEnv(cfg *Config) {
	if v := os.Getenv(EnvListenAddr); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv(EnvBaseURL); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv(EnvAPIURL); v != "" {
		cfg.APIURL = v
	}
	if v := os.Getenv(EnvPublicURL); v != "" {
		cfg.PublicURL = v
	}
	if v := os.Getenv(EnvToken); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv(EnvIndexTemplate); v != "" {
		cfg.IndexTemplate = v
	}
	if v := os.Getenv(EnvStorageBackend); v != "" {
		cfg.StorageBackend = v
	}
	if v := os.Getenv(EnvS3Endpoint); v != "" {
		cfg.S3.Endpoint = v
	}
	if v := os.Getenv(EnvS3Bucket); v != "" {
		cfg.S3.Bucket = v
	}
	if v := os.Getenv(EnvS3Region); v != "" {
		cfg.S3.Region = v
	}
	if v := os.Getenv(EnvS3AccessKey); v != "" {
		cfg.S3.AccessKey = v
	}
	if v := os.Getenv(EnvS3SecretKey); v != "" {
		cfg.S3.SecretKey = v
	}
	if v := os.Getenv(EnvS3Prefix); v != "" {
		cfg.S3.Prefix = v
	}
	if v := os.Getenv(EnvS3CacheBytes); v != "" {
		// Ignore an unparseable value rather than failing startup: the
		// built-in default is a safe fallback and this is a tuning knob.
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.S3.CacheBytes = n
		}
	}
}

func generateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
