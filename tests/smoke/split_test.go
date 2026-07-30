//go:build smoke

package smoke

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSplitBrokerAndWebLifecycle proves the optional two-process topology
// without a shared reverse proxy: the CLI talks directly to the broker while
// humans fetch the returned URL from a separate read-only web process.
func TestSplitBrokerAndWebLifecycle(t *testing.T) {
	root := t.TempDir()
	brokerPort := freePort(t)
	webPort := freePort(t)
	brokerURL := fmt.Sprintf("http://127.0.0.1:%d", brokerPort)
	webURL := fmt.Sprintf("http://127.0.0.1:%d", webPort)

	brokerCfg := filepath.Join(root, "broker.yaml")
	webCfg := filepath.Join(root, "web.yaml")
	writeConfig(t, brokerCfg, fmt.Sprintf(
		"listen_addr: 127.0.0.1:%d\nbase_url: %s\napi_url: %s\npublic_url: %s\ntoken: %s\n",
		brokerPort, brokerURL, brokerURL, webURL, suiteToken,
	))
	writeConfig(t, webCfg, fmt.Sprintf(
		"listen_addr: 127.0.0.1:%d\npublic_url: %s\n",
		webPort, webURL,
	))

	web := startSplitDaemon(t, root, "web", webCfg, webURL+"/healthz", true, "CRATE_TOKEN=must-not-enable-web-api")
	sitesDir := filepath.Join(root, "data", "crate", "sites")
	if _, err := os.Stat(sitesDir); !os.IsNotExist(err) {
		t.Fatalf("web cold start wrote to data path: %v", err)
	}
	broker := startSplitDaemon(t, root, "broker", brokerCfg, brokerURL+"/api/status", false)

	dir := writeFiles(t, map[string]string{"index.html": "split topology"})
	out := runSplitCrate(t, brokerCfg, "push", dir, "split-lifecycle")
	if !strings.Contains(out, webURL+"/split-lifecycle/") {
		t.Fatalf("push output missing public web URL %q:\n%s", webURL, out)
	}
	if code, body := getBody(t, webURL+"/split-lifecycle/"); code != http.StatusOK || body != "split topology" {
		t.Fatalf("web GET: status=%d body=%q", code, body)
	}
	if code, _ := getBody(t, brokerURL+"/split-lifecycle/"); code != http.StatusNotFound {
		t.Fatalf("broker served public site: status=%d", code)
	}
	if code, _ := getBody(t, webURL+"/api/status"); code != http.StatusNotFound {
		t.Fatalf("web exposed broker API: status=%d", code)
	}

	implicitClientCfg := filepath.Join(root, "implicit-client.yaml")
	writeConfig(t, implicitClientCfg, fmt.Sprintf(
		"api_url: %s\ntoken: %s\n", brokerURL, suiteToken,
	))
	if out := runSplitCrate(t, implicitClientCfg, "status"); !strings.Contains(out, "public="+webURL) {
		t.Fatalf("status did not use broker-reported public URL:\n%s", out)
	}
	if out := runSplitCrate(t, implicitClientCfg, "open", "split-lifecycle"); !strings.Contains(out, webURL+"/split-lifecycle/") {
		t.Fatalf("open did not use broker-reported public URL:\n%s", out)
	}

	expiringDir := writeFiles(t, map[string]string{"index.html": "expires through broker"})
	runSplitCrate(t, brokerCfg, "push", expiringDir, "split-expiry", "--expires", "1s")
	broker.stop()
	if _, err := os.Stat(filepath.Join(sitesDir, "split-expiry")); err != nil {
		t.Fatalf("web read-time enforcement unexpectedly deleted site: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if code, _ := getBody(t, webURL+"/split-expiry/"); code != http.StatusNotFound {
		t.Fatalf("web served expired site while broker was down: status=%d", code)
	}

	broker = startSplitDaemon(t, root, "broker-restart", brokerCfg, brokerURL+"/api/status", false)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(sitesDir, "split-expiry")); os.IsNotExist(err) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(sitesDir, "split-expiry")); !os.IsNotExist(err) {
		t.Fatalf("broker startup did not collect expired site: %v", err)
	}

	runSplitCrate(t, brokerCfg, "rm", "split-lifecycle")
	if code, _ := getBody(t, webURL+"/split-lifecycle/"); code != http.StatusNotFound {
		t.Fatalf("web still served deleted site: status=%d", code)
	}

	broker.stop()
	web.stop()
}

// TestS3SplitBrokerAndWeb proves that independently cached web and broker
// processes converge through the same bucket without sharing a filesystem or
// reverse proxy.
func TestS3SplitBrokerAndWeb(t *testing.T) {
	s3 := ensureS3(t)
	root := t.TempDir()
	brokerPort := freePort(t)
	webPort := freePort(t)
	brokerURL := fmt.Sprintf("http://127.0.0.1:%d", brokerPort)
	webURL := fmt.Sprintf("http://127.0.0.1:%d", webPort)

	brokerCfg := filepath.Join(root, "broker.yaml")
	webCfg := filepath.Join(root, "web.yaml")
	writeConfig(t, brokerCfg, fmt.Sprintf(
		"listen_addr: 127.0.0.1:%d\nbase_url: %s\napi_url: %s\npublic_url: %s\ntoken: %s\n",
		brokerPort, brokerURL, brokerURL, webURL, suiteToken,
	))
	writeConfig(t, webCfg, fmt.Sprintf(
		"listen_addr: 127.0.0.1:%d\npublic_url: %s\n",
		webPort, webURL,
	))
	s3Vars := []string{
		"CRATE_STORAGE_BACKEND=s3",
		"CRATE_S3_ENDPOINT=http://" + s3.endpoint,
		"CRATE_S3_BUCKET=" + s3Bucket,
		"CRATE_S3_ACCESS_KEY=" + s3.accessKey,
		"CRATE_S3_SECRET_KEY=" + s3.secretKey,
	}
	web := startSplitDaemon(t, root, "s3-web", webCfg, webURL+"/healthz", true, s3Vars...)
	broker := startSplitDaemon(t, root, "s3-broker", brokerCfg, brokerURL+"/api/status", false, s3Vars...)

	dir := writeFiles(t, map[string]string{
		"index.html":  "split s3 topology v1",
		"only-v1.txt": "removed by replacement",
	})
	out := runSplitCrate(t, brokerCfg, "push", dir, "split-s3")
	if !strings.Contains(out, webURL+"/split-s3/") {
		t.Fatalf("push output missing public URL:\n%s", out)
	}
	waitForHTTPStatus(t, webURL+"/split-s3/", http.StatusOK, 15*time.Second)
	if code, body := getBody(t, webURL+"/split-s3/"); code != http.StatusOK || body != "split s3 topology v1" {
		t.Fatalf("S3 web GET: status=%d body=%q", code, body)
	}
	if code, _ := getBody(t, webURL+"/split-s3/only-v1.txt"); code != http.StatusOK {
		t.Fatalf("S3 web did not cache v1-only file: status=%d", code)
	}

	replacement := writeFiles(t, map[string]string{"index.html": "split s3 topology v2"})
	runSplitCrate(t, brokerCfg, "push", replacement, "split-s3")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		code, body := getBody(t, webURL+"/split-s3/")
		if code == http.StatusOK && body == "split s3 topology v2" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code, body := getBody(t, webURL+"/split-s3/"); code != http.StatusOK || body != "split s3 topology v2" {
		t.Fatalf("S3 web did not converge to v2: status=%d body=%q", code, body)
	}
	if code, _ := getBody(t, webURL+"/split-s3/only-v1.txt"); code != http.StatusNotFound {
		t.Fatalf("S3 web retained v1-only file after replacement: status=%d", code)
	}

	runSplitCrate(t, brokerCfg, "rm", "split-s3")
	waitForHTTPStatus(t, webURL+"/split-s3/", http.StatusNotFound, 15*time.Second)

	broker.stop()
	web.stop()
}

type splitDaemon struct {
	cmd      *exec.Cmd
	logFile  *os.File
	logPath  string
	stopOnce sync.Once
}

func startSplitDaemon(t *testing.T, root, name, cfgPath, readyURL string, roleFromEnv bool, extraEnv ...string) *splitDaemon {
	t.Helper()
	args := []string{"--config", cfgPath}
	if !roleFromEnv {
		args = append(args, "--role", "broker")
	}
	cmd := exec.Command(cratedBin, args...)
	env := append(splitBaseEnv(),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
		"XDG_DATA_HOME="+filepath.Join(root, "data"),
		"XDG_STATE_HOME="+filepath.Join(root, "state"),
	)
	if roleFromEnv {
		env = append(env, "CRATE_ROLE=web")
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	logPath := filepath.Join(root, name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start %s: %v", name, err)
	}
	d := &splitDaemon{cmd: cmd, logFile: logFile, logPath: logPath}
	t.Cleanup(d.stop)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(readyURL)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return d
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	body, _ := os.ReadFile(logPath)
	d.stop()
	t.Fatalf("%s did not become ready:\n%s", name, body)
	return nil
}

func (d *splitDaemon) stop() {
	d.stopOnce.Do(func() {
		if d.cmd != nil && d.cmd.Process != nil {
			_ = d.cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() {
				_, _ = d.cmd.Process.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = d.cmd.Process.Kill()
				<-done
			}
		}
		if d.logFile != nil {
			_ = d.logFile.Close()
		}
	})
}

func runSplitCrate(t *testing.T, cfgPath string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"--config", cfgPath}, args...)
	cmd := exec.Command(crateBin, cmdArgs...)
	cmd.Env = append(splitBaseEnv(), "BROWSER="+noopBrowser())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("crate %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitForHTTPStatus(t *testing.T, url string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	code, body := getBody(t, url)
	t.Fatalf("GET %s: got %d %q, want %d within %v", url, code, body, want, timeout)
}

func splitBaseEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "CRATE_") ||
			strings.HasPrefix(value, "XDG_CONFIG_HOME=") ||
			strings.HasPrefix(value, "XDG_DATA_HOME=") ||
			strings.HasPrefix(value, "XDG_STATE_HOME=") {
			continue
		}
		env = append(env, value)
	}
	return env
}
