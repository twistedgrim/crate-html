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
		})
	}
}

func TestApplyEnv(t *testing.T) {
	// Save + restore env so the test doesn't leak.
	for _, k := range []string{EnvListenAddr, EnvBaseURL, EnvToken} {
		t.Setenv(k, "")
	}

	cfg := Config{ListenAddr: "127.0.0.1:7777", BaseURL: "http://localhost:7777", Token: "from-file"}
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
	if cfg.Token != "env-token" {
		t.Errorf("Token: got %q", cfg.Token)
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
