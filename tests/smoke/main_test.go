//go:build smoke

// Package smoke holds the end-to-end integration tests for crate-html.
//
// Build tag `smoke` keeps these out of `go test ./...` so unit tests stay
// fast. Run via `task smoke` (which is `go test -tags smoke -count=1
// ./tests/smoke/...`) or directly via the same command.
//
// TestMain owns the lifecycle:
//   - builds fresh crate + crated binaries into a tempdir
//   - writes an isolated XDG home with a known config (port 17777, fixed token)
//   - starts crated, waits for it to be healthy
//   - runs the suite
//   - tears down the daemon and removes the tempdir
//
// Tests reach the daemon via the package-level baseURL and exec the CLI via
// runCrate(t, args...). Each test should use unique site names so parallel
// runs don't collide.
package smoke

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

var (
	// crateBin is the absolute path to the built `crate` CLI binary.
	crateBin string
	// cratedBin is the absolute path to the built `crated` daemon binary.
	cratedBin string
	// baseURL is the daemon's base URL (set per TestMain).
	baseURL string
	// token is the fixed bearer token written into the suite's config.yaml.
	token string
	// xdgHome is the suite's isolated $HOME-equivalent for XDG dirs.
	xdgHome string
	// daemon is the running crated process.
	daemon *exec.Cmd
)

const (
	suitePort  = 17777
	suiteToken = "smoke-suite-token"
)

func TestMain(m *testing.M) {
	code := func() int {
		if err := setup(); err != nil {
			fmt.Fprintln(os.Stderr, "smoke setup:", err)
			return 1
		}
		defer teardown()
		return m.Run()
	}()
	os.Exit(code)
}

func setup() error {
	// Suppress real browser pops from any `crate push --open` invocation
	// during the suite. crate's openBrowser honors BROWSER, so pointing it
	// at /usr/bin/true makes it a no-op. Set before MkdirTemp so children
	// inherit it via os.Environ() in the helpers.
	if err := os.Setenv("BROWSER", noopBrowser()); err != nil {
		return fmt.Errorf("set BROWSER: %w", err)
	}

	tmp, err := os.MkdirTemp("", "crate-smoke-*")
	if err != nil {
		return fmt.Errorf("tempdir: %w", err)
	}
	xdgHome = tmp

	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	crateBin = filepath.Join(binDir, "crate")
	cratedBin = filepath.Join(binDir, "crated")

	if err := goBuild(crateBin, "./cmd/crate"); err != nil {
		return fmt.Errorf("build crate: %w", err)
	}
	if err := goBuild(cratedBin, "./cmd/crated"); err != nil {
		return fmt.Errorf("build crated: %w", err)
	}

	// Seed the isolated XDG config with a known port and token so we don't
	// depend on the on-disk config of the developer running the suite.
	configDir := filepath.Join(tmp, "config", "crate")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	cfgPath := filepath.Join(configDir, "config.yaml")
	cfgBody := fmt.Sprintf("port: %d\nlisten_addr: 127.0.0.1:%d\nbase_url: http://localhost:%d\ntoken: %s\n",
		suitePort, suitePort, suitePort, suiteToken)
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		return err
	}
	baseURL = fmt.Sprintf("http://localhost:%d", suitePort)
	token = suiteToken

	// Start the daemon with the isolated XDG home so it picks up the config above.
	logPath := filepath.Join(tmp, "crated.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	daemon = exec.Command(cratedBin)
	daemon.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+filepath.Join(tmp, "config"),
		"XDG_DATA_HOME="+filepath.Join(tmp, "data"),
		"XDG_STATE_HOME="+filepath.Join(tmp, "state"),
	)
	daemon.Stdout = logFile
	daemon.Stderr = logFile
	if err := daemon.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Wait up to 5s for the daemon to respond to /api/status.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/api/status")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become healthy in time; see %s", logPath)
}

func teardown() {
	if daemon != nil && daemon.Process != nil {
		_ = daemon.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() {
			_, _ = daemon.Process.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = daemon.Process.Kill()
			<-done
		}
	}
	if xdgHome != "" {
		_ = os.RemoveAll(xdgHome)
	}
}

func goBuild(out, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	// Run from the repo root so module-relative pkg paths resolve.
	cmd.Dir = repoRoot()
	return cmd.Run()
}

// repoRoot resolves to the repository root from this test file's location.
func repoRoot() string {
	// tests/smoke/main_test.go → ../../
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// noopBrowser returns a do-nothing command to use as BROWSER during tests.
// Falls through to /usr/bin/true on Unix, where it's universally present.
func noopBrowser() string {
	if _, err := os.Stat("/usr/bin/true"); err == nil {
		return "/usr/bin/true"
	}
	// Fallback for any platform without that path: PowerShell exit-0 etc.
	// Tests will then see openBrowser fail-fast with an exec error instead
	// of popping a real browser.
	return "true"
}
