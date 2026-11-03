// Package oauth signs the broker in to remote connectors that require OAuth (authorization
// code with PKCE, per the MCP authorization spec), keeps each connector's token in
// oauth/<connector>.json next to the config, and refreshes it.
//
// Signing in needs a person at a browser, so it never blocks an agent's call: when a server
// asks for authorization, the connection fails at once with a *SignInRequired error that
// carries the sign-in link, and the sign-in carries on in the background until the browser
// comes back to /oauth/callback on the broker. The next connection uses the saved token.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

// CallbackPath is where the authorization server sends the browser back to.
const CallbackPath = "/oauth/callback"

// signInTimeout is how long a sign-in waits for the browser to come back.
var signInTimeout = 10 * time.Minute

// SignInRequired is returned when a connector has no usable token. URL starts the sign-in.
type SignInRequired struct {
	Connector string
	URL       string
}

func (e *SignInRequired) Error() string {
	return fmt.Sprintf("%s needs you to sign in: open %s in a browser on this computer (the tray offers it too), then try again", e.Connector, e.URL)
}

// Options configures a Manager.
type Options struct {
	// Dir holds the token files.
	Dir string
	// Redirect is the default redirect URL: this broker's CallbackPath.
	Redirect string
	Logger   *slog.Logger
	// Client makes the metadata, token and probe requests. Defaults to a 30s-timeout client.
	Client *http.Client
	// OnSignIn, if set, is told when a sign-in is waiting for the browser.
	OnSignIn func(connector, url string)
}

// Manager runs sign-ins and owns the token files.
type Manager struct {
	o Options

	mu      sync.Mutex
	pending map[string]*signIn // by connector
	byState map[string]*signIn
}

// New returns a Manager.
func New(o Options) *Manager {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Manager{o: o, pending: map[string]*signIn{}, byState: map[string]*signIn{}}
}

// signIn is one sign-in waiting for, or finishing after, the browser.
type signIn struct {
	connector string

	readyOnce sync.Once
	ready     chan struct{} // closed once url or err is set
	url       string
	err       error // why the sign-in could not start

	result chan callbackResult // the browser's return
	done   chan struct{}       // closed once final is set
	final  error
}

type callbackResult struct {
	res auth.AuthorizationResult
	err error
}

func (s *signIn) markReady(url string, err error) {
	s.readyOnce.Do(func() {
		s.url, s.err = url, err
		close(s.ready)
	})
}

// Handler returns the OAuth handler for one connection to a connector. It sends the saved
// token, refreshing it as needed; when the server still refuses, it starts a sign-in and
// fails the connection with *SignInRequired.
func (m *Manager) Handler(name, endpoint string, o *config.OAuth) (auth.OAuthHandler, error) {
	cfg := m.handlerConfig(name, o)
	cfg.AuthorizationCodeFetcher = func(ctx context.Context, _ *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		url, err := m.SignIn(ctx, name, endpoint, o)
		if err != nil {
			return nil, err
		}
		return nil, &SignInRequired{Connector: name, URL: url}
	}
	cfg.InitialTokenSource = m.stored(name, o)
	return auth.NewAuthorizationCodeHandler(cfg)
}

func (m *Manager) handlerConfig(name string, o *config.OAuth) *auth.AuthorizationCodeHandlerConfig {
	client := &oauthex.ClientCredentials{ClientID: o.ClientID}
	if o.ClientSecret != "" {
		client.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: o.ClientSecret}
	}
	redirect := o.RedirectURL
	if redirect == "" {
		redirect = m.o.Redirect
	}
	cfg := &auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: client,
		RedirectURL:         redirect,
		RequestRefreshToken: true,
		// Servers that send iss without advertising it are common; it must still match.
		AcceptUnadvertisedIss: true,
		Client:                m.o.Client,
		NewTokenSource: func(ctx context.Context, c *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			rec := record{ClientID: c.ClientID, TokenURL: c.Endpoint.TokenURL, AuthStyle: c.Endpoint.AuthStyle, Token: tok}
			if err := m.save(name, rec); err != nil {
				return nil, err
			}
			return &savingSource{m: m, name: name, rec: rec, base: c.TokenSource(ctx, tok)}, nil
		},
	}
	if len(o.Scopes) > 0 {
		scopes := o.Scopes
		cfg.ScopeFilter = func([]string) []string { return scopes }
	}
	return cfg
}

// SignIn starts a sign-in for the connector, or joins the one already waiting, and returns
// the URL to open in a browser.
func (m *Manager) SignIn(ctx context.Context, name, endpoint string, o *config.OAuth) (string, error) {
	s := m.begin(name, endpoint, o)
	select {
	case <-s.ready:
		return s.url, s.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Wait blocks until the connector's pending sign-in finishes and returns its outcome; nil
// when none is pending.
func (m *Manager) Wait(ctx context.Context, name string) error {
	m.mu.Lock()
	s := m.pending[name]
	m.mu.Unlock()
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
		return s.final
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) begin(name, endpoint string, o *config.OAuth) *signIn {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.pending[name]; s != nil {
		return s
	}
	s := &signIn{connector: name, ready: make(chan struct{}), result: make(chan callbackResult, 1), done: make(chan struct{})}
	m.pending[name] = s
	go m.run(s, endpoint, o)
	return s
}

func (m *Manager) run(s *signIn, endpoint string, o *config.OAuth) {
	ctx, cancel := context.WithTimeout(context.Background(), signInTimeout)
	defer cancel()
	err := m.authorize(ctx, s, endpoint, o)
	m.mu.Lock()
	delete(m.pending, s.connector)
	for state, p := range m.byState {
		if p == s {
			delete(m.byState, state)
		}
	}
	m.mu.Unlock()
	s.markReady("", err) // no-op when the URL was already handed out
	s.final = err
	close(s.done)
	if err != nil {
		m.o.Logger.Warn("oauth sign-in failed", "connector", s.connector, "err", err)
	} else {
		m.o.Logger.Info("oauth signed in", "connector", s.connector)
	}
}

// probeBody is an initialize request: the server's 401 answer to it says where its
// authorization metadata is.
const probeBody = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mcp-broker","version":"0"}}}`

func (m *Manager) authorize(ctx context.Context, s *signIn, endpoint string, o *config.OAuth) error {
	cfg := m.handlerConfig(s.connector, o)
	cfg.AuthorizationCodeFetcher = func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		u, err := url.Parse(args.URL)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.byState[u.Query().Get("state")] = s
		m.mu.Unlock()
		s.markReady(args.URL, nil)
		if m.o.OnSignIn != nil {
			m.o.OnSignIn(s.connector, args.URL)
		}
		select {
		case r := <-s.result:
			if r.err != nil {
				return nil, r.err
			}
			return &r.res, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("the browser did not come back within %s", signInTimeout)
		}
	}
	h, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(probeBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := m.o.Client.Do(req)
	if err != nil {
		return fmt.Errorf("reaching %s: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		return fmt.Errorf("%s did not ask for authorization (HTTP %d)", endpoint, resp.StatusCode)
	}
	return h.Authorize(ctx, req, resp) // closes resp.Body
}

// Callback handles the browser's return from the authorization server.
func (m *Manager) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m.mu.Lock()
	s := m.byState[q.Get("state")]
	m.mu.Unlock()
	if s == nil {
		page(w, http.StatusBadRequest, "Sign-in expired", "This sign-in link has expired or was already used. Ask the agent to try again, or run mcp-broker signin.")
		return
	}
	var res callbackResult
	if e := q.Get("error"); e != "" {
		res.err = fmt.Errorf("the authorization server refused: %s %s", e, q.Get("error_description"))
	} else {
		res.res = auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}
	}
	select {
	case s.result <- res:
	default:
		page(w, http.StatusConflict, "Already answered", "This sign-in has already been answered.")
		return
	}
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		page(w, http.StatusAccepted, "Signing in", "Still exchanging the code for a token; the broker log has the outcome.")
		return
	}
	if s.final != nil {
		page(w, http.StatusBadGateway, "Sign-in failed", "Signing in to "+s.connector+" failed: "+s.final.Error())
		return
	}
	page(w, http.StatusOK, "Signed in", "mcp-broker is signed in to "+s.connector+". You can close this tab.")
}

func page(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title><body style="font-family:system-ui;margin:3em">`+
		`<h1>%s</h1><p>%s</p></body>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}

// State reports a connector's sign-in: "signed in", "signing in" or "not signed in".
func (m *Manager) State(name string) string {
	m.mu.Lock()
	_, waiting := m.pending[name]
	m.mu.Unlock()
	switch {
	case waiting:
		return "signing in"
	case m.hasToken(name):
		return "signed in"
	}
	return "not signed in"
}

// SignOut forgets the connector's token. Connections already open keep theirs until they
// are unloaded.
func (m *Manager) SignOut(name string) error {
	err := os.Remove(m.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// record is a token file: enough to refresh the token without rediscovering the server.
type record struct {
	ClientID  string           `json:"client_id"`
	TokenURL  string           `json:"token_url"`
	AuthStyle oauth2.AuthStyle `json:"auth_style"`
	Token     *oauth2.Token    `json:"token"`
}

func (m *Manager) path(name string) string { return filepath.Join(m.o.Dir, name+".json") }

func (m *Manager) hasToken(name string) bool {
	_, err := os.Stat(m.path(name))
	return err == nil
}

func (m *Manager) load(name string) (record, error) {
	var rec record
	b, err := os.ReadFile(m.path(name))
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(b, &rec)
}

func (m *Manager) save(name string, rec record) error {
	if err := os.MkdirAll(m.o.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path(name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path(name))
}

// stored is the saved token as a refreshing source, or nil when there is none for this
// client.
func (m *Manager) stored(name string, o *config.OAuth) oauth2.TokenSource {
	rec, err := m.load(name)
	if err != nil || rec.Token == nil || rec.ClientID != o.ClientID {
		return nil
	}
	c := &oauth2.Config{ClientID: o.ClientID, ClientSecret: o.ClientSecret,
		Endpoint: oauth2.Endpoint{TokenURL: rec.TokenURL, AuthStyle: rec.AuthStyle}}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, m.o.Client)
	return &savingSource{m: m, name: name, rec: rec, base: c.TokenSource(ctx, rec.Token)}
}

// savingSource writes the token back whenever a refresh replaces it; servers that rotate
// refresh tokens would otherwise strand the next start with a dead one.
type savingSource struct {
	m    *Manager
	name string
	base oauth2.TokenSource

	mu  sync.Mutex
	rec record
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec.Token == nil || t.AccessToken != s.rec.Token.AccessToken {
		s.rec.Token = t
		if err := s.m.save(s.name, s.rec); err != nil {
			s.m.o.Logger.Warn("saving refreshed oauth token", "connector", s.name, "err", err)
		}
	}
	return t, nil
}
