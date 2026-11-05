package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

func TestConnectorSignIn(t *testing.T) {
	f := newFixture(t, strings.Replace(base, "connectors:\n", "connectors:\n  atlassian: {url: https://mcp.atlassian.com/v1/mcp}\n", 1))
	f.signIn()
	if code, out := f.do("POST", "/ui/api/connectors/atlassian/oauth/signin", nil, nil); code != 200 || !strings.HasPrefix(out["url"].(string), "https://") {
		t.Errorf("signin: %d %v", code, out)
	}
	// fake is local: refused with the backend's reason.
	if code, out := f.do("POST", "/ui/api/connectors/fake/oauth/signin", nil, nil); code != 400 || out["error"] == nil {
		t.Errorf("local signin: %d %v", code, out)
	}
	if code, _ := f.do("POST", "/ui/api/connectors/fake/oauth/bogus", nil, nil); code != 404 {
		t.Errorf("unknown action: %d", code)
	}
	if code, _ := f.do("POST", "/ui/api/connectors/fake/oauth/signin", nil, map[string]string{"X-Mcpb": ""}); code == 200 {
		t.Error("signin without the CSRF header")
	}
}

// TestFreshConfigPages loads every page's data on the first-run config (agents and roles,
// no connectors). The pages read .length on these lists, so null breaks them.
func TestFreshConfigPages(t *testing.T) {
	p := filepath.Join(t.TempDir(), "broker.yaml")
	if err := config.WriteStarter(p); err != nil {
		t.Fatal(err)
	}
	starter, _ := os.ReadFile(p)
	f := newFixture(t, string(starter))
	f.signIn()

	code, out := f.do("GET", "/ui/api/agents", nil, nil)
	agents, _ := out["agents"].([]any)
	if code != 200 || len(agents) == 0 {
		t.Fatalf("agents: %d %v", code, out)
	}
	for _, a := range agents {
		a := a.(map[string]any)
		for _, k := range []string{"visible", "tokens"} {
			if _, ok := a[k].([]any); !ok {
				t.Errorf("agent %v: %s = %v, want a list", a["name"], k, a[k])
			}
		}
	}
	for _, r := range out["roles"].([]any) {
		if _, ok := r.(map[string]any)["connectors"].([]any); !ok {
			t.Errorf("role %v: connectors isn't a list", r)
		}
	}

	name := agents[0].(map[string]any)["name"].(string)
	for _, path := range []string{"status", "approvals", "connectors", "profiles", "config", "tree", "tree?agent=" + name} {
		if code, out := f.do("GET", "/ui/api/"+path, nil, nil); code != 200 {
			t.Errorf("%s: %d %v", path, code, out)
		}
	}
	if code, out := f.do("GET", "/ui/api/tree?agent="+name, nil, nil); code == 200 {
		if _, ok := out["connectors"].([]any); !ok {
			t.Errorf("tree connectors = %v, want a list", out["connectors"])
		}
	}
}
