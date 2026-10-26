package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
	"github.com/trustypangolin/mcp-broker/internal/supervisor"
)

// AdminToken reads the dashboard/admin token, creating it (owner-only) on first use. The
// tray, the dashboard and `mcp-broker approvals` present it as a bearer token.
func AdminToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := "mcpba_" + hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// Control is what the admin API needs from the running broker.
type Control interface {
	// Config is the config currently in force.
	Config() *config.Config
	// Reload re-reads the config file and applies it, or leaves the running config
	// untouched and returns an error if the file is invalid.
	Reload() (Reloaded, error)
	// Status describes the broker and its connectors.
	Status() Status
	// Unload stops a connector instance, reporting whether it was loaded.
	Unload(key string) bool
	// Supervisor runs profiles.
	Supervisor() *supervisor.Supervisor
}

// Reloaded reports what a reload changed.
type Reloaded struct {
	Unloaded []string `json:"unloaded,omitempty"` // connector instances stopped because they changed
	Restart  []string `json:"restart,omitempty"`  // settings that only take effect after a restart
	Issues   []string `json:"issues,omitempty"`   // config.Check findings
}

// Status is the broker's state as the tray and dashboard show it.
type Status struct {
	Version    string              `json:"version"`
	Config     string              `json:"config"`
	URL        string              `json:"url"`
	Started    time.Time           `json:"started"`
	Pending    int                 `json:"pending_approvals"`
	Connectors []gateway.Connector `json:"connectors"`
	Profiles   []supervisor.Status `json:"profiles"`
	Agents     []string            `json:"agents"`
}

// AdminOptions configures the admin API.
type AdminOptions struct {
	Control   Control
	Token     string
	Approvals *approval.Queue
	Audit     *audit.Log
}

// MountAdmin adds the admin API to mux. Approvals and status are reachable from any allowed
// source (so a phone can approve); changing the broker is limited to dashboard.admin_from.
func MountAdmin(mux *http.ServeMux, o AdminOptions) {
	a := &admin{o: o}
	anyAllowed := func(c *config.Config) []config.Prefix { return c.Server.AllowedSources }
	adminOnly := func(c *config.Config) []config.Prefix { return c.Dashboard.AdminFrom }
	mux.Handle("GET /admin/approvals", a.guard(anyAllowed, a.listApprovals))
	mux.Handle("POST /admin/approvals/{id}", a.guard(anyAllowed, a.decide))
	mux.Handle("GET /admin/status", a.guard(anyAllowed, a.status))
	mux.Handle("POST /admin/reload", a.guard(adminOnly, a.reload))
	mux.Handle("POST /admin/connectors/{key}/unload", a.guard(adminOnly, a.unload))
	mux.Handle("GET /admin/profiles", a.guard(anyAllowed, a.profiles))
	mux.Handle("GET /admin/profiles/{name}/logs", a.guard(anyAllowed, a.profileLogs))
	mux.Handle("GET /admin/profiles/{name}/plan", a.guard(adminOnly, a.profilePlan))
	mux.Handle("POST /admin/profiles/{name}/{action}", a.guard(adminOnly, a.profileAction))
}

type admin struct{ o AdminOptions }

func (a *admin) guard(from func(*config.Config) []config.Prefix, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote, _, ok := addrs(r)
		cfg := a.o.Control.Config()
		if !ok || !config.ContainsAddr(cfg.Server.AllowedSources, remote.Addr()) || !config.ContainsAddr(from(cfg), remote.Addr()) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(a.o.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp-broker-admin"`)
			http.Error(w, "admin token required", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (a *admin) listApprovals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"approvals": a.o.Approvals.List(r.URL.Query().Get("all") == "")})
}

func (a *admin) decide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Approve bool           `json:"approve"`
		Channel config.Channel `json:"channel"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	ch := config.ChanDashboard
	if body.Channel == config.ChanTray {
		ch = config.ChanTray
	}
	e, err := a.o.Approvals.Decide(r.PathValue("id"), body.Approve, ch)
	switch {
	case errors.Is(err, approval.ErrUnknown):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": err.Error(), "approval": e})
		return
	}
	writeJSON(w, e)
}

func (a *admin) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, a.o.Control.Status())
}

func (a *admin) reload(w http.ResponseWriter, _ *http.Request) {
	res, err := a.o.Control.Reload()
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, res)
}

func (a *admin) unload(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"unloaded": a.o.Control.Unload(r.PathValue("key"))})
}

func (a *admin) profiles(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"profiles": a.o.Control.Supervisor().List()})
}

func (a *admin) profileLogs(w http.ResponseWriter, r *http.Request) {
	n := 200
	fmt.Sscan(r.URL.Query().Get("n"), &n)
	writeJSON(w, map[string]any{"lines": a.o.Control.Supervisor().Logs(r.PathValue("name"), min(max(n, 1), 5000))})
}

func (a *admin) profilePlan(w http.ResponseWriter, r *http.Request) {
	what := r.URL.Query().Get("for")
	if what == "" {
		what = "start"
	}
	pl, err := a.o.Control.Supervisor().Plan(r.PathValue("name"), what)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"plan": pl, "text": pl.Text()})
}

// profileAction starts, stops or updates a profile for the user (the CLI, the tray, the
// dashboard): no approval, since the admin token is the user.
func (a *admin) profileAction(w http.ResponseWriter, r *http.Request) {
	st, err := ProfileAction(r.Context(), a.o.Control.Supervisor(), r.PathValue("name"), r.PathValue("action"))
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": err.Error(), "status": st})
		return
	}
	writeJSON(w, st)
}

// ProfileAction runs start, stop or update on behalf of the user.
func ProfileAction(ctx context.Context, sup *supervisor.Supervisor, name, action string) (supervisor.Status, error) {
	switch action {
	case "start":
		return sup.Start(name)
	case "stop":
		return sup.Stop(ctx, name)
	case "update":
		return sup.Update(name)
	}
	return supervisor.Status{}, fmt.Errorf("unknown action %q (want start, stop or update)", action)
}
