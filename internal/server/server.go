// Package server is the broker's HTTP front door: every request is identified (network
// gate, token, match conditions) before the MCP handler sees it, and each MCP session is
// bound to the token that opened it.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/version"
)

// Options configures Handler.
type Options struct {
	Config     *config.Config
	Identifier *identity.Identifier
	Gateway    *gateway.Gateway
	Audit      *audit.Log
	Logger     *slog.Logger
	// Admin, if set, mounts the admin API (approvals now; config later).
	Admin *AdminOptions
}

type identKey struct{}

// Handler returns the broker's HTTP handler: /mcp (MCP Streamable HTTP) and /healthz.
func Handler(o Options) http.Handler {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	srv := o.Gateway.Server()
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		SessionTimeout: time.Hour,
		Logger:         o.Logger,
	})
	// RequireBearerToken is the SDK's hook for binding sessions to a user: a session opened
	// by one agent's token rejects requests carrying another's.
	bound := auth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		id := ctx.Value(identKey{}).(identity.Identity)
		return &auth.TokenInfo{UserID: id.Agent + "/" + id.TokenID, Extra: map[string]any{gateway.IdentityKey: id}}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(mcpHandler)

	g := &gate{o: o, refusals: map[string]time.Time{}}
	mux := http.NewServeMux()
	mux.Handle("/mcp", g.identify(bound))
	mux.HandleFunc("/healthz", g.health)
	if o.Admin != nil {
		MountAdmin(mux, *o.Admin)
	}
	return mux
}

type gate struct {
	o Options

	mu       sync.Mutex
	refusals map[string]time.Time // source+reason → last audited, to keep a retry loop from flooding the log
}

func addrs(r *http.Request) (remote, local netip.AddrPort, ok bool) {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return remote, local, false
	}
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if la == nil {
		return remote, local, false
	}
	local, err = netip.ParseAddrPort(la.String())
	return remote, local, err == nil
}

func (g *gate) identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote, local, ok := addrs(r)
		if !ok {
			http.Error(w, "cannot determine connection addresses", http.StatusInternalServerError)
			return
		}
		token := identity.BearerToken(r.Header.Get("Authorization"))
		id, err := g.o.Identifier.Identify(remote, local, token)
		if err == nil && id.Anonymous() {
			err = &identity.Error{Status: http.StatusUnauthorized, Public: "token required", Detail: "no token from " + remote.Addr().String()}
		}
		if err != nil {
			ie, _ := err.(*identity.Error)
			if ie == nil {
				ie = &identity.Error{Status: http.StatusInternalServerError, Public: "identification failed", Detail: err.Error()}
			}
			g.refused(remote.Addr(), ie)
			if ie.Status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="mcp-broker"`)
			}
			http.Error(w, ie.Public, ie.Status)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identKey{}, id)))
	})
}

func (g *gate) refused(src netip.Addr, e *identity.Error) {
	key := src.String() + "|" + e.Detail
	g.mu.Lock()
	last, seen := g.refusals[key]
	if seen && time.Since(last) < time.Minute {
		g.mu.Unlock()
		return
	}
	g.refusals[key] = time.Now()
	g.mu.Unlock()
	if g.o.Audit != nil {
		g.o.Audit.Write(audit.Event{Kind: audit.KindConnection, Source: src.String(), Decision: "refused", Error: e.Detail})
	}
}

// health answers allowed sources only, without a token, so the shim and tray can tell
// whether a broker is running before they have credentials sorted out.
func (g *gate) health(w http.ResponseWriter, r *http.Request) {
	remote, _, ok := addrs(r)
	if !ok || !config.ContainsAddr(g.o.Config.Server.AllowedSources, remote.Addr()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": version.Version})
}
