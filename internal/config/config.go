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

	"github.com/adrg/xdg"
	"gopkg.in/yaml.v3"
)

const (
	appName        = "crate"
	configFileName = "config.yaml"
	defaultPort    = 7777
	tokenBytes     = 32
)

// Environment variable names that override the corresponding config fields.
// Used primarily for containerized deployments where the on-disk config
// can't carry deploy-specific values (notably ListenAddr, which must be
// 0.0.0.0 inside a container even when the user wants 127.0.0.1 outside it).
const (
	EnvPort       = "CRATE_PORT"
	EnvListenAddr = "CRATE_LISTEN_ADDR"
	EnvBaseURL    = "CRATE_BASE_URL"
	EnvToken      = "CRATE_TOKEN"
)

// Config is the on-disk shape of config.yaml.
type Config struct {
	// BaseURL is what the CLI dials. Defaults to http://localhost:<Port>.
	BaseURL string `yaml:"base_url"`
	// ListenAddr is the host:port crated binds. Defaults to 127.0.0.1:<Port>
	// so the daemon is unreachable from other hosts on the network.
	ListenAddr string `yaml:"listen_addr"`
	// Port is the default port used by ListenAddr and BaseURL.
	Port int `yaml:"port"`
	// Token is the shared bearer token for /api endpoints.
	Token string `yaml:"token"`
}

// Paths bundles the resolved on-disk locations used by both binaries.
type Paths struct {
	ConfigFile string
	SitesDir   string
	LogDir     string
}

// ResolvePaths returns the XDG-backed paths used by crate-html.
// Directories are created as needed.
func ResolvePaths() (Paths, error) {
	configDir := filepath.Join(xdg.ConfigHome, appName)
	dataDir := filepath.Join(xdg.DataHome, appName)
	stateDir := filepath.Join(xdg.StateHome, appName)

	sitesDir := filepath.Join(dataDir, "sites")
	logDir := filepath.Join(stateDir, "log")

	for _, d := range []string{configDir, sitesDir, logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Paths{}, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	return Paths{
		ConfigFile: filepath.Join(configDir, configFileName),
		SitesDir:   sitesDir,
		LogDir:     logDir,
	}, nil
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

// Save writes cfg to disk with 0600 permissions (it contains a secret).
func Save(paths Paths, cfg Config) error {
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
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
}

func applyEnv(cfg *Config) {
	if v := os.Getenv(EnvPort); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Port = n
		}
	}
	if v := os.Getenv(EnvListenAddr); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv(EnvBaseURL); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv(EnvToken); v != "" {
		cfg.Token = v
	}
}

func generateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
