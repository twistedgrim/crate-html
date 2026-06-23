//go:build smoke

package smoke

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigFlagOverridesPath confirms `crate --config <path>` reads the
// given file instead of the XDG default. The alternate config points at the
// same daemon so we can still issue a status call.
func TestConfigFlagOverridesPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "alt.yaml")
	body := fmt.Sprintf("port: %d\nlisten_addr: 127.0.0.1:%d\nbase_url: %s\ntoken: %s\n",
		suitePort, suitePort, baseURL, token)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Don't pass the suite's XDG_*_HOME — we want to prove --config alone
	// works. HOME stays set so adrg/xdg can fall back to per-user defaults
	// for the (unused) sites/log dirs.
	cmd := exec.Command(crateBin, "--config", cfgPath, "status")
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("crate --config %s status: %v\n%s", cfgPath, err, out)
	}
	if !strings.Contains(string(out), baseURL) {
		t.Errorf("output missing base URL %q: %s", baseURL, out)
	}
}

// TestConfigFlagCreatesParentDir covers the "fresh path under a non-existent
// directory" case — Save() should MkdirAll the parent.
func TestConfigFlagCreatesParentDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Two levels of non-existent parent dirs.
	cfgPath := filepath.Join(dir, "a", "b", "fresh.yaml")

	cmd := exec.Command(crateBin, "--config", cfgPath, "token")
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected fresh-path init to succeed: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(cfgPath); statErr != nil {
		t.Errorf("config file not created at %s: %v", cfgPath, statErr)
	}
	// Fresh config gets a random token, not the suite token — just check it's
	// non-empty.
	if strings.TrimSpace(string(out)) == "" {
		t.Errorf("expected a token in output: %q", out)
	}
}

// TestCrateTokenEnvOverride confirms CRATE_TOKEN is applied on top of the
// loaded config.
func TestCrateTokenEnvOverride(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(crateBin, "token")
	cmd.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+filepath.Join(xdgHome, "config"),
		"XDG_DATA_HOME="+filepath.Join(xdgHome, "data"),
		"XDG_STATE_HOME="+filepath.Join(xdgHome, "state"),
		"CRATE_TOKEN=overridden-by-env",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("crate token with env: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "overridden-by-env" {
		t.Errorf("CRATE_TOKEN override: got %q, want 'overridden-by-env'", strings.TrimSpace(string(out)))
	}
}
