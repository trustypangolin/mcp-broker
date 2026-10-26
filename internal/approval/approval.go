// Package approval holds plans waiting for a human decision. The first decision from an
// allowed channel wins, a timeout denies, and an approval is good for exactly one use.
package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

// Status of a plan.
type Status string

const (
	Pending  Status = "pending"
	Approved Status = "approved"
	Denied   Status = "denied"
	Expired  Status = "expired"
)

// Plan describes what will happen if approved. Every field is safe to show the user: callers
// redact before submitting.
type Plan struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"` // "tool.call", "profile.start", ...
	Agent     string           `json:"agent"`
	Connector string           `json:"connector,omitempty"`
	Tool      string           `json:"tool,omitempty"`
	Profile   string           `json:"profile,omitempty"`
	Action    string           `json:"action,omitempty"`
	Class     string           `json:"class,omitempty"`
	ArgsHash  string           `json:"args_hash,omitempty"`
	Summary   string           `json:"summary"`
	Reason    string           `json:"reason"`
	Channels  []config.Channel `json:"channels"`
	Created   time.Time        `json:"created"`
	Expires   time.Time        `json:"expires"`
}

// Entry is a plan and its current state.
type Entry struct {
	Plan
	Status    Status         `json:"status"`
	Channel   config.Channel `json:"channel,omitempty"` // who decided
	DecidedAt time.Time      `json:"decided_at,omitzero"`
	Used      bool           `json:"used,omitempty"`
}

var (
	ErrUnknown      = errors.New("unknown approval id")
	ErrDecided      = errors.New("already decided")
	ErrChannel      = errors.New("channel not allowed for this agent")
	ErrNotApproved  = errors.New("not approved")
	ErrUsed         = errors.New("approval already used")
	ErrWrongBinding = errors.New("approval does not match this call")
	ErrNoChannel    = errors.New("agent has no approval channel")
)

type entry struct {
	Entry
	done chan struct{} // closed once the entry leaves Pending
}

// Queue is the set of plans. Safe for concurrent use.
type Queue struct {
	timeout time.Duration
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
	subs    map[chan Entry]struct{}
}

// retain is how long decided entries stay listed (for the dashboard and approval_status).
const retain = 30 * time.Minute

// NewQueue returns a queue whose plans expire after timeout.
func NewQueue(timeout time.Duration) *Queue {
	return &Queue{timeout: timeout, now: time.Now, entries: map[string]*entry{}, subs: map[chan Entry]struct{}{}}
}

// Submit adds a plan and returns it with ID and times set. A plan with no channels is
// denied immediately: nobody could ever approve it.
func (q *Queue) Submit(p Plan) Entry {
	b := make([]byte, 8)
	rand.Read(b)
	now := q.now().UTC()
	p.ID = "pln_" + hex.EncodeToString(b)
	p.Created = now
	p.Expires = now.Add(q.timeout)

	e := &entry{Entry: Entry{Plan: p, Status: Pending}, done: make(chan struct{})}
	if len(p.Channels) == 0 {
		e.Status, e.DecidedAt = Denied, now
		close(e.done)
	}

	q.mu.Lock()
	q.gc()
	q.entries[p.ID] = e
	snap := e.Entry
	q.mu.Unlock()

	if snap.Status == Pending {
		time.AfterFunc(q.timeout, func() { q.expire(p.ID) })
	}
	q.publish(snap)
	return snap
}

func (q *Queue) expire(id string) {
	q.mu.Lock()
	e, ok := q.entries[id]
	if !ok || e.Status != Pending {
		q.mu.Unlock()
		return
	}
	e.Status, e.DecidedAt = Expired, q.now().UTC()
	close(e.done)
	snap := e.Entry
	q.mu.Unlock()
	q.publish(snap)
}

// Decide records a decision. Only the first decision counts, and only from a channel the
// plan allows.
func (q *Queue) Decide(id string, approve bool, ch config.Channel) (Entry, error) {
	q.mu.Lock()
	e, ok := q.entries[id]
	if !ok {
		q.mu.Unlock()
		return Entry{}, ErrUnknown
	}
	if e.Status != Pending {
		snap := e.Entry
		q.mu.Unlock()
		return snap, ErrDecided
	}
	if !slices.Contains(e.Channels, ch) {
		snap := e.Entry
		q.mu.Unlock()
		return snap, ErrChannel
	}
	e.Status = Denied
	if approve {
		e.Status = Approved
	}
	e.Channel, e.DecidedAt = ch, q.now().UTC()
	close(e.done)
	snap := e.Entry
	q.mu.Unlock()
	q.publish(snap)
	return snap, nil
}

// Get returns an entry.
func (q *Queue) Get(id string) (Entry, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.entries[id]
	if !ok {
		return Entry{}, false
	}
	return e.Entry, true
}

// Wait blocks until the plan is decided or expires, or ctx ends (then the entry is returned
// as it stands, still Pending).
func (q *Queue) Wait(ctx context.Context, id string) (Entry, error) {
	q.mu.Lock()
	e, ok := q.entries[id]
	q.mu.Unlock()
	if !ok {
		return Entry{}, ErrUnknown
	}
	select {
	case <-e.done:
	case <-ctx.Done():
	}
	got, _ := q.Get(id)
	return got, nil
}

// Binding identifies the call an approval is for, so it cannot be spent on anything else.
type Binding struct {
	Agent, Connector, Tool, Profile, Action, ArgsHash string
}

func (p Plan) binding() Binding {
	return Binding{p.Agent, p.Connector, p.Tool, p.Profile, p.Action, p.ArgsHash}
}

// Consume spends an approval on the call described by b.
func (q *Queue) Consume(id string, b Binding) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.entries[id]
	switch {
	case !ok:
		return ErrUnknown
	case e.binding() != b:
		return ErrWrongBinding
	case e.Status != Approved:
		return fmt.Errorf("%w (%s)", ErrNotApproved, e.Status)
	case e.Used:
		return ErrUsed
	}
	e.Used = true
	return nil
}

// List returns entries newest first; pendingOnly limits it to undecided plans.
func (q *Queue) List(pendingOnly bool) []Entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.gc()
	out := []Entry{} // never nil: the dashboard expects a JSON array
	for _, e := range q.entries {
		if !pendingOnly || e.Status == Pending {
			out = append(out, e.Entry)
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return b.Created.Compare(a.Created) })
	return out
}

// Subscribe returns a channel that receives every new or changed entry, for the tray and
// the dashboard. Slow subscribers miss updates rather than blocking the broker.
func (q *Queue) Subscribe() (<-chan Entry, func()) {
	ch := make(chan Entry, 32)
	q.mu.Lock()
	q.subs[ch] = struct{}{}
	q.mu.Unlock()
	return ch, func() {
		q.mu.Lock()
		if _, ok := q.subs[ch]; ok {
			delete(q.subs, ch)
			close(ch)
		}
		q.mu.Unlock()
	}
}

func (q *Queue) publish(e Entry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for ch := range q.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// gc drops decided entries past retention. Callers hold mu.
func (q *Queue) gc() {
	cutoff := q.now().Add(-retain)
	for id, e := range q.entries {
		if e.Status != Pending && e.DecidedAt.Before(cutoff) {
			delete(q.entries, id)
		}
	}
}
