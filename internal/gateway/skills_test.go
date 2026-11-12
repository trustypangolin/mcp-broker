package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/skillgen"
	"github.com/trustypangolin/mcp-broker/internal/testutil/fakemcp"
)

func toolNamed(c skillgen.Connector, name string) *skillgen.Tool {
	for i := range c.Tools {
		if c.Tools[i].Name == name {
			return &c.Tools[i]
		}
	}
	return nil
}

func TestAgentSkillsLive(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, func(cfg *config.Config) {
		cn := cfg.Connectors["fake"]
		cn.Aliases = []string{"pretend"}
		cfg.Connectors["fake"] = cn
	})
	conns, report := h.gw.AgentSkills(context.Background(), "coder", SkillOptions{})
	if len(conns) != 1 || len(report) != 1 || report[0].Source != "live" || report[0].Tools != len(conns[0].Tools) {
		t.Fatalf("report: %+v", report)
	}
	c := conns[0]
	if c.Name != "fake" || c.Description != "Fake downstream" || !slices.Equal(c.Aliases, []string{"pretend"}) || c.Direct != nil {
		t.Errorf("connector: %+v", c)
	}
	echo := toolNamed(c, "get_echo")
	if echo == nil || echo.Class != "read" || echo.Approval || !slices.Equal(echo.Required, []string{"text"}) || echo.Schema == nil || echo.Summary != "Echo text back" {
		t.Errorf("get_echo: %+v", echo)
	}
	if del := toolNamed(c, "delete_thing"); del == nil || !del.Approval || del.Class != "destructive" {
		t.Errorf("delete_thing: %+v", del)
	}
	// coder's role denies external: a denied tool isn't there at all.
	if toolNamed(c, "send_message") != nil {
		t.Error("a denied tool is in the skill")
	}
}

func TestAgentSkillsFallback(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, func(cfg *config.Config) {
		cn := cfg.Connectors["fake"]
		cn.Command = "definitely-not-a-program-xyz"
		cfg.Connectors["fake"] = cn
	})
	conns, _ := h.gw.AgentSkills(context.Background(), "coder", SkillOptions{})
	if c := conns[0]; c.Source != "none" || c.Err == "" || len(c.Tools) != 0 {
		t.Fatalf("never seen, won't start: %+v", c)
	}
	h.gw.catMu.Lock()
	h.gw.loadCatalogLocked()
	h.gw.catalog["fake"] = ToolList{Tools: []ToolMeta{{Name: "get_echo", Description: "Echo text back"}, {Name: "send_message", Description: "Pretend to send"}}}
	h.gw.catMu.Unlock()
	conns, _ = h.gw.AgentSkills(context.Background(), "coder", SkillOptions{})
	if c := conns[0]; c.Source != "cache" || len(c.Tools) != 1 || c.Tools[0].Name != "get_echo" || c.Tools[0].Schema != nil {
		t.Errorf("cached: %+v", c)
	}
}

func TestAgentSkillsFilterAndVisibility(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	if conns, _ := h.gw.AgentSkills(context.Background(), "outsider", SkillOptions{}); len(conns) != 0 {
		t.Errorf("outsider sees nothing, got %d", len(conns))
	}
	if conns, _ := h.gw.AgentSkills(context.Background(), "coder", SkillOptions{Connectors: []string{"other"}}); len(conns) != 0 {
		t.Errorf("filter to an unknown connector: %d", len(conns))
	}
}

// TestDirectSpecNoSecrets: the settings for reaching a server without the broker name the
// variables to set but never carry a secret value.
func TestDirectSpecNoSecrets(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "own.env")
	shared := filepath.Join(dir, "shared.env")
	os.WriteFile(own, []byte("OWN_SECRET=s3cret-own\nOWN_SETTING=1\n"), 0o600)
	os.WriteFile(shared, []byte("PROJECT=G:\\proj\nUNRELATED_KEY=s3cret-unrelated\n"), 0o600)
	h := newHarness(t, fakemcp.ModeCurrent, func(cfg *config.Config) {
		cfg.Connectors["local"] = config.Connector{
			Command: "C:\\Tools\\server.exe", Args: []string{"--root", "${PROJECT}\\out"}, EnvFile: shared,
			Env: map[string]string{"MODE": "fast", "API_TOKEN": "literal-should-not-appear"},
		}
		cfg.Connectors["other"] = config.Connector{Command: "x", EnvFile: shared} // makes shared.env shared
		cfg.Connectors["remote"] = config.Connector{
			URL: "https://example.test/mcp", EnvFile: own,
			Headers: map[string]string{"Authorization": "Bearer ${OWN_SECRET}", "X-Api-Key": "literal-header-secret"},
		}
		cfg.Connectors["signin"] = config.Connector{URL: "https://xero.test/mcp", OAuth: &config.OAuth{ClientID: "abc"}}
	})
	cfg := h.gw.st().cfg

	local := h.gw.directSpec(cfg, "local", cfg.Connectors["local"])
	remote := h.gw.directSpec(cfg, "remote", cfg.Connectors["remote"])
	signin := h.gw.directSpec(cfg, "signin", cfg.Connectors["signin"])
	all, _ := json.Marshal([]any{local, remote, signin})
	for _, secret := range []string{"s3cret-own", "s3cret-unrelated", "literal-should-not-appear", "literal-header-secret"} {
		if strings.Contains(string(all), secret) {
			t.Errorf("a secret value leaked: %s in %s", secret, all)
		}
	}

	if local.Transport != "stdio" || local.Args[1] != "${PROJECT}\\out" || local.Env["MODE"] != "fast" || local.Env["API_TOKEN"] != "${API_TOKEN}" {
		t.Errorf("local: %+v", local)
	}
	// shared.env is shared: only the variable this connector references comes along.
	if _, ok := local.Env["UNRELATED_KEY"]; ok || local.Env["PROJECT"] != "${PROJECT}" {
		t.Errorf("shared env file: %+v", local.Env)
	}
	if !slices.Equal(local.Vars, []string{"API_TOKEN", "PROJECT"}) {
		t.Errorf("local vars: %v", local.Vars)
	}

	// own.env is this connector's alone: every name in it comes along, values never.
	if remote.Transport != "http" || remote.Headers["Authorization"] != "Bearer ${OWN_SECRET}" || remote.Headers["X-Api-Key"] != "${REMOTE_X_API_KEY}" {
		t.Errorf("remote headers: %+v", remote.Headers)
	}
	if remote.Env["OWN_SETTING"] != "${OWN_SETTING}" || !slices.Equal(remote.Vars, []string{"OWN_SECRET", "OWN_SETTING", "REMOTE_X_API_KEY"}) {
		t.Errorf("remote: env %v vars %v", remote.Env, remote.Vars)
	}
	if !signin.OAuth || local.OAuth || remote.OAuth {
		t.Errorf("oauth flags: signin %v local %v remote %v", signin.OAuth, local.OAuth, remote.OAuth)
	}
}
