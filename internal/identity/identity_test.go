package identity

import (
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/procinfo"
)

func TestStoreCreateLookupRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.yaml")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, e, err := s.Create("vscode", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "mcpb_vscode_") || len(tok) < 50 {
		t.Errorf("token shape %q", tok)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), tok) || strings.Contains(string(data), tok[len("mcpb_vscode_"):]) {
		t.Fatal("raw token written to tokens.yaml")
	}

	got, ok := s.Lookup(tok)
	if !ok || got.Agent != "vscode" || got.ID != e.ID {
		t.Fatalf("lookup = %+v %v", got, ok)
	}
	if _, ok := s.Lookup(tok + "x"); ok {
		t.Error("altered token accepted")
	}
	if _, ok := s.Lookup("not-a-token"); ok {
		t.Error("garbage accepted")
	}

	tok2, _, _ := s.Create("vscode", "")
	if n, _ := s.Revoke(e.ID); n != 1 {
		t.Fatalf("revoke by id removed %d", n)
	}
	if _, ok := s.Lookup(tok); ok {
		t.Error("revoked token still valid")
	}
	if _, ok := s.Lookup(tok2); !ok {
		t.Error("other token revoked too")
	}
	if n, _ := s.Revoke("vscode"); n != 1 {
		t.Errorf("revoke by agent removed %d", n)
	}
}

// A revoke written by another process (the tray, the CLI) reaches a running broker.
func TestStoreSeesExternalRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.yaml")
	broker, _ := OpenStore(path)
	cli, _ := OpenStore(path)

	tok, _, _ := cli.Create("wsl", "")
	broker.checked = time.Time{}
	if _, ok := broker.Lookup(tok); !ok {
		t.Fatal("broker does not see new token")
	}
	cli.Revoke("wsl")
	broker.checked = time.Time{}
	if _, ok := broker.Lookup(tok); ok {
		t.Fatal("broker still accepts revoked token")
	}
}

func TestStoreFailsClosedOnCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.yaml")
	s, _ := OpenStore(path)
	tok, _, _ := s.Create("wsl", "")
	os.WriteFile(path, []byte("tokens: [this is: not: valid"), 0o600)
	s.checked = time.Time{}
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("corrupt store still accepted a token")
	}
}

func TestBearerToken(t *testing.T) {
	if BearerToken("Bearer abc") != "abc" || BearerToken("bearer  abc ") != "abc" || BearerToken("Basic abc") != "" || BearerToken("") != "" {
		t.Error("BearerToken parsing wrong")
	}
}

const testConfig = `
server:
  allowed_sources: [127.0.0.0/8, "::1/128", 172.16.0.0/12, 192.168.0.0/16]
agents:
  vscode:
    role: coder
    match:
      process: ["*\\Code.exe"]
  wsl:
    role: sandbox
    match:
      from: [172.16.0.0/12]
  lan:
    role: coder
roles:
  coder: {}
  sandbox: {}
`

type fixture struct {
	id     *Identifier
	tokens map[string]string
	pid    int
	chain  []procinfo.Proc
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg, err := config.Parse([]byte(testConfig), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, _ := OpenStore(filepath.Join(t.TempDir(), "tokens.yaml"))
	f := &fixture{tokens: map[string]string{}, pid: 4242}
	for _, a := range []string{"vscode", "wsl", "lan", "removed"} {
		tok, _, _ := store.Create(a, "")
		f.tokens[a] = tok
	}
	f.id = New(cfg, store)
	f.id.connOwner = func(netip.AddrPort, netip.AddrPort) (int, error) { return f.pid, nil }
	f.id.chain = func(pid int) ([]procinfo.Proc, error) { return f.chain, nil }
	return f
}

var (
	loopClient = netip.MustParseAddrPort("127.0.0.1:50000")
	loopServer = netip.MustParseAddrPort("127.0.0.1:8765")
	wslClient  = netip.MustParseAddrPort("172.22.1.5:50000")
	lanClient  = netip.MustParseAddrPort("192.168.1.20:50000")
	wanClient  = netip.MustParseAddrPort("203.0.113.9:50000")
	srv        = netip.MustParseAddrPort("192.168.1.2:8765")
)

func status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func TestIdentify(t *testing.T) {
	f := newFixture(t)
	code := []procinfo.Proc{{PID: 4242, Path: `C:\Users\me\.vscode\ext\claude.exe`}, {PID: 1, Path: `C:\Program Files\Microsoft VS Code\Code.exe`}}
	other := []procinfo.Proc{{PID: 4242, Path: `C:\Tools\curl.exe`}, {PID: 1, Path: `C:\Windows\explorer.exe`}}

	cases := []struct {
		name          string
		remote, local netip.AddrPort
		token         string
		chain         []procinfo.Proc
		wantAgent     string
		wantStatus    int
	}{
		{"anonymous loopback", loopClient, loopServer, "", nil, "", 0},
		{"no token from LAN", lanClient, srv, "", nil, "", http.StatusUnauthorized},
		{"outside allowed_sources", wanClient, srv, f.tokens["lan"], nil, "", http.StatusForbidden},
		{"bad token", lanClient, srv, "mcpb_lan_nope", nil, "", http.StatusUnauthorized},
		{"agent removed from config", loopClient, loopServer, f.tokens["removed"], nil, "", http.StatusUnauthorized},
		{"vscode from Code.exe chain", loopClient, loopServer, f.tokens["vscode"], code, "vscode", 0},
		{"vscode token stolen by curl", loopClient, loopServer, f.tokens["vscode"], other, "", http.StatusForbidden},
		{"vscode token from LAN", lanClient, srv, f.tokens["vscode"], code, "", http.StatusForbidden},
		{"wsl from WSL range", wslClient, srv, f.tokens["wsl"], nil, "wsl", 0},
		{"wsl token from LAN", lanClient, srv, f.tokens["wsl"], nil, "", http.StatusForbidden},
		{"unrestricted lan agent", lanClient, srv, f.tokens["lan"], nil, "lan", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.chain = c.chain
			ident, err := f.id.Identify(c.remote, c.local, c.token)
			if got := status(err); got != c.wantStatus {
				t.Fatalf("status = %d (%v), want %d", got, err, c.wantStatus)
			}
			if err == nil && ident.Agent != c.wantAgent {
				t.Fatalf("agent = %q, want %q", ident.Agent, c.wantAgent)
			}
			if err != nil {
				var e *Error
				errors.As(err, &e)
				if strings.Contains(e.Public, "Code.exe") || strings.Contains(e.Public, "curl") {
					t.Errorf("public message leaks detail: %q", e.Public)
				}
			}
		})
	}
}

func TestIdentifyFailsClosedWhenOwnerUnknown(t *testing.T) {
	f := newFixture(t)
	f.id.connOwner = func(netip.AddrPort, netip.AddrPort) (int, error) { return 0, procinfo.ErrNotFound }
	if _, err := f.id.Identify(loopClient, loopServer, f.tokens["vscode"]); status(err) != http.StatusForbidden {
		t.Fatalf("err = %v, want 403", err)
	}
}
