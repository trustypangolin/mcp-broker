package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/proc"
)

// record is one run in the state file.
type record struct {
	Profile string    `json:"profile"`
	RunID   string    `json:"run_id"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

// saveState writes the live runs. It runs before Start returns, so a broker that dies a
// moment later still knows what it owned.
func (s *Supervisor) saveState() {
	if s.opts.StateFile == "" {
		return
	}
	s.mu.Lock()
	var recs []record
	for _, r := range s.runs {
		if r.getState().running() && r.proc != nil {
			recs = append(recs, record{Profile: r.profile, RunID: r.id, PID: r.proc.PID(), Started: r.started})
		}
	}
	s.mu.Unlock()
	b, _ := json.MarshalIndent(map[string]any{"runs": recs}, "", "  ")
	tmp := s.opts.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		s.log.Warn("writing run state", "err", err)
		return
	}
	if err := os.Rename(tmp, s.opts.StateFile); err != nil {
		s.log.Warn("writing run state", "err", err)
	}
}

// adoptAll takes back runs a previous broker recorded that are still alive (only possible
// where the OS did not take them down with it), and forgets the rest.
func (s *Supervisor) adoptAll() {
	if s.opts.StateFile == "" {
		return
	}
	b, err := os.ReadFile(s.opts.StateFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("reading run state", "err", err)
		}
		return
	}
	var st struct{ Runs []record }
	if err := json.Unmarshal(b, &st); err != nil {
		s.log.Warn("reading run state", "err", err)
		return
	}
	for _, rec := range st.Runs {
		p, ok := s.profile(rec.Profile)
		pr, alive := proc.Adopt(rec.PID, rec.Started)
		if !alive {
			s.log.Info("stale run record", "profile", rec.Profile, "pid", rec.PID)
			continue
		}
		if !ok {
			// The profile was removed from the config while its run outlived the broker.
			s.log.Warn("killing run of a profile no longer configured", "profile", rec.Profile, "pid", rec.PID)
			pr.Kill()
			continue
		}
		r := &run{id: rec.RunID, profile: rec.Profile, p: p, proc: pr, started: rec.Started, adopted: true, state: Starting, done: make(chan struct{})}
		r.out = newLines(ringSize, s.logFile(rec.Profile), s.opts.Redactor, nil)
		r.out.add(fmt.Sprintf("[mcp-broker] adopted %s (pid %d) after a broker restart; earlier output is in the profile log file", r.id, rec.PID))
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		s.runs[rec.Profile] = r
		s.audit(audit.Event{Kind: audit.KindProcessStart, Profile: rec.Profile, PID: rec.PID, Detail: "adopted " + rec.RunID})
		go s.watchHealth(ctx, r)
		go s.pollExit(r)
	}
	s.saveState()
}

// pollExit waits for an adopted run's process, which the broker cannot Wait on.
func (s *Supervisor) pollExit(r *run) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		if !r.proc.Alive() {
			r.proc.Kill()
			s.finish(r, -1, nil)
			return
		}
	}
}
