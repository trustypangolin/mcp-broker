package supervisor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/audit"
)

// unhealthyAfter is how many failed checks in a row turn a ready run unhealthy.
const unhealthyAfter = 3

var healthClient = &http.Client{Timeout: 5 * time.Second}

func checkHTTP(ctx context.Context, url string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return false, err.Error()
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return false, resp.Status
	}
	return true, resp.Status
}

// watchHealth moves a run from starting to ready (or unhealthy at start_timeout), and
// keeps checking afterwards so a hung app shows as unhealthy. Without a health check or
// ready regex, a run is ready as soon as it has started.
func (s *Supervisor) watchHealth(ctx context.Context, r *run) {
	h := r.p.Health
	if h.HTTP == "" {
		if h.LogReadyRegex == "" {
			s.becomeReady(r, "started (no health check configured)")
			return
		}
		// Readiness comes from the output; only the start timeout is watched here.
		select {
		case <-ctx.Done():
		case <-time.After(h.StartTimeout):
			s.timedOut(r)
		}
		return
	}

	interval := h.Interval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(h.StartTimeout)
	fails := 0
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		ok, detail := checkHTTP(ctx, h.HTTP)
		if ctx.Err() != nil {
			return
		}
		state := r.getState()
		switch {
		case ok:
			fails = 0
			if state == Starting || state == Unhealthy {
				s.becomeReady(r, "GET "+h.HTTP+": "+detail)
			} else {
				r.set(func(r *run) { r.health = "GET " + h.HTTP + ": " + detail })
			}
		case state == Starting && time.Now().After(deadline):
			r.set(func(r *run) { r.health = "GET " + h.HTTP + ": " + detail })
			s.timedOut(r)
		case state == Ready:
			fails++
			r.set(func(r *run) { r.health = "GET " + h.HTTP + ": " + detail })
			if fails >= unhealthyAfter {
				turned := false
				r.set(func(r *run) {
					if r.state == Ready {
						r.state, turned = Unhealthy, true
					}
				})
				if turned {
					s.changed(r)
					s.audit(audit.Event{Kind: audit.KindHealth, Profile: r.profile, Decision: string(Unhealthy), Detail: detail})
				}
			}
		default:
			r.set(func(r *run) { r.health = "waiting: GET " + h.HTTP + ": " + detail })
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Supervisor) timedOut(r *run) {
	changed := false
	r.set(func(r *run) {
		if r.state == Starting {
			r.state, changed = Unhealthy, true
			r.err = fmt.Sprintf("not ready after start_timeout %s; still running", r.p.Health.StartTimeout)
		}
	})
	if changed {
		s.changed(r)
		s.audit(audit.Event{Kind: audit.KindHealth, Profile: r.profile, Decision: string(Unhealthy), Detail: "start timeout"})
	}
}
