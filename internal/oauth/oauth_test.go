package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

// fakeAS is an authorization server plus the MCP server it protects.
type fakeAS struct {
	t      *testing.T
	as, rs *httptest.Server

	mu        sync.Mutex
	challenge string // PKCE challenge of the last authorize request
	scope     string // scope of the last authorize request
	valid     string // the access token the MCP server accepts
	refresh   string // the refresh token the token endpoint accepts
	issued    int

	clientID      string          // the client the token endpoint accepts
	known         map[string]bool // clients the authorize endpoint accepts
	dcr           bool            // advertise a registration endpoint
	noPRM         bool            // like Atlassian: no protected resource metadata, AS metadata at the MCP origin
	registrations int
	registered    map[string]any // the last registration request
}

func newFakeAS(t *testing.T) *fakeAS { return newFakeASAt(t, "") }

// newFakeASAt is newFakeAS listening on bind instead of loopback.
func newFakeASAt(t *testing.T, bind string) *fakeAS {
	serve := func(h http.Handler) *httptest.Server {
		if bind == "" {
			return httptest.NewServer(h)
		}
		l, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
		if err != nil {
			t.Skipf("cannot listen on %s: %v", bind, err)
		}
		s := httptest.NewUnstartedServer(h)
		s.Listener.Close()
		s.Listener = l
		s.StartTLS() // the SDK only accepts HTTPS beyond loopback
		return s
	}
	f := &fakeAS{t: t, clientID: "cid", known: map[string]bool{"cid": true}}
	meta := func(issuer string) map[string]any {
		m := map[string]any{
			"issuer": issuer, "authorization_endpoint": f.as.URL + "/authorize", "token_endpoint": f.as.URL + "/token",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_post", "none"},
			"scopes_supported":                      []string{"read", "write", "offline_access"},
		}
		if f.dcr {
			m["registration_endpoint"] = f.as.URL + "/register"
		}
		return m
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ping"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	rs := http.NewServeMux()
	f.rs = serve(rs)
	rs.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		if f.noPRM {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"resource": f.rs.URL + "/mcp", "authorization_servers": []string{f.as.URL}})
	})
	rs.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		if !f.noPRM {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(meta(f.rs.URL))
	})
	rs.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		ok := f.valid != "" && r.Header.Get("Authorization") == "Bearer "+f.valid
		noPRM := f.noPRM
		f.mu.Unlock()
		if !ok {
			if noPRM {
				w.Header().Set("WWW-Authenticate", `Bearer`)
			} else {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.rs.URL+`/.well-known/oauth-protected-resource/mcp"`)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})

	as := http.NewServeMux()
	f.as = serve(as)
	as.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(meta(f.as.URL))
	})
	as.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.registrations++
		f.registered = req
		f.clientID = "dyn-" + itoa(f.registrations)
		f.known[f.clientID] = true
		id := f.clientID
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"client_id": id, "redirect_uris": req["redirect_uris"], "token_endpoint_auth_method": "none"})
	})
	as.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		known := f.known[q.Get("client_id")]
		f.mu.Unlock()
		if !known || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.challenge, f.scope = q.Get("code_challenge"), q.Get("scope")
		f.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=c1&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	as.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Form.Get("client_id") != f.clientID {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"invalid_client"}`)
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "c1" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != f.refresh {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}
		f.issued++
		f.valid, f.refresh = "at-"+itoa(f.issued), "rt-"+itoa(f.issued)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": f.valid, "refresh_token": f.refresh, "token_type": "Bearer", "expires_in": 3600})
	})
	t.Cleanup(func() { f.rs.Close(); f.as.Close() })
	return f
}

func itoa(n int) string { return string(rune('0' + n)) }

// browser follows the sign-in link the way a person's browser would.
func browser(t *testing.T, signIn string) string {
	t.Helper()
	c := &http.Client{Transport: testTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(signIn)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize %s: %s", signIn, resp.Status)
	}
	resp, err = http.Get(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: %s: %s", resp.Status, b)
	}
	return string(b)
}

func newManager(t *testing.T, dir string) *Manager {
	t.Helper()
	m := New(Options{Dir: dir})
	cb := httptest.NewServer(http.HandlerFunc(m.Callback))
	t.Cleanup(cb.Close)
	m.o.Redirect = cb.URL + CallbackPath
	return m
}

func connect(t *testing.T, m *Manager, f *fakeAS) (*mcp.ClientSession, error) {
	return connectAs(t, m, f, &config.OAuth{ClientID: "cid", Scopes: []string{"read"}})
}

func connectAs(t *testing.T, m *Manager, f *fakeAS, o *config.OAuth) (*mcp.ClientSession, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := m.Handler("svc", f.rs.URL+"/mcp", o)
	if err != nil {
		t.Fatal(err)
	}
	tr := &mcp.StreamableClientTransport{Endpoint: f.rs.URL + "/mcp", OAuthHandler: h, MaxRetries: -1, HTTPClient: &http.Client{Transport: testTransport()}}
	return mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, tr, nil)
}

func signInRequired(t *testing.T, err error) string {
	t.Helper()
	var sr *SignInRequired
	if !errors.As(err, &sr) {
		t.Fatalf("want SignInRequired, got %v", err)
	}
	return sr.URL
}

func TestSignInRefreshAndReauth(t *testing.T) {
	f := newFakeAS(t)
	dir := t.TempDir()
	m := newManager(t, dir)

	// No token: the connection fails at once with a link, and the sign-in waits.
	_, err := connect(t, m, f)
	link := signInRequired(t, err)
	if m.State("svc") != "signing in" {
		t.Errorf("state: %s", m.State("svc"))
	}
	if !strings.Contains(browser(t, link), "Signed in") {
		t.Error("callback page should say signed in")
	}
	if err := m.Wait(context.Background(), "svc"); err != nil {
		t.Fatal(err)
	}
	if m.State("svc") != "signed in" {
		t.Errorf("state: %s", m.State("svc"))
	}
	if got := strings.Fields(f.scope); len(got) != 2 || !slices.Contains(got, "read") || !slices.Contains(got, "offline_access") {
		t.Errorf("configured scopes should replace the advertised ones, plus offline_access: %q", f.scope)
	}

	sess, err := connect(t, m, f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	sess.Close()

	// A restarted broker uses the saved token without asking again.
	if sess, err = connect(t, newManager(t, dir), f); err != nil {
		t.Fatal("saved token not used:", err)
	}
	sess.Close()

	// An expired token is refreshed, and the rotated refresh token is saved.
	path := filepath.Join(dir, "svc.json")
	rec := readRecord(t, path)
	rec.Token.Expiry = time.Now().Add(-time.Hour)
	writeRecord(t, path, rec)
	if sess, err = connect(t, newManager(t, dir), f); err != nil {
		t.Fatal("refresh:", err)
	}
	sess.Close()
	if rec = readRecord(t, path); rec.Token.AccessToken != "at-2" || rec.Token.RefreshToken != "rt-2" {
		t.Errorf("refreshed token not saved: %+v", rec.Token)
	}

	// A refresh token the server no longer accepts leads back to a sign-in.
	f.mu.Lock()
	f.refresh, f.valid = "revoked", "elsewhere"
	f.mu.Unlock()
	rec.Token.Expiry = time.Now().Add(-time.Hour)
	writeRecord(t, path, rec)
	m = newManager(t, dir)
	_, err = connect(t, m, f)
	signInRequired(t, err)

	if err := m.SignOut("svc"); err != nil || m.State("svc") != "signing in" {
		t.Errorf("sign out: %v, %s", err, m.State("svc"))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("sign out should delete the token file")
	}
}

// follow is browser without the success check: it returns the callback's status and page.
func follow(t *testing.T, signIn string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: testTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(signIn)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = http.Get(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestDynamicRegistration(t *testing.T) {
	for _, atlassian := range []bool{false, true} {
		t.Run(map[bool]string{false: "protected resource metadata", true: "atlassian style"}[atlassian], func(t *testing.T) {
			f := newFakeAS(t)
			f.dcr, f.noPRM, f.clientID = true, atlassian, ""
			dir := t.TempDir()
			m := newManager(t, dir)
			dyn := &config.OAuth{} // a URL-only connector

			// First use: registers, then hands out the sign-in link at once.
			_, err := connectAs(t, m, f, dyn)
			browser(t, signInRequired(t, err))
			if err := m.Wait(context.Background(), "svc"); err != nil {
				t.Fatal(err)
			}
			if f.registrations != 1 || f.registered["token_endpoint_auth_method"] != "none" || f.registered["client_name"] != "mcp-broker" {
				t.Fatalf("registration: %d %v", f.registrations, f.registered)
			}
			if uris, _ := f.registered["redirect_uris"].([]any); len(uris) != 1 || uris[0] != m.o.Redirect {
				t.Errorf("redirect_uris = %v", f.registered["redirect_uris"])
			}
			if !m.Registered("svc") {
				t.Error("registration not saved")
			}
			sess, err := connectAs(t, m, f, dyn)
			if err != nil {
				t.Fatal(err)
			}
			sess.Close()

			// A restart reuses both the token and the registration.
			m2 := New(Options{Dir: dir, Redirect: m.o.Redirect})
			if sess, err = connectAs(t, m2, f, dyn); err != nil {
				t.Fatal("after restart:", err)
			}
			sess.Close()

			// Signing out keeps the registration: signing in again doesn't register again.
			m.SignOut("svc")
			f.mu.Lock()
			f.valid = "elsewhere"
			f.mu.Unlock()
			_, err = connectAs(t, m, f, dyn)
			browser(t, signInRequired(t, err))
			if err := m.Wait(context.Background(), "svc"); err != nil {
				t.Fatal(err)
			}
			if f.registrations != 1 {
				t.Errorf("registered again after sign-out: %d", f.registrations)
			}

			// The server forgets the client: the sign-in fails with invalid_client, the
			// registration is dropped, and the next sign-in registers a new one.
			m.SignOut("svc")
			f.mu.Lock()
			f.valid, f.clientID = "elsewhere", "someone-else"
			f.mu.Unlock()
			_, err = connectAs(t, m, f, dyn)
			if code, page := follow(t, signInRequired(t, err)); code == http.StatusOK || !strings.Contains(page, "register a new one") {
				t.Errorf("sign-in with a forgotten client: %d %s", code, page)
			}
			if m.Registered("svc") {
				t.Error("rejected registration kept")
			}
			_, err = connectAs(t, m, f, dyn)
			browser(t, signInRequired(t, err))
			if err := m.Wait(context.Background(), "svc"); err != nil || f.registrations != 2 {
				t.Errorf("re-registration: %v, %d registrations", err, f.registrations)
			}

			// A new redirect URL (another port) needs a new registration.
			m3 := newManager(t, dir)
			_, err = connectAs(t, m3, f, dyn)
			browser(t, signInRequired(t, err))
			m3.Wait(context.Background(), "svc")
			if f.registrations != 3 {
				t.Errorf("redirect change: %d registrations", f.registrations)
			}
		})
	}
}

// A URL-only connector gets the implicit handler; a server that needs no sign-in must
// connect as before, without registering or asking anyone.
func TestImplicitOAuthOnPublicServer(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "public", Version: "1"}, nil)
	pub := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(pub.Close)
	m := newManager(t, t.TempDir())
	h, err := m.Handler("pub", pub.URL, &config.OAuth{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: pub.URL, OAuthHandler: h, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.Close()
	if m.Registered("pub") || m.State("pub") != "not signed in" {
		t.Errorf("public server: registered %v, state %s", m.Registered("pub"), m.State("pub"))
	}
}

func TestNoRegistrationEndpoint(t *testing.T) {
	f := newFakeAS(t) // advertises no registration_endpoint
	m := newManager(t, t.TempDir())
	_, err := connectAs(t, m, f, &config.OAuth{})
	if err == nil || !strings.Contains(err.Error(), "doesn't offer dynamic client registration") || !strings.Contains(err.Error(), "oauth.client_id") {
		t.Errorf("want a clear client_id error, got %v", err)
	}
	if f.registrations != 0 {
		t.Error("registered without a registration endpoint")
	}
}

func TestCallbackUnknownState(t *testing.T) {
	m := New(Options{Dir: t.TempDir()})
	w := httptest.NewRecorder()
	m.Callback(w, httptest.NewRequest(http.MethodGet, CallbackPath+"?code=x&state=nope", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown state: %d", w.Code)
	}
}

func readRecord(t *testing.T, path string) record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Token == nil {
		rec.Token = &oauth2.Token{}
	}
	return rec
}

func writeRecord(t *testing.T, path string, rec record) {
	t.Helper()
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// privateAddr is an RFC 1918 address of this machine, the kind the SDK refuses to dial
// for OAuth discovery.
func privateAddr(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsPrivate() {
			return n.IP.String()
		}
	}
	t.Skip("no private IPv4 address on this machine")
	return ""
}

// TestPrivateNetworkRefused: an authorization server on a private address is refused by
// the SDK unless the connector opts in, and the refusal names the option. (Opting in can't
// be shown end to end here: the SDK also refuses metadata URLs that are private IP
// literals, whatever the transport, and test servers have no host name. TestPrivateClient
// covers what the opt-in changes.)
func TestPrivateNetworkRefused(t *testing.T) {
	f := newFakeASAt(t, privateAddr(t))
	f.dcr, f.clientID = true, ""
	m := newManager(t, t.TempDir())
	m.o.Client = &http.Client{Timeout: 30 * time.Second, Transport: testTransport()}
	m.private = privateClient(m.o.Client)

	_, err := connectAs(t, m, f, &config.OAuth{})
	var sr *SignInRequired
	if err == nil || errors.As(err, &sr) || !strings.Contains(err.Error(), "private_network: true") {
		t.Fatalf("want a refusal naming private_network, got %v", err)
	}
	if f.registrations != 0 {
		t.Fatal("registered with a refused server")
	}
	// Already opted in: no hint pointing at the option that's set.
	_, err = connectAs(t, m, f, &config.OAuth{PrivateNetwork: true})
	if err == nil || strings.Contains(err.Error(), "private_network: true") {
		t.Fatalf("want a refusal without the hint, got %v", err)
	}
}

func TestPrivateClient(t *testing.T) {
	m := New(Options{})
	if m.httpClient(&config.OAuth{}) != m.o.Client || m.httpClient(nil) != m.o.Client {
		t.Error("connectors without private_network must keep the guarded client")
	}
	tr, ok := m.httpClient(&config.OAuth{PrivateNetwork: true}).Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatal("private_network client must dial for itself, or the SDK keeps its address checks")
	}
	if m.private.Timeout != m.o.Client.Timeout {
		t.Error("private client lost the timeout")
	}
	custom := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("x") })}
	if privateClient(custom) != custom {
		t.Error("a custom transport already skips the checks and must be kept")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// testTransport trusts httptest's certificate, which names loopback only, so a server on a
// private address fails verification otherwise. DialContext stays unset: the SDK's checks
// still apply to it.
func testTransport() *http.Transport {
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test servers only
}
