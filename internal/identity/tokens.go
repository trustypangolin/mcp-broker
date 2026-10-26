// Package identity turns a connection into an agent: it owns the token store and checks an
// agent's match conditions (docs/PERMISSIONS.md section 2).
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// TokenPrefix starts every agent token, so a leaked one is recognisable in logs and by
// secret scanners.
const TokenPrefix = "mcpb_"

// TokenEntry is one stored token. Only the hash is kept; the token itself is shown once.
type TokenEntry struct {
	ID      string    `yaml:"id"` // public handle for list and revoke
	Agent   string    `yaml:"agent"`
	SHA256  string    `yaml:"sha256"`
	Created time.Time `yaml:"created"`
	Label   string    `yaml:"label,omitempty"`
}

type tokenFile struct {
	Tokens []TokenEntry `yaml:"tokens"`
}

// Store is tokens.yaml. Lookups re-read the file when it changes on disk, so a revoke from
// the tray or CLI takes effect on the next request without restarting the broker.
type Store struct {
	path string

	mu      sync.Mutex
	entries []TokenEntry
	byHash  map[string]TokenEntry
	modTime time.Time
	size    int64
	checked time.Time
}

// recheckEvery bounds how often Lookup stats the file.
const recheckEvery = time.Second

// OpenStore loads path; a missing file is an empty store.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) reload() error {
	st, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.set(nil, time.Time{}, 0)
		return nil
	}
	if err != nil {
		return err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var f tokenFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	s.set(f.Tokens, st.ModTime(), st.Size())
	return nil
}

func (s *Store) set(entries []TokenEntry, mod time.Time, size int64) {
	s.entries = entries
	s.byHash = make(map[string]TokenEntry, len(entries))
	for _, e := range entries {
		s.byHash[e.SHA256] = e
	}
	s.modTime, s.size = mod, size
}

func (s *Store) refresh() {
	if time.Since(s.checked) < recheckEvery {
		return
	}
	s.checked = time.Now()
	st, err := os.Stat(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if len(s.entries) > 0 {
			s.set(nil, time.Time{}, 0) // file deleted: every token is revoked
		}
	case err == nil && (!st.ModTime().Equal(s.modTime) || st.Size() != s.size):
		// On a parse error keep nothing rather than the stale set: fail closed.
		if s.reload() != nil {
			s.set(nil, time.Time{}, 0)
		}
	}
}

// Lookup returns the entry for a presented token.
func (s *Store) Lookup(token string) (TokenEntry, bool) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return TokenEntry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
	e, ok := s.byHash[hashToken(token)]
	return e, ok
}

// List returns all entries, sorted by agent then creation time.
func (s *Store) List() []TokenEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = time.Time{}
	s.refresh()
	out := slices.Clone(s.entries)
	slices.SortFunc(out, func(a, b TokenEntry) int {
		if c := strings.Compare(a.Agent, b.Agent); c != 0 {
			return c
		}
		return a.Created.Compare(b.Created)
	})
	return out
}

// Create issues a new token for agent and returns it. The caller must show it to the user
// exactly once; it cannot be recovered.
func (s *Store) Create(agent, label string) (string, TokenEntry, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", TokenEntry{}, err
	}
	token := TokenPrefix + agent + "_" + base64.RawURLEncoding.EncodeToString(secret)
	h := hashToken(token)
	e := TokenEntry{ID: "tok_" + h[:12], Agent: agent, SHA256: h, Created: time.Now().UTC().Truncate(time.Second), Label: label}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return "", TokenEntry{}, err
	}
	if err := s.save(append(slices.Clone(s.entries), e)); err != nil {
		return "", TokenEntry{}, err
	}
	return token, e, nil
}

// Revoke removes tokens by ID, or every token of an agent when idOrAgent names one. It
// returns how many were removed.
func (s *Store) Revoke(idOrAgent string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return 0, err
	}
	keep := slices.DeleteFunc(slices.Clone(s.entries), func(e TokenEntry) bool {
		return e.ID == idOrAgent || e.Agent == idOrAgent
	})
	n := len(s.entries) - len(keep)
	if n == 0 {
		return 0, nil
	}
	return n, s.save(keep)
}

// save writes atomically (temp file + rename) with owner-only permissions.
func (s *Store) save(entries []TokenEntry) error {
	data, err := yaml.Marshal(tokenFile{Tokens: entries})
	if err != nil {
		return err
	}
	data = append([]byte("# Managed by mcp-broker. Token hashes only; tokens are shown once at creation.\n"), data...)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tokens-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	return s.reload()
}

// SaveToken writes a token to an owner-only file, for the local shim to read.
func SaveToken(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
