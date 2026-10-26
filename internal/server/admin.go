package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
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

// AdminOptions configures the admin API.
type AdminOptions struct {
	Config    *config.Config
	Token     string
	Approvals *approval.Queue
	Audit     *audit.Log
}

// MountAdmin adds the admin API to mux. Approvals are reachable from any allowed source
// (so a phone can approve); everything else will be loopback-only (dashboard.admin_from).
func MountAdmin(mux *http.ServeMux, o AdminOptions) {
	a := &admin{o: o}
	mux.Handle("GET /admin/approvals", a.guard(o.Config.Server.AllowedSources, a.listApprovals))
	mux.Handle("POST /admin/approvals/{id}", a.guard(o.Config.Server.AllowedSources, a.decide))
}

type admin struct{ o AdminOptions }

func (a *admin) guard(from []config.Prefix, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote, _, ok := addrs(r)
		if !ok || !config.ContainsAddr(from, remote.Addr()) {
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
