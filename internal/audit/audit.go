// Package audit writes one JSON line per broker event. Raw tool arguments, env values and
// secrets never reach the log: arguments are hashed and every string field is redacted.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/redact"
)

// Event kinds.
const (
	KindToolCall      = "tool_call"
	KindPlanCreated   = "plan_created"
	KindApproval      = "approval"
	KindProcessStart  = "process_start"
	KindHealth        = "health"
	KindStop          = "stop"
	KindExit          = "exit"
	KindConnection    = "connection"
	KindConfigChanged = "config_changed"
)

// Event is one audit line. Empty fields are omitted.
type Event struct {
	TS         time.Time `json:"ts"`
	Kind       string    `json:"kind"`
	Agent      string    `json:"agent,omitempty"`
	Source     string    `json:"source,omitempty"` // remote address
	ClientName string    `json:"client_name,omitempty"`
	Connector  string    `json:"connector,omitempty"`
	Profile    string    `json:"profile,omitempty"`
	Tool       string    `json:"tool,omitempty"`
	Action     string    `json:"action,omitempty"`
	Class      string    `json:"class,omitempty"`
	ArgsHash   string    `json:"args_hash,omitempty"`
	Decision   string    `json:"decision,omitempty"`
	Channel    string    `json:"channel,omitempty"`
	PlanID     string    `json:"plan_id,omitempty"`
	PID        int       `json:"pid,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Log appends events to a JSONL file. It is safe for concurrent use.
type Log struct {
	mu  sync.Mutex
	f   *os.File
	red *redact.Redactor
	now func() time.Time
}

// Open appends to path, creating it (and its directory) owner-only if needed.
func Open(path string, red *redact.Redactor) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening audit log: %w", err)
	}
	return &Log{f: f, red: red, now: time.Now}, nil
}

// Write records e, stamping TS if unset. Write errors are returned, not swallowed: a broker
// that cannot audit should refuse the action it was about to take.
func (l *Log) Write(e Event) error {
	if e.TS.IsZero() {
		e.TS = l.now().UTC()
	}
	for _, s := range []*string{&e.Agent, &e.Source, &e.ClientName, &e.Connector, &e.Profile, &e.Tool, &e.Action, &e.Detail, &e.Error} {
		*s = l.red.String(*s)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.f.Write(append(line, '\n'))
	return err
}

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// HashArgs returns a stable hash of JSON tool arguments: key order and whitespace do not
// change it, so the same call can be correlated across log lines without storing content.
func HashArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	data := []byte(raw)
	if err := json.Unmarshal(raw, &v); err == nil {
		if canon, err := json.Marshal(v); err == nil { // Marshal sorts map keys
			data = canon
		}
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
