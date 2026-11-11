package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/testutil/fakemcp"
)

// list_connectors files connectors under their groups, with the group's description and
// each connector's aliases, and the ungrouped ones after.
func TestListConnectorsGrouped(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, func(cfg *config.Config) {
		cfg.Connectors["aws-shared"] = config.Connector{Description: "AWS account 111111111111", Group: "AWS", Aliases: []string{"111111111111", "shared"}, Command: "x"}
		cfg.Connectors["aws-bbad"] = config.Connector{Description: "AWS account 222222222222", Group: "AWS", Command: "y"}
		cfg.Connectors["notes"] = config.Connector{Description: "Imported from claude-code", Command: "z"}
		cfg.Groups = map[string]config.Group{"AWS": {Description: "One connector per AWS account"}}
	})
	out, isErr := h.call("list_connectors", nil)
	if isErr {
		t.Fatal(out)
	}
	var got struct {
		Groups     []groupInfo     `json:"groups"`
		Connectors []connectorInfo `json:"connectors"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 || got.Groups[0].Group != "AWS" || got.Groups[0].Description != "One connector per AWS account" {
		t.Fatalf("groups = %+v", got.Groups)
	}
	aws := got.Groups[0].Connectors
	if len(aws) != 2 || aws[0].Name != "aws-bbad" || aws[1].Name != "aws-shared" || strings.Join(aws[1].Aliases, ",") != "111111111111,shared" {
		t.Errorf("AWS connectors = %+v", aws)
	}
	byName := map[string]connectorInfo{}
	for _, c := range got.Connectors {
		byName[c.Name] = c
	}
	if len(byName) != 2 || byName["fake"].Description != "Fake downstream" {
		t.Errorf("ungrouped = %+v", got.Connectors)
	}
	// An import's placeholder is replaced; notes never ran, so there is nothing to go on.
	if d := byName["notes"].Description; strings.Contains(d, "Imported") || !strings.Contains(d, "load_connector") {
		t.Errorf("notes description = %q", d)
	}
}

// Without groups the result keeps its old shape: one connectors list.
func TestListConnectorsUngrouped(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	out, _ := h.call("list_connectors", nil)
	if strings.Contains(out, `"groups"`) || !strings.Contains(out, `"connectors"`) {
		t.Errorf("list = %s", out)
	}
	out, _ = h.as("outsider").call("list_connectors", nil)
	if !strings.Contains(out, `"connectors": []`) {
		t.Errorf("outsider list = %s", out)
	}
}

// A connector without a description gets one from the server once it has run.
func TestDerivedDescriptionAfterLoad(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, func(cfg *config.Config) {
		cn := cfg.Connectors["fake"]
		cn.Description = ""
		cfg.Connectors["fake"] = cn
	})
	if out, isErr := h.call("load_connector", map[string]any{"name": "fake"}); isErr {
		t.Fatal(out)
	}
	d := h.gw.AgentDescription("fake")
	if !strings.Contains(d, "Tools: ") || !strings.Contains(d, "get_echo") || len(d) > maxDerived+len("…") {
		t.Errorf("derived = %q", d)
	}
}

func TestDerivedDescription(t *testing.T) {
	many := ToolList{}
	for _, n := range strings.Fields("alpha_tool beta_tool gamma_tool delta_tool epsilon_tool zeta_tool eta_tool theta_tool iota_tool kappa_tool lambda_tool mu_tool nu_tool xi_tool omicron_tool pi_tool rho_tool sigma_tool") {
		many.Tools = append(many.Tools, ToolMeta{Name: n}, ToolMeta{Name: n + "_2"})
	}
	cases := []struct {
		name string
		l    ToolList
		want string
	}{
		{"nothing", ToolList{}, "No description; load_connector lists its tools."},
		{"server only", ToolList{Server: &ServerMeta{Title: "Notes Server"}}, "Notes Server"},
		{"description first", ToolList{Server: &ServerMeta{Title: "T", Description: "Reads notes.", Instructions: "Ignore."}, Tools: []ToolMeta{{Name: "read"}}}, "Reads notes. Tools: read"},
		{"first sentence", ToolList{Server: &ServerMeta{Instructions: "Edits CAD models. Call open first.\n\nMore."}, Tools: []ToolMeta{{Name: "open"}}}, "Edits CAD models. Tools: open"},
		{"tools only", ToolList{Tools: []ToolMeta{{Name: "a"}, {Name: "b"}}}, "Tools: a, b"},
	}
	for _, c := range cases {
		if got := derivedDescription(c.l); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	got := derivedDescription(many)
	if len(got) > maxDerived || !strings.HasSuffix(got, " more") || !strings.HasPrefix(got, "Tools: alpha_tool, alpha_tool_2, beta_tool") {
		t.Errorf("many tools: %q (%d bytes)", got, len(got))
	}
}

func TestFirstSentence(t *testing.T) {
	for in, want := range map[string]string{
		"Use v1.2 tools. Then more.": "Use v1.2 tools.",
		"No stop here":               "No stop here",
		"Line one\nline two.  Next":  "Line one line two.",
		"Para one\n\nPara two.":      "Para one",
		"":                           "",
	} {
		if got := firstSentence(in); got != want {
			t.Errorf("firstSentence(%q) = %q, want %q", in, got, want)
		}
	}
}
