package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/skillgen"
	"github.com/trustypangolin/mcp-broker/internal/version"
	"github.com/trustypangolin/mcp-broker/skills"
)

// The agent HTTP API (/api/v1) is the meta-tools over plain HTTP + JSON, for agents that
// cannot run an MCP client. Every endpoint runs exactly one meta-tool through
// Gateway.Invoke, behind the same identify gate as /mcp, so token, match conditions,
// policy, approvals, audit and redaction are the ones an MCP client gets. It is not an
// admin API: nothing here changes config, rules or tokens.

// apiMaxBody caps a request body; tool arguments may carry file contents.
const apiMaxBody = 32 << 20

// apiMaxWait caps ?wait= on approval polling.
const apiMaxWait = 120 * time.Second

// apiRoute is one endpoint: the meta-tool it runs and how its arguments are read from the
// request. TestSkillDocumentsEveryRoute keeps skills/mcp-broker-api/SKILL.md in step.
type apiRoute struct {
	method, path string
	tool         string // meta-tool name, or "" for the index
	args         func(r *http.Request, body map[string]any) (map[string]any, error)
}

func apiRoutes() []apiRoute {
	return []apiRoute{
		{"GET", "/api/v1/connectors", "list_connectors", func(*http.Request, map[string]any) (map[string]any, error) {
			return map[string]any{}, nil
		}},
		{"GET", "/api/v1/connectors/{name}/tools", "load_connector", func(r *http.Request, _ map[string]any) (map[string]any, error) {
			a := map[string]any{"name": r.PathValue("name")}
			if d := r.URL.Query().Get("detail"); d != "" {
				a["detail"] = d
			}
			return a, nil
		}},
		{"GET", "/api/v1/connectors/{name}/tools/{tool}", "describe_tool", func(r *http.Request, _ map[string]any) (map[string]any, error) {
			return map[string]any{"connector": r.PathValue("name"), "tool": r.PathValue("tool")}, nil
		}},
		{"POST", "/api/v1/connectors/{name}/tools/{tool}/call", "call_tool", func(r *http.Request, body map[string]any) (map[string]any, error) {
			a := map[string]any{"connector": r.PathValue("name"), "tool": r.PathValue("tool")}
			for k, v := range body {
				switch k {
				case "arguments", "approval_id":
					a[k] = v
				default:
					return nil, fmt.Errorf("unknown field %q (the body is {\"arguments\": {...}, \"approval_id\": \"...\"})", k)
				}
			}
			return a, nil
		}},
		{"POST", "/api/v1/connectors/{name}/unload", "unload_connector", func(r *http.Request, _ map[string]any) (map[string]any, error) {
			return map[string]any{"name": r.PathValue("name")}, nil
		}},
		{"GET", "/api/v1/approvals/{id}", "approval_status", func(r *http.Request, _ map[string]any) (map[string]any, error) {
			return map[string]any{"approval_id": r.PathValue("id")}, nil
		}},
		{"GET", "/api/v1/profiles", "list_profiles", func(*http.Request, map[string]any) (map[string]any, error) {
			return map[string]any{}, nil
		}},
		{"GET", "/api/v1/profiles/{name}", "profile", func(r *http.Request, _ map[string]any) (map[string]any, error) {
			return map[string]any{"name": r.PathValue("name"), "action": "status"}, nil
		}},
		{"GET", "/api/v1/profiles/{name}/logs", "profile", func(r *http.Request, _ map[string]any) (map[string]any, error) {
			a := map[string]any{"name": r.PathValue("name"), "action": "logs"}
			if t := r.URL.Query().Get("tail"); t != "" {
				n, err := strconv.Atoi(t)
				if err != nil {
					return nil, fmt.Errorf("tail must be a number, not %q", t)
				}
				a["tail"] = n
			}
			return a, nil
		}},
		{"GET", "/api/v1/profiles/{name}/plan", "profile", func(r *http.Request, _ map[string]any) (map[string]any, error) {
			a := map[string]any{"name": r.PathValue("name"), "action": "plan"}
			if f := r.URL.Query().Get("for"); f != "" {
				a["for"] = f
			}
			return a, nil
		}},
		{"POST", "/api/v1/profiles/{name}/{action}", "profile", func(r *http.Request, body map[string]any) (map[string]any, error) {
			act := r.PathValue("action")
			if act != "start" && act != "stop" && act != "update" {
				return nil, errNotFound
			}
			a := map[string]any{"name": r.PathValue("name"), "action": act}
			for k, v := range body {
				switch k {
				case "approval_id", "kill_foreign":
					a[k] = v
				default:
					return nil, fmt.Errorf("unknown field %q (the body is {\"approval_id\": \"...\", \"kill_foreign\": true})", k)
				}
			}
			return a, nil
		}},
		{"POST", "/api/v1/rpc/{tool}", "*", func(_ *http.Request, body map[string]any) (map[string]any, error) {
			return body, nil
		}},
	}
}

var errNotFound = errors.New("not found")

// mountAPI adds /api/v1 to mux. Everything but the skill sits behind identify.
func (g *gate) mountAPI(mux *http.ServeMux) {
	for _, rt := range apiRoutes() {
		mux.Handle(rt.method+" "+rt.path, g.identify(g.apiHandler(rt)))
	}
	mux.Handle("POST /api/v1/skills", g.identify(http.HandlerFunc(g.apiSkills)))
	mux.Handle("GET /api/v1", g.identify(http.HandlerFunc(g.apiIndex)))
	mux.Handle("GET /api/v1/{$}", g.identify(http.HandlerFunc(g.apiIndex)))
	mux.HandleFunc("GET /api/v1/skill.md", g.apiSkill)
}

func apiJSON(w http.ResponseWriter, status int, v any) {
	// The charset is not optional in practice: Windows PowerShell 5.1's Invoke-RestMethod
	// decodes a bare application/json as Latin-1, garbling every non-ASCII character.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func apiError(w http.ResponseWriter, status int, format string, args ...any) {
	apiJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// readBody reads an optional JSON object body. An empty body is an empty object.
func readBody(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, apiMaxBody))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	body := map[string]any{}
	if len(strings.TrimSpace(string(b))) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, fmt.Errorf("body must be a JSON object: %w", err)
	}
	return body, nil
}

// clientName labels API calls in the audit log, with the caller's User-Agent if it sent one.
func clientName(r *http.Request) string {
	ua := strings.TrimSpace(r.UserAgent())
	if len(ua) > 80 {
		ua = ua[:80]
	}
	if ua == "" {
		return "http-api"
	}
	return "http-api (" + ua + ")"
}

func (g *gate) apiHandler(rt apiRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(identKey{}).(identity.Identity)
		var body map[string]any
		if r.Method == http.MethodPost {
			var err error
			if body, err = readBody(w, r); err != nil {
				apiError(w, http.StatusBadRequest, "%v", err)
				return
			}
		}
		args, err := rt.args(r, body)
		if errors.Is(err, errNotFound) {
			apiError(w, http.StatusNotFound, "no such endpoint: %s %s", r.Method, r.URL.Path)
			return
		}
		if err != nil {
			apiError(w, http.StatusBadRequest, "%v", err)
			return
		}
		tool := rt.tool
		if tool == "*" {
			tool = r.PathValue("tool")
		}
		if tool == "approval_status" {
			if ws := r.URL.Query().Get("wait"); ws != "" {
				n, err := strconv.Atoi(ws)
				if err != nil || n < 0 {
					apiError(w, http.StatusBadRequest, "wait is a number of seconds, not %q", ws)
					return
				}
				g.o.Gateway.AwaitApproval(r.Context(), id.Agent, r.PathValue("id"), min(time.Duration(n)*time.Second, apiMaxWait))
			}
		}
		raw, _ := json.Marshal(args)
		res, err := g.o.Gateway.Invoke(r.Context(), id, tool, raw, clientName(r))
		if errors.Is(err, gateway.ErrNotOffered) {
			apiError(w, http.StatusNotFound, "%s is not available to agent %s", tool, id.Agent)
			return
		}
		if err != nil {
			apiError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		status := http.StatusOK
		if pendingApproval(res) {
			status = http.StatusAccepted
		}
		if m, ok := res.StructuredContent.(map[string]any); ok {
			if n, ok := m["retry_after_seconds"].(int); ok {
				// Standard advice for HTTP clients on when to check a pending approval.
				w.Header().Set("Retry-After", strconv.Itoa(n))
			}
		}
		apiJSON(w, status, res)
	})
}

// pendingApproval reports whether res is the gateway's pending_approval answer.
func pendingApproval(res *mcp.CallToolResult) bool {
	m, ok := res.StructuredContent.(map[string]any)
	return ok && m["status"] == "pending_approval"
}

// apiIndex tells the caller who it is and which endpoints its role gives it.
func (g *gate) apiIndex(w http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(identKey{}).(identity.Identity)
	offered := g.o.Gateway.Offered(id.Agent)
	eps := []string{"GET /api/v1/skill.md", "POST /api/v1/skills"}
	for _, rt := range apiRoutes() {
		if rt.tool == "*" || offered[rt.tool] {
			eps = append(eps, rt.method+" "+rt.path)
		}
	}
	tools := []string{}
	for name := range offered {
		tools = append(tools, name)
	}
	slices.Sort(tools)
	apiJSON(w, http.StatusOK, map[string]any{
		"broker": "mcp-broker", "version": version.Version, "agent": id.Agent, "role": id.Role,
		"endpoints": eps, "meta_tools": tools,
	})
}

// APIBase is the /api/v1 URL as the client of r reached the broker: right for skills it
// will use from the same place (loopback, WSL, another PC).
func APIBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/v1"
}

// SkillGen is Gateway.AgentSkills, or the dashboard backend's route to it.
type SkillGen func(context.Context, string, gateway.SkillOptions) ([]skillgen.Connector, []gateway.SkillConnector)

// SkillReport is one line of the X-Mcpb-Skills header.
type SkillReport struct {
	gateway.SkillConnector
	Skill string `json:"skill"`
}

// SkillZip generates skills and writes them as a zip download. Query parameters:
//
//	connectors  comma-separated; empty means every connector agent sees ("none" for none)
//	format      a skillgen.Formats id (default claude)
//	target      broker (calls go to the broker's API, default) or direct (to each server)
//	base        the API URL the skills use (default: how this request reached the broker)
//	portable    1: the files go to another computer, so no local token file is named
//	meta        0 leaves out the mcp-broker-api skill (broker target)
//
// An empty agent makes the meta-skill alone. The X-Mcpb-Skills header carries how each
// connector went, as JSON, for a caller that wants to report it.
func SkillZip(w http.ResponseWriter, r *http.Request, gen SkillGen, cfg *config.Config, agent string) {
	q := r.URL.Query()
	o := skillgen.Options{Format: q.Get("format"), Target: skillgen.Target(q.Get("target")), Agent: agent, APIBase: q.Get("base"), Meta: q.Get("meta") != "0"}
	if o.APIBase == "" {
		o.APIBase = APIBase(r)
	}
	if _, ok := skillgen.FormatByID(o.Format); !ok {
		apiError(w, http.StatusBadRequest, "unknown format %q", o.Format)
		return
	}
	if o.Target == "" {
		o.Target = skillgen.TargetBroker
	}
	if o.Target != skillgen.TargetBroker && o.Target != skillgen.TargetDirect {
		apiError(w, http.StatusBadRequest, "target is broker or direct, not %q", o.Target)
		return
	}
	if q.Get("portable") != "1" && agent != "" {
		if p := cfg.AgentTokenPath(agent); p != "" {
			if _, err := os.Stat(p); err == nil {
				o.TokenFile = filepath.ToSlash(p)
			}
		}
	}
	var conns []skillgen.Connector
	var report []SkillReport
	if sel := q.Get("connectors"); agent != "" && sel != "none" {
		opts := gateway.SkillOptions{Direct: o.Target == skillgen.TargetDirect}
		for c := range strings.SplitSeq(sel, ",") {
			if c = strings.TrimSpace(c); c != "" {
				opts.Connectors = append(opts.Connectors, c)
			}
		}
		var rep []gateway.SkillConnector
		conns, rep = gen(r.Context(), agent, opts)
		names := skillgen.SkillNames(conns)
		for i, c := range rep {
			report = append(report, SkillReport{c, names[i]})
		}
	}
	if len(conns) == 0 && !(o.Meta && o.Target == skillgen.TargetBroker) {
		apiError(w, http.StatusBadRequest, "nothing to generate: no connectors selected (or visible), and no meta-skill")
		return
	}
	files, err := skillgen.Build(conns, o)
	if err == nil {
		var data []byte
		if data, err = skillgen.Zip(files); err == nil {
			rj, _ := json.Marshal(report)
			name := "mcp-broker-skills"
			if agent != "" {
				name += "-" + agent
			}
			name += "-" + o.Format
			if o.Format == "" {
				name += "claude"
			}
			if o.Target == skillgen.TargetDirect {
				name += "-direct"
			}
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.zip"`)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Mcpb-Skills", string(rj))
			w.Write(data)
			return
		}
	}
	apiError(w, http.StatusInternalServerError, "%v", err)
}

// apiSkills is SkillZip for the calling agent: an agent can fetch its own skills.
func (g *gate) apiSkills(w http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(identKey{}).(identity.Identity)
	if !g.o.Gateway.Offered(id.Agent)["list_connectors"] {
		apiError(w, http.StatusNotFound, "agent %s can see no connectors", id.Agent)
		return
	}
	SkillZip(w, r, g.o.Gateway.AgentSkills, g.o.Identifier.Config(), id.Agent)
}

// apiSkill serves the agent skill without a token (from allowed sources only, like
// /healthz), so an agent can learn the API before it has its token sorted out.
func (g *gate) apiSkill(w http.ResponseWriter, r *http.Request) {
	remote, _, ok := addrs(r)
	if !ok || !config.ContainsAddr(g.o.Identifier.Config().Server.AllowedSources, remote.Addr()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	io.WriteString(w, skills.BrokerAPI)
}
