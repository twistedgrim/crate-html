package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyDefaults(t *testing.T) {
	cases := []struct {
		name     string
		in       Config
		wantPort int
		wantList string
		wantBase string
	}{
		{
			name:     "all zero",
			in:       Config{},
			wantPort: defaultPort,
			wantList: "127.0.0.1:7777",
			wantBase: "http://localhost:7777",
		},
		{
			name:     "port set, other blank",
			in:       Config{Port: 9000},
			wantPort: 9000,
			wantList: "127.0.0.1:9000",
			wantBase: "http://localhost:9000",
		},
		{
			name: "all set leaves existing",
			in: Config{
				Port:       9000,
				ListenAddr: "0.0.0.0:9000",
				BaseURL:    "http://example:9000",
			},
			wantPort: 9000,
			wantList: "0.0.0.0:9000",
			wantBase: "http://example:9000",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cfg := c.in
			applyDefaults(&cfg)
			if cfg.Port != c.wantPort {
				t.Errorf("Port: got %d, want %d", cfg.Port, c.wantPort)
			}
			if cfg.ListenAddr != c.wantList {
				t.Errorf("ListenAddr: got %q, want %q", cfg.ListenAddr, c.wantList)
			}
			if cfg.BaseURL != c.wantBase {
				t.Errorf("BaseURL: got %q, want %q", cfg.BaseURL, c.wantBase)
			}
			if cfg.EffectiveAPIURL() != c.wantBase {
				t.Errorf("EffectiveAPIURL: got %q, want %q", cfg.EffectiveAPIURL(), c.wantBase)
			}
			if cfg.EffectivePublicURL() != c.wantBase {
				t.Errorf("EffectivePublicURL: got %q, want %q", cfg.EffectivePublicURL(), c.wantBase)
			}
		})
	}
}

func TestApplyEnv(t *testing.T) {
	// Save + restore env so the test doesn't leak.
	for _, k := range []string{EnvListenAddr, EnvBaseURL, EnvAPIURL, EnvPublicURL, EnvToken} {
		t.Setenv(k, "")
	}

	cfg := Config{ListenAddr: "127.0.0.1:7777", BaseURL: "http://localhost:7777", Token: "from-file"}
	applyDefaults(&cfg)
	applyEnv(&cfg)
	if cfg.ListenAddr != "127.0.0.1:7777" || cfg.Token != "from-file" {
		t.Errorf("no env vars set: config should be unchanged; got %+v", cfg)
	}

	t.Setenv(EnvListenAddr, "0.0.0.0:7777")
	t.Setenv(EnvBaseURL, "http://override:7777")
	t.Setenv(EnvToken, "env-token")
	applyEnv(&cfg)
	if cfg.ListenAddr != "0.0.0.0:7777" {
		t.Errorf("ListenAddr: got %q", cfg.ListenAddr)
	}
	if cfg.BaseURL != "http://override:7777" {
		t.Errorf("BaseURL: got %q", cfg.BaseURL)
	}
	if cfg.EffectiveAPIURL() != "http://override:7777" || cfg.EffectivePublicURL() != "http://override:7777" {
		t.Errorf("legacy base URL should supply both fallbacks; got api=%q public=%q", cfg.EffectiveAPIURL(), cfg.EffectivePublicURL())
	}
	if cfg.Token != "env-token" {
		t.Errorf("Token: got %q", cfg.Token)
	}

	t.Setenv(EnvAPIURL, "http://broker:7777")
	t.Setenv(EnvPublicURL, "https://crate.example")
	applyEnv(&cfg)
	if cfg.APIURL != "http://broker:7777" {
		t.Errorf("APIURL: got %q", cfg.APIURL)
	}
	if cfg.PublicURL != "https://crate.example" {
		t.Errorf("PublicURL: got %q", cfg.PublicURL)
	}
}

func TestLegacyBaseEnvDoesNotOverwriteExplicitDestinations(t *testing.T) {
	t.Setenv(EnvBaseURL, "http://legacy-env:7777")
	t.Setenv(EnvAPIURL, "")
	t.Setenv(EnvPublicURL, "")
	cfg := Config{
		BaseURL:   "http://legacy-file:7777",
		APIURL:    "http://broker:7777",
		PublicURL: "https://crate.example",
	}
	applyDefaults(&cfg)
	applyEnv(&cfg)
	if got := cfg.EffectiveAPIURL(); got != "http://broker:7777" {
		t.Fatalf("EffectiveAPIURL = %q", got)
	}
	if got := cfg.EffectivePublicURL(); got != "https://crate.example" {
		t.Fatalf("EffectivePublicURL = %q", got)
	}
}

func TestExplicitURLsOverrideLegacyFallback(t *testing.T) {
	cfg := Config{
		BaseURL:   "http://legacy:7777",
		APIURL:    "http://broker:7777",
		PublicURL: "https://crate.example",
	}
	applyDefaults(&cfg)
	if got := cfg.EffectiveAPIURL(); got != "http://broker:7777" {
		t.Fatalf("EffectiveAPIURL = %q", got)
	}
	if got := cfg.EffectivePublicURL(); got != "https://crate.example" {
		t.Fatalf("EffectivePublicURL = %q", got)
	}
}

// TestNoCratePortEnvVar pins the design choice: CRATE_PORT is not a
// recognized override. If someone re-adds it, this test fails loudly so the
// decision in docs/design.md can be revisited.
func TestNoCratePortEnvVar(t *testing.T) {
	t.Setenv("CRATE_PORT", "9999")
	cfg := Config{Port: defaultPort}
	applyDefaults(&cfg)
	applyEnv(&cfg)
	if cfg.Port != defaultPort {
		t.Errorf("CRATE_PORT should be a no-op; got Port=%d", cfg.Port)
	}
}

func TestLoadOrInitFreshFile(t *testing.T) {
	tmp := t.TempDir()
	paths := Paths{
		ConfigFile: filepath.Join(tmp, "config.yaml"),
		SitesDir:   filepath.Join(tmp, "sites"),
		LogDir:     filepath.Join(tmp, "log"),
	}
	if err := os.MkdirAll(paths.SitesDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOrInit(paths)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	if cfg.Token == "" || len(cfg.Token) != 64 {
		t.Errorf("expected 64-char hex token, got %q (len=%d)", cfg.Token, len(cfg.Token))
	}
	if cfg.Port != defaultPort {
		t.Errorf("Port: got %d, want %d", cfg.Port, defaultPort)
	}
	if cfg.ListenAddr == "" || cfg.BaseURL == "" {
		t.Errorf("defaults not applied: %+v", cfg)
	}

	// File was written.
	if _, err := os.Stat(paths.ConfigFile); err != nil {
		t.Errorf("config file not created: %v", err)
	}
}

func TestLoadOrInitExistingFile(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	body := `port: 8765
listen_addr: 0.0.0.0:8765
base_url: http://example:8765
token: known-token
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOrInit(Paths{ConfigFile: cfgPath})
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	if cfg.Port != 8765 || cfg.Token != "known-token" || cfg.ListenAddr != "0.0.0.0:8765" {
		t.Errorf("file values not preserved: %+v", cfg)
	}
}

func TestLoadReadOnlyDoesNotCreateConfigOrKeepToken(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "missing", "config.yaml")
	cfg, err := LoadReadOnly(Paths{ConfigFile: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "" {
		t.Fatalf("read-only config should not contain a token: %q", cfg.Token)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("read-only load created config file: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "base_url: http://legacy:7777\ntoken: broker-secret\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadReadOnly(Paths{ConfigFile: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "" {
		t.Fatal("read-only load retained the broker token")
	}
}

func TestLoadOrInitEnvOverrideDoesNotRewriteFile(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	body := "port: 7777\nlisten_addr: 127.0.0.1:7777\nbase_url: http://localhost:7777\ntoken: file-token\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvToken, "env-token")

	cfg, err := LoadOrInit(Paths{ConfigFile: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "env-token" {
		t.Errorf("env override not applied: got %q", cfg.Token)
	}

	// The on-disk file should still hold the original token.
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "token: file-token") {
		t.Errorf("env override should NOT have rewritten the file:\n%s", got)
	}
}

func TestSaveCreatesParentDir(t *testing.T) {
	tmp := t.TempDir()
	deeper := filepath.Join(tmp, "a", "b", "c", "config.yaml")
	err := Save(Paths{ConfigFile: deeper}, Config{Port: 7777, Token: "x"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(deeper); err != nil {
		t.Errorf("file not written: %v", err)
	}
	info, _ := os.Stat(deeper)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm: got %v, want 0600", info.Mode().Perm())
	}
}

func TestResolvePathsHonorsXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	// adrg/xdg caches at init time, so this test only verifies the package's
	// path-derivation logic rather than the env round-trip. It still proves
	// the relative shape of the returned Paths.
	paths, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(paths.ConfigFile, "/crate/config.yaml") {
		t.Errorf("ConfigFile suffix: %s", paths.ConfigFile)
	}
	if !strings.HasSuffix(paths.SitesDir, "/crate/sites") {
		t.Errorf("SitesDir suffix: %s", paths.SitesDir)
	}
	if !strings.HasSuffix(paths.LogDir, "/crate/log") {
		t.Errorf("LogDir suffix: %s", paths.LogDir)
	}
}

func TestResolvePathsReadOnlyMatchesWritablePaths(t *testing.T) {
	readOnly, err := ResolvePathsReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	writable, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if readOnly != writable {
		t.Fatalf("read-only paths differ: got %+v, want %+v", readOnly, writable)
	}
}

func TestStorageBackendDefaultsToLocal(t *testing.T) {
	var cfg Config
	applyDefaults(&cfg)
	if cfg.StorageBackend != BackendLocal {
		t.Errorf("StorageBackend = %q, want %q", cfg.StorageBackend, BackendLocal)
	}
	if err := cfg.ValidateStorage(); err != nil {
		t.Errorf("default config should validate, got %v", err)
	}
}

func TestValidateStorage(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"local needs nothing", Config{StorageBackend: BackendLocal}, false},
		{"s3 with endpoint and bucket", Config{
			StorageBackend: BackendS3,
			S3:             S3Config{Endpoint: "http://localhost:9000", Bucket: "crate"},
		}, false},
		{"s3 missing bucket", Config{
			StorageBackend: BackendS3,
			S3:             S3Config{Endpoint: "http://localhost:9000"},
		}, true},
		{"s3 missing endpoint", Config{
			StorageBackend: BackendS3,
			S3:             S3Config{Bucket: "crate"},
		}, true},
		{"unknown backend", Config{StorageBackend: "gcs"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.ValidateStorage()
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateStorage() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// The S3 backend is meant to run where there is no durable config file, so
// every S3 setting must be reachable from the environment alone.
func TestApplyEnvS3(t *testing.T) {
	t.Setenv(EnvStorageBackend, "s3")
	t.Setenv(EnvS3Endpoint, "http://rustfs:9000")
	t.Setenv(EnvS3Bucket, "sites")
	t.Setenv(EnvS3Region, "us-east-1")
	t.Setenv(EnvS3AccessKey, "ak")
	t.Setenv(EnvS3SecretKey, "sk")
	t.Setenv(EnvS3Prefix, "crate")
	t.Setenv(EnvS3CacheBytes, "1234")

	var cfg Config
	applyEnv(&cfg)

	if cfg.StorageBackend != BackendS3 {
		t.Errorf("StorageBackend = %q", cfg.StorageBackend)
	}
	if cfg.S3.Endpoint != "http://rustfs:9000" || cfg.S3.Bucket != "sites" {
		t.Errorf("endpoint/bucket = %q/%q", cfg.S3.Endpoint, cfg.S3.Bucket)
	}
	if cfg.S3.Region != "us-east-1" || cfg.S3.Prefix != "crate" {
		t.Errorf("region/prefix = %q/%q", cfg.S3.Region, cfg.S3.Prefix)
	}
	if cfg.S3.AccessKey != "ak" || cfg.S3.SecretKey != "sk" {
		t.Errorf("credentials not applied")
	}
	if cfg.S3.CacheBytes != 1234 {
		t.Errorf("CacheBytes = %d, want 1234", cfg.S3.CacheBytes)
	}
	if err := cfg.ValidateStorage(); err != nil {
		t.Errorf("env-only config should validate, got %v", err)
	}
}

// An unparseable cache budget is a tuning knob, not a reason to refuse to boot.
func TestApplyEnvS3CacheBytesIgnoresGarbage(t *testing.T) {
	t.Setenv(EnvS3CacheBytes, "not-a-number")
	cfg := Config{S3: S3Config{CacheBytes: 99}}
	applyEnv(&cfg)
	if cfg.S3.CacheBytes != 99 {
		t.Errorf("CacheBytes = %d, want the previous value 99", cfg.S3.CacheBytes)
	}
}
