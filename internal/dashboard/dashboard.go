// Package dashboard is the broker's web UI at /ui: approvals and connector status from any
// allowed source (so a phone can approve), and settings (connectors, agents, tokens, raw
// config, audit) from dashboard.admin_from only. The tray's Settings window is this page.
//
// A browser signs in once, with a one-time code from the tray or `mcp-broker dashboard`, or
// by pasting the admin token; it then holds a session cookie. State-changing requests also
// need a custom header, which a cross-site form or image cannot send.
package dashboard

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/server"
)

//go:embed web
var web embed.FS

// Backend is what the dashboard needs from the running broker.
type Backend interface {
	server.Control
	Approvals() *approval.Queue
	Tokens() *identity.Store
	AdminToken() string
	Logger() *slog.Logger
}

const (
	cookieName = "mcpb_ui"
	sessionTTL = 12 * time.Hour
	codeTTL    = 2 * time.Minute
	// csrfHeader must be present on every state-changing request.
	csrfHeader = "X-Mcpb"
)

// csp allows only the embedded assets. Inline style attributes are allowed for layout;
// scripts are never inline.
const csp = "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; form-action 'self'"

// Dashboard serves /ui.
type Dashboard struct {
	b Backend

	mu       sync.Mutex
	sessions map[string]time.Time // id → expiry
	codes    map[string]time.Time // one-time login code → expiry
}

// New returns a Dashboard for b.
func New(b Backend) *Dashboard {
	return &Dashboard{b: b, sessions: map[string]time.Time{}, codes: map[string]time.Time{}}
}

func randomID() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// LoginCode mints a single-use code; /ui/login?code=<code> turns it into a session. The
// tray opens that URL so the admin token never appears in browser history.
func (d *Dashboard) LoginCode() string {
	c := randomID()
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, exp := range d.codes {
		if now.After(exp) {
			delete(d.codes, k)
		}
	}
	d.codes[c] = now.Add(codeTTL)
	return c
}

func (d *Dashboard) redeem(code string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	exp, ok := d.codes[code]
	delete(d.codes, code)
	return ok && time.Now().Before(exp)
}

func (d *Dashboard) newSession() string {
	id := randomID()
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, exp := range d.sessions {
		if now.After(exp) {
			delete(d.sessions, k)
		}
	}
	d.sessions[id] = now.Add(sessionTTL)
	return id
}

func (d *Dashboard) validSession(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	exp, ok := d.sessions[c.Value]
	return ok && time.Now().Before(exp)
}

func remoteAddr(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

// level is who may use an endpoint.
type level int

const (
	viewer level = iota // allowed_sources
	admin               // dashboard.admin_from
)

func (d *Dashboard) allowed(r *http.Request, lv level) bool {
	src, ok := remoteAddr(r)
	if !ok {
		return false
	}
	cfg := d.b.Config()
	if !config.ContainsAddr(cfg.Server.AllowedSources, src) {
		return false
	}
	return lv == viewer || config.ContainsAddr(cfg.Dashboard.AdminFrom, src)
}

// sameOrigin rejects requests a browser marked as coming from another site.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		host := strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
		return host == r.Host
	}
	return true
}

// api wraps a JSON endpoint: source check, session, and CSRF header for writes.
func (d *Dashboard) api(lv level, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.allowed(r, lv) {
			jsonError(w, http.StatusForbidden, "not allowed from this address")
			return
		}
		if !d.validSession(r) {
			jsonError(w, http.StatusUnauthorized, "sign in again")
			return
		}
		if r.Method != http.MethodGet && (r.Header.Get(csrfHeader) == "" || !sameOrigin(r)) {
			jsonError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	})
}

// Mount adds the dashboard's routes to mux.
func (d *Dashboard) Mount(mux *http.ServeMux) {
	static, _ := fs.Sub(web, "web")
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))
	mux.HandleFunc("GET /ui/{$}", d.index)
	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /ui/login", d.loginPage)
	mux.HandleFunc("POST /ui/login", d.loginPost)
	mux.HandleFunc("POST /ui/logout", d.logout)
	mux.HandleFunc("POST /ui/api/login-code", d.loginCodeAPI)

	mux.Handle("GET /ui/api/status", d.api(viewer, d.status))
	mux.Handle("GET /ui/api/events", d.api(viewer, d.events))
	mux.Handle("GET /ui/api/approvals", d.api(viewer, d.listApprovals))
	mux.Handle("POST /ui/api/approvals/{id}", d.api(viewer, d.decide))

	mux.Handle("POST /ui/api/reload", d.api(admin, d.reload))
	mux.Handle("GET /ui/api/connectors", d.api(admin, d.listConnectors))
	mux.Handle("PUT /ui/api/connectors/{name}", d.api(admin, d.putConnector))
	mux.Handle("DELETE /ui/api/connectors/{name}", d.api(admin, d.deleteConnector))
	mux.Handle("POST /ui/api/connectors/{key}/unload", d.api(admin, d.unload))
	mux.Handle("GET /ui/api/agents", d.api(admin, d.listAgents))
	mux.Handle("PUT /ui/api/agents/{name}", d.api(admin, d.putAgent))
	mux.Handle("DELETE /ui/api/agents/{name}", d.api(admin, d.deleteAgent))
	mux.Handle("POST /ui/api/agents/{name}/tokens", d.api(admin, d.createToken))
	mux.Handle("DELETE /ui/api/tokens/{id}", d.api(admin, d.revokeToken))
	mux.Handle("GET /ui/api/config", d.api(admin, d.getConfig))
	mux.Handle("POST /ui/api/config/check", d.api(admin, d.checkConfig))
	mux.Handle("PUT /ui/api/config", d.api(admin, d.putConfig))
	mux.Handle("GET /ui/api/audit", d.api(admin, d.audit))
}

func (d *Dashboard) index(w http.ResponseWriter, r *http.Request) {
	if !d.allowed(r, viewer) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !d.validSession(r) {
		http.Redirect(w, r, "/ui/login", http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	b, _ := web.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (d *Dashboard) setSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: d.newSession(), Path: "/ui", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
}

func (d *Dashboard) loginPage(w http.ResponseWriter, r *http.Request) {
	if !d.allowed(r, viewer) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if code := r.URL.Query().Get("code"); code != "" {
		if d.redeem(code) {
			d.setSession(w, r)
			dest := "/ui/"
			if page := r.URL.Query().Get("page"); validName.MatchString(page) {
				dest += "#" + page
			}
			http.Redirect(w, r, dest, http.StatusFound)
			return
		}
		d.loginForm(w, "That sign-in link has expired or was already used. Open the dashboard from the tray again, or paste the admin token.")
		return
	}
	d.loginForm(w, "")
}

func (d *Dashboard) loginForm(w http.ResponseWriter, msg string) {
	b, _ := web.ReadFile("web/login.html")
	page := strings.Replace(string(b), "{{MESSAGE}}", htmlEscape(msg), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	w.Write([]byte(page))
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func (d *Dashboard) loginPost(w http.ResponseWriter, r *http.Request) {
	if !d.allowed(r, viewer) || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tok := strings.TrimSpace(r.PostFormValue("token"))
	if subtle.ConstantTimeCompare([]byte(tok), []byte(d.b.AdminToken())) != 1 {
		d.b.Logger().Warn("dashboard sign-in failed", "source", r.RemoteAddr)
		time.Sleep(500 * time.Millisecond)
		d.loginForm(w, "That is not the admin token.")
		return
	}
	d.setSession(w, r)
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (d *Dashboard) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		d.mu.Lock()
		delete(d.sessions, c.Value)
		d.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/ui", MaxAge: -1})
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

// loginCodeAPI lets the CLI (holding the admin token) open a signed-in browser.
func (d *Dashboard) loginCodeAPI(w http.ResponseWriter, r *http.Request) {
	if !d.allowed(r, admin) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tok := identity.BearerToken(r.Header.Get("Authorization"))
	if subtle.ConstantTimeCompare([]byte(tok), []byte(d.b.AdminToken())) != 1 {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]string{"code": d.LoginCode()})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func (d *Dashboard) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, struct {
		server.Status
		Admin bool `json:"admin"`
	}{d.b.Status(), d.allowed(r, admin)})
}

func (d *Dashboard) listApprovals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"approvals": d.b.Approvals().List(r.URL.Query().Get("all") == "")})
}

func (d *Dashboard) decide(w http.ResponseWriter, r *http.Request) {
	var body struct{ Approve bool }
	if err := decode(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body")
		return
	}
	e, err := d.b.Approvals().Decide(r.PathValue("id"), body.Approve, config.ChanDashboard)
	switch {
	case errors.Is(err, approval.ErrUnknown):
		jsonError(w, http.StatusNotFound, err.Error())
	case err != nil:
		jsonError(w, http.StatusConflict, err.Error())
	default:
		writeJSON(w, e)
	}
}

// events streams approval changes as server-sent events, so a phone left on the
// approvals page sees new requests immediately.
func (d *Dashboard) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch, cancel := d.b.Approvals().Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(": hello\n\n"))
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
		case e, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(e)
			w.Write([]byte("event: approval\ndata: "))
			w.Write(b)
			w.Write([]byte("\n\n"))
		}
		fl.Flush()
	}
}

func (d *Dashboard) reload(w http.ResponseWriter, _ *http.Request) {
	res, err := d.b.Reload()
	if err != nil {
		jsonError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, res)
}

func (d *Dashboard) unload(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{"unloaded": d.b.Unload(r.PathValue("key"))})
}
