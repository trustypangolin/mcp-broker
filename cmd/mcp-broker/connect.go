package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

const connectUsage = `usage: mcp-broker connect [--agent NAME] [--url URL] [--token-file PATH] [--config PATH]

Bridges a stdio MCP client (Claude Desktop, Claude Code, VS Code) to the running broker.

Token, first found:  $MCP_BROKER_TOKEN, --token-file, agent-tokens/<agent>.token next to
                     broker.yaml, <user config dir>/mcp-broker/token
URL, first found:    --url, $MCP_BROKER_URL, server.listen from broker.yaml, the Windows
                     host when running under WSL, http://127.0.0.1:8765/mcp
`

func cmdConnect(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, connectUsage) }
	agent := fs.String("agent", "", "agent name (selects agent-tokens/<agent>.token)")
	urlFlag := fs.String("url", "", "broker MCP endpoint")
	tokenFile := fs.String("token-file", "", "file holding the agent token")
	cfgFlag := fs.String("config", "", "broker.yaml, to locate token and URL")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	var cfg *config.Config
	if p, err := config.ResolvePath(*cfgFlag); err == nil {
		cfg, _ = config.Load(p) // optional: WSL and containers usually have no broker.yaml
	}
	token, err := findToken(*tokenFile, *agent, cfg)
	if err != nil {
		return err
	}
	endpoint := findURL(*urlFlag, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pv := &protocolVersion{}
	httpClient := &http.Client{Transport: shimTransport{token: token, pv: pv}}
	down, err := (&mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true}).Connect(ctx)
	if err != nil {
		return fmt.Errorf("connecting to broker at %s: %w", endpoint, err)
	}
	up, err := (&mcp.StdioTransport{}).Connect(ctx)
	if err != nil {
		return err
	}
	return relay(ctx, up, down, pv, stderr)
}

func findToken(file, agent string, cfg *config.Config) (string, error) {
	if t := strings.TrimSpace(os.Getenv("MCP_BROKER_TOKEN")); t != "" {
		return t, nil
	}
	var candidates []string
	if file != "" {
		candidates = append(candidates, file)
	}
	if agent != "" && cfg != nil {
		candidates = append(candidates, AgentTokenPath(cfg, agent))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "mcp-broker", "token"))
	}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				return t, nil
			}
		}
	}
	return "", fmt.Errorf("no agent token: set MCP_BROKER_TOKEN or create one with 'mcp-broker token create <agent> --save' (looked in %s)",
		strings.Join(candidates, ", "))
}

func findURL(flagURL string, cfg *config.Config) string {
	if flagURL != "" {
		return flagURL
	}
	if u := os.Getenv("MCP_BROKER_URL"); u != "" {
		return u
	}
	if cfg != nil {
		if host, port, err := net.SplitHostPort(cfg.Server.Listen); err == nil {
			if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
				host = "127.0.0.1"
			}
			scheme := "http"
			if cfg.Server.TLS.CertFile != "" {
				scheme = "https"
			}
			return fmt.Sprintf("%s://%s/mcp", scheme, net.JoinHostPort(host, port))
		}
	}
	if gw, ok := wslHost(); ok {
		return fmt.Sprintf("http://%s/mcp", net.JoinHostPort(gw, "8765"))
	}
	return "http://127.0.0.1:8765/mcp"
}

// wslHost returns the Windows host's address from inside a WSL NAT distro: the default
// gateway in /proc/net/route.
func wslHost() (string, bool) {
	if os.Getenv("WSL_DISTRO_NAME") == "" {
		return "", false
	}
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(fields[2])
		if err != nil || len(b) != 4 {
			continue
		}
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, binary.LittleEndian.Uint32(b))
		return ip.String(), true
	}
	return "", false
}

// protocolVersion carries the version negotiated in initialize to the HTTP layer: the SDK
// transport normally learns it from its own client session, which a raw relay does not have.
type protocolVersion struct {
	mu sync.Mutex
	v  string
}

func (p *protocolVersion) get() string  { p.mu.Lock(); defer p.mu.Unlock(); return p.v }
func (p *protocolVersion) set(v string) { p.mu.Lock(); p.v = v; p.mu.Unlock() }

type shimTransport struct {
	token string
	pv    *protocolVersion
}

func (t shimTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	if r.Header.Get("Mcp-Protocol-Version") == "" {
		if v := t.pv.get(); v != "" {
			r.Header.Set("Mcp-Protocol-Version", v)
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// relay copies JSON-RPC messages both ways until either side closes. It watches for the
// initialize exchange only to learn the negotiated protocol version.
func relay(ctx context.Context, up, down mcp.Connection, pv *protocolVersion, stderr io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer up.Close()
	defer down.Close()

	var initMu sync.Mutex
	initIDs := map[string]bool{}
	idKey := func(id jsonrpc.ID) string { return fmt.Sprintf("%T:%v", id.Raw(), id.Raw()) }

	errc := make(chan error, 2)
	go func() {
		for {
			msg, err := up.Read(ctx)
			if err != nil {
				errc <- err
				return
			}
			if req, ok := msg.(*jsonrpc.Request); ok && req.Method == "initialize" && req.ID.IsValid() {
				initMu.Lock()
				initIDs[idKey(req.ID)] = true
				initMu.Unlock()
			}
			if err := down.Write(ctx, msg); err != nil {
				errc <- fmt.Errorf("to broker: %w", err)
				return
			}
		}
	}()
	go func() {
		for {
			msg, err := down.Read(ctx)
			if err != nil {
				errc <- fmt.Errorf("from broker: %w", err)
				return
			}
			if resp, ok := msg.(*jsonrpc.Response); ok && resp.Error == nil {
				initMu.Lock()
				isInit := initIDs[idKey(resp.ID)]
				delete(initIDs, idKey(resp.ID))
				initMu.Unlock()
				if isInit {
					var r struct {
						ProtocolVersion string `json:"protocolVersion"`
					}
					if json.Unmarshal(resp.Result, &r) == nil {
						pv.set(r.ProtocolVersion)
					}
				}
			}
			if err := up.Write(ctx, msg); err != nil {
				errc <- err
				return
			}
		}
	}()

	err := <-errc
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	fmt.Fprintln(stderr, "mcp-broker connect:", err)
	return errUsage
}
