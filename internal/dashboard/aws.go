package dashboard

import (
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/trustypangolin/mcp-broker/internal/awsconfig"
	"github.com/trustypangolin/mcp-broker/internal/config"
)

// awsEndpoint is AWS's managed MCP server, and awsSignRegion the region its requests
// are signed for. Calls to it are SigV4-signed by AWS's local
// proxy, which runs as one AWS profile; so each attached profile is its own connector,
// and roles, tags and approvals apply per account.
const (
	awsEndpoint   = "https://aws-mcp.us-east-1.api.aws/mcp"
	awsSignRegion = "us-east-1"
)

// awsProxyArgs start the signing proxy through uvx as profile. The proxy signs for
// AWS_REGION unless --region is given, so the signing region is pinned to the endpoint's;
// the profile's own region goes in as metadata, the region AWS calls default to.
func awsProxyArgs(p awsconfig.Profile) []string {
	args := []string{"mcp-proxy-for-aws@latest", awsEndpoint, "--profile", p.Name, "--region", awsSignRegion}
	if p.Region != "" {
		args = append(args, "--metadata", "AWS_REGION="+p.Region)
	}
	return args
}

// awsReadTools are the tools a read-only IAM role can be trusted with: the role, not the
// broker, stops writes.
var awsReadTools = []string{"aws___run_script", "aws___get_presigned_url"}

var notName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// awsProfileOf returns the AWS profile a connector runs as: the proxy's --profile, else
// AWS_PROFILE in its environment, else "".
func awsProfileOf(c config.Connector) string {
	for i, a := range c.Args {
		if a == "--profile" && i+1 < len(c.Args) {
			return c.Args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--profile="); ok {
			return v
		}
	}
	return c.Env["AWS_PROFILE"]
}

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
			if awsProfileOf(c) == p.Name {
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
		if awsProfileOf(c) == profile {
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
	e := awsEntry(p, body.ReadOnly)
	if old, ok := cfg.Connectors[name]; ok {
		e = keepLabels(e, old)
	}
	d.apply(w, "connectors", name, e)
}

// awsDescPrefix starts every description awsEntry writes, old and new, so attaching again
// can tell its own text from one a person wrote.
const awsDescPrefix = "AWS account"

// keepLabels carries what a person set on an attached connector into its rewritten entry:
// a description they wrote, their aliases, and the group.
func keepLabels(e config.ConnectorEntry, old config.Connector) config.ConnectorEntry {
	if d := strings.TrimSpace(old.Description); d != "" && !strings.HasPrefix(d, awsDescPrefix) {
		e.Description = old.Description
	}
	for _, a := range old.Aliases {
		if !slices.Contains(e.Aliases, a) {
			e.Aliases = append(e.Aliases, a)
		}
	}
	if old.Group != "" {
		e.Group = old.Group
	}
	return e
}

// awsDescription says which account, role and region a profile reaches, so an agent can
// match "the BBAD account" or an account number to the connector.
func awsDescription(p *awsconfig.Profile, readOnly bool) string {
	var b strings.Builder
	b.WriteString(awsDescPrefix)
	if p.AccountID != "" {
		b.WriteString(" " + p.AccountID)
	}
	var facts []string
	if p.Role != "" {
		facts = append(facts, "role "+p.Role)
	}
	if p.Region != "" {
		facts = append(facts, "region "+p.Region)
	}
	if len(facts) > 0 {
		b.WriteString(" (" + strings.Join(facts, ", ") + ")")
	}
	fmt.Fprintf(&b, " as profile %s (%s)", p.Name, p.Kind)
	if readOnly {
		b.WriteString(", read-only role")
	}
	return b.String()
}

func awsEntry(p *awsconfig.Profile, readOnly bool) config.ConnectorEntry {
	cmd := "uvx"
	// The tray may not have the shell's PATH, so write where uvx is now.
	if path, err := exec.LookPath("uvx"); err == nil {
		cmd = path
	}
	e := config.ConnectorEntry{
		Description: awsDescription(p, readOnly),
		Group:       "AWS",
		Tags:        []string{"aws"},
		Command:     cmd,
		Args:        awsProxyArgs(*p),
		// --profile picks the credentials; AWS_PROFILE also marks the connector as this
		// profile's for the dashboard.
		Env:        map[string]string{"AWS_PROFILE": p.Name},
		IdleUnload: "30m",
	}
	if p.AccountID != "" {
		e.Aliases = []string{p.AccountID}
	}
	if readOnly {
		e.ToolClasses = map[string]string{}
		for _, t := range awsReadTools {
			e.ToolClasses[t] = string(config.Read)
		}
	}
	return e
}
