// Package token implements named API tokens for the crated daemon.
//
// Tokens have the form "crate_<id>_<secret>": the 8-hex-char id is public
// (it appears in listings and logs and makes verification an O(1) lookup);
// the secret is 32 random bytes, hex-encoded. Only a SHA-256 hash of the
// secret is stored, so tokens.yaml never contains a usable credential.
// SHA-256 without stretching is appropriate here because secrets are
// high-entropy random strings, not passwords.
//
// The daemon owns tokens.yaml exclusively; the CLI mints and revokes tokens
// through /api/tokens rather than touching the file.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Prefix identifies crate API tokens in bearer headers.
const Prefix = "crate_"

const (
	idBytes     = 4
	secretBytes = 32
	// touchInterval throttles last_used_at persistence so a busy client does
	// not rewrite tokens.yaml on every request.
	touchInterval = time.Minute
)

var (
	// ErrNotFound is returned by Revoke when no token matches.
	ErrNotFound = errors.New("token not found")
	// ErrDuplicateName is returned by Create when the name is taken.
	ErrDuplicateName = errors.New("token name already in use")

	nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)
)

// ValidateName reports whether name is usable as a token name.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid token name %q (must match %s)", name, nameRE.String())
	}
	return nil
}

// Record is one token as stored in tokens.yaml. The plaintext secret is
// never stored; SHA256 is the hex-encoded hash of the secret half.
type Record struct {
	ID         string     `yaml:"id"`
	Name       string     `yaml:"name"`
	SHA256     string     `yaml:"sha256"`
	CreatedAt  time.Time  `yaml:"created_at"`
	ExpiresAt  *time.Time `yaml:"expires_at,omitempty"`
	LastUsedAt *time.Time `yaml:"last_used_at,omitempty"`
}

type fileShape struct {
	Tokens []Record `yaml:"tokens"`
}

// Store is the daemon-side token set, backed by a yaml file. All methods are
// safe for concurrent use.
type Store struct {
	path string

	mu        sync.Mutex
	recs      []Record
	lastSaved map[string]time.Time // id → last_used_at value most recently persisted
}

// Load reads the store at path. A missing file yields an empty store; the
// file is created on first mutation.
func Load(path string) (*Store, error) {
	s := &Store{path: path, lastSaved: make(map[string]time.Time)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	var f fileShape
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse tokens: %w", err)
	}
	s.recs = f.Tokens
	for _, r := range s.recs {
		if r.LastUsedAt != nil {
			s.lastSaved[r.ID] = *r.LastUsedAt
		}
	}
	return s, nil
}

// Create mints a new token. ttl == nil means the token never expires.
// The returned plaintext is shown once and cannot be recovered later.
func (s *Store) Create(name string, ttl *time.Duration, now time.Time) (plaintext string, rec Record, err error) {
	if err := ValidateName(name); err != nil {
		return "", Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range s.recs {
		if r.Name == name {
			return "", Record{}, fmt.Errorf("%w: %q", ErrDuplicateName, name)
		}
	}

	id, err := s.freshIDLocked()
	if err != nil {
		return "", Record{}, err
	}
	secretRaw := make([]byte, secretBytes)
	if _, err := rand.Read(secretRaw); err != nil {
		return "", Record{}, fmt.Errorf("generate secret: %w", err)
	}
	secret := hex.EncodeToString(secretRaw)

	rec = Record{
		ID:        id,
		Name:      name,
		SHA256:    hashSecret(secret),
		CreatedAt: now.UTC(),
	}
	if ttl != nil {
		t := now.UTC().Add(*ttl)
		rec.ExpiresAt = &t
	}
	s.recs = append(s.recs, rec)
	if err := s.saveLocked(); err != nil {
		s.recs = s.recs[:len(s.recs)-1]
		return "", Record{}, err
	}
	return Prefix + id + "_" + secret, rec, nil
}

func (s *Store) freshIDLocked() (string, error) {
	for range 10 {
		raw := make([]byte, idBytes)
		if _, err := rand.Read(raw); err != nil {
			return "", fmt.Errorf("generate id: %w", err)
		}
		id := hex.EncodeToString(raw)
		collision := false
		for _, r := range s.recs {
			if r.ID == id {
				collision = true
				break
			}
		}
		if !collision {
			return id, nil
		}
	}
	return "", errors.New("could not generate a unique token id")
}

// List returns a copy of all records, in creation order.
func (s *Store) List() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.recs))
	copy(out, s.recs)
	return out
}

// Revoke removes the token matching id (preferred) or name.
func (s *Store) Revoke(idOrName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, r := range s.recs {
		if r.ID == idOrName {
			idx = i
			break
		}
	}
	if idx < 0 {
		for i, r := range s.recs {
			if r.Name == idOrName {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, idOrName)
	}
	removed := s.recs[idx]
	s.recs = append(s.recs[:idx], s.recs[idx+1:]...)
	delete(s.lastSaved, removed.ID)
	return s.saveLocked()
}

// Verify checks a bearer value. It returns the matching record and true only
// if the value is a well-formed crate token whose secret hashes to a stored,
// unexpired record. On success the record's last_used_at is updated in
// memory and persisted at most once per touchInterval.
func (s *Store) Verify(bearer string, now time.Time) (Record, bool) {
	rest, ok := strings.CutPrefix(bearer, Prefix)
	if !ok {
		return Record{}, false
	}
	id, secret, ok := strings.Cut(rest, "_")
	if !ok || id == "" || secret == "" {
		return Record{}, false
	}
	gotHash := hashSecret(secret)

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.recs {
		r := &s.recs[i]
		if r.ID != id {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(gotHash), []byte(r.SHA256)) != 1 {
			return Record{}, false
		}
		if r.ExpiresAt != nil && now.After(*r.ExpiresAt) {
			return Record{}, false
		}
		t := now.UTC()
		r.LastUsedAt = &t
		if saved, ok := s.lastSaved[r.ID]; !ok || t.Sub(saved) >= touchInterval {
			// Best-effort: a failed write here must not fail auth.
			if err := s.saveLocked(); err == nil {
				s.lastSaved[r.ID] = t
			}
		}
		return *r, true
	}
	return Record{}, false
}

func (s *Store) saveLocked() error {
	out, err := yaml.Marshal(fileShape{Tokens: s.recs})
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write tokens: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("install tokens: %w", err)
	}
	return nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
