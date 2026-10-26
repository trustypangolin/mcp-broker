package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/supervisor"
)

func (g *Gateway) addProfileTools(s *mcp.Server) {
	s.AddTool(&mcp.Tool{
		Name:        "list_profiles",
		Description: "List app profiles (long-running local apps such as ComfyUI) you may use, with their state.",
		InputSchema: schema(nil),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, g.listProfiles)
	s.AddTool(&mcp.Tool{
		Name: "profile",
		Description: "Act on an app profile. action: status | logs | plan | start | stop | update. " +
			"start returns at once with state starting; poll status until ready. plan shows what start, stop or update would do. " +
			"start, stop and update may need the user's approval.",
		InputSchema: schema(map[string]any{
			"name":         str("Profile name from list_profiles"),
			"action":       map[string]any{"type": "string", "enum": []string{"status", "logs", "plan", "start", "stop", "update"}},
			"tail":         map[string]any{"type": "integer", "description": "logs: how many lines (default 100, max 1000)"},
			"for":          map[string]any{"type": "string", "enum": []string{"start", "stop", "update"}, "description": "plan: which action (default start)"},
			"kill_foreign": map[string]any{"type": "boolean", "description": "stop: kill the process holding the profile's port that the broker did not start (always needs approval)"},
			"approval_id":  str("Approval id from an earlier pending_approval result"),
		}, "name", "action"),
	}, g.profileTool)
}

type profileInfo struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	State       supervisor.State `json:"state"`
	Ports       []int            `json:"ports,omitempty"`
}

func (g *Gateway) profileVisible(agent, name string) bool {
	if _, ok := g.st().cfg.Profiles[name]; !ok || g.opts.Supervisor == nil {
		return false
	}
	return g.st().pol.Profile(agent, name, "status", false).Visible
}

func (g *Gateway) listProfiles(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := g.opts.Identity(req)
	out := []profileInfo{}
	if g.opts.Supervisor != nil {
		for _, st := range g.opts.Supervisor.List() {
			if g.profileVisible(id.Agent, st.Profile) {
				out = append(out, profileInfo{st.Profile, st.Description, st.State, st.Ports})
			}
		}
	}
	return jsonResult(map[string]any{"profiles": out}), nil
}

type profileArgs struct {
	Name        string `json:"name"`
	Action      string `json:"action"`
	Tail        int    `json:"tail"`
	For         string `json:"for"`
	KillForeign bool   `json:"kill_foreign"`
	ApprovalID  string `json:"approval_id"`
}

func (g *Gateway) profileTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a profileArgs
	if err := decodeArgs(req, &a); err != nil {
		return textResult(true, "%v", err), nil
	}
	id := g.opts.Identity(req)
	ev := audit.Event{Kind: audit.KindToolCall, Agent: id.Agent, Source: id.Source.String(), Profile: a.Name, Action: a.Action}
	if !g.profileVisible(id.Agent, a.Name) {
		ev.Decision, ev.Detail = string(config.Deny), "profile not visible"
		g.write(ev)
		return textResult(true, "unknown profile %q", a.Name), nil // hidden and missing look alike
	}
	sup := g.opts.Supervisor

	switch a.Action {
	case "status":
		if res := g.gate(ctx, req, id, &ev, a, "status", supervisor.Plan{}); res != nil {
			return res, nil
		}
		return jsonResult(sup.Status(a.Name)), nil

	case "logs":
		if res := g.gate(ctx, req, id, &ev, a, "logs", supervisor.Plan{}); res != nil {
			return res, nil
		}
		n := a.Tail
		if n <= 0 {
			n = 100
		}
		lines := sup.Logs(a.Name, min(n, 1000))
		if len(lines) == 0 {
			return textResult(false, "no output yet for %s", a.Name), nil
		}
		return textResult(false, "%s", strings.Join(lines, "\n")), nil

	case "plan":
		if res := g.gate(ctx, req, id, &ev, a, "plan", supervisor.Plan{}); res != nil {
			return res, nil
		}
		what := a.For
		if what == "" {
			what = "start"
		}
		pl, err := sup.Plan(a.Name, what)
		if err != nil {
			return textResult(true, "%v", err), nil
		}
		res := textResult(false, "%s", pl.Text())
		res.StructuredContent = pl
		return res, nil

	case "start", "update":
		pl, err := sup.Plan(a.Name, a.Action)
		if err != nil {
			return textResult(true, "%v", err), nil
		}
		if len(pl.Blocking) > 0 {
			ev.Decision, ev.Detail = "blocked", strings.Join(pl.Blocking, "; ")
			g.write(ev)
			msg := pl.Text()
			if pl.ForeignPID != 0 {
				msg += "\nTo free the port, call profile with action stop and kill_foreign true (the user must approve)."
			}
			return textResult(true, "%s", msg), nil
		}
		if res := g.gate(ctx, req, id, &ev, a, a.Action, pl); res != nil {
			return res, nil
		}
		var st supervisor.Status
		if a.Action == "start" {
			st, err = sup.Start(a.Name)
		} else {
			st, err = sup.Update(a.Name)
		}
		if err != nil {
			return textResult(true, "%s %s failed: %s", a.Action, a.Name, g.red.String(err.Error())), nil
		}
		res := jsonResult(st)
		res.Content = []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%s %s: %s (pid %d). Poll profile status until it is ready; logs shows its output.", a.Action, a.Name, st.State, st.PID)}}
		return res, nil

	case "stop":
		if a.KillForeign {
			pl, err := sup.Plan(a.Name, "start")
			if err != nil {
				return textResult(true, "%v", err), nil
			}
			if pl.ForeignPID == 0 {
				return textResult(true, "no foreign process holds a port of %s", a.Name), nil
			}
			pl.Hash += ":" + strconv.Itoa(pl.ForeignPID) // the approval names this PID
			if res := g.gate(ctx, req, id, &ev, a, "kill_foreign", pl); res != nil {
				return res, nil
			}
			if err := sup.KillForeign(a.Name, pl.ForeignPID); err != nil {
				return textResult(true, "%v", err), nil
			}
			return textResult(false, "killed pid %d (%s), which held a port of %s", pl.ForeignPID, pl.ForeignPath, a.Name), nil
		}
		pl, err := sup.Plan(a.Name, "stop")
		if err != nil {
			return textResult(true, "%v", err), nil
		}
		if len(pl.Blocking) > 0 {
			msg := strings.Join(pl.Blocking, "; ")
			if start, err := sup.Plan(a.Name, "start"); err == nil && start.ForeignPID != 0 {
				msg += fmt.Sprintf(". Its port is held by pid %d (%s), which the broker did not start; use kill_foreign to stop it (needs approval).", start.ForeignPID, start.ForeignPath)
			}
			return textResult(true, "%s", msg), nil
		}
		if res := g.gate(ctx, req, id, &ev, a, "stop", pl); res != nil {
			return res, nil
		}
		st, err := sup.Stop(ctx, a.Name)
		if err != nil && !errors.Is(err, supervisor.ErrNotRunning) {
			return textResult(true, "stop %s: %s", a.Name, err), nil
		}
		return jsonResult(st), nil
	}
	return textResult(true, "unknown action %q (want status, logs, plan, start, stop or update)", a.Action), nil
}

// gate applies policy to a profile action and, when it says approve, gets the approval.
// It returns nil when the action may go ahead, else the result to send back.
func (g *Gateway) gate(ctx context.Context, req *mcp.CallToolRequest, id identity.Identity, ev *audit.Event, a profileArgs, action string, pl supervisor.Plan) *mcp.CallToolResult {
	dec := g.st().pol.Profile(id.Agent, a.Name, action, pl.NetworkExposed)
	ev.Decision, ev.Detail = string(dec.Action), dec.Reason
	switch dec.Action {
	case config.Allow:
		g.write(*ev)
		return nil
	case config.Approve:
		summary := fmt.Sprintf("%s wants to %s", id.Agent, pl.Summary())
		if action == "kill_foreign" {
			summary = fmt.Sprintf("%s wants to kill pid %d (%s), which holds a port of %s and was not started by the broker", id.Agent, pl.ForeignPID, pl.ForeignPath, a.Name)
		}
		res, ch, ok := g.approveFor(ctx, req, id, approvalSpec{
			plan: approval.Plan{
				Kind: "profile." + action, Agent: id.Agent, Profile: a.Name, Action: action, ArgsHash: pl.Hash,
				Summary: g.red.String(summary), Detail: pl.Text(), Reason: dec.Reason,
			},
			approvalID: a.ApprovalID,
			label:      action + " " + a.Name,
			retry:      "call profile again with the same name and action",
		})
		ev.Channel = string(ch)
		if !ok {
			ev.Detail = "not approved"
			g.write(*ev)
			return res
		}
		ev.Detail = "approved"
		g.write(*ev)
		return nil
	}
	g.write(*ev)
	return textResult(true, "%s %s is not allowed for %s (%s)", action, a.Name, id.Agent, dec.Reason)
}
