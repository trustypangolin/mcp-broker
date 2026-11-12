package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/identity"
)

// ErrNotOffered is returned by Invoke for a meta-tool the agent's role does not give it, or
// one that does not exist: the HTTP API offers an agent exactly what its MCP server does.
var ErrNotOffered = errors.New("not available to this agent")

// Invoke runs the meta-tool named tool for id with args (the tool's JSON arguments), the
// way the HTTP API reaches the gateway. It is the same handler an MCP client reaches, so
// policy, approval, audit, redaction and truncation are identical. The call has no MCP
// session: approvals use the agent's out-of-band channels (tray, dashboard) and come back
// as pending_approval, and a downstream server's elicitation is declined. client names the
// caller in the audit log.
func (g *Gateway) Invoke(ctx context.Context, id identity.Identity, tool string, args json.RawMessage, client string) (*mcp.CallToolResult, error) {
	groups := g.groupsFor(id.Agent)
	for _, t := range g.metaTools() {
		if t.tool.Name != tool {
			continue
		}
		if !groups[t.group] {
			return nil, ErrNotOffered
		}
		params := &mcp.CallToolParamsRaw{Name: tool, Arguments: args}
		if client != "" {
			params.Meta = mcp.Meta{mcp.MetaKeyClientInfo: &mcp.Implementation{Name: client}}
		}
		req := &mcp.CallToolRequest{
			Params: params,
			Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{
				UserID: id.Agent + "/" + id.TokenID,
				Extra:  map[string]any{IdentityKey: id},
			}},
		}
		return t.handler(ctx, req)
	}
	return nil, ErrNotOffered
}

// Offered is the meta-tools agent's role gives it, by name: what its MCP server lists.
func (g *Gateway) Offered(agent string) map[string]bool {
	groups := g.groupsFor(agent)
	out := map[string]bool{}
	for _, t := range g.metaTools() {
		if groups[t.group] {
			out[t.tool.Name] = true
		}
	}
	return out
}

// AwaitApproval waits up to d for agent's pending approval approvalID to be decided, so a
// caller polling over HTTP can long-poll instead of retrying in a tight loop. It returns
// at once for an approval that is unknown, someone else's, or already decided; the caller
// then reads the status through approval_status as usual.
func (g *Gateway) AwaitApproval(ctx context.Context, agent, approvalID string, d time.Duration) {
	q := g.opts.Approvals
	if q == nil || d <= 0 {
		return
	}
	e, ok := q.Get(approvalID)
	if !ok || e.Agent != agent || e.Status != approval.Pending {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	q.Wait(wctx, approvalID)
}
