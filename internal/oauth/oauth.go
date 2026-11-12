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
	"net"
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

	"github.com/trustypangolin/mcp-broker/internal/atomicfile"
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
	o       Options
	private *http.Client // o.Client, dialing for itself: see httpClient

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
	return &Manager{o: o, private: privateClient(o.Client), pending: map[string]*signIn{}, byState: map[string]*signIn{}}
}

// httpClient is the client for a connector's OAuth requests. The MCP SDK refuses metadata,
// registration and token requests that dial private, link-local or CGNAT addresses (SSRF
// protection), except through a transport that dials for itself. private_network opts a
// connector into that.
func (m *Manager) httpClient(o *config.OAuth) *http.Client {
	if o != nil && o.PrivateNetwork {
		return m.private
	}
	return m.o.Client
}

// privateClient is c with a transport that sets its own DialContext, which the SDK takes
// as the caller owning dialing and so skips its address checks. A custom transport already
// skips them.
func privateClient(c *http.Client) *http.Client {
	var t *http.Transport
	switch base := c.Transport.(type) {
	case nil:
		t = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		t = base.Clone()
	default:
		return c
	}
	if t.DialContext == nil {
		t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	pc := *c
	pc.Transport = t
	return &pc
}

// privateHint explains the SDK's refusal of a private address, which otherwise reads as
// a bare dial error.
func privateHint(err error, o *config.OAuth) error {
	if err == nil || (o != nil && o.PrivateNetwork) || !strings.Contains(err.Error(), "non-public") {
		return err
	}
	return fmt.Errorf("%w (the authorization server is on a private network: if you trust it, set private_network: true under the connector's oauth block)", err)
}

// hintHandler adds privateHint to the SDK handler's sign-in errors.
type hintHandler struct {
	auth.OAuthHandler
	o *config.OAuth
}

func (h hintHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	return privateHint(h.OAuthHandler.Authorize(ctx, req, resp), h.o)
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
	client := m.client(name, o)
	if client == nil {
		// Not registered yet. The handler only reaches the fetcher below, which hands the
		// work to SignIn: that registers and builds the real authorization URL, so this
		// placeholder never leaves the broker.
		client = &oauthex.ClientCredentials{ClientID: "unregistered"}
	}
	cfg := m.handlerConfig(name, o, client)
	cfg.AuthorizationCodeFetcher = func(ctx context.Context, _ *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		url, err := m.SignIn(ctx, name, endpoint, o)
		if err != nil {
			return nil, err
		}
		return nil, &SignInRequired{Connector: name, URL: url}
	}
	cfg.InitialTokenSource = m.stored(name, o)
	h, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return nil, err
	}
	return hintHandler{h, o}, nil
}

func (m *Manager) redirect(o *config.OAuth) string {
	if o.RedirectURL != "" {
		return o.RedirectURL
	}
	return m.o.Redirect
}

// client is the OAuth client the broker signs in as: the configured client_id, or the one
// it registered with the server (dynamic registration). nil when it has yet to register,
// or when the saved registration was for another redirect URL.
func (m *Manager) client(name string, o *config.OAuth) *oauthex.ClientCredentials {
	if o.ClientID != "" {
		c := &oauthex.ClientCredentials{ClientID: o.ClientID}
		if o.ClientSecret != "" {
			c.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: o.ClientSecret}
		}
		return c
	}
	reg, err := m.loadRegistration(name)
	if err != nil || reg.ClientID == "" || reg.RedirectURL != m.redirect(o) {
		return nil
	}
	c := &oauthex.ClientCredentials{ClientID: reg.ClientID, Issuer: reg.Issuer}
	if reg.ClientSecret != "" {
		c.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: reg.ClientSecret}
	}
	return c
}

func (m *Manager) handlerConfig(name string, o *config.OAuth, client *oauthex.ClientCredentials) *auth.AuthorizationCodeHandlerConfig {
	cfg := &auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: client,
		RedirectURL:         m.redirect(o),
		RequestRefreshToken: true,
		// Servers that send iss without advertising it are common; it must still match.
		AcceptUnadvertisedIss: true,
		Client:                m.httpClient(o),
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(probeBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := m.httpClient(o).Do(req)
	if err != nil {
		return privateHint(fmt.Errorf("reaching %s: %w", endpoint, err), o)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		return fmt.Errorf("%s did not ask for authorization (HTTP %d)", endpoint, resp.StatusCode)
	}
	client := m.client(s.connector, o)
	dynamic := o.ClientID == ""
	if client == nil {
		reg, err := m.register(ctx, s.connector, endpoint, resp.Header, o)
		if err != nil {
			resp.Body.Close()
			return privateHint(err, o)
		}
		client = &oauthex.ClientCredentials{ClientID: reg.ClientID, Issuer: reg.Issuer}
		if reg.ClientSecret != "" {
			client.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: reg.ClientSecret}
		}
	}
	cfg := m.handlerConfig(s.connector, o, client)
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
		resp.Body.Close()
		return err
	}
	err = h.Authorize(ctx, req, resp) // closes resp.Body
	if err != nil && dynamic && strings.Contains(err.Error(), "invalid_client") {
		// The server has forgotten the registration (or never kept it): drop it, so the
		// next sign-in registers again.
		m.forgetRegistration(s.connector)
		return fmt.Errorf("%s no longer knows the broker's client registration; sign in again to register a new one (%w)", s.connector, err)
	}
	return privateHint(err, o)
}

// registration is a client the broker registered with an authorization server (RFC 7591),
// kept in oauth/<connector>.client.json so it is reused rather than registered again on
// every sign-in. Signing out keeps it; only the token goes.
type registration struct {
	ClientID             string    `json:"client_id"`
	ClientSecret         string    `json:"client_secret,omitempty"`
	Issuer               string    `json:"issuer"`
	RegistrationEndpoint string    `json:"registration_endpoint"`
	RedirectURL          string    `json:"redirect_url"` // registering again when it changes
	Registered           time.Time `json:"registered"`
}

func (m *Manager) registrationPath(name string) string {
	return filepath.Join(m.o.Dir, name+".client.json")
}

func (m *Manager) loadRegistration(name string) (registration, error) {
	var reg registration
	b, err := os.ReadFile(m.registrationPath(name))
	if err != nil {
		return reg, err
	}
	return reg, json.Unmarshal(b, &reg)
}

func (m *Manager) forgetRegistration(name string) {
	if err := os.Remove(m.registrationPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.o.Logger.Warn("removing oauth client registration", "connector", name, "err", err)
	}
}

// Registered reports whether the broker registered its own client for the connector.
func (m *Manager) Registered(name string) bool {
	_, err := os.Stat(m.registrationPath(name))
	return err == nil
}

// register finds the connector's authorization server and registers a public client with
// it, for servers that offer dynamic client registration instead of a developer console.
func (m *Manager) register(ctx context.Context, name, endpoint string, challenge http.Header, o *config.OAuth) (registration, error) {
	issuer, err := m.authServer(ctx, endpoint, challenge, m.httpClient(o))
	if err != nil {
		return registration{}, err
	}
	asm, err := auth.GetAuthServerMetadata(ctx, issuer, m.httpClient(o))
	if err != nil {
		return registration{}, fmt.Errorf("reading %s's authorization server metadata: %w", name, err)
	}
	regEndpoint := issuer + "/register" // the 2025-03-26 fallback the SDK uses too
	if asm != nil {
		issuer, regEndpoint = asm.Issuer, asm.RegistrationEndpoint
	}
	if regEndpoint == "" {
		return registration{}, fmt.Errorf("%s's authorization server (%s) doesn't offer dynamic client registration: register a client with it and set oauth.client_id (README: Remote servers that need a sign-in)", name, issuer)
	}
	redirect := m.redirect(o)
	meta := &oauthex.ClientRegistrationMetadata{
		ClientName:              "mcp-broker",
		RedirectURIs:            []string{redirect},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scope:                   strings.Join(o.Scopes, " "),
	}
	resp, err := oauthex.RegisterClient(ctx, regEndpoint, meta, m.httpClient(o))
	if err != nil {
		return registration{}, fmt.Errorf("registering a client for %s at %s: %w", name, regEndpoint, err)
	}
	reg := registration{ClientID: resp.ClientID, ClientSecret: resp.ClientSecret, Issuer: issuer,
		RegistrationEndpoint: regEndpoint, RedirectURL: redirect, Registered: time.Now().UTC()}
	b, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return registration{}, err
	}
	if err := writeFile(m.o.Dir, m.registrationPath(name), b); err != nil {
		return registration{}, err
	}
	m.o.Logger.Info("oauth client registered", "connector", name, "issuer", issuer, "client_id", reg.ClientID)
	return reg, nil
}

// authServer finds the authorization server for an MCP endpoint the way the SDK does:
// protected resource metadata from the 401 challenge, then at the endpoint's path, then at
// the root; failing all of those, the endpoint's origin (servers such as Atlassian publish
// only authorization server metadata).
func (m *Manager) authServer(ctx context.Context, endpoint string, challenge http.Header, hc *http.Client) (string, error) {
	ru, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	type try struct{ meta, resource string }
	var tries []try
	if cs, err := oauthex.ParseWWWAuthenticate(challenge.Values("WWW-Authenticate")); err == nil {
		for _, c := range cs {
			if c.Scheme == "bearer" && c.Params["resource_metadata"] != "" {
				tries = append(tries, try{c.Params["resource_metadata"], endpoint})
			}
		}
	}
	mu := *ru
	mu.RawQuery, mu.Fragment = "", ""
	mu.Path = "/.well-known/oauth-protected-resource/" + strings.TrimLeft(ru.Path, "/")
	tries = append(tries, try{mu.String(), endpoint})
	mu.Path = "/.well-known/oauth-protected-resource"
	origin := *ru
	origin.Path, origin.RawQuery, origin.Fragment = "", "", ""
	tries = append(tries, try{mu.String(), origin.String()})
	for _, t := range tries {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, t.meta, t.resource, hc)
		if err != nil || prm == nil {
			continue
		}
		if len(prm.AuthorizationServers) == 0 {
			return "", fmt.Errorf("%s's protected resource metadata names no authorization server", endpoint)
		}
		return prm.AuthorizationServers[0], nil
	}
	return origin.String(), nil
}

// writeFile writes b to path atomically, private to the user.
func writeFile(dir, path string, b []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return atomicfile.Rename(tmp, path)
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
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(m.o.Dir, m.path(name), b)
}

// stored is the saved token as a refreshing source, or nil when there is none for this
// client.
func (m *Manager) stored(name string, o *config.OAuth) oauth2.TokenSource {
	client := m.client(name, o)
	rec, err := m.load(name)
	if err != nil || rec.Token == nil || client == nil || rec.ClientID != client.ClientID {
		return nil
	}
	secret := ""
	if client.ClientSecretAuth != nil {
		secret = client.ClientSecretAuth.ClientSecret
	}
	c := &oauth2.Config{ClientID: client.ClientID, ClientSecret: secret,
		Endpoint: oauth2.Endpoint{TokenURL: rec.TokenURL, AuthStyle: rec.AuthStyle}}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, m.httpClient(o))
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
