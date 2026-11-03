package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
}

func newFakeAS(t *testing.T) *fakeAS {
	f := &fakeAS{t: t}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ping"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	rs := http.NewServeMux()
	f.rs = httptest.NewServer(rs)
	rs.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"resource": f.rs.URL + "/mcp", "authorization_servers": []string{f.as.URL}})
	})
	rs.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		ok := f.valid != "" && r.Header.Get("Authorization") == "Bearer "+f.valid
		f.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.rs.URL+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})

	as := http.NewServeMux()
	f.as = httptest.NewServer(as)
	as.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.as.URL, "authorization_endpoint": f.as.URL + "/authorize", "token_endpoint": f.as.URL + "/token",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
			"scopes_supported":                      []string{"read", "write", "offline_access"},
		})
	})
	as.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "cid" || q.Get("code_challenge_method") != "S256" {
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
		if r.Form.Get("client_id") != "cid" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
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
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := m.Handler("svc", f.rs.URL+"/mcp", &config.OAuth{ClientID: "cid", Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	tr := &mcp.StreamableClientTransport{Endpoint: f.rs.URL + "/mcp", OAuthHandler: h, MaxRetries: -1}
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
	if f.scope != "read offline_access" {
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
