// Package redact keeps secret values out of everything the broker emits: plans, tool
// results, logs, audit lines and the dashboard.
package redact

import (
	"sort"
	"strings"
	"sync"

	"github.com/trustypangolin/mcp-broker/internal/glob"
)

// Placeholder replaces every secret value.
const Placeholder = "<redacted>"

// minSecretLen keeps very short values (ports, flags) from shredding unrelated output.
// A real secret this short is not a secret worth the name.
const minSecretLen = 4

var secretKeyPatterns = []string{"*KEY*", "*TOKEN*", "*SECRET*", "*PASSWORD*"}

// IsSecretKey reports whether an env var or header name always holds a secret.
func IsSecretKey(name string) bool {
	for _, p := range secretKeyPatterns {
		if glob.Match(p, name) {
			return true
		}
	}
	return false
}

// Redactor replaces known secret values in strings. It is safe for concurrent use and
// grows as connectors and profiles resolve their environments.
type Redactor struct {
	mu   sync.RWMutex
	vals []string // sorted longest first so overlapping secrets redact fully
	set  map[string]struct{}
}

// New returns an empty Redactor.
func New() *Redactor {
	return &Redactor{set: map[string]struct{}{}}
}

// Add registers secret values.
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, v := range values {
		if len(v) < minSecretLen {
			continue
		}
		if _, ok := r.set[v]; ok {
			continue
		}
		r.set[v] = struct{}{}
		r.vals = append(r.vals, v)
		changed = true
	}
	if changed {
		sort.Slice(r.vals, func(i, j int) bool { return len(r.vals[i]) > len(r.vals[j]) })
	}
}

// String returns s with every registered secret replaced by Placeholder.
func (r *Redactor) String(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.vals {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, Placeholder)
		}
	}
	return s
}
