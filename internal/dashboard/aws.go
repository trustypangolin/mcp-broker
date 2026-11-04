package dashboard

import (
	"fmt"
	"net/http"
	"os/exec"
	"regexp"

	"github.com/trustypangolin/mcp-broker/internal/awsconfig"
	"github.com/trustypangolin/mcp-broker/internal/config"
)

// awsEndpoint is AWS's managed MCP server. Calls to it are SigV4-signed by AWS's local
// proxy, which runs as one AWS profile; so each attached profile is its own connector,
// and roles, tags and approvals apply per account.
const awsEndpoint = "https://aws-mcp.us-east-1.api.aws/mcp"

// awsProxyArgs start the signing proxy through uvx.
var awsProxyArgs = []string{"mcp-proxy-for-aws@latest", awsEndpoint}

// awsReadTools are the tools a read-only IAM role can be trusted with: the role, not the
// broker, stops writes.
var awsReadTools = []string{"aws___run_script", "aws___get_presigned_url"}

var notName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

type awsProfileView struct {
	awsconfig.Profile
	Attached string `json:"attached,omitempty"` // connector name, if attached
	ReadOnly bool   `json:"read_only,omitempty"`
}

func (d *Dashboard) listAWS(w http.ResponseWriter, _ *http.Request) {
	ps, err := awsconfig.Load()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg := d.b.Config()
	out := []awsProfileView{}
	for _, p := range ps {
		v := awsProfileView{Profile: p}
		for name, c := range cfg.Connectors {
			if c.Env["AWS_PROFILE"] == p.Name {
				v.Attached = name
				v.ReadOnly = c.ToolClasses["aws___run_script"] == config.Read
			}
		}
		out = append(out, v)
	}
	cfgPath, _ := awsconfig.Paths()
	writeJSON(w, map[string]any{"profiles": out, "config": cfgPath, "endpoint": awsEndpoint})
}

// attachAWS writes (or rewrites) the connector for one AWS profile.
func (d *Dashboard) attachAWS(w http.ResponseWriter, r *http.Request) {
	profile := r.PathValue("profile")
	var body struct {
		ReadOnly bool `json:"read_only"`
	}
	if err := decode(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	ps, err := awsconfig.Load()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var p *awsconfig.Profile
	for i := range ps {
		if ps[i].Name == profile {
			p = &ps[i]
		}
	}
	if p == nil {
		jsonError(w, http.StatusNotFound, "no AWS profile "+profile)
		return
	}
	cfg := d.b.Config()
	name := ""
	for n, c := range cfg.Connectors {
		if c.Env["AWS_PROFILE"] == profile {
			name = n
		}
	}
	if name == "" {
		name = "aws-" + notName.ReplaceAllString(profile, "-")
		if len(name) > 64 {
			name = name[:64]
		}
		if _, taken := cfg.Connectors[name]; taken {
			jsonError(w, http.StatusConflict, "a connector named "+name+" already exists")
			return
		}
	}
	d.apply(w, "connectors", name, awsEntry(*p, body.ReadOnly))
}

func awsEntry(p awsconfig.Profile, readOnly bool) config.ConnectorEntry {
	cmd := "uvx"
	// The tray may not have the shell's PATH, so write where uvx is now.
	if path, err := exec.LookPath("uvx"); err == nil {
		cmd = path
	}
	desc := fmt.Sprintf("AWS account access as profile %s (%s)", p.Name, p.Kind)
	if readOnly {
		desc += ", read-only role"
	}
	e := config.ConnectorEntry{
		Description: desc,
		Group:       "AWS",
		Tags:        []string{"aws"},
		Command:     cmd,
		Args:        awsProxyArgs,
		Env:         map[string]string{"AWS_PROFILE": p.Name},
		IdleUnload:  "30m",
	}
	if p.Region != "" {
		e.Env["AWS_REGION"] = p.Region
	}
	if readOnly {
		e.ToolClasses = map[string]string{}
		for _, t := range awsReadTools {
			e.ToolClasses[t] = string(config.Read)
		}
	}
	return e
}
