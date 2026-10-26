package identity

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/glob"
	"github.com/trustypangolin/mcp-broker/internal/procinfo"
)

// Identity is who a connection is.
type Identity struct {
	Agent   string // empty for anonymous
	Role    string
	TokenID string
	Source  netip.Addr
	PID     int      // set when the agent has process conditions
	Process []string // executable chain, nearest first, when PID is set
}

// Anonymous reports whether no agent was identified.
func (i Identity) Anonymous() bool { return i.Agent == "" }

// Error is a refusal. Public is safe to send to the client; Detail is for the audit log
// only, since it says exactly which check failed.
type Error struct {
	Status int
	Public string
	Detail string
}

func (e *Error) Error() string { return e.Detail }

func refuse(status int, public, format string, args ...any) *Error {
	return &Error{Status: status, Public: public, Detail: fmt.Sprintf(format, args...)}
}

// Identifier resolves connections against one config and the token store.
type Identifier struct {
	cfg   *config.Config
	store *Store

	// Seams for tests; procinfo on a real host.
	connOwner func(client, server netip.AddrPort) (int, error)
	chain     func(pid int) ([]procinfo.Proc, error)
}

// New returns an Identifier.
func New(cfg *config.Config, store *Store) *Identifier {
	return &Identifier{cfg: cfg, store: store, connOwner: procinfo.ConnOwner, chain: procinfo.Chain}
}

// BearerToken extracts the token from an Authorization header value.
func BearerToken(header string) string {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// Identify decides who is on the other end of a connection. remote is the client's address
// (http.Request.RemoteAddr) and local the server's (http.LocalAddrContextKey).
func (id *Identifier) Identify(remote, local netip.AddrPort, token string) (Identity, error) {
	src := remote.Addr().Unmap()
	if !config.ContainsAddr(id.cfg.Server.AllowedSources, src) {
		return Identity{}, refuse(http.StatusForbidden, "source not allowed", "source %s not in server.allowed_sources", src)
	}

	if token == "" {
		if src.IsLoopback() {
			return Identity{Role: config.AnonymousRole, Source: src}, nil
		}
		return Identity{}, refuse(http.StatusUnauthorized, "token required", "no token from non-loopback %s", src)
	}

	entry, ok := id.store.Lookup(token)
	if !ok {
		return Identity{}, refuse(http.StatusUnauthorized, "invalid token", "unknown or revoked token from %s", src)
	}
	agent, ok := id.cfg.Agents[entry.Agent]
	if !ok {
		return Identity{}, refuse(http.StatusUnauthorized, "invalid token", "token %s names agent %q which is not in config", entry.ID, entry.Agent)
	}
	ident := Identity{Agent: entry.Agent, Role: agent.Role, TokenID: entry.ID, Source: src}

	const notHere = "token not valid from here"
	if len(agent.Match.From) > 0 && !config.ContainsAddr(agent.Match.From, src) {
		return Identity{}, refuse(http.StatusForbidden, notHere, "agent %s token used from %s, outside match.from", entry.Agent, src)
	}

	if len(agent.Match.Process) > 0 {
		if !src.IsLoopback() {
			return Identity{}, refuse(http.StatusForbidden, notHere, "agent %s has process conditions but connected from non-loopback %s", entry.Agent, src)
		}
		pid, err := id.connOwner(remote, local)
		if err != nil {
			return Identity{}, refuse(http.StatusForbidden, notHere, "agent %s: cannot find owning process: %v", entry.Agent, err)
		}
		procs, err := id.chain(pid)
		if err != nil {
			return Identity{}, refuse(http.StatusForbidden, notHere, "agent %s: cannot read process chain of pid %d: %v", entry.Agent, pid, err)
		}
		ident.PID = pid
		for _, p := range procs {
			ident.Process = append(ident.Process, p.Path)
		}
		if !matchesChain(agent.Match.Process, procs) {
			return Identity{}, refuse(http.StatusForbidden, notHere, "agent %s token used by pid %d (%s), which matches no match.process pattern",
				entry.Agent, pid, strings.Join(ident.Process, " < "))
		}
	}
	return ident, nil
}

func matchesChain(patterns []string, procs []procinfo.Proc) bool {
	for _, p := range procs {
		if p.Path == "" {
			continue
		}
		for _, pat := range patterns {
			if glob.Match(pat, p.Path) {
				return true
			}
		}
	}
	return false
}
