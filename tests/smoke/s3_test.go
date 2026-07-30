//go:build smoke

package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Twistedgrim/crate-html/internal/s3store"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// The S3 end-to-end suite runs the whole daemon against a real S3-compatible
// server, because the object-storage backend's interesting properties — atomic
// pointer-flip publishes, metadata as the existence record, cache versioning —
// only exist in terms of real bucket behavior. A mocked client would test the
// mock.
//
// It uses rustfs, started on demand via Docker. Set CRATE_TEST_S3_ENDPOINT to
// point at an already-running server instead (any S3-compatible one), which is
// what CI does when it provides the service itself. With neither, the suite
// skips rather than fails: the S3 backend is optional and contributors without
// Docker should still get a green run.
const (
	rustfsImage     = "rustfs/rustfs@sha256:84ce557a0245a06a9aae5516f55ee0f007fca78d41df356f419306fdc0cb168c"
	rustfsContainer = "crate-smoke-rustfs"
	rustfsPort      = 19100
	s3AccessKey     = "rustfsadmin"
	s3SecretKey     = "rustfsadmin"
	s3Bucket        = "crate-smoke"
	s3DaemonPort    = 17778
)

// s3Env describes a reachable S3 endpoint for the suite.
type s3Env struct {
	endpoint  string // host:port, no scheme
	accessKey string
	secretKey string
}

// ensureS3 returns a usable endpoint, starting rustfs if needed, or skips.
func ensureS3(t *testing.T) s3Env {
	t.Helper()

	if ep := os.Getenv("CRATE_TEST_S3_ENDPOINT"); ep != "" {
		env := s3Env{
			endpoint:  strings.TrimPrefix(strings.TrimPrefix(ep, "http://"), "https://"),
			accessKey: envOr("CRATE_TEST_S3_ACCESS_KEY", s3AccessKey),
			secretKey: envOr("CRATE_TEST_S3_SECRET_KEY", s3SecretKey),
		}
		waitForS3(t, env)
		return env
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no CRATE_TEST_S3_ENDPOINT and docker not installed; skipping S3 backend tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no CRATE_TEST_S3_ENDPOINT and docker daemon not running; skipping S3 backend tests")
	}

	// Remove a container left behind by an interrupted run before starting.
	_ = exec.Command("docker", "rm", "-f", rustfsContainer).Run()

	start := exec.Command("docker", "run", "-d", "--rm",
		"--name", rustfsContainer,
		"-p", fmt.Sprintf("%d:9000", rustfsPort),
		"-e", "RUSTFS_ACCESS_KEY="+s3AccessKey,
		"-e", "RUSTFS_SECRET_KEY="+s3SecretKey,
		rustfsImage,
	)
	if out, err := start.CombinedOutput(); err != nil {
		t.Skipf("could not start %s (%v): %s", rustfsImage, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", rustfsContainer).Run() })

	env := s3Env{
		endpoint:  fmt.Sprintf("127.0.0.1:%d", rustfsPort),
		accessKey: s3AccessKey,
		secretKey: s3SecretKey,
	}
	waitForS3(t, env)
	return env
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// waitForS3 blocks until the endpoint answers, then ensures the bucket exists.
func waitForS3(t *testing.T, env s3Env) {
	t.Helper()
	client := s3Client(t, env)
	ctx := context.Background()

	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := client.ListBuckets(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("S3 endpoint %s did not become ready in time", env.endpoint)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Start from an empty bucket so state cannot leak between runs.
	if err := emptyBucket(ctx, client, s3Bucket); err != nil {
		t.Fatalf("reset bucket: %v", err)
	}
	exists, err := client.BucketExists(ctx, s3Bucket)
	if err != nil {
		t.Fatalf("bucket exists: %v", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, s3Bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatalf("make bucket: %v", err)
		}
	}
}

func s3Client(t *testing.T, env s3Env) *minio.Client {
	t.Helper()
	client, err := minio.New(env.endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(env.accessKey, env.secretKey, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("s3 client: %v", err)
	}
	return client
}

func emptyBucket(ctx context.Context, client *minio.Client, bucket string) error {
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil || !exists {
		return nil //nolint:nilerr // a missing bucket is created by the caller
	}
	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if err := client.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// startS3Daemon boots a crated backed by the bucket. It returns the daemon's
// URL and a stop function; stop is also registered as cleanup, and is safe to
// call early when a test needs the port back before it ends.
func startS3Daemon(t *testing.T, env s3Env) (string, func()) {
	t.Helper()

	home := t.TempDir()
	configDir := filepath.Join(home, "config", "crate")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("port: %d\nlisten_addr: 127.0.0.1:%d\nbase_url: http://localhost:%d\ntoken: %s\n",
		s3DaemonPort, s3DaemonPort, s3DaemonPort, suiteToken)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(home, "crated-s3.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(cratedBin)
	cmd.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
		"CRATE_STORAGE_BACKEND=s3",
		"CRATE_S3_ENDPOINT=http://"+env.endpoint,
		"CRATE_S3_BUCKET="+s3Bucket,
		"CRATE_S3_ACCESS_KEY="+env.accessKey,
		"CRATE_S3_SECRET_KEY="+env.secretKey,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start s3 daemon: %v", err)
	}
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() { _, _ = cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			logFile.Close()
			// Give the OS a moment to release the listener so a replacement
			// daemon can bind the same port.
			time.Sleep(200 * time.Millisecond)
		})
	}
	t.Cleanup(stop)

	url := fmt.Sprintf("http://localhost:%d", s3DaemonPort)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/api/status")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return url, stop
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	body, _ := os.ReadFile(logPath)
	stop()
	t.Fatalf("s3-backed daemon did not become healthy:\n%s", body)
	return "", stop
}

// pushTo uploads a directory through the real CLI, pointed at the given daemon.
func pushTo(t *testing.T, url, dir, name string, extra ...string) string {
	t.Helper()
	args := append([]string{"push", dir, name}, extra...)
	cmd := exec.Command(crateBin, args...)
	cmd.Env = append(os.Environ(),
		"CRATE_BASE_URL="+url,
		"CRATE_TOKEN="+suiteToken,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("push %s: %v\n%s", name, err, out)
	}
	return string(out)
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestS3BackendLifecycle is the core end-to-end path: push, serve, replace,
// list, and delete, all with sites living only in the bucket.
func TestS3BackendLifecycle(t *testing.T) {
	env := ensureS3(t)
	url, _ := startS3Daemon(t, env)

	dir := writeFiles(t, map[string]string{
		"index.html":      "<h1>s3 home</h1>",
		"docs/guide.html": "<h1>s3 guide</h1>",
	})
	out := pushTo(t, url, dir, "s3-site")
	if !strings.Contains(out, url+"/s3-site/") {
		t.Errorf("push output missing URL: %s", out)
	}

	if code, body := getBody(t, url+"/s3-site/"); code != http.StatusOK || !strings.Contains(body, "s3 home") {
		t.Errorf("GET /s3-site/ = %d %q", code, body)
	}
	// A nested path proves the archive was unpacked into a real tree, not just
	// stored and echoed back.
	if code, body := getBody(t, url+"/s3-site/docs/guide.html"); code != http.StatusOK || !strings.Contains(body, "s3 guide") {
		t.Errorf("GET nested = %d %q", code, body)
	}

	// Replacing must flip atomically to the new content.
	dir2 := writeFiles(t, map[string]string{"index.html": "<h1>s3 replaced</h1>"})
	pushTo(t, url, dir2, "s3-site")
	if code, body := getBody(t, url+"/s3-site/"); code != http.StatusOK || !strings.Contains(body, "s3 replaced") {
		t.Errorf("after replace = %d %q", code, body)
	}
	// The old file is gone: a replace is a replace, not a merge.
	if code, _ := getBody(t, url+"/s3-site/docs/guide.html"); code != http.StatusNotFound {
		t.Errorf("stale nested file = %d, want 404", code)
	}

	if code, body := getBody(t, url+"/"); code != http.StatusOK || !strings.Contains(body, "s3-site") {
		t.Errorf("index = %d, missing site", code)
	}

	// Delete through the CLI, then confirm it is really gone.
	rm := exec.Command(crateBin, "rm", "s3-site")
	rm.Env = append(os.Environ(), "CRATE_BASE_URL="+url, "CRATE_TOKEN="+suiteToken)
	if out, err := rm.CombinedOutput(); err != nil {
		t.Fatalf("rm: %v\n%s", err, out)
	}
	if code, _ := getBody(t, url+"/s3-site/"); code != http.StatusNotFound {
		t.Errorf("after delete = %d, want 404", code)
	}
}

// TestS3BackendSurvivesRestart is the whole point of the feature: the daemon
// holds no durable local state, so a fresh process with an empty XDG home must
// still serve everything in the bucket.
func TestS3BackendSurvivesRestart(t *testing.T) {
	env := ensureS3(t)

	url, stop := startS3Daemon(t, env)
	dir := writeFiles(t, map[string]string{"index.html": "<h1>persisted</h1>"})
	pushTo(t, url, dir, "s3-persist")
	if code, _ := getBody(t, url+"/s3-persist/"); code != http.StatusOK {
		t.Fatalf("site not served before restart: %d", code)
	}

	// Kill the daemon and start a replacement with a brand-new XDG home, so
	// nothing but the bucket carries the site across.
	stop()
	url2, _ := startS3Daemon(t, env)

	code, body := getBody(t, url2+"/s3-persist/")
	if code != http.StatusOK || !strings.Contains(body, "persisted") {
		t.Errorf("after restart = %d %q, want the site restored from the bucket", code, body)
	}
	if code, body := getBody(t, url2+"/"); code != http.StatusOK || !strings.Contains(body, "s3-persist") {
		t.Errorf("index after restart = %d, site missing from listing", code)
	}
}

// TestS3BackendRejectsTraversal confirms the shared tar safety rule is applied
// on the object-storage path too, not just on local disk.
func TestS3BackendRejectsTraversal(t *testing.T) {
	env := ensureS3(t)
	url, _ := startS3Daemon(t, env)

	tarball := tarFromMap(t, map[string]string{"../escape.html": "pwned"})
	req, err := http.NewRequest(http.MethodPut, url+"/api/sites/s3-traversal", bytes.NewReader(tarball))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+suiteToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("traversal upload = %d, want 400", resp.StatusCode)
	}
	// The rejected upload must not have left a site behind.
	if code, _ := getBody(t, url+"/s3-traversal/"); code != http.StatusNotFound {
		t.Errorf("rejected site is reachable: %d", code)
	}
}

// TestS3BackendExpiry covers the reaper on the object-storage path. Expiry
// metadata lives in the same object that records the site's existence, so a
// reap has to remove both that pointer and the content behind it.
func TestS3BackendExpiry(t *testing.T) {
	env := ensureS3(t)
	url, stop := startS3Daemon(t, env)

	// A site that will already be past its deadline when the reaper next runs.
	dir := writeFiles(t, map[string]string{"index.html": "<h1>ephemeral</h1>"})
	pushTo(t, url, dir, "s3-expiring", "--expires", "1s")

	keep := writeFiles(t, map[string]string{"index.html": "<h1>durable</h1>"})
	pushTo(t, url, keep, "s3-durable", "--expires", "never")

	if code, _ := getBody(t, url+"/s3-expiring/"); code != http.StatusOK {
		t.Fatalf("site should serve before expiry: %d", code)
	}

	// The reaper runs on a one-minute ticker but also fires once at startup, so
	// cycling the daemon triggers a reap without waiting out the interval.
	time.Sleep(1500 * time.Millisecond)
	stop()
	url2, _ := startS3Daemon(t, env)

	if code, _ := getBody(t, url2+"/s3-expiring/"); code != http.StatusNotFound {
		t.Errorf("expired site = %d, want 404", code)
	}
	if code, body := getBody(t, url2+"/s3-durable/"); code != http.StatusOK || !strings.Contains(body, "durable") {
		t.Errorf("non-expiring site = %d %q, want it retained", code, body)
	}
}

// TestS3BackendTokensSurviveRestart is the other half of statelessness: a
// minted token has to keep working across a restart with a fresh XDG home. If
// tokens stayed on local disk, every restart would silently invalidate every
// client's credentials.
func TestS3BackendTokensSurviveRestart(t *testing.T) {
	env := ensureS3(t)
	url, stop := startS3Daemon(t, env)

	// Mint through the API using the root token.
	body := `{"name":"s3-persisted-token"}`
	req, err := http.NewRequest(http.MethodPost, url+"/api/tokens", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+suiteToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint token = %d: %s", resp.StatusCode, raw)
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &minted); err != nil || minted.Token == "" {
		t.Fatalf("no token in response %s (%v)", raw, err)
	}

	// Restart onto a brand-new XDG home: nothing local carries over.
	stop()
	url2, _ := startS3Daemon(t, env)

	// The minted token must still authenticate a push.
	tarball := tarFromMap(t, map[string]string{"index.html": "<h1>token survived</h1>"})
	pushReq, err := http.NewRequest(http.MethodPut, url2+"/api/sites/s3-token-check", bytes.NewReader(tarball))
	if err != nil {
		t.Fatal(err)
	}
	pushReq.Header.Set("Authorization", "Bearer "+minted.Token)
	pushResp, err := http.DefaultClient.Do(pushReq)
	if err != nil {
		t.Fatal(err)
	}
	defer pushResp.Body.Close()
	if pushResp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(pushResp.Body)
		t.Fatalf("push with pre-restart token = %d: %s", pushResp.StatusCode, out)
	}

	// And it should still be listed.
	listReq, _ := http.NewRequest(http.MethodGet, url2+"/api/tokens", nil)
	listReq.Header.Set("Authorization", "Bearer "+suiteToken)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	listBody, _ := io.ReadAll(listResp.Body)
	if !strings.Contains(string(listBody), "s3-persisted-token") {
		t.Errorf("token missing from listing after restart: %s", listBody)
	}
}

// TestS3DocumentCompareAndSwap pins the concurrency guarantee behind the token
// store. Two daemons sharing a bucket both hold the whole token set in memory,
// so a blind overwrite would delete tokens the other had just minted. Writes
// are therefore conditioned on the version last read, and the loser is told so.
func TestS3DocumentCompareAndSwap(t *testing.T) {
	env := ensureS3(t)
	store, err := s3store.New(context.Background(), s3store.Config{
		Endpoint:  "http://" + env.endpoint,
		Bucket:    s3Bucket,
		AccessKey: env.accessKey,
		SecretKey: env.secretKey,
	})
	if err != nil {
		t.Fatalf("s3store.New: %v", err)
	}

	// Two independent handles on one key: two replicas of the daemon.
	a := store.Document("cas-test.yaml")
	b := store.Document("cas-test.yaml")

	// Both observe the same (absent) starting state.
	for _, d := range []*s3store.Document{a, b} {
		got, err := d.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if got != nil {
			t.Fatalf("expected an absent document, got %q", got)
		}
	}

	if err := a.Save([]byte("first")); err != nil {
		t.Fatalf("first writer should succeed: %v", err)
	}
	// b is still working from the pre-write state, so its write must be refused
	// rather than discarding a's.
	if err := b.Save([]byte("second")); !errors.Is(err, s3store.ErrConflict) {
		t.Fatalf("stale writer error = %v, want ErrConflict", err)
	}

	// a still holds the current version and may continue.
	if err := a.Save([]byte("third")); err != nil {
		t.Fatalf("current writer should still succeed: %v", err)
	}
	// Re-reading re-syncs b, which can then write.
	if _, err := b.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := b.Save([]byte("fourth")); err != nil {
		t.Fatalf("writer should succeed after reloading: %v", err)
	}

	got, err := a.Load()
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	if string(got) != "fourth" {
		t.Errorf("final contents = %q, want %q", got, "fourth")
	}
}
