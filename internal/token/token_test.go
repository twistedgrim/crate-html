package token

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestCreateVerifyRoundTrip(t *testing.T) {
	s, path := newStore(t)
	now := time.Now()

	plain, rec, err := s.Create("pi-agent", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, Prefix+rec.ID+"_") {
		t.Errorf("plaintext %q does not embed id %q", plain, rec.ID)
	}
	if strings.Contains(mustRead(t, path), strings.TrimPrefix(plain, Prefix+rec.ID+"_")) {
		t.Error("plaintext secret leaked into tokens.yaml")
	}

	got, ok := s.Verify(plain, now)
	if !ok || got.ID != rec.ID {
		t.Fatalf("verify: ok=%v rec=%+v", ok, got)
	}
	if got.LastUsedAt == nil {
		t.Error("verify should set last_used_at")
	}

	// Reload from disk: still verifies (hash persisted correctly).
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Verify(plain, now); !ok {
		t.Error("reloaded store rejects valid token")
	}
}

func TestVerifyRejects(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	plain, rec, err := s.Create("a", nil, now)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"empty":         "",
		"no prefix":     strings.TrimPrefix(plain, Prefix),
		"bad secret":    Prefix + rec.ID + "_" + strings.Repeat("0", 64),
		"unknown id":    Prefix + "ffffffff_" + strings.Repeat("0", 64),
		"missing parts": Prefix + rec.ID,
		"root-style":    "aabbccdd",
	}
	for name, bearer := range cases {
		if _, ok := s.Verify(bearer, now); ok {
			t.Errorf("%s: %q verified but should not", name, bearer)
		}
	}
}

func TestExpiry(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	ttl := time.Hour
	plain, _, err := s.Create("short", &ttl, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(plain, now.Add(30*time.Minute)); !ok {
		t.Error("token rejected before expiry")
	}
	if _, ok := s.Verify(plain, now.Add(2*time.Hour)); ok {
		t.Error("token accepted after expiry")
	}
}

func TestRevoke(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	plain, rec, err := s.Create("gone", nil, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Revoke(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(plain, now); ok {
		t.Error("revoked token still verifies")
	}
	if err := s.Revoke(rec.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second revoke: got %v, want ErrNotFound", err)
	}

	// Revoke by name.
	plain2, _, err := s.Create("byname", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke("byname"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(plain2, now); ok {
		t.Error("name-revoked token still verifies")
	}
}

func TestDuplicateName(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	if _, _, err := s.Create("dup", nil, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create("dup", nil, now); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("got %v, want ErrDuplicateName", err)
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "pi-agent", "x9", "a.b_c-d"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-lead", "trail-", "UPPER", "has space", strings.Repeat("a", 65)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRevokeKeepsStateOnSaveFailure(t *testing.T) {
	s, path := newStore(t)
	now := time.Now()
	plain, rec, err := s.Create("sticky", nil, now)
	if err != nil {
		t.Fatal(err)
	}

	// Make the directory unwritable so the tokens.yaml rewrite fails.
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := s.Revoke(rec.ID); err == nil {
		t.Fatal("revoke succeeded despite unwritable store")
	}
	// Memory must still match disk: the token was NOT revoked, so it must
	// keep verifying — otherwise a restart would silently resurrect it after
	// the caller was told it still exists.
	if _, ok := s.Verify(plain, now); !ok {
		t.Error("failed revoke left the token unverifiable in memory")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Verify(plain, now); !ok {
		t.Error("on-disk store lost the token after a failed revoke")
	}
	// And a retry once the disk recovers works.
	if err := s.Revoke(rec.ID); err != nil {
		t.Fatalf("retry revoke: %v", err)
	}
}

func TestFilePermissions(t *testing.T) {
	s, path := newStore(t)
	if _, _, err := s.Create("perm", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("tokens.yaml perm = %o, want 600", perm)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
